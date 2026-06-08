package coherence

import (
	"fmt"
	"strings"

	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/akita/v4/tracing"
)

type bottomSender struct {
	cache *Comp

	writeBufferCapacity     int
	maxInflightRequest      int
	maxInflightInvalidation int

	// Phase 2 inv-emit budget. When maxInvEmitPerCycle > 0, the
	// directory caps InvReq emission per output channel per cycle to
	// model the directory controller's per-channel serialization (real
	// hardware can't blast all sharers' invs in a single cycle even
	// when the outgoing buffer has room). Two separate counters
	// because the RDMA-bound (topPort -> rdmaEngine -> remote GPUs)
	// and local-L2-bound (bottomPort -> local L2) paths use distinct
	// physical output channels in real silicon.
	// 0 (default) disables the cap, preserving baseline behavior.
	maxInvEmitPerCycle          int
	invEmittedToRDMAThisCycle   int
	invEmittedToBottomThisCycle int
	lastEmitCycleTime           sim.VTimeInSec

	inflightRequest      []*transaction
	inflightInvToOutside []*transaction
	inflightInvToBottom  []*mem.InvReq

	// iter19 D1-D3-CD: typed top-egress queues so a stalled egress
	// port no longer head-of-line blocks the other ports.
	//   sendToTopRspQue   -> topPort (local L1 RSPs)
	//   sendToRDMAQue     -> RDMAPort (peer data RSPs)
	//   sendToRDMAInvQue  -> RDMAInvPort (outbound InvReq)
	//   sendToRDMAInvRspQue -> RDMAInvRspPort (outbound InvRsp)
	sendToTopRspQue     []sim.Msg
	sendToRDMAQue       []sim.Msg
	sendToRDMAInvQue    []sim.Msg
	sendToRDMAInvRspQue []sim.Msg

	returnFalse0 string
	returnFalse1 string
	returnFalse2 string
}

// refreshEmitBudget resets per-cycle inv emit counters when the engine
// has advanced into a new cycle. runStage may invoke Tick numReqPerCycle
// times within one cycle (same engine time across all calls), so the
// counters legitimately persist across those repeated invocations.
func (bs *bottomSender) refreshEmitBudget() {
	now := bs.cache.Engine.CurrentTime()
	if now > bs.lastEmitCycleTime {
		bs.invEmittedToRDMAThisCycle = 0
		bs.invEmittedToBottomThisCycle = 0
		bs.lastEmitCycleTime = now
	}
}

func (bs *bottomSender) canEmitInvToRDMA() bool {
	return bs.maxInvEmitPerCycle <= 0 ||
		bs.invEmittedToRDMAThisCycle < bs.maxInvEmitPerCycle
}

func (bs *bottomSender) canEmitInvToBottom() bool {
	return bs.maxInvEmitPerCycle <= 0 ||
		bs.invEmittedToBottomThisCycle < bs.maxInvEmitPerCycle
}

func (bs *bottomSender) Tick() bool {
	bs.refreshEmitBudget()

	madeProgress := false

	madeProgress = bs.processReturnRsp() || madeProgress
	madeProgress = bs.processInputReq() || madeProgress
	madeProgress = bs.processInvalidationRsp() || madeProgress
	madeProgress = bs.sendToTop() || madeProgress

	return madeProgress
}

// processInputReq drains BOTH the transaction buffer and the InvReq
// buffer per tick so a stall on one cannot block the other.
func (bs *bottomSender) processInputReq() bool {
	progress := false

	item := bs.cache.bottomSenderTransBuffer.Peek()
	if item != nil {
		if t, ok := item.(*transaction); ok {
			if bs.processNewTransaction(t) {
				progress = true
			}
		}
	} else {
		bs.returnFalse1 = "There is no trans from bottomSenderTransBuffer"
	}

	item = bs.cache.bottomSenderInvBuffer.Peek()
	if item != nil {
		if req, ok := item.(*mem.InvReq); ok {
			if bs.sendInvReqToBottom(req) {
				progress = true
			}
		}
	}

	return progress
}

func (bs *bottomSender) processNewTransaction(trans *transaction) bool {
	progress := false
	switch trans.action {
	case Nothing, InsertNewEntry, UpdateEntry:
		progress = bs.sendRequestToBottom(trans)
	case EvictAndInsertNewEntry, InvalidateAndUpdateEntry, InvalidateEntry:
		progress = bs.sendInvalidationRequest(trans)
	default:
		panic("unknown transaction action")
	}

	return progress
}

func (bs *bottomSender) sendRequestToBottom(
	trans *transaction,
) bool {
	if !bs.cache.bottomPort.CanSend() {
		bs.returnFalse1 = "[sendRequestToBottom] Cannot send to bottomPort"
		return false
	}

	req := bs.cache.cloneReq(trans.accessReq())
	req.Meta().Src = bs.cache.bottomPort.AsRemote()
	req.Meta().Dst = bs.cache.addressToPortMapper.Find(trans.accessReq().GetAddress())
	req.SetReqFrom(trans.accessReq().Meta().ID)
	err := bs.cache.bottomPort.Send(req)
	if err != nil {
		bs.returnFalse1 = "[sendRequestToBottom] Failed to send to bottomPort"
		return false
	}

	bs.cache.bottomSenderTransBuffer.Pop()

	bs.inflightRequest = append(bs.inflightRequest, trans)
	trans.reqIDToBottom = req.Meta().ID

	what := ""
	if trans.action != Nothing {
		what = "UpdateEntry"
	}
	tracing.AddTaskStep(
		tracing.MsgIDAtReceiver(trans.accessReq(), bs.cache),
		bs.cache,
		what,
	)

	tracing.TraceReqFinalize(trans.accessReq(), bs.cache)

	return true
}

func (bs *bottomSender) sendInvalidationRequest(
	trans *transaction,
) bool {
	// Phase 2: enforce in-flight inv cap to match SD/REC variants.
	if bs.findInvTransactionByID(
		trans.accessReq().Meta().ID, bs.inflightInvToOutside) == -1 &&
		bs.tooManyInflightInvalidation() {
		bs.returnFalse1 = "[sendInvalidationRequest] tooManyInflightInvalidation"
		return false
	}

	progress := false

	i := bs.findInvTransactionByID(trans.accessReq().Meta().ID, bs.inflightInvToOutside)
	if i == -1 {
		bs.inflightInvToOutside = append(bs.inflightInvToOutside, trans)
		progress = true
	}

	for i := 0; i < len(trans.invalidationList); i++ {
		sh := trans.invalidationList[i]
		if sh == trans.accessReq().GetSrcRDMA() || sh == "" {
			trans.invalidationList = append(trans.invalidationList[:i], trans.invalidationList[i+1:]...)
			i--
			continue
		}

		if !bs.canEmitInvToRDMA() {
			if !progress {
				bs.returnFalse1 = "[sendInvalidationRequest] inv-emit budget exhausted (RDMA)"
			}
			return progress
		}

		// iter19 D1-D3-CD: outbound InvReq is enqueued to typed
		// sendToRDMAInvQue and drained by sendToTop via RDMAInvPort.
		invDst := bs.cache.ToRDMAInv
		if invDst == "" {
			invDst = bs.cache.ToRDMA
		}
		req := mem.InvReqBuilder{}.
			WithSrc(bs.cache.topPort.AsRemote()).
			WithDst(invDst).
			WithAddress(trans.evictingAddr).
			WithPID(trans.evictingPID).
			WithReqFrom(trans.accessReq().Meta().ID).
			WithDstRDMA(sh).
			WithIsWriteInv(trans.action == InvalidateAndUpdateEntry).
			Build()

		bs.sendToRDMAInvQue = append(bs.sendToRDMAInvQue, req)
		bs.invEmittedToRDMAThisCycle++
		trans.invalidationList = append(trans.invalidationList[:i], trans.invalidationList[i+1:]...)
		i--
		trans.pendingEviction = append(trans.pendingEviction, sh)
		progress = progress || true

		what := ""
		if trans.action == EvictAndInsertNewEntry {
			what = "InvalidateByEviction"
		} else if trans.action == InvalidateAndUpdateEntry {
			what = "InvalidateByWrite"
		}
		if what != "" {
			tracing.AddTaskStep(
				tracing.MsgIDAtReceiver(trans.accessReq(), bs.cache),
				bs.cache,
				what,
			)
		}
	}

	if trans.action != InvalidateEntry {
		return bs.sendRequestToBottom(trans) || progress
	}
	bs.cache.bottomSenderTransBuffer.Pop()

	tracing.TraceReqFinalize(trans.accessReq(), bs.cache)

	return progress
}

func (bs *bottomSender) sendInvReqToBottom(req *mem.InvReq) bool {
	// Phase 2: in-flight cap on local-L2-bound invs.
	if bs.tooManyInflightInvalidationToBottom() {
		bs.returnFalse1 = "[sendInvReqToBottom] tooManyInflightInvalidationToBottom"
		return false
	}

	if !bs.canEmitInvToBottom() {
		bs.returnFalse1 = "[sendInvReqToBottom] inv-emit budget exhausted (local L2)"
		return false
	}

	if !bs.cache.bottomPort.CanSend() {
		bs.returnFalse1 = "[sendInvReqToBottom] Cannot send to bottomPort"
		return false
	}

	bs.inflightInvToBottom = append(bs.inflightInvToBottom, req)
	reqToBottom := mem.InvReqBuilder{}.
		WithSrc(bs.cache.bottomPort.AsRemote()).
		WithDst(bs.cache.addressToPortMapper.Find(req.Address)).
		WithPID(req.PID).
		WithAddress(req.Address).
		WithReqFrom(req.Meta().ID).
		WithIsWriteInv(req.IsWriteInv).
		Build()

	err := bs.cache.bottomPort.Send(reqToBottom)
	if err != nil {
		bs.returnFalse1 = "[sendInvReqToBottom] Failed to send to bottomPort"
		return false
	}

	bs.invEmittedToBottomThisCycle++
	bs.cache.bottomSenderInvBuffer.Pop()

	return true
}

func (bs *bottomSender) processReturnRsp() bool {
	msg := bs.cache.bottomPort.PeekIncoming()
	if msg == nil {
		bs.returnFalse0 = "There is no msg from bottomPort"
		return false
	}

	switch msg := msg.(type) {
	case *mem.DataReadyRsp:
		return bs.processDataReadyRsp(msg)
	case *mem.WriteDoneRsp:
		return bs.processWriteDoneRsp(msg)
	case *mem.InvRsp:
		return bs.processInvRspFromBottom(msg)
	default:
		panic("unknown msg type")
	}
}

// enqueueRsp routes an outbound response message to the typed queue
// matching its destination egress port (iter19 D1-D3-CD).
func (bs *bottomSender) enqueueRsp(msg sim.Msg) {
	dst := fmt.Sprintf("%s", msg.Meta().Dst)
	if strings.Contains(dst, "RDMAInvRsp") {
		bs.sendToRDMAInvRspQue = append(bs.sendToRDMAInvRspQue, msg)
	} else if strings.Contains(dst, "RDMAInv") {
		bs.sendToRDMAInvQue = append(bs.sendToRDMAInvQue, msg)
	} else if strings.Contains(dst, "RDMA") {
		bs.sendToRDMAQue = append(bs.sendToRDMAQue, msg)
	} else {
		bs.sendToTopRspQue = append(bs.sendToTopRspQue, msg)
	}
}

func (bs *bottomSender) processDataReadyRsp(msg *mem.DataReadyRsp) bool {
	i := bs.findTransactionByID(msg.GetRspTo(), bs.inflightRequest)
	if i == -1 {
		fmt.Printf("[%s]\t3. Cannot find transaction for DataReadyRsp with RspTo %s\n", bs.cache.Name(), msg.GetRspTo())
		bs.cache.bottomPort.RetrieveIncoming()
		return true
	}

	trans := bs.inflightRequest[i]
	msg.RespondTo = trans.accessReq().Meta().ID
	msg.Src = bs.cache.topPort.AsRemote()
	msg.Dst = trans.accessReq().Meta().Src

	if bs.cache.flushLocalAccess && !strings.Contains(fmt.Sprintf("%s", msg.Meta().Dst), "RDMA") {
		bs.cache.bottomPort.RetrieveIncoming()
		bs.removeInflightRequest(i)
		return true
	}

	bs.enqueueRsp(msg)
	bs.cache.bottomPort.RetrieveIncoming()
	bs.removeInflightRequest(i)

	return true
}

func (bs *bottomSender) processWriteDoneRsp(msg *mem.WriteDoneRsp) bool {
	i := bs.findTransactionByID(msg.GetRspTo(), bs.inflightRequest)
	if i == -1 {
		fmt.Printf("[%s]\t3. Cannot find transaction for WriteDoneRsp with RspTo %s\n", bs.cache.Name(), msg.GetRspTo())
		bs.cache.bottomPort.RetrieveIncoming()
		return true
	}

	trans := bs.inflightRequest[i]
	msg.RespondTo = trans.accessReq().Meta().ID
	msg.Src = bs.cache.topPort.AsRemote()
	msg.Dst = trans.accessReq().Meta().Src

	if bs.cache.flushLocalAccess && !strings.Contains(fmt.Sprintf("%s", msg.Meta().Dst), "RDMA") {
		bs.cache.bottomPort.RetrieveIncoming()
		bs.removeInflightRequest(i)
		return true
	}

	bs.enqueueRsp(msg)
	bs.cache.bottomPort.RetrieveIncoming()
	bs.removeInflightRequest(i)

	return true
}

func (bs *bottomSender) processInvRspFromBottom(rsp *mem.InvRsp) bool {
	i := bs.findInvalidationByID(rsp.RespondTo, bs.inflightInvToBottom)
	if i == -1 {
		fmt.Printf("[%s]\tCannot find transaction for InvRsp with RspTo %s\n", bs.cache.Name(), rsp.RespondTo)
		bs.cache.bottomPort.RetrieveIncoming()
		return true
	}

	req := bs.inflightInvToBottom[i]
	// iter19 D1-D3-CD: outbound InvRsp routed via RDMAInvRspPort to
	// avoid head-of-line blocking on RDMAInvPort.
	invRspDst := bs.cache.ToRDMAInvRsp
	if invRspDst == "" {
		invRspDst = req.Meta().Src
	}
	rspToOutside := mem.InvRspBuilder{}.
		WithSrc(bs.cache.topPort.AsRemote()).
		WithDst(invRspDst).
		WithRspTo(req.ReqFrom).
		Build()

	if bs.cache.flushLocalAccess && !strings.Contains(fmt.Sprintf("%s", rspToOutside.Meta().Dst), "RDMA") {
		bs.cache.bottomPort.RetrieveIncoming()
		bs.removeInflightInvalidation(i)
		return true
	}

	bs.enqueueRsp(rspToOutside)
	bs.cache.bottomPort.RetrieveIncoming()
	bs.removeInflightInvalidation(i)

	return true
}

func (bs *bottomSender) processInvalidationRsp() bool {
	rsp := bs.cache.invRspBuffer.Pop()
	if rsp == nil {
		bs.returnFalse2 = "There is no invalidation response from invRspBuffer"
		return false
	}

	switch rsp := rsp.(type) {
	case *mem.InvRsp:
		return bs.processInvRsp(rsp)
	default:
		panic("unknown msg type")
	}
}

func (bs *bottomSender) processInvRsp(rsp *mem.InvRsp) bool {
	i := bs.findInvTransactionByID(rsp.RespondTo, bs.inflightInvToOutside)
	if i == -1 {
		return true
	}
	trans := bs.inflightInvToOutside[i]

	for j, sh := range trans.pendingEviction {
		if sh == rsp.SrcRDMA {
			trans.pendingEviction = append(trans.pendingEviction[:j], trans.pendingEviction[j+1:]...)
			break
		}
	}

	if len(trans.pendingEviction) == 0 {
		bs.inflightInvToOutside = append(bs.inflightInvToOutside[:i], bs.inflightInvToOutside[i+1:]...)
	}

	return true
}

// sendToTop drains each typed queue INDEPENDENTLY per tick so a stalled
// egress port cannot HoL-block the other ports (iter19 D1-D3-CD).
func (bs *bottomSender) sendToTop() bool {
	progress := false

	if bs.drainOneTypedQueue(&bs.sendToTopRspQue, bs.cache.topPort) {
		progress = true
	}
	if bs.drainRDMAQueue(&bs.sendToRDMAQue, bs.cache.RDMAPort, bs.cache.topPort) {
		progress = true
	}
	if bs.drainRDMAQueue(&bs.sendToRDMAInvQue, bs.cache.RDMAInvPort, bs.cache.topPort) {
		progress = true
	}
	if bs.drainRDMAQueue(&bs.sendToRDMAInvRspQue, bs.cache.RDMAInvRspPort, bs.cache.topPort) {
		progress = true
	}

	return progress
}

// drainOneTypedQueue tries to send the head of que via port; returns
// true if a message was sent.
func (bs *bottomSender) drainOneTypedQueue(que *[]sim.Msg, port sim.Port) bool {
	if len(*que) == 0 {
		return false
	}
	if port == nil {
		return false
	}
	if !port.CanSend() {
		return false
	}

	msg := (*que)[0]
	err := port.Send(msg)
	if err != nil {
		return false
	}

	(*que)[0] = nil
	*que = (*que)[1:]
	return true
}

// drainRDMAQueue tries to send via dedicated RDMA-side port. If that
// port is unavailable (nil) the queue falls back to the topPort so
// behavior degrades gracefully on builds that have not yet wired the
// split ports.
func (bs *bottomSender) drainRDMAQueue(que *[]sim.Msg, primary sim.Port, fallback sim.Port) bool {
	if len(*que) == 0 {
		return false
	}

	port := primary
	if port == nil {
		port = fallback
	}
	if port == nil {
		return false
	}
	if !port.CanSend() {
		return false
	}

	msg := (*que)[0]
	msg.Meta().Src = port.AsRemote()
	err := port.Send(msg)
	if err != nil {
		return false
	}

	(*que)[0] = nil
	*que = (*que)[1:]
	return true
}

func (bs *bottomSender) writeBufferFull() bool {
	numEntry := len(bs.inflightInvToOutside) + len(bs.inflightRequest)
	return numEntry >= bs.writeBufferCapacity
}

func (bs *bottomSender) tooManyInflightRequest() bool {
	return len(bs.inflightRequest) >= bs.maxInflightRequest
}

func (bs *bottomSender) tooManyInflightInvalidation() bool {
	return len(bs.inflightInvToOutside) >= bs.maxInflightInvalidation
}

// tooManyInflightInvalidationToBottom mirrors SD/REC's same-named helper —
// caps the number of local-L2-bound InvReqs the directory is waiting on.
func (bs *bottomSender) tooManyInflightInvalidationToBottom() bool {
	return len(bs.inflightInvToBottom) >= bs.maxInflightInvalidation
}

func (bs *bottomSender) Reset() {
	bs.cache.bottomSenderTransBuffer.Clear()
	bs.cache.bottomSenderInvBuffer.Clear()
	bs.inflightRequest = nil
	bs.inflightInvToBottom = nil
	bs.inflightInvToOutside = nil
	bs.sendToTopRspQue = nil
	bs.sendToRDMAQue = nil
	bs.sendToRDMAInvQue = nil
	bs.sendToRDMAInvRspQue = nil
}

func (bs *bottomSender) findTransactionByID(ID string, list []*transaction) int {
	for i, tr := range list {
		if tr.reqIDToBottom == ID {
			return i
		}
	}
	return -1
}

func (bs *bottomSender) findInvTransactionByID(ID string, list []*transaction) int {
	for i, tr := range list {
		if tr.accessReq().Meta().ID == ID {
			return i
		}
	}
	return -1
}

func (bs *bottomSender) findInvalidationByID(ID string, list []*mem.InvReq) int {
	for i, req := range list {
		if req.Meta().ID == ID {
			return i
		}
	}
	return -1
}

func (bs *bottomSender) removeInflightRequest(i int) {
	if len(bs.inflightRequest) <= i {
		panic(fmt.Sprintf("Trying to remove inflight request at index %d, but there are only %d inflight requests", i, len(bs.inflightRequest)))
	}
	bs.inflightRequest = append(bs.inflightRequest[:i], bs.inflightRequest[i+1:]...)
}

func (bs *bottomSender) removeInflightInvalidation(i int) {
	if len(bs.inflightInvToBottom) <= i {
		panic(fmt.Sprintf("Trying to remove inflight invalidation at index %d, but there are only %d inflight invalidations", i, len(bs.inflightInvToBottom)))
	}
	bs.inflightInvToBottom = append(bs.inflightInvToBottom[:i], bs.inflightInvToBottom[i+1:]...)
}
