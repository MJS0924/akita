package gmmu

import (
	"fmt"
	"log"
	"reflect"

	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/mem/vm/tlb"
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

type Comp struct {
	sim.TickingComponent
	sim.MiddlewareHolder

	deviceID uint64

	topPort     sim.Port
	bottomPort  sim.Port
	controlPort sim.Port
	LowModule   sim.RemotePort

	topSender    sim.BufferedSender
	bottomSender sim.BufferedSender

	log2PageSize        uint64
	pageTable           vm.LevelPageTable
	pageTableCache      vm.UPTC
	latency             int
	maxRequestsInFlight int

	walkingTranslations []transaction
	remoteMemReqs       map[uint64]transaction
	migrationReqs       map[uint64]transaction
	invalidationReqs    map[uint64]transaction

	toRemoveFromPTW        []int
	PageAccessedByDeviceID map[uint64][]uint64

	isFlushing bool

	AccessCounter *map[vm.PID]map[uint64]uint8
	dirtyMask     *[]map[vm.PID]map[uint64][]uint8
	readMask      *[]map[vm.PID]map[uint64][]uint8

	pageMigrationPolicy uint64
}

func (gmmu *Comp) Tick() bool {
	madeProgress := false

	if !gmmu.isFlushing {
		madeProgress = gmmu.topSender.Tick() || madeProgress
	}
	madeProgress = gmmu.parseFromTop() || madeProgress
	madeProgress = gmmu.walkPageTable() || madeProgress
	madeProgress = gmmu.fetchFromBottom() || madeProgress
	madeProgress = gmmu.parseFromControl() || madeProgress

	return madeProgress
}

func (gmmu *Comp) parseFromTop() bool {
	if gmmu.isFlushing {
		return false
	}

	if len(gmmu.walkingTranslations) >= gmmu.maxRequestsInFlight {
		return false
	}

	req := gmmu.topPort.PeekIncoming()
	if req == nil {
		return false
	}

	tracing.TraceReqReceive(req, gmmu)

	switch req := req.(type) {
	case *vm.TranslationReq:
		// fmt.Printf("[%s]\tReceived Translation Request from Top for VA %x, PID %d, ForMigration %t, ForInvalidation %t, ForWrite %t\n",
		// 	gmmu.Name(), req.VAddr, req.PID, req.ForMigration, req.ForInvalidation, req.ForWrite)
		if req.ForMigration {
			gmmu.sendMigrationReq(req)
		} else if req.ForInvalidation {
			gmmu.sendInvalidationReq(req)
		} else {
			gmmu.startWalking(req)
		}
	default:
		log.Panicf("GMMU canot handle request of type %s", reflect.TypeOf(req))
	}

	return true
}

func (gmmu *Comp) startWalking(req *vm.TranslationReq) {
	translationInPipeline := transaction{
		req:       req,
		cycleLeft: gmmu.latency,
		nextPT:    nil,
		found:     true,
	}

	gmmu.walkingTranslations = append(gmmu.walkingTranslations, translationInPipeline)
	gmmu.topPort.RetrieveIncoming()
}

func (gmmu *Comp) sendMigrationReq(req *vm.TranslationReq) {
	_, found := gmmu.pageTable.Find(req.PID, req.VAddr)
	if found {
		gmmu.pageTable.Remove(req.PID, req.VAddr)
	}

	migReq := vm.TranslationReqBuilder{}.
		WithSrc(gmmu.bottomPort.AsRemote()).
		WithDst(gmmu.LowModule).
		WithPID(req.PID).
		WithVAddr(req.VAddr).
		WithDeviceID(req.DeviceID).
		SetForMigration(true).
		SetForWrite(req.ForWrite).
		Build()

	err := gmmu.bottomPort.Send(migReq)
	if err != nil {
		return
	}

	migTrans := transaction{
		req:       req,
		cycleLeft: 0,
		nextPT:    nil,
		found:     true,
	}
	gmmu.migrationReqs[req.VAddr] = migTrans
	gmmu.topPort.RetrieveIncoming()
}

func (gmmu *Comp) sendInvalidationReq(req *vm.TranslationReq) {
	_, found := gmmu.pageTable.Find(req.PID, req.VAddr)
	if found {
		gmmu.pageTable.Remove(req.PID, req.VAddr)
	}

	migReq := vm.TranslationReqBuilder{}.
		WithSrc(gmmu.bottomPort.AsRemote()).
		WithDst(gmmu.LowModule).
		WithPID(req.PID).
		WithVAddr(req.VAddr).
		WithDeviceID(req.DeviceID).
		SetForInvalidation(true).
		SetForWrite(req.ForWrite).
		Build()

	err := gmmu.bottomPort.Send(migReq)
	if err != nil {
		return
	}
	// fmt.Printf("[%s]\tSend Invalidation Request to Bottom for VA %x, PID %d\n",
	// 	gmmu.Name(), req.VAddr, req.PID)

	invTrans := transaction{
		req:       req,
		cycleLeft: 0,
		nextPT:    nil,
		found:     true,
	}
	gmmu.invalidationReqs[req.VAddr] = invTrans
	gmmu.topPort.RetrieveIncoming()
}

func (gmmu *Comp) walkPageTable() bool {
	if gmmu.isFlushing {
		return false
	}

	madeProgress := false

	stageLatency := gmmu.latency / 5
	ptcLatency := gmmu.pageTableCache.Latency
	if ptcLatency >= stageLatency {
		panic(fmt.Sprintf("[GMMU]\tPTC latency(%d) can't be larger than page walk stage latency(%d)", ptcLatency, stageLatency))
	}

	for i := 0; i < len(gmmu.walkingTranslations); i++ {
		trans := &(gmmu.walkingTranslations[i])
		cycleLeft := trans.cycleLeft

		if cycleLeft > 0 {
			if cycleLeft%stageLatency == 0 {
				gmmu.pageTableWalk(trans)
			} else {
				trans.cycleLeft--
			}

			madeProgress = true

			continue
		}

		req := trans.req
		found := trans.found
		nextPT := trans.nextPT
		var page vm.Page
		if found {
			pg, f := nextPT.Find(req.PID, req.VAddr)
			found = found && f
			page = pg
		}

		if found {
			if page.IsShared && req.ForWrite {
				gmmu.walkingTranslations[i].req.ForInvalidation = true
				gmmu.pageTable.Remove(page.PID, page.VAddr)
				madeProgress = gmmu.processRemoteMemReq(i) || madeProgress
			} else {
				madeProgress = gmmu.finalizePageWalk(i) || madeProgress
			}
		} else {
			madeProgress = gmmu.processRemoteMemReq(i) || madeProgress
		}
	}

	tmp := gmmu.walkingTranslations[:0]

	for i := 0; i < len(gmmu.walkingTranslations); i++ {
		if !gmmu.toRemove(i) {
			tmp = append(tmp, gmmu.walkingTranslations[i])
		}
	}

	gmmu.walkingTranslations = tmp
	gmmu.toRemoveFromPTW = nil

	return madeProgress
}

func (gmmu *Comp) pageTableWalk(trans *transaction) {
	stageLatency := gmmu.latency / 5
	ptcLatency := gmmu.pageTableCache.Latency

	req := trans.req
	pid := req.PID
	vAddr := req.VAddr

	if trans.nextPT == nil {
		// L5 page table base address 참조: 실제 GPU에서는 CR3에 저장되어 있으므로 메모리 접근 필요 없음
		nextPT, found := gmmu.pageTable.FindStepByStep(pid, vAddr)
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

	index := vm.AddrSlice(vAddr, trans.nextPT.GetLevel(), gmmu.log2PageSize)
	nextPT, _ := gmmu.pageTableCache.Find(trans.nextPT.GetName(), index)
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
	gmmu.pageTableCache.Insert(trans.nextPT.GetName(), index, &PT, nil)

	trans.found = true
	trans.cycleLeft--
	trans.nextPT = PT

	return
}

func (gmmu *Comp) processRemoteMemReq(walkingIndex int) bool {
	walking := gmmu.walkingTranslations[walkingIndex].req

	gmmu.remoteMemReqs[walking.VAddr] = gmmu.walkingTranslations[walkingIndex]

	req := vm.TranslationReqBuilder{}.
		WithSrc(gmmu.bottomPort.AsRemote()).
		WithDst(gmmu.LowModule).
		WithPID(walking.PID).
		WithVAddr(walking.VAddr).
		WithDeviceID(walking.DeviceID).
		SetForMigration(false).
		SetForWrite(walking.ForWrite).
		Build()

	err := gmmu.bottomPort.Send(req)
	if err != nil {
		return false
	}

	gmmu.toRemoveFromPTW = append(gmmu.toRemoveFromPTW, walkingIndex)

	return true
}

func (gmmu *Comp) finalizePageWalk(
	walkingIndex int,
) bool {
	req := gmmu.walkingTranslations[walkingIndex].req
	page, found := gmmu.pageTable.Find(req.PID, req.VAddr)

	if !found {
		panic("page not found")
	}

	gmmu.walkingTranslations[walkingIndex].page = page

	return gmmu.doPageWalkHit(walkingIndex)
}

func (gmmu *Comp) doPageWalkHit(
	walkingIndex int,
) bool {
	if !gmmu.topSender.CanSend(1) {
		return false
	}

	walking := gmmu.walkingTranslations[walkingIndex]
	rsp := vm.TranslationRspBuilder{}.
		WithSrc(gmmu.topPort.AsRemote()).
		WithDst(walking.req.Src).
		WithRspTo(walking.req.ID).
		WithPage(walking.page).
		Build()

	gmmu.topSender.Send(rsp)

	gmmu.toRemoveFromPTW = append(gmmu.toRemoveFromPTW, walkingIndex)

	tracing.TraceReqComplete(walking.req, gmmu)

	return true
}

func (gmmu *Comp) toRemove(index int) bool {
	for i := 0; i < len(gmmu.toRemoveFromPTW); i++ {
		remove := gmmu.toRemoveFromPTW[i]
		if remove == index {
			return true
		}
	}

	return false
}

func (gmmu *Comp) fetchFromBottom() bool {
	if gmmu.isFlushing {
		return false
	}

	if !gmmu.topSender.CanSend(1) {
		return false
	}

	req := gmmu.bottomPort.RetrieveIncoming()
	if req == nil {
		return false
	}

	tracing.TraceReqReceive(req, gmmu)

	switch req := req.(type) {
	case *vm.TranslationRsp:
		if req.OriginalReq.ForInvalidation {
			return gmmu.handleInvalidationRsp(req)
		} else if req.OriginalReq.ForMigration {
			return gmmu.handleMigrationRsp(req)
		} else {
			return gmmu.handleTranslationRsp(req)
		}
	default:
		log.Panicf("gmmu cannot handle request of type %s", reflect.TypeOf(req))
	}

	return true
}

func (gmmu *Comp) handleTranslationRsp(rsponse *vm.TranslationRsp) bool {
	if gmmu.isFlushing {
		fmt.Printf("[GMMU %d]\tWarning: GMMU is flushing, cannot handle translation rsp\n", gmmu.deviceID)
		return false
	}

	// fmt.Printf("[GMMU %d]\tReceived translation response from Driver\n",
	// 	rsponse.OriginalReq.DeviceID)
	// fmt.Printf("\t\t\tPage Status: VA %x, PA %x, Owner GPU %d, IsShared %t\n",
	// 	rsponse.Page.VAddr, rsponse.Page.PAddr, rsponse.Page.DeviceID, rsponse.Page.IsShared)

	_, f := gmmu.pageTable.Find(rsponse.Page.PID, rsponse.Page.VAddr)
	if f {
		gmmu.pageTable.Update(rsponse.Page)
	} else {
		gmmu.pageTable.Insert(rsponse.Page)
	}

	reqTransaction, found := gmmu.remoteMemReqs[rsponse.Page.VAddr]
	if !found {
		// fmt.Printf("[GMMU %d]\tWARNING: remoteMemReqs not found: for VA %x\n", gmmu.deviceID, rsponse.Page.VAddr)
		return true
	}

	rsp := vm.TranslationRspBuilder{}.
		WithSrc(gmmu.topPort.AsRemote()).
		WithDst(reqTransaction.req.Src).
		WithRspTo(rsponse.ID).
		WithPage(rsponse.Page).
		Build()

	gmmu.topSender.Send(rsp)

	delete(gmmu.remoteMemReqs, rsponse.Page.VAddr)
	return true
}

func (gmmu *Comp) handleMigrationRsp(rsp *vm.TranslationRsp) bool {
	if gmmu.isFlushing {
		fmt.Printf("[GMMU %d]\tWarning: GMMU is flushing, cannot handle migration rsp\n", gmmu.deviceID)
		return false
	}

	// fmt.Printf("[GMMU %d]\tReceived migration response from Driver\n",
	// 	rsp.OriginalReq.DeviceID)
	// fmt.Printf("\t\t\tPage Status: VA %x, PA %x, Owner GPU %d, IsShared %t\n",
	// rsp.Page.VAddr, rsp.Page.PAddr, rsp.Page.DeviceID, rsp.Page.IsShared)
	_, f := gmmu.pageTable.Find(rsp.Page.PID, rsp.Page.VAddr)
	if f {
		gmmu.pageTable.Update(rsp.Page)
	} else {
		gmmu.pageTable.Insert(rsp.Page)
	}

	reqTransaction, found := gmmu.migrationReqs[rsp.Page.VAddr]
	if !found {
		// fmt.Printf("[GMMU %d]\tWARNING: migrationReqs not found: for VA %x\n", gmmu.deviceID, rsp.Page.VAddr)
		return true
	}

	rsp = vm.TranslationRspBuilder{}.
		WithSrc(gmmu.topPort.AsRemote()).
		WithDst(reqTransaction.req.Src).
		WithRspTo(rsp.ID).
		WithPage(rsp.Page).
		Build()

	gmmu.topSender.Send(rsp)

	delete(gmmu.migrationReqs, rsp.Page.VAddr)
	(*(gmmu.AccessCounter))[reqTransaction.req.PID][rsp.Page.VAddr>>gmmu.log2PageSize] = 0
	return true
}

func (gmmu *Comp) handleInvalidationRsp(rsp *vm.TranslationRsp) bool {
	if gmmu.isFlushing {
		fmt.Printf("[GMMU %d]\tWarning: GMMU is flushing, cannot handle invalidation rsp\n", gmmu.deviceID)
		return false
	}

	// fmt.Printf("[GMMU %d]\tReceived invalidation response from Driver\n",
	// 	rsp.OriginalReq.DeviceID)
	// fmt.Printf("\t\t\tPage Status: VA %x, PA %x, Owner GPU %d, IsShared %t\n",
	// rsp.Page.VAddr, rsp.Page.PAddr, rsp.Page.DeviceID, rsp.Page.IsShared)

	_, f := gmmu.pageTable.Find(rsp.Page.PID, rsp.Page.VAddr)
	if f {
		gmmu.pageTable.Update(rsp.Page)
	} else {
		gmmu.pageTable.Insert(rsp.Page)
	}

	reqTransaction, found := gmmu.invalidationReqs[rsp.Page.VAddr]
	if !found {
		// fmt.Printf("[GMMU %d]\tWARNING: invalidationReqs not found: for VA %x\n", gmmu.deviceID, rsp.Page.VAddr)
		return true
	}

	rsp = vm.TranslationRspBuilder{}.
		WithSrc(gmmu.topPort.AsRemote()).
		WithDst(reqTransaction.req.Src).
		WithRspTo(rsp.ID).
		WithPage(rsp.Page).
		Build()

	gmmu.topSender.Send(rsp)

	delete(gmmu.invalidationReqs, rsp.Page.VAddr)
	if gmmu.AccessCounter != nil && (*(gmmu.AccessCounter))[reqTransaction.req.PID] != nil {
		(*(gmmu.AccessCounter))[reqTransaction.req.PID][rsp.Page.VAddr>>gmmu.log2PageSize] = 0
	}
	return true
}

func (gmmu *Comp) parseFromControl() bool {
	req := gmmu.controlPort.RetrieveIncoming()
	if req == nil {
		return false
	}

	switch req := req.(type) {
	case *vm.PTEInvalidationReq:
		return gmmu.handleInvalidationRequest(req)
	case *tlb.FlushReq:
		return gmmu.handleFlushRequest(req)
	case *vm.RestartReq:
		return gmmu.handleRestartRequest(req)
	default:
		log.Panicf("[GMMU]\tgmmu canot handle request of type %s", reflect.TypeOf(req))
	}

	return true
}

func (gmmu *Comp) handleInvalidationRequest(req *vm.PTEInvalidationReq) bool {
	gmmu.invalidatePTEAfterMigration(req.VAddr, req.PID)

	rsp := vm.PTEInvalidationRspBuilder{}.
		WithSrc(gmmu.controlPort.AsRemote()).
		WithDst(req.Src).
		Build()

	gmmu.controlPort.Send(rsp)

	return true
}

func (gmmu *Comp) invalidatePTEAfterMigration(vAddr uint64, pid vm.PID) {
	_, found := gmmu.pageTable.Find(pid, vAddr)
	if found {
		gmmu.pageTable.Remove(pid, vAddr)
	}

	ac := *(gmmu.AccessCounter)
	delete(ac[pid], vAddr>>gmmu.log2PageSize)
}

func (gmmu *Comp) handleFlushRequest(req *tlb.FlushReq) bool {
	rsp := vm.FlushRspBuilder{}.
		WithSrc(gmmu.controlPort.AsRemote()).
		WithDst(req.Src).
		Build()

	err := gmmu.controlPort.Send(rsp)
	if err != nil {
		return false
	}

	gmmu.isFlushing = true
	for i := 0; i < len(req.VAddr); i++ {
		addr := req.VAddr[i]

		_, found := gmmu.pageTable.Find(req.PID, uint64(addr))
		if found {
			gmmu.pageTable.Remove(req.PID, uint64(addr))
		}

		innerMap, found := (*(gmmu.AccessCounter))[req.PID]
		if found {
			delete(innerMap, uint64(addr)>>gmmu.log2PageSize)
		}
	}

	gmmu.walkingTranslations = gmmu.walkingTranslations[:0]
	clear(gmmu.remoteMemReqs)
	clear(gmmu.migrationReqs)
	clear(gmmu.invalidationReqs)
	gmmu.toRemoveFromPTW = gmmu.toRemoveFromPTW[:0]

	gmmu.pageTableCache.Clear()

	for gmmu.bottomPort.RetrieveIncoming() != nil {
	}

	return true
}

func (gmmu *Comp) handleRestartRequest(req *vm.RestartReq) bool {
	rsp := vm.RestartRspBuilder{}.
		WithSrc(gmmu.controlPort.AsRemote()).
		WithDst(req.Src).
		Build()

	err := gmmu.controlPort.Send(rsp)
	if err != nil {
		return false
	}

	gmmu.isFlushing = false

	for gmmu.topPort.RetrieveIncoming() != nil {
	}

	for gmmu.bottomPort.RetrieveIncoming() != nil {
	}

	return true
}
