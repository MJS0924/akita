package coherence

import (
	"fmt"
	"strings"

	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/tracing"
)

type bottomSender struct {
	cache *Comp

	writeBufferCapacity     int
	maxInflightRequest      int
	maxInflightInvalidation int

	inflightRequest      []*transaction
	inflightInvToOutside []*transaction
	inflightInvToBottom  []*mem.InvReq

	returnFalse0 string
	returnFalse1 string
	returnFalse2 string
}

func (bs *bottomSender) Tick() bool {
	madeProgress := false

	madeProgress = bs.processReturnRsp() || madeProgress
	madeProgress = bs.processInputReq() || madeProgress
	madeProgress = bs.processInvalidationRsp() || madeProgress

	return madeProgress
}

func (bs *bottomSender) processInputReq() bool {
	item := bs.cache.bottomSenderBuffer.Peek()
	if item == nil {
		bs.returnFalse1 = "There is no msg from bottomSenderBuffer"
		return false
	}

	progress := false
	switch req := item.(type) {
	case *transaction:
		progress = bs.processNewTransaction(req)
	case *mem.InvReq:
		progress = bs.sendInvReqToBottom(req)
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

	// if progress {
	// 	temp := bs.cache.bottomSenderBuffer.Pop().(*transaction)
	// 	if temp.accessReq().Meta().ID != trans.accessReq().Meta().ID {
	// 		panic("Popped transaction mismatch")
	// 	}
	// }
	return progress
}

func (bs *bottomSender) sendRequestToBottom(
	trans *transaction,
) bool {
	// if bs.tooManyInflightRequest() {
	// 	return false
	// }

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

	bs.cache.bottomSenderBuffer.Pop()

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
	// if bs.tooManyInflightInvalidation() {
	// 	return false
	// }

	progress := false

	i := bs.findInvTransactionByID(trans.accessReq().Meta().ID, bs.inflightInvToOutside) // reqToBottom이 아니라 accessReq의 ID로 찾기위해 InvTransaction 함수 사용
	if i == -1 {
		bs.inflightInvToOutside = append(bs.inflightInvToOutside, trans)
		progress = true
		// fmt.Printf("[%s]\tA.1. Add transaction to inflightInvToOutside: %s\n", bs.cache.Name(), trans.accessReq().Meta().ID)
	}

	for i := 0; i < len(trans.invalidationList); i++ {
		sh := trans.invalidationList[i]
		if sh == trans.accessReq().GetSrcRDMA() || sh == "" { // 이거 없애는 게 맞을지도?
			trans.invalidationList = append(trans.invalidationList[:i], trans.invalidationList[i+1:]...)
			i--
			continue
		}

		if !bs.cache.topPort.CanSend() {
			if !progress {
				bs.returnFalse1 = "[sendRequestToBottom] Cannot send to bottomPort"
			}

			return progress
		}

		req := mem.InvReqBuilder{}.
			WithSrc(bs.cache.topPort.AsRemote()).
			WithDst(bs.cache.ToRDMA).
			WithAddress(trans.evictingAddr).
			WithPID(trans.evictingPID).
			WithReqFrom(trans.accessReq().Meta().ID).
			WithDstRDMA(sh).
			Build()
		err := bs.cache.topPort.Send(req)

		if err != nil {
			if !progress {
				bs.returnFalse1 = "[sendRequestToBottom] Failed send to bottomPort"
			}

			return progress
		}
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

		// fmt.Printf("[%s]\tA.2. (%s -> %s) Send Invalidation Request for Addr %x to %s, pending eviction: %s\n",
		// 	bs.cache.Name(), trans.accessReq().Meta().ID, req.Meta().ID, trans.evictingAddr, req.Meta().Dst, sh)
	}

	if trans.action != InvalidateEntry { // InvalidateEntry는 실제 read/write 요청이 발생한 것이 아니므로 bottom으로 보내지 않음
		return bs.sendRequestToBottom(trans) || progress // response 받기 전에 request 아래로 보내버리기
	}
	bs.cache.bottomSenderBuffer.Pop()

	tracing.TraceReqFinalize(trans.accessReq(), bs.cache)

	return progress
}

func (bs *bottomSender) sendInvReqToBottom(req *mem.InvReq) bool {
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
		Build()

	err := bs.cache.bottomPort.Send(reqToBottom)
	if err != nil {
		bs.returnFalse1 = "[sendInvReqToBottom] Failed to send to bottomPort"
		return false
	}

	bs.cache.bottomSenderBuffer.Pop()

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

func (bs *bottomSender) processDataReadyRsp(msg *mem.DataReadyRsp) bool {
	if !bs.cache.topPort.CanSend() {
		bs.returnFalse0 = "[processDataReadyRsp] Cannot send to topPort"
		return false
	}

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
		// migration 중에는 local access에 대한 응답을 보내지 않음
	}

	err := bs.cache.topPort.Send(msg)
	if err == nil {
		bs.cache.bottomPort.RetrieveIncoming()
		bs.removeInflightRequest(i)

		return true
	}

	bs.returnFalse0 = "[processDataReadyRsp] Failed to send to topPort"
	return false
}

func (bs *bottomSender) processWriteDoneRsp(msg *mem.WriteDoneRsp) bool {
	if !bs.cache.topPort.CanSend() {
		bs.returnFalse0 = "[processWriteDoneRsp] Cannot send to topPort"
		return false
	}

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
		// migration 중에는 local access에 대한 응답을 보내지 않음
	}

	err := bs.cache.topPort.Send(msg)
	if err == nil {
		bs.cache.bottomPort.RetrieveIncoming()
		bs.removeInflightRequest(i)

		return true
	}

	bs.returnFalse0 = "[processWriteDoneRsp] Failed to send to topPort"
	return false
}

func (bs *bottomSender) processInvRspFromBottom(rsp *mem.InvRsp) bool {
	if !bs.cache.topPort.CanSend() {
		bs.returnFalse0 = "[processInvRspFromBottom] Cannot send to topPort"
		return false
	}

	i := bs.findInvalidationByID(rsp.RespondTo, bs.inflightInvToBottom)
	if i == -1 {
		fmt.Printf("[%s]\tCannot find transaction for InvRsp with RspTo %s\n", bs.cache.Name(), rsp.RespondTo)
		bs.cache.bottomPort.RetrieveIncoming()
		return true
	}

	req := bs.inflightInvToBottom[i]
	rspToOutside := mem.InvRspBuilder{}.
		WithSrc(bs.cache.topPort.AsRemote()).
		WithDst(req.Meta().Src).
		WithRspTo(req.ReqFrom).
		Build()

	if bs.cache.flushLocalAccess && !strings.Contains(fmt.Sprintf("%s", rspToOutside.Meta().Dst), "RDMA") {
		bs.cache.bottomPort.RetrieveIncoming()
		bs.removeInflightInvalidation(i)
		// migration 중에는 local access에 대한 응답을 보내지 않음
	}

	err := bs.cache.topPort.Send(rspToOutside)
	if err == nil {
		bs.cache.bottomPort.RetrieveIncoming()
		bs.removeInflightInvalidation(i)

		return true
	}

	bs.returnFalse0 = "[processInvRspFromBottom] Failed to send to topPort"
	return false
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
	// fmt.Printf("[%s.BS]\tF.0. Process InvRsp: rspTo %s, SrcRDMA %s\n", bs.cache.Name(), rsp.RespondTo, rsp.SrcRDMA)
	i := bs.findInvTransactionByID(rsp.RespondTo, bs.inflightInvToOutside)
	if i == -1 {
		// fmt.Printf("[%s]\tF. Cannot find transaction for InvRsp with RspTo %s\n", bs.cache.Name(), rsp.RespondTo)
		return true
	}
	trans := bs.inflightInvToOutside[i]

	for j, sh := range trans.pendingEviction {
		// fmt.Printf("[%s]\tF.1.0. Check pending eviction: %s\n", bs.cache.Name(), sh)
		if sh == rsp.SrcRDMA {
			trans.pendingEviction = append(trans.pendingEviction[:j], trans.pendingEviction[j+1:]...)
			// fmt.Printf("[%s]\tF.1.1. Remove pending Eviction: %s\n", bs.cache.Name(), rsp.SrcRDMA)
			break
		}
	}

	if len(trans.pendingEviction) == 0 {
		bs.inflightInvToOutside = append(bs.inflightInvToOutside[:i], bs.inflightInvToOutside[i+1:]...)
		// fmt.Printf("[%s]\tF.2. Remove inflight invalidation to outside\n", bs.cache.Name())
	}

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

func (bs *bottomSender) Reset() {
	bs.cache.bottomSenderBuffer.Clear()
	bs.inflightRequest = nil
	bs.inflightInvToBottom = nil
	bs.inflightInvToOutside = nil
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
