package mmu

import (
	"fmt"
	"log"
	"reflect"

	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/akita/v4/tracing"
)

type transaction struct {
	req       *vm.TranslationReq
	page      vm.Page
	cycleLeft int
	migration *vm.PageMigrationReqToDriver
	nextPT    vm.LevelPageTable
	found     bool
}

// Comp is the default mmu implementation. It is also an akita Component.
type Comp struct {
	sim.TickingComponent
	sim.MiddlewareHolder

	topPort       sim.Port
	migrationPort sim.Port
	topPortBuf    []sim.Msg

	MigrationServiceProvider sim.RemotePort

	pageTable           vm.LevelPageTable
	latency             int
	maxRequestsInFlight int
	autoPageAllocation  bool
	log2PageSize        uint64

	walkingTranslations      []transaction
	migrationQueue           []transaction
	migrationQueueSize       int
	invalidationQueue        []transaction
	invalidationQueueSize    int
	duplicationQueue         []transaction
	duplicationQueueSize     int
	currentOnDemandMigration transaction
	isDoingMigration         bool

	toRemoveFromPTW        []int
	PageAccessedByDeviceID map[uint64][]uint64

	// Physical page allocation tracking for auto page allocation
	nextPhysicalPage uint64

	pageTableCache      vm.UPTC
	pageMigrationPolicy uint64
}

func (c *Comp) Tick() bool {
	return c.MiddlewareHolder.Tick()
}

type middleware struct {
	*Comp
}

// Tick defines how the MMU update state each cycle
func (m *middleware) Tick() bool {
	madeProgress := false

	madeProgress = m.sendMigrationToDriver() || madeProgress
	madeProgress = m.sendInvalidationToDriver() || madeProgress
	madeProgress = m.sendDuplicationToDriver() || madeProgress
	madeProgress = m.walkPageTable() || madeProgress
	madeProgress = m.parseMigrationPort() || madeProgress
	madeProgress = m.parseFromTop() || madeProgress

	return madeProgress
}

func (m *middleware) parseMigrationPort() bool {
	item := m.migrationPort.PeekIncoming()
	if item == nil {
		return false
	}

	switch req := item.(type) {
	case *vm.PageMigrationRspFromDriver:
		return m.processMigrationReturn()
	case *vm.RemoveRequestInMMUFromDriver:
		return m.removeInFlightRequests()
	default:
		log.Panicf("MMU cannot handle request of type %s",
			reflect.TypeOf(req))
	}

	return false
}

func (m *middleware) walkPageTable() bool {
	madeProgress := false

	stageLatency := m.latency / 5
	ptcLatency := m.pageTableCache.Latency
	if ptcLatency >= stageLatency {
		panic(fmt.Sprintf("[GMMU]\tPTC latency(%d) can't be larger than page walk stage latency(%d)", ptcLatency, stageLatency))
	}

	for i := 0; i < len(m.walkingTranslations); i++ {
		trans := &(m.walkingTranslations[i])
		cycleLeft := trans.cycleLeft

		if cycleLeft > 0 {
			if cycleLeft%stageLatency == 0 {
				m.pageTableWalk(trans)
			} else {
				trans.cycleLeft--
			}

			madeProgress = true

			continue
		}

		// page table cache 탐색 완료
		req := trans.req
		found := trans.found
		nextPT := trans.nextPT
		if found {
			_, f := nextPT.Find(req.PID, req.VAddr)
			// last level page table에서 hit인지 확인
			found = found && f
		}

		madeProgress = m.finalizePageWalk(i) || madeProgress
	}

	tmp := m.walkingTranslations[:0]

	for i := 0; i < len(m.walkingTranslations); i++ {
		if !m.toRemove(i) {
			tmp = append(tmp, m.walkingTranslations[i])
		}
	}

	m.walkingTranslations = tmp
	m.toRemoveFromPTW = nil

	// if len(m.walkingTranslations) > 0 {
	// 	fmt.Printf("[%s]\tWalking Translations in PTW: ", m.Name())
	// 	for i := 0; i < len(m.walkingTranslations); i++ {
	// 		req := m.walkingTranslations[i].req
	// 		fmt.Printf("%d %x | ", req.DeviceID, req.VAddr)
	// 	}
	// 	fmt.Printf("\n")
	// }

	return madeProgress
}

func (m *middleware) pageTableWalk(trans *transaction) {
	// 현재까지 walking 한 정보를 기반으로 다음 page table의 주소를 PTC와 pagetable에서 찾아 nextPT에 저장
	// pagetable에서 miss 발생 시, walking 완료 후 false 반환
	stageLatency := m.latency / 5
	ptcLatency := m.pageTableCache.Latency

	req := trans.req
	pid := req.PID
	vAddr := req.VAddr

	if trans.nextPT == nil {
		// L5 page table base address 참조: 실제 GPU에서는 CR3에 저장되어 있으므로? 메모리 접근 필요 없음
		nextPT, found := m.pageTable.FindStepByStep(pid, vAddr)
		if !found {
			trans.found = false
			trans.cycleLeft = 0
			return
		}

		trans.found = true
		trans.nextPT = nextPT
	}

	if trans.nextPT.GetLevel() == 1 {
		// PTC로 L1은 참조하지 않음
		trans.found = true
		trans.cycleLeft--
		return
	}

	index := vm.AddrSlice(vAddr, trans.nextPT.GetLevel(), m.log2PageSize)
	nextPT, _ := m.pageTableCache.Find(trans.nextPT.GetName(), index)
	if nextPT != nil {
		// page table cache에서 찾은 경우
		trans.found = true
		trans.nextPT = *nextPT
		trans.cycleLeft = trans.cycleLeft - stageLatency + ptcLatency
		return
	}

	PT, found := trans.nextPT.FindStepByStep(pid, vAddr)
	if !found {
		// page table에 없는 경우 -> local page fault
		trans.found = false
		trans.cycleLeft = 0
		return
	}

	// page table에서 찾은 경우
	m.pageTableCache.Insert(trans.nextPT.GetName(), index, &PT, nil)

	trans.found = true
	trans.cycleLeft--
	trans.nextPT = PT

	return
}

func (m *middleware) finalizePageWalk(
	walkingIndex int,
) bool {
	req := m.walkingTranslations[walkingIndex].req
	page, found := m.pageTable.Find(req.PID, req.VAddr)

	if !found {
		if m.autoPageAllocation {
			page = m.createDefaultPage(req.PID, req.VAddr, req.DeviceID)
			m.pageTable.Insert(page)
		} else {
			panic("page not found")
		}
	}

	m.walkingTranslations[walkingIndex].page = page

	if page.IsMigrating {
		return false
	}

	if m.pageNeedMigrate(m.walkingTranslations[walkingIndex]) {
		// fmt.Printf("[MMU]\tGPU %d needs migration for VA %x\n", m.walkingTranslations[walkingIndex].req.DeviceID, m.walkingTranslations[walkingIndex].req.VAddr)
		return m.addTransactionToMigrationQueue(walkingIndex)
	}
	if m.pageNeedDuplication(m.walkingTranslations[walkingIndex]) {
		// fmt.Printf("[MMU]\tGPU %d needs duplication for VA %x\n", m.walkingTranslations[walkingIndex].req.DeviceID, m.walkingTranslations[walkingIndex].req.VAddr)
		return m.addTransactionToDuplicationQueue(walkingIndex)
	}
	if m.pageNeedInvalidation(m.walkingTranslations[walkingIndex]) {
		// fmt.Printf("[MMU]\tGPU %d needs invalidation for VA %x\n", m.walkingTranslations[walkingIndex].req.DeviceID, m.walkingTranslations[walkingIndex].req.VAddr)
		return m.addTransactionToInvalidationQueue(walkingIndex)
	}

	return m.doPageWalkHit(walkingIndex)
}

func (m *middleware) addTransactionToMigrationQueue(walkingIndex int) bool {
	if len(m.migrationQueue) >= m.migrationQueueSize {
		return false
	}

	m.toRemoveFromPTW = append(m.toRemoveFromPTW, walkingIndex)
	m.migrationQueue = append(m.migrationQueue,
		m.walkingTranslations[walkingIndex])

	page := m.walkingTranslations[walkingIndex].page
	page.IsMigrating = true
	m.pageTable.Update(page)

	return true
}

func (m *middleware) addTransactionToDuplicationQueue(walkingIndex int) bool {
	if len(m.duplicationQueue) >= m.duplicationQueueSize {
		return false
	}

	m.toRemoveFromPTW = append(m.toRemoveFromPTW, walkingIndex)
	m.duplicationQueue = append(m.duplicationQueue,
		m.walkingTranslations[walkingIndex])

	page := m.walkingTranslations[walkingIndex].page
	page.IsMigrating = true
	m.pageTable.Update(page)

	return true
}

func (m *middleware) addTransactionToInvalidationQueue(walkingIndex int) bool {
	if len(m.invalidationQueue) >= m.invalidationQueueSize {
		return false
	}

	m.toRemoveFromPTW = append(m.toRemoveFromPTW, walkingIndex)
	m.invalidationQueue = append(m.invalidationQueue, m.walkingTranslations[walkingIndex])

	page := m.walkingTranslations[walkingIndex].page
	page.IsMigrating = true
	m.pageTable.Update(page)

	return true
}

func (m *middleware) pageNeedMigrate(walking transaction) bool {
	if walking.page.DeviceID == 1 && walking.req.DeviceID != 1 { // first touch
		return true
	}

	if m.Comp.pageMigrationPolicy != 0 {
		return false
	}

	if walking.req.DeviceID == walking.page.DeviceID {
		return false
	}

	if !walking.page.Unified {
		return false
	}

	if walking.page.IsPinned {
		fmt.Printf("[%s]\t[Warning]\tPage is pinned VA %x, Device %d\n", m.Name(), walking.page.VAddr, walking.page.DeviceID)
		return false
	}

	if walking.req.ForMigration {
		return true
	}

	return false
}

func (m *middleware) pageNeedDuplication(walking transaction) bool {
	if m.Comp.pageMigrationPolicy != 1 {
		return false
	}

	_, f := m.GetSharedPageByDeviceID(walking.page, walking.req.DeviceID)
	if f {
		return false
	}

	if !walking.page.Unified {
		return false
	}

	if walking.page.IsPinned {
		fmt.Printf("[%s]\t[Warning]\tPage is pinned VA %x, Device %d\n", m.Name(), walking.page.VAddr, walking.page.DeviceID)
		return false
	}

	return true
}

func (m *middleware) pageNeedInvalidation(walking transaction) bool {
	if walking.req.ForInvalidation {
		return true
	}

	if walking.req.DeviceID == walking.page.DeviceID && !walking.page.IsShared {
		return false
	}
	// 소유 GPU가 아니거나, 다른 GPU끼리 공유되는 경우에만 invalidation 필요

	if !walking.req.ForWrite {
		return false
	}

	if !walking.page.Unified {
		return false
	}

	if walking.page.IsPinned {
		fmt.Printf("[%s]\t[Warning]\tPage is pinned VA %x, Device %d\n", m.Name(), walking.page.VAddr, walking.page.DeviceID)
		return false
	}

	return true
}

func (m *middleware) doPageWalkHit(
	walkingIndex int,
) bool {
	if !m.topPort.CanSend() {
		return false
	}

	walking := m.walkingTranslations[walkingIndex]

	setMigrationRsp := false
	if walking.req.ForMigration {
		setMigrationRsp = true
	}

	page, f := m.GetSharedPageByDeviceID(walking.page, walking.req.DeviceID)
	if !f {
		page = walking.page
	}
	rsp := vm.TranslationRspBuilder{}.
		WithSrc(m.topPort.AsRemote()).
		WithDst(walking.req.Src).
		WithRspTo(walking.req.ID).
		WithPage(page).
		SetIsMigrationRsp(setMigrationRsp).
		WithOriginalReq(*walking.req).
		Build()

	m.topPort.Send(rsp)
	m.toRemoveFromPTW = append(m.toRemoveFromPTW, walkingIndex)
	m.PageAccessedByDeviceID[walking.page.VAddr] = append(m.PageAccessedByDeviceID[walking.req.VAddr], walking.req.DeviceID)
	m.PageAccessedByDeviceID[walking.page.VAddr] = unique(m.PageAccessedByDeviceID[walking.page.VAddr])

	tracing.TraceReqComplete(walking.req, m.Comp)

	return true
}

func (m *middleware) sendMigrationToDriver() (madeProgress bool) {
	if len(m.migrationQueue) == 0 {
		return false
	}

	trans := m.migrationQueue[0]
	req := trans.req
	page, found := m.pageTable.Find(req.PID, req.VAddr)

	if !found {
		panic("page not found")
	}

	trans.page = page

	if req.DeviceID == page.DeviceID || page.IsPinned {
		if !m.topPort.CanSend() {
			return false
		}

		m.sendTranslationRsp(trans)
		m.migrationQueue = m.migrationQueue[1:]
		m.markPageAsNotMigratingIfNotInTheMigrationQueue(page)

		return true
	}

	if m.isDoingMigration {
		return false
	}

	migrationReq := m.createMigrationRequest(trans, page)

	err := m.migrationPort.Send(migrationReq)
	if err != nil {
		return false
	}
	// fmt.Printf("[MMU]\tGPU %d sent migration request to Driver for VA %x from GPU %d to GPU %d\n",
	// 	req.DeviceID, req.VAddr, page.DeviceID, req.DeviceID)

	trans.page.IsMigrating = true
	m.pageTable.Update(trans.page)
	trans.migration = migrationReq
	m.isDoingMigration = true
	m.currentOnDemandMigration = trans
	m.migrationQueue = m.migrationQueue[1:]

	return true
}

func (m *middleware) sendInvalidationToDriver() (madeProgress bool) {
	if len(m.invalidationQueue) == 0 {
		return false
	}

	trans := m.invalidationQueue[0]
	req := trans.req
	page, found := m.pageTable.Find(req.PID, req.VAddr)

	if !found {
		panic("page not found")
	}

	trans.page = page

	if req.DeviceID == page.DeviceID || !page.IsShared {
		if !m.topPort.CanSend() {
			return false
		}

		m.sendTranslationRsp(trans)
		m.invalidationQueue = m.invalidationQueue[1:]
		m.markPageAsNotInvalidatingIfNotInTheInvalidationQueue(page)

		return true
	}

	if m.isDoingMigration {
		return false
	}

	migrationReq := m.createInvalidationRequest(trans, page)

	err := m.migrationPort.Send(migrationReq)
	if err != nil {
		return false
	}

	trans.page.IsMigrating = true
	m.pageTable.Update(trans.page)
	trans.migration = migrationReq
	m.isDoingMigration = true
	m.currentOnDemandMigration = trans
	m.invalidationQueue = m.invalidationQueue[1:]

	return true
}

func (m *middleware) sendDuplicationToDriver() (madeProgress bool) {
	if len(m.duplicationQueue) == 0 {
		return false
	}

	trans := m.duplicationQueue[0]
	req := trans.req
	page, found := m.pageTable.Find(req.PID, req.VAddr)

	if !found {
		panic("page not found")
	}

	trans.page = page

	_, f := m.GetSharedPageByDeviceID(page, req.DeviceID)
	if page.IsPinned || f {
		if !m.topPort.CanSend() {
			return false
		}

		m.sendTranslationRsp(trans)
		m.duplicationQueue = m.duplicationQueue[1:]
		m.markPageAsNotDuplicatingIfNotInTheDuplicationQueue(page)

		return true
	}

	if m.isDoingMigration {
		return false
	}

	duplicationReq := m.createDuplicationRequest(trans, page)

	err := m.migrationPort.Send(duplicationReq)
	if err != nil {
		return false
	}

	trans.page.IsMigrating = true
	m.pageTable.Update(trans.page)
	trans.migration = duplicationReq
	m.isDoingMigration = true
	m.currentOnDemandMigration = trans
	m.duplicationQueue = m.duplicationQueue[1:]

	return true
}

func (m *middleware) markPageAsNotMigratingIfNotInTheMigrationQueue(
	page vm.Page,
) vm.Page {
	inQueue := false

	for _, t := range m.migrationQueue {
		if page.PAddr == t.page.PAddr {
			inQueue = true
			break
		}
	}

	if !inQueue {
		page.IsMigrating = false
		m.pageTable.Update(page)

		return page
	}

	return page
}

func (m *middleware) markPageAsNotDuplicatingIfNotInTheDuplicationQueue(
	page vm.Page,
) vm.Page {
	inQueue := false

	for _, t := range m.duplicationQueue {
		if page.PAddr == t.page.PAddr {
			inQueue = true
			break
		}
	}

	if !inQueue {
		page, _ = m.pageTable.Find(page.PID, page.VAddr)
		page.IsMigrating = false
		m.pageTable.Update(page)

		return page
	}

	return page
}

func (m *middleware) markPageAsNotInvalidatingIfNotInTheInvalidationQueue(
	page vm.Page,
) vm.Page {
	inQueue := false

	for _, t := range m.invalidationQueue {
		if page.PAddr == t.page.PAddr {
			inQueue = true
			break
		}
	}

	if !inQueue {
		page.IsMigrating = false
		m.pageTable.Update(page)

		return page
	}

	return page
}

func (m *middleware) sendTranslationRsp(
	trans transaction,
) (madeProgress bool) {
	req := trans.req
	page := trans.page
	page, _ = m.GetSharedPageByDeviceID(page, req.DeviceID)

	rsp := vm.TranslationRspBuilder{}.
		WithSrc(m.topPort.AsRemote()).
		WithDst(req.Src).
		WithRspTo(req.ID).
		WithPage(page).
		WithOriginalReq(*req).
		Build()
	m.topPort.Send(rsp)

	return true
}

func (m *middleware) removeInFlightRequests() bool {
	CurAccessingGPUs := m.currentOnDemandMigration.migration.CurrAccessingGPUs
	req := m.currentOnDemandMigration.req
	ReqVA := req.VAddr

	for i := 0; i < len(m.migrationQueue); i++ {
		for j := 0; j < len(CurAccessingGPUs); j++ {
			if CurAccessingGPUs[j] == req.DeviceID {
				continue
			}

			if m.migrationQueue[i].req.DeviceID == CurAccessingGPUs[j] && m.migrationQueue[i].req.VAddr == ReqVA {
				VA := m.migrationQueue[i].page.VAddr
				pid := m.migrationQueue[i].req.PID
				page, found := m.pageTable.Find(pid, VA)
				if !found {
					panic("[MMU] page not found")
				}
				page.IsMigrating = false
				m.pageTable.Update(page)

				m.migrationQueue = append(m.migrationQueue[:i], m.migrationQueue[i+1:]...)
				i--
				break
			}
		}
	}

	for i := 0; i < len(m.invalidationQueue); i++ {
		for j := 0; j < len(CurAccessingGPUs); j++ {
			if CurAccessingGPUs[j] == req.DeviceID {
				continue
			}

			if m.invalidationQueue[i].req.DeviceID == CurAccessingGPUs[j] && m.invalidationQueue[i].req.VAddr == ReqVA {
				VA := m.invalidationQueue[i].page.VAddr
				pid := m.invalidationQueue[i].req.PID
				page, found := m.pageTable.Find(pid, VA)
				if !found {
					panic("[MMU] page not found")
				}
				page.IsMigrating = false
				m.pageTable.Update(page)

				m.invalidationQueue = append(m.invalidationQueue[:i], m.invalidationQueue[i+1:]...)
				i--
				break
			}
		}
	}

	for i := 0; i < len(m.duplicationQueue); i++ {
		for j := 0; j < len(CurAccessingGPUs); j++ {
			if CurAccessingGPUs[j] == req.DeviceID {
				continue
			}

			if m.duplicationQueue[i].req.DeviceID == CurAccessingGPUs[j] && m.duplicationQueue[i].req.VAddr == ReqVA {
				VA := m.duplicationQueue[i].page.VAddr
				pid := m.duplicationQueue[i].req.PID
				page, found := m.pageTable.Find(pid, VA)
				if !found {
					panic("[MMU] page not found")
				}
				page.IsMigrating = false
				m.pageTable.Update(page)

				m.duplicationQueue = append(m.duplicationQueue[:i], m.duplicationQueue[i+1:]...)
				i--
				break
			}
		}
	}

	for i := 0; i < len(m.walkingTranslations); i++ {
		for j := 0; j < len(CurAccessingGPUs); j++ {
			if CurAccessingGPUs[j] == req.DeviceID {
				continue
			}

			if m.walkingTranslations[i].req.DeviceID == CurAccessingGPUs[j] && m.walkingTranslations[i].req.VAddr == ReqVA {
				m.walkingTranslations = append(m.walkingTranslations[:i], m.walkingTranslations[i+1:]...)
				i--
				break
			}
		}
	}

	rsp := vm.NewRemoveRequestRspFromMMU(
		m.migrationPort.AsRemote(),
		m.MigrationServiceProvider,
	)
	err := m.migrationPort.Send(rsp)
	if err != nil {
		fmt.Printf("[MMU]\t[Warning]\tUnable to send RemoveRequestRspFromMMU")
		return false
	}

	for i := 0; i < len(m.topPortBuf); i++ {
		for j := 0; j < len(CurAccessingGPUs); j++ {
			if CurAccessingGPUs[j] == req.DeviceID {
				continue
			}

			translationReq, ok := m.topPortBuf[i].(*vm.TranslationReq)
			if !ok || translationReq == nil {
				m.topPortBuf = append(m.topPortBuf[:i], m.topPortBuf[i+1:]...)
				i--
				break
			}

			if translationReq.DeviceID == CurAccessingGPUs[j] {
				m.topPortBuf = append(m.topPortBuf[:i], m.topPortBuf[i+1:]...)
				i--
				break
			}
		}
	}

	// remove requests in topPort from other GPUs which is to be reset
	// Re-add the requests that are not from the resetting GPUs back to the topPortBuf
	newReq := m.topPort.RetrieveIncoming()
	for newReq != nil {
		for _, gpuID := range CurAccessingGPUs {
			if gpuID == req.DeviceID {
				continue
			}

			if gpuID == newReq.(*vm.TranslationReq).DeviceID {
				newReq = m.topPort.RetrieveIncoming()
				break
			}
		}

		m.topPortBuf = append(m.topPortBuf, newReq)
		newReq = m.topPort.RetrieveIncoming()
	}

	m.migrationPort.RetrieveIncoming()
	return true
}

func (m *middleware) processMigrationReturn() bool {
	if !m.topPort.CanSend() {
		return false
	}

	req := m.currentOnDemandMigration.req
	page, found := m.pageTable.Find(req.PID, req.VAddr)

	if !found {
		panic("page not found")
	}

	page, _ = m.GetSharedPageByDeviceID(page, req.DeviceID)

	rsp := vm.TranslationRspBuilder{}.
		WithSrc(m.topPort.AsRemote()).
		WithDst(req.Src).
		WithRspTo(req.ID).
		WithPage(page).
		WithOriginalReq(*req).
		Build()
	m.topPort.Send(rsp)

	if !m.currentOnDemandMigration.migration.ForDuplication {
		m.PageAccessedByDeviceID[req.VAddr] = nil
	}
	m.PageAccessedByDeviceID[req.VAddr] = append(m.PageAccessedByDeviceID[req.VAddr], req.DeviceID)
	m.isDoingMigration = false

	if m.currentOnDemandMigration.migration.ForDuplication {
		page = m.markPageAsNotDuplicatingIfNotInTheDuplicationQueue(page)
	} else if m.currentOnDemandMigration.migration.ForInvalidation {
		page = m.markPageAsNotInvalidatingIfNotInTheInvalidationQueue(page)
	} else {
		page = m.markPageAsNotMigratingIfNotInTheMigrationQueue(page)
	}

	m.migrationPort.RetrieveIncoming()

	return true
}

func (m *middleware) parseFromTop() bool {
	if len(m.walkingTranslations) >= m.maxRequestsInFlight {
		return false
	}

	req := sim.Msg(nil)
	if len(m.topPortBuf) > 0 {
		req = m.topPortBuf[0]
		m.topPortBuf = m.topPortBuf[1:]
	} else {
		req = m.topPort.RetrieveIncoming()
	}
	if req == nil {
		return false
	}

	tracing.TraceReqReceive(req, m.Comp)

	switch req := req.(type) {
	case *vm.TranslationReq:
		m.startWalking(req)
	default:
		log.Panicf("MMU canot handle request of type %s", reflect.TypeOf(req))
	}

	return true
}

func (m *middleware) startWalking(req *vm.TranslationReq) {
	translationInPipeline := transaction{
		req:       req,
		cycleLeft: m.latency,
		nextPT:    nil,
		found:     true,
	}

	m.walkingTranslations = append(m.walkingTranslations, translationInPipeline)
}

func (m *middleware) toRemove(index int) bool {
	for i := 0; i < len(m.toRemoveFromPTW); i++ {
		remove := m.toRemoveFromPTW[i]
		if remove == index {
			return true
		}
	}

	return false
}

func unique(intSlice []uint64) []uint64 {
	keys := make(map[int]bool)
	list := []uint64{}

	for _, entry := range intSlice {
		if _, value := keys[int(entry)]; !value {
			keys[int(entry)] = true

			list = append(list, entry)
		}
	}

	return list
}

func (m *middleware) createDefaultPage(pid vm.PID, vAddr uint64, deviceID uint64) vm.Page {
	alignedVAddr := (vAddr >> m.log2PageSize) << m.log2PageSize
	pageSize := uint64(1) << m.log2PageSize
	pAddr := m.allocatePhysicalPage()

	return vm.Page{
		PID:         pid,
		VAddr:       alignedVAddr,
		PAddr:       pAddr,
		PageSize:    pageSize,
		Valid:       true,
		DeviceID:    deviceID,
		Unified:     true,
		IsMigrating: false,
		IsPinned:    false,
	}
}

func (m *middleware) allocatePhysicalPage() uint64 {
	pageSize := uint64(1) << m.log2PageSize

	for {
		candidatePage := (m.nextPhysicalPage >> m.log2PageSize) << m.log2PageSize

		if _, found := m.pageTable.ReverseLookup(candidatePage); !found {
			m.nextPhysicalPage = candidatePage + pageSize
			return candidatePage
		}

		m.nextPhysicalPage += pageSize
	}
}

func (m *middleware) createMigrationRequest(
	trans transaction,
	page vm.Page,
) *vm.PageMigrationReqToDriver {
	migrationInfo := new(vm.PageMigrationInfo)
	migrationInfo.GPUReqToVAddrMap = make(map[uint64][]uint64)
	migrationInfo.GPUReqToVAddrMap[trans.req.DeviceID] =
		append(migrationInfo.GPUReqToVAddrMap[trans.req.DeviceID],
			trans.req.VAddr)

	m.PageAccessedByDeviceID[page.VAddr] =
		append(m.PageAccessedByDeviceID[page.VAddr], page.DeviceID)

	migrationReq := vm.NewPageMigrationReqToDriver(
		m.migrationPort.AsRemote(), m.MigrationServiceProvider)
	migrationReq.PID = page.PID
	migrationReq.PageSize = page.PageSize
	migrationReq.CurrPageHostGPU = page.DeviceID
	migrationReq.MigrationInfo = migrationInfo
	migrationReq.CurrAccessingGPUs = unique(
		m.PageAccessedByDeviceID[page.VAddr])
	migrationReq.RespondToTop = true
	migrationReq.RequestingDevice = trans.req.DeviceID

	return migrationReq
}

func (m *middleware) createInvalidationRequest(
	trans transaction,
	page vm.Page,
) *vm.PageMigrationReqToDriver {
	migrationInfo := new(vm.PageMigrationInfo)
	migrationInfo.GPUReqToVAddrMap = make(map[uint64][]uint64)
	migrationInfo.GPUReqToVAddrMap[trans.req.DeviceID] =
		append(migrationInfo.GPUReqToVAddrMap[trans.req.DeviceID],
			trans.req.VAddr)

	m.PageAccessedByDeviceID[page.VAddr] =
		append(m.PageAccessedByDeviceID[page.VAddr], page.DeviceID)

	migrationReq := vm.NewPageMigrationReqToDriver(
		m.migrationPort.AsRemote(), m.MigrationServiceProvider)
	migrationReq.PID = page.PID
	migrationReq.PageSize = page.PageSize
	migrationReq.CurrPageHostGPU = page.DeviceID
	migrationReq.MigrationInfo = migrationInfo
	migrationReq.CurrAccessingGPUs = unique(
		m.PageAccessedByDeviceID[page.VAddr])
	migrationReq.RespondToTop = true
	migrationReq.RequestingDevice = trans.req.DeviceID
	migrationReq.ForInvalidation = true

	return migrationReq
}

func (m *middleware) createDuplicationRequest(
	trans transaction,
	page vm.Page,
) *vm.PageMigrationReqToDriver {
	migrationInfo := new(vm.PageMigrationInfo)
	migrationInfo.GPUReqToVAddrMap = make(map[uint64][]uint64)
	migrationInfo.GPUReqToVAddrMap[trans.req.DeviceID] =
		append(migrationInfo.GPUReqToVAddrMap[trans.req.DeviceID],
			trans.req.VAddr)

	m.PageAccessedByDeviceID[page.VAddr] =
		append(m.PageAccessedByDeviceID[page.VAddr], page.DeviceID)

	migrationReq := vm.NewPageMigrationReqToDriver(
		m.migrationPort.AsRemote(), m.MigrationServiceProvider)
	migrationReq.PID = page.PID
	migrationReq.PageSize = page.PageSize
	migrationReq.CurrPageHostGPU = page.DeviceID
	migrationReq.MigrationInfo = migrationInfo
	migrationReq.CurrAccessingGPUs = unique(
		m.PageAccessedByDeviceID[page.VAddr])
	migrationReq.RespondToTop = true
	migrationReq.RequestingDevice = trans.req.DeviceID
	migrationReq.ForDuplication = true

	return migrationReq
}

func (m *middleware) GetSharedPageByDeviceID(page vm.Page, deviceID uint64) (vm.Page, bool) {
	if page.DeviceID == deviceID {
		return page, true
	}

	for _, pg := range page.SharedPages {
		if pg.DeviceID == deviceID {
			return pg, true
		}
	}

	return vm.Page{}, false
}
