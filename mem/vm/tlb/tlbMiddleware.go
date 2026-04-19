package tlb

import (
	"fmt"
	"log"
	"reflect"

	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/tracing"
)

type pipelineTLBReq struct {
	req *vm.TranslationReq
}

func (r *pipelineTLBReq) TaskID() string {
	return r.req.ID
}

type tlbMiddleware struct {
	*Comp
}

func (m *tlbMiddleware) Tick() bool {
	madeProgress := m.performCtrlReq()

	switch m.state {
	case "drain":
		madeProgress = m.handleDrain() || madeProgress

	case "pause":
		// No action

	default: // When state is enable or in initial state
		madeProgress = m.handleEnable() || madeProgress
	}

	return madeProgress
}

func (m *tlbMiddleware) performCtrlReq() bool {
	item := m.controlPort.PeekIncoming()
	if item == nil {
		return false
	}

	item = m.controlPort.RetrieveIncoming()
	tracing.AddMilestone(
		tracing.MsgIDAtReceiver(item, m.Comp),
		tracing.MilestoneKindNetworkBusy,
		m.controlPort.Name(),
		m.Comp.Name(),
		m.Comp,
	)

	switch req := item.(type) {
	case *FlushReq:
		return m.handleTLBFlush(req)
	case *RestartReq:
		return m.handleTLBRestart(req)
	case *mem.ControlMsg:
		if req.Enable {
			m.state = "enable"
		} else if req.Drain {
			m.state = "drain"
		} else if req.Pause {
			m.state = "pause"
		}
	default:
		log.Panicf("cannot process request %s", reflect.TypeOf(req))
	}

	return true
}

func (m *tlbMiddleware) handleDrain() bool {
	madeProgress := false
	for i := 0; i < m.numReqPerCycle; i++ {
		madeProgress = m.respondMSHREntry() || madeProgress
	}

	for i := 0; i < m.numReqPerCycle; i++ {
		madeProgress = m.parseBottom() || madeProgress
	}

	madeProgress = m.processPipeline() || madeProgress

	if m.mshr.IsEmpty() && m.bottomPort.PeekIncoming() == nil {
		m.state = "pause"
		tracing.AddMilestone(
			m.Comp.Name()+".drain",
			tracing.MilestoneKindHardwareResource,
			m.Comp.Name()+".MSHR",
			m.Comp.Name(),
			m.Comp,
		)
	}

	return madeProgress
}

func (m *tlbMiddleware) handleEnable() bool {
	madeProgress := false
	for i := 0; i < m.numReqPerCycle; i++ {
		madeProgress = m.respondMSHREntry() || madeProgress
	}

	for i := 0; i < m.numReqPerCycle; i++ {
		madeProgress = m.parseBottom() || madeProgress
	}

	madeProgress = m.processPipeline() || madeProgress

	return madeProgress
}

func (m *tlbMiddleware) processPipeline() bool {
	madeProgress := false

	madeProgress = m.extractFromPipeline() || madeProgress
	madeProgress = m.sendMigrationReq() || madeProgress
	madeProgress = m.sendInvalidationReq() || madeProgress

	madeProgress = m.responsePipeline.Tick() || madeProgress

	madeProgress = m.insertIntoPipeline() || madeProgress

	return madeProgress
}

// get req from port buffer and insert into pipeline
func (m *tlbMiddleware) insertIntoPipeline() bool {
	madeProgress := false

	for i := 0; i < m.numReqPerCycle; i++ {
		rq := m.topPort.PeekIncoming()
		if rq == nil {
			break
		}
		req := rq.(*vm.TranslationReq)

		// cond := (*m.accessCounter)[req.PID][req.VAddr>>12] == 255 || req.ForMigration
		// cond = cond && m.Comp.pageMigrationPolicy == 0
		// if cond {
		// 	// migration request 보내기
		// 	if len(m.migrationBuffer) >= m.migrationBufferCap {
		// 		break
		// 	}

		// 	m.lookupAndInvalidation(req)
		// 	m.migrationBuffer = append(m.migrationBuffer, req)
		// 	(*m.accessCounter)[req.PID][req.VAddr>>12] = 0
		// 	m.topPort.RetrieveIncoming()
		// 	madeProgress = true
		// } else if req.ForInvalidation {
		// 	if len(m.invalidationBuffer) >= m.invalidationBufferCap {
		// 		break
		// 	}

		// 	m.lookupAndInvalidation(req)
		// 	m.invalidationBuffer = append(m.invalidationBuffer, req)
		// 	m.topPort.RetrieveIncoming()
		// 	madeProgress = true
		// } else {
		// 기존과 동일하게 pipeline에 삽입
		if !m.responsePipeline.CanAccept() {
			break
		}

		m.responsePipeline.Accept(&pipelineTLBReq{
			req: req,
		})
		m.topPort.RetrieveIncoming()
		madeProgress = true
		// }
	}

	return madeProgress
}

func (m *tlbMiddleware) extractFromPipeline() bool {
	madeProgress := false

	for i := 0; i < m.numReqPerCycle; i++ {
		item := m.responseBuffer.Peek()

		if item == nil {
			break
		}

		req := item.(*pipelineTLBReq).req

		ok := m.lookup(req)
		if ok {
			m.responseBuffer.Pop()

			madeProgress = true
		}
	}

	return madeProgress
}

func (m *tlbMiddleware) lookup(req *vm.TranslationReq) bool {
	mshrEntry := m.mshr.GetEntry(req.PID, req.VAddr)
	if mshrEntry != nil {
		return m.processTLBMSHRHit(mshrEntry, req)
	}

	mshrEntry = m.migrationMshr.GetEntry(req.PID, req.VAddr)
	if mshrEntry != nil {
		return m.processTLBMSHRHit(mshrEntry, req)
	}

	mshrEntry = m.invalidationMshr.GetEntry(req.PID, req.VAddr)
	if mshrEntry != nil {
		return m.processTLBMSHRHit(mshrEntry, req)
	}

	setID := m.vAddrToSetID(req.VAddr)
	set := m.sets[setID]
	wayID, page, found := set.Lookup(req.PID, req.VAddr)

	migCond := (*m.accessCounter)[req.PID][req.VAddr>>m.log2PageSize] == 255 || req.ForMigration
	migCond = migCond && m.Comp.pageMigrationPolicy == 0
	migCond = migCond && page.DeviceID != req.DeviceID
	invCond := req.ForWrite && page.IsShared
	invCond = invCond || req.ForInvalidation
	if migCond {
		// migration request 보내기
		if len(m.migrationBuffer) >= m.migrationBufferCap {
			return false
		}

		m.lookupAndInvalidation(req)
		m.migrationBuffer = append(m.migrationBuffer, req)
		(*m.accessCounter)[req.PID][req.VAddr>>m.log2PageSize] = 0

		return true
	} else if invCond {
		if len(m.invalidationBuffer) >= m.invalidationBufferCap {
			return false
		}

		m.lookupAndInvalidation(req)
		m.invalidationBuffer = append(m.invalidationBuffer, req)

		return true
	}

	if found && page.Valid {
		return m.handleTranslationHit(req, setID, wayID, page)
	}
	return m.handleTranslationMiss(req)
}

func (m *tlbMiddleware) vAddrToSetID(vAddr uint64) (setID int) {
	return int(vAddr / m.pageSize % uint64(m.numSets))
}

func (m *tlbMiddleware) processTLBMSHRHit(
	mshrEntry *mshrEntry,
	req *vm.TranslationReq,
) bool {
	mshrEntry.Requests = append(mshrEntry.Requests, req)

	tracing.TraceReqReceive(req, m.Comp)
	tracing.AddTaskStep(
		tracing.MsgIDAtReceiver(req, m.Comp), m.Comp, "mshr-hit")

	return true
}

func (m *tlbMiddleware) handleTranslationHit(
	req *vm.TranslationReq,
	setID, wayID int,
	page vm.Page,
) bool {
	ok := m.sendRspToTop(req, page)
	if !ok {
		return false
	}
	m.visit(setID, wayID)

	tracing.AddMilestone(
		tracing.MsgIDAtReceiver(req, m.Comp),
		tracing.MilestoneKindData,
		m.Comp.Name()+".Sets",
		m.Comp.Name(),
		m.Comp,
	)

	tracing.TraceReqReceive(req, m.Comp)
	tracing.AddTaskStep(tracing.MsgIDAtReceiver(req, m.Comp), m.Comp, "hit")
	tracing.TraceReqComplete(req, m.Comp)

	return true
}

func (m *tlbMiddleware) visit(setID, wayID int) {
	set := m.sets[setID]
	set.Visit(wayID)
}

func (m *tlbMiddleware) handleTranslationMiss(
	req *vm.TranslationReq,
) bool {
	if req.ForInvalidation && m.invalidationMshr.IsFull() {
		return false
	}

	if !req.ForInvalidation && m.mshr.IsFull() {
		return false
	}

	tracing.AddMilestone(
		tracing.MsgIDAtReceiver(req, m.Comp),
		tracing.MilestoneKindHardwareResource,
		m.Comp.Name()+".MSHR",
		m.Comp.Name(),
		m.Comp,
	)

	fetched := m.fetchBottom(req)
	if fetched {
		tracing.TraceReqReceive(req, m.Comp)
		tracing.AddTaskStep(
			tracing.MsgIDAtReceiver(req, m.Comp),
			m.Comp,
			"miss",
		)

		return true
	}
	return false
}

func (m *tlbMiddleware) fetchBottom(req *vm.TranslationReq) bool {
	fetchBottom := vm.TranslationReqBuilder{}.
		WithSrc(m.bottomPort.AsRemote()).
		WithDst(m.addressMapper.Find(req.VAddr)).
		WithPID(req.PID).
		WithVAddr(req.VAddr).
		WithDeviceID(req.DeviceID).
		SetForMigration(false).
		SetForInvalidation(req.ForInvalidation).
		SetForWrite(req.ForWrite).
		Build()

	err := m.bottomPort.Send(fetchBottom)
	if err != nil {
		return false
	}

	tracing.AddMilestone(
		tracing.MsgIDAtReceiver(req, m.Comp),
		tracing.MilestoneKindNetworkBusy,
		m.bottomPort.Name(),
		m.Comp.Name(),
		m.Comp,
	)

	if req.ForInvalidation {
		invalidationMshrEntry := m.invalidationMshr.Add(req.PID, req.VAddr)
		invalidationMshrEntry.Requests = append(invalidationMshrEntry.Requests, req)
		invalidationMshrEntry.reqToBottom = fetchBottom
	} else {
		mshrEntry := m.mshr.Add(req.PID, req.VAddr)
		mshrEntry.Requests = append(mshrEntry.Requests, req)
		mshrEntry.reqToBottom = fetchBottom
	}

	tracing.TraceReqInitiate(fetchBottom, m.Comp,
		tracing.MsgIDAtReceiver(req, m.Comp))

	return true
}

func (m *tlbMiddleware) sendMigrationReq() bool {
	madeProgress := false

	for i := 0; i < m.numReqPerCycle; i++ {
		if len(m.migrationBuffer) == 0 {
			break
		}

		req := m.migrationBuffer[0]

		m.lookupAndInvalidation(req)
		if !m.mergeMigrationAndSend(req) {
			break
		}

		m.migrationBuffer = m.migrationBuffer[1:]
		madeProgress = true
	}

	return madeProgress
}

func (m *tlbMiddleware) sendInvalidationReq() bool {
	madeProgress := false

	for i := 0; i < m.numReqPerCycle; i++ {
		if len(m.invalidationBuffer) == 0 {
			break
		}

		req := m.invalidationBuffer[0]
		m.lookupAndInvalidation(req)
		if !m.mergeInvalidationAndSend(req) {
			break
		}
		// fmt.Printf("[%s]\tSend Invalidation Request to Bottom for VA %x, PID %d\n",
		// 	m.Name(), req.VAddr, req.PID)

		m.invalidationBuffer = m.invalidationBuffer[1:]
		madeProgress = true
	}

	return madeProgress
}

func (m *tlbMiddleware) lookupAndInvalidation(req *vm.TranslationReq) {
	setID := m.vAddrToSetID(req.VAddr)
	set := m.sets[setID]
	wayID, page, found := set.Lookup(req.PID, req.VAddr)

	if found {
		page.Valid = false
		set.Update(wayID, page)
	}
}

func (m *tlbMiddleware) mergeMigrationAndSend(req *vm.TranslationReq) bool {
	mshrEntry := m.migrationMshr.GetEntry(req.PID, req.VAddr)
	if mshrEntry != nil {
		mshrEntry.Requests = append(mshrEntry.Requests, req)
		return true
	}

	if m.migrationMshr.IsFull() {
		return false
	}

	fetchBottom := vm.TranslationReqBuilder{}.
		WithSrc(m.bottomPort.AsRemote()).
		WithDst(m.addressMapper.Find(req.VAddr)).
		WithPID(req.PID).
		WithVAddr(req.VAddr).
		WithDeviceID(req.DeviceID).
		SetForMigration(true).
		SetForWrite(req.ForWrite).
		Build()
	err := m.bottomPort.Send(fetchBottom)

	if err != nil {
		return false
	}

	mshrEntry = m.migrationMshr.Add(req.PID, req.VAddr)
	mshrEntry.Requests = append(mshrEntry.Requests, req)
	mshrEntry.reqToBottom = fetchBottom
	return false
}

func (m *tlbMiddleware) mergeInvalidationAndSend(req *vm.TranslationReq) bool {
	mshrEntry := m.invalidationMshr.GetEntry(req.PID, req.VAddr)
	if mshrEntry != nil {
		mshrEntry.Requests = append(mshrEntry.Requests, req)
		return true
	}

	if m.invalidationMshr.IsFull() {
		return false
	}

	fetchBottom := vm.TranslationReqBuilder{}.
		WithSrc(m.bottomPort.AsRemote()).
		WithDst(m.addressMapper.Find(req.VAddr)).
		WithPID(req.PID).
		WithVAddr(req.VAddr).
		WithDeviceID(req.DeviceID).
		SetForInvalidation(true).
		SetForWrite(req.ForWrite).
		Build()
	err := m.bottomPort.Send(fetchBottom)

	if err != nil {
		return false
	}

	mshrEntry = m.invalidationMshr.Add(req.PID, req.VAddr)
	mshrEntry.Requests = append(mshrEntry.Requests, req)
	mshrEntry.reqToBottom = fetchBottom
	return false
}

func (m *tlbMiddleware) respondMSHREntry() bool {
	if m.respondingMSHREntry == nil {
		return false
	}
	mshrEntry := m.respondingMSHREntry
	page := mshrEntry.page
	req := mshrEntry.Requests[0]
	rspToTop := vm.TranslationRspBuilder{}.
		WithSrc(m.topPort.AsRemote()).
		WithDst(req.Src).
		WithRspTo(req.ID).
		WithPage(page).
		Build()

	err := m.topPort.Send(rspToTop)
	if err != nil {
		return false
	}

	tracing.AddMilestone(
		tracing.MsgIDAtReceiver(req, m.Comp),
		tracing.MilestoneKindNetworkBusy,
		m.topPort.Name(),
		m.Comp.Name(),
		m.Comp,
	)

	mshrEntry.Requests = mshrEntry.Requests[1:]
	if len(mshrEntry.Requests) == 0 {
		m.respondingMSHREntry = nil
	}

	tracing.TraceReqComplete(req, m.Comp)

	return true
}

func (m *tlbMiddleware) sendRspToTop(
	req *vm.TranslationReq,
	page vm.Page,
) bool {
	rsp := vm.TranslationRspBuilder{}.
		WithSrc(m.topPort.AsRemote()).
		WithDst(req.Src).
		WithRspTo(req.ID).
		WithPage(page).
		Build()

	err := m.topPort.Send(rsp)
	if err == nil {
		tracing.AddMilestone(
			tracing.MsgIDAtReceiver(req, m.Comp),
			tracing.MilestoneKindNetworkBusy,
			m.topPort.Name(),
			m.Comp.Name(),
			m.Comp,
		)
	}
	return err == nil
}

func (m *tlbMiddleware) parseBottom() bool {
	if m.respondingMSHREntry != nil {
		return false
	}
	item := m.bottomPort.PeekIncoming()
	if item == nil {
		return false
	}

	rsp := item.(*vm.TranslationRsp)
	tracing.AddMilestone(
		tracing.MsgIDAtReceiver(rsp, m.Comp),
		tracing.MilestoneKindData,
		m.bottomPort.Name(),
		m.Comp.Name(),
		m.Comp,
	)
	page := rsp.Page

	mshrEntryPresent := m.mshr.IsEntryPresent(rsp.Page.PID, rsp.Page.VAddr)
	migrationMshrEntryPresent := m.migrationMshr.IsEntryPresent(rsp.Page.PID, rsp.Page.VAddr)
	invalidationMshrEntryPresent := m.invalidationMshr.IsEntryPresent(rsp.Page.PID, rsp.Page.VAddr)
	if !mshrEntryPresent && !migrationMshrEntryPresent && !invalidationMshrEntryPresent {
		m.bottomPort.RetrieveIncoming()
		return true
	}

	setID := m.vAddrToSetID(page.VAddr)
	set := m.sets[setID]

	wayID, ok := m.sets[setID].Evict()
	if !ok {
		panic(fmt.Sprintf("[%s]\tfailed to evict", m.Name()))
	}

	set.Update(wayID, page)
	set.Visit(wayID)

	var mshrEntry *mshrEntry
	if migrationMshrEntryPresent {
		mshrEntry = m.migrationMshr.GetEntry(rsp.Page.PID, rsp.Page.VAddr)
		m.migrationMshr.Remove(rsp.Page.PID, rsp.Page.VAddr)
	} else if invalidationMshrEntryPresent {
		mshrEntry = m.invalidationMshr.GetEntry(rsp.Page.PID, rsp.Page.VAddr)
		m.invalidationMshr.Remove(rsp.Page.PID, rsp.Page.VAddr)
	} else {
		mshrEntry = m.mshr.GetEntry(rsp.Page.PID, rsp.Page.VAddr)
		m.mshr.Remove(rsp.Page.PID, rsp.Page.VAddr)
	}

	m.respondingMSHREntry = mshrEntry
	mshrEntry.page = page

	m.bottomPort.RetrieveIncoming()
	tracing.TraceReqFinalize(mshrEntry.reqToBottom, m.Comp)

	return true
}

func (m *tlbMiddleware) handleTLBFlush(req *FlushReq) bool {
	// if m.Name() == "GPU[4].SA[0].L1VTLB[0]" || m.Name() == "GPU[4].L2TLB" {
	// 	fmt.Printf("[%s]\tFlushing TLB, len of migrationMSHR %d, mshr %d\n",
	// 		m.Name(), len(m.migrationMshr.AllEntries()), len(m.mshr.AllEntries()))
	// }

	rsp := FlushRspBuilder{}.
		WithSrc(m.controlPort.AsRemote()).
		WithDst(req.Src).
		Build()

	err := m.controlPort.Send(rsp)
	if err != nil {
		return false
	}
	tracing.AddMilestone(
		tracing.MsgIDAtReceiver(req, m.Comp),
		tracing.MilestoneKindNetworkBusy,
		m.controlPort.Name(),
		m.Comp.Name(),
		m.Comp,
	)

	if req.InvalidateAllLines {
		for _, vAddr := range req.VAddr {
			setID := m.vAddrToSetID(vAddr)
			set := m.sets[setID]
			wayID, page, found := set.Lookup(req.PID, vAddr)

			if !found {
				continue
			}
			tracing.AddMilestone(
				tracing.MsgIDAtReceiver(req, m.Comp),
				tracing.MilestoneKindDependency,
				m.Comp.Name()+".Sets",
				m.Comp.Name(),
				m.Comp,
			)
			page.Valid = false
			set.Update(wayID, page)
		}
	}

	m.mshr.Reset()
	m.migrationMshr.Reset()
	m.migrationBuffer = make([]*vm.TranslationReq, 0)
	m.invalidationMshr.Reset()
	m.invalidationBuffer = make([]*vm.TranslationReq, 0)
	m.isPaused = true
	m.state = "pause"

	m.responseBuffer.Clear()
	m.responsePipeline.Clear()

	return true
}

func (m *tlbMiddleware) handleTLBRestart(req *RestartReq) bool {
	// if m.Name() == "GPU[4].SA[0].L1VTLB[0]" || m.Name() == "GPU[4].L2TLB" {
	// 	fmt.Printf("[%s]\tRestart TLB, len of migrationMSHR %d, mshr %d\n",
	// 		m.Name(), len(m.migrationMshr.AllEntries()), len(m.mshr.AllEntries()))
	// }

	rsp := RestartRspBuilder{}.
		WithSrc(m.controlPort.AsRemote()).
		WithDst(req.Src).
		Build()

	err := m.controlPort.Send(rsp)
	if err != nil {
		return false
	}
	tracing.AddMilestone(
		tracing.MsgIDAtReceiver(req, m.Comp),
		tracing.MilestoneKindNetworkBusy,
		m.controlPort.Name(),
		m.Comp.Name(),
		m.Comp,
	)
	m.isPaused = false
	m.state = "enable"

	for m.topPort.RetrieveIncoming() != nil {
		m.topPort.RetrieveIncoming()
	}

	for m.bottomPort.RetrieveIncoming() != nil {
		m.bottomPort.RetrieveIncoming()
	}

	return true
}
