package optdirectory

import (
	"fmt"

	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/akita/v4/tracing"
)

type bottomSender struct {
	cache *Comp

	writeBufferCapacity     int
	maxInflightRequest      int
	maxInflightInvalidation int

	inflightRequest      []*transaction
	inflightInvToOutside []*transaction
	inflightInvToBottom  []*invTrans

	sendToBottomQue []sim.Msg
	sendToTopQue    []sim.Msg

	returnFalse0 string
	returnFalse1 string
	returnFalse2 string
}

func (bs *bottomSender) Tick() bool {
	madeProgress := false

	// madeProgress = bs.processReturnRsp() || madeProgress
	// madeProgress = bs.processInputReq() || madeProgress
	// madeProgress = bs.processInvalidationRsp() || madeProgress

	// madeProgress = bs.sendToBottom() || madeProgress
	// madeProgress = bs.sendToTop() || madeProgress

	temp := false
	temp = bs.processReturnRsp()
	madeProgress = madeProgress || temp
	if bs.cache.printReturn {
		// fmt.Printf("[DEBUG CohDir %d]\treturn 1.3.1: %v\n", bs.cache.deviceID, temp)
	}

	temp = bs.processInputReq()
	madeProgress = madeProgress || temp
	if bs.cache.printReturn {
		// fmt.Printf("[DEBUG CohDir %d]\treturn 1.3.2: %v\n", bs.cache.deviceID, temp)
	}

	temp = bs.processInvalidationRsp()
	madeProgress = madeProgress || temp
	if bs.cache.printReturn {
		// fmt.Printf("[DEBUG CohDir %d]\treturn 1.3.3: %v\n", bs.cache.deviceID, temp)
	}

	temp = bs.sendToBottom()
	madeProgress = madeProgress || temp
	if bs.cache.printReturn {
		// fmt.Printf("[DEBUG CohDir %d]\treturn 1.3.4: %v\n", bs.cache.deviceID, temp)
	}

	temp = bs.sendToTop()
	madeProgress = madeProgress || temp
	if bs.cache.printReturn {
		// fmt.Printf("[DEBUG CohDir %d]\treturn 1.3.5: %v\n", bs.cache.deviceID, temp)
	}

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
	case Nothing:
		progress = bs.sendRequestToBottom(trans)
	case InsertNewEntry, UpdateEntry:
		progress = bs.sendMultipleRequest(trans)
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

	req := bs.cache.cloneReq(trans.accessReq())
	req.Meta().Src = bs.cache.bottomPort.AsRemote()
	req.Meta().Dst = bs.cache.addressToPortMapper.Find(trans.accessReq().GetAddress())
	req.SetReqFrom(trans.accessReq().Meta().ID)

	bs.sendToBottomQue = append(bs.sendToBottomQue, req)
	if req.GetAddress()%(1<<bs.cache.log2BlockSize)+req.GetByteSize() > 1<<bs.cache.log2BlockSize {
		fmt.Printf("[%s][sendRequestToBottom]\tERROR: addr %x, offset %d, bytesize %d, blkSize %d, from %s, action %d, ID %s\n",
			bs.cache.name, req.GetAddress(), req.GetAddress()%(1<<bs.cache.log2BlockSize), req.GetByteSize(), bs.cache.log2BlockSize,
			trans.accessReq().Meta().Src, trans.action, trans.accessReq().Meta().ID)
	}

	bs.cache.bottomSenderBuffer.Pop()

	bs.inflightRequest = append(bs.inflightRequest, trans)
	trans.reqToBottom = append(trans.reqToBottom, &req)
	trans.ack++

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

func (bs *bottomSender) sendMultipleRequest(
	trans *transaction,
) bool {
	// if bs.tooManyInflightRequest() {
	// 	return false
	// }

	req := bs.cache.cloneReq(trans.accessReq())
	req.Meta().Src = bs.cache.bottomPort.AsRemote()
	req.Meta().Dst = bs.cache.addressToPortMapper.Find(trans.accessReq().GetAddress())
	req.SetReqFrom(trans.accessReq().Meta().ID)
	// fmt.Printf("[CohDir %d]\tAccess Req %x to %s\n", bs.cache.deviceID, req.GetAddress(), req.Meta().Dst)

	bs.sendToBottomQue = append(bs.sendToBottomQue, req)
	if req.GetAddress()%(1<<bs.cache.log2BlockSize)+req.GetByteSize() > 1<<bs.cache.log2BlockSize {
		fmt.Printf("[%s][sendMultipleRequest]\tERROR: addr %x, offset %d, bytesize %d, blkSize %d\n",
			bs.cache.name, req.GetAddress()%(1<<bs.cache.log2BlockSize), req.GetByteSize(), bs.cache.log2BlockSize)
		panic("ERR")
	}

	bs.cache.bottomSenderBuffer.Pop()

	bs.inflightRequest = append(bs.inflightRequest, trans)
	trans.reqToBottom = append(trans.reqToBottom, &req)
	trans.ack++

	originAddr := trans.accessReq().GetAddress()
	regionLen := bs.cache.log2BlockSize + bs.cache.log2UnitSize
	blkSize := bs.cache.log2BlockSize
	addr := originAddr >> regionLen << regionLen
	endAddr := addr + 1<<regionLen
	// fmt.Printf("[CohDir %d]\tPrefetch Req %x - %x from %s, ack: ", bs.cache.deviceID, addr, endAddr, req.GetSrcRDMA())

	for addr < endAddr {
		if addr>>blkSize == originAddr>>blkSize {
			addr += 1 << blkSize
			continue
		}

		req := bs.cache.cloneReq(trans.accessReq())
		req.SetAddress(addr)
		req.Meta().Src = bs.cache.bottomPort.AsRemote()
		req.Meta().Dst = bs.cache.addressToPortMapper.Find(addr)

		if trans.fromLocal { // L1 cache로 응답할 예정인 경우, L2 cache에서 response를 전송하지 않을 예정
			req.SetNoNeedToReply(true)
			// req.SetNiceness(true)

		} else { // RDMA로 응답할 예정인 경우, L2 cache에서 response를 전송해야 함
			req.SetNoNeedToReply(false)
			// req.SetNiceness(true)

			trans.reqToBottom = append(trans.reqToBottom, &req)
			trans.ack++
		}

		bs.sendToBottomQue = append(bs.sendToBottomQue, req)

		addr += 1 << blkSize
	}
	// fmt.Printf("%d\n", trans.ack)

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
		if sh == trans.accessReq().GetSrcRDMA() || sh == "" {
			trans.invalidationList = append(trans.invalidationList[:i], trans.invalidationList[i+1:]...)
			i--
			continue
		}

		req := mem.InvReqBuilder{}.
			WithSrc(bs.cache.topPort.AsRemote()).
			WithDst(bs.cache.ToRDMA).
			WithAddress(trans.evictingAddr).
			WithPID(trans.evictingPID).
			WithReqFrom(trans.accessReq().Meta().ID).
			WithDstRDMA(sh).
			Build()
		bs.sendToTopQue = append(bs.sendToTopQue, req)

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

		fmt.Printf("[%s]\tA. Send Invalidation Request to %s\n", bs.cache.Name(), sh)
	}

	if trans.action != InvalidateEntry { // InvalidateEntry는 실제 read/write 요청이 발생한 것이 아니므로 bottom으로 보내지 않음
		return bs.sendRequestToBottom(trans) || progress // response 받기 전에 request 아래로 보내버리기
	}
	bs.cache.bottomSenderBuffer.Pop()

	tracing.TraceReqFinalize(trans.accessReq(), bs.cache)

	return progress
}

func (bs *bottomSender) sendInvReqToBottom(req *mem.InvReq) bool {
	regionLen := bs.cache.log2BlockSize + bs.cache.log2UnitSize
	blockSize := bs.cache.log2BlockSize
	addr := req.GetAddress() >> regionLen << regionLen
	endAddr := addr + 1<<regionLen

	tr := invTrans{}
	tr.req = req

	for addr < endAddr {
		reqToBottom := mem.InvReqBuilder{}.
			WithSrc(bs.cache.bottomPort.AsRemote()).
			WithDst(bs.cache.addressToPortMapper.Find(req.Address)).
			WithPID(req.PID).
			WithAddress(req.Address).
			WithReqFrom(req.Meta().ID).
			Build()

		bs.sendToBottomQue = append(bs.sendToBottomQue, reqToBottom)

		addr += 1 << blockSize
		tr.ack++
	}

	bs.inflightInvToBottom = append(bs.inflightInvToBottom, &tr)
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
	i, j := bs.findTransactionByID(msg.GetRspTo(), bs.inflightRequest)
	if i == -1 {
		// fmt.Printf("[%s]\t3. Cannot find transaction for DataReadyRsp with RspTo %s\n", bs.cache.Name(), msg.GetRspTo())
		bs.cache.bottomPort.RetrieveIncoming()
		// return false
		return true
	}

	trans := bs.inflightRequest[i]
	trans.reqToBottom[j] = nil
	trans.reqToBottom = append(trans.reqToBottom[:j], trans.reqToBottom[j+1:]...)

	if len(trans.reqToBottom) == 0 {
		bs.removeInflightRequest(i)

		// if strings.Contains(fmt.Sprintf("%s", trans.accessReq().Meta().Src), "RDMA") {
		// 	fmt.Printf("[CohDir %d]\tSend last rsp for %x to %s, ack: %d\n",
		// 		bs.cache.deviceID, trans.accessReq().GetAddress(), trans.accessReq().Meta().Src, trans.ack)
		// }
	}

	msg.RespondTo = trans.accessReq().Meta().ID
	msg.Src = bs.cache.topPort.AsRemote()
	msg.Dst = trans.accessReq().Meta().Src
	msg.WaitFor = trans.ack

	// if bs.cache.flushLocalAccess && !strings.Contains(fmt.Sprintf("%s", msg.Meta().Dst), "RDMA") {
	// 	bs.cache.bottomPort.RetrieveIncoming()
	// 	bs.removeInflightRequest(i)

	// 	return true
	// 	// migration 중에는 local access에 대한 응답을 보내지 않음
	// }

	bs.sendToTopQue = append(bs.sendToTopQue, msg)

	bs.cache.bottomPort.RetrieveIncoming()

	return true
}

func (bs *bottomSender) processWriteDoneRsp(msg *mem.WriteDoneRsp) bool {
	i, j := bs.findTransactionByID(msg.GetRspTo(), bs.inflightRequest)
	if i == -1 {
		// fmt.Printf("[%s]\t3. Cannot find transaction for WriteDoneRsp with RspTo %s\n", bs.cache.Name(), msg.GetRspTo())
		bs.cache.bottomPort.RetrieveIncoming()
		// return false
		return true
	}

	trans := bs.inflightRequest[i]
	trans.reqToBottom[j] = nil
	trans.reqToBottom = append(trans.reqToBottom[:j], trans.reqToBottom[j+1:]...)

	if len(trans.reqToBottom) == 0 {
		bs.removeInflightRequest(i)
	}

	msg.RespondTo = trans.accessReq().Meta().ID
	msg.Src = bs.cache.topPort.AsRemote()
	msg.Dst = trans.accessReq().Meta().Src
	msg.WaitFor = trans.ack

	// if bs.cache.flushLocalAccess && !strings.Contains(fmt.Sprintf("%s", msg.Meta().Dst), "RDMA") {
	// 	bs.cache.bottomPort.RetrieveIncoming()
	// 	bs.removeInflightRequest(i)

	// 	return true
	// 	// migration 중에는 local access에 대한 응답을 보내지 않음
	// }

	bs.sendToTopQue = append(bs.sendToTopQue, msg)
	bs.cache.bottomPort.RetrieveIncoming()

	bs.returnFalse0 = "[processWriteDoneRsp] Failed to send to topPort"
	return true
}

func (bs *bottomSender) processInvRspFromBottom(rsp *mem.InvRsp) bool {
	i := bs.findInvalidationByID(rsp.RespondTo, bs.inflightInvToBottom)
	if i == -1 {
		fmt.Printf("[%s]\t1. Cannot find transaction for InvRsp with RspTo %s\n", bs.cache.Name(), rsp.RespondTo)
		bs.cache.bottomPort.RetrieveIncoming()
		// return false
		return true
	}

	inflightInv := bs.inflightInvToBottom[i]
	inflightInv.ack--
	if inflightInv.ack > 0 {
		bs.cache.bottomPort.RetrieveIncoming()
		return true
	}

	req := inflightInv.req
	rspToOutside := mem.InvRspBuilder{}.
		WithSrc(bs.cache.topPort.AsRemote()).
		WithDst(req.Meta().Src).
		WithRspTo(req.ReqFrom).
		Build()

	// if bs.cache.flushLocalAccess && !strings.Contains(fmt.Sprintf("%s", rspToOutside.Meta().Dst), "RDMA") {
	// 	bs.cache.bottomPort.RetrieveIncoming()
	// 	bs.removeInflightInvalidation(i)

	// 	return true
	// 	// migration 중에는 local access에 대한 응답을 보내지 않음
	// }

	bs.sendToTopQue = append(bs.sendToTopQue, rspToOutside)
	bs.cache.bottomPort.RetrieveIncoming()
	// fmt.Printf("[%s]\t1. Send Invalidation Response to %s, rspTo %s\n", bs.cache.Name(), rspToOutside.Dst, rsp.RespondTo)

	return true
}

func (bs *bottomSender) processInvalidationRsp() bool {
	rsp := bs.cache.invRspBuffer.Peek()
	if rsp == nil {
		bs.returnFalse2 = "There is no invalidation response from invRspBuffer"
		return false
	}

	progress := false
	switch rsp := rsp.(type) {
	case *mem.InvRsp:
		progress = bs.processInvRsp(rsp)
	default:
		panic("unknown msg type")
	}

	if progress {
		bs.cache.invRspBuffer.Pop()
	}
	return progress
}

func (bs *bottomSender) processInvRsp(rsp *mem.InvRsp) bool {
	// fmt.Printf("[%s.BS]\tF.0. Process InvRsp: rspTo %s, SrcRDMA %s\n", bs.cache.Name(), rsp.RespondTo, rsp.SrcRDMA)
	i := bs.findInvTransactionByID(rsp.RespondTo, bs.inflightInvToOutside)
	if i == -1 {
		fmt.Printf("[%s]\t4. Cannot find transaction for InvRsp with RspTo %s\n", bs.cache.Name(), rsp.RespondTo)
		return true
	}
	trans := bs.inflightInvToOutside[i]

	for j, sh := range trans.pendingEviction {
		// fmt.Printf("[%s]\tF.1.0. Check pending eviction: %s\n", bs.cache.Name(), sh)
		if sh == rsp.SrcRDMA {
			trans.pendingEviction = append(trans.pendingEviction[:j], trans.pendingEviction[j+1:]...)
			// fmt.Printf("[%s]\t4. Remove pending Eviction: %s\n", bs.cache.Name(), rsp.SrcRDMA)
			break
		}
	}

	if len(trans.pendingEviction) == 0 {
		bs.inflightInvToOutside = append(bs.inflightInvToOutside[:i], bs.inflightInvToOutside[i+1:]...)
		// fmt.Printf("[%s]\tF.2. Remove inflight invalidation to outside\n", bs.cache.Name())
	}

	return true
}

func (bs *bottomSender) sendToTop() bool {
	if len(bs.sendToTopQue) == 0 {
		return false
	}

	if !bs.cache.topPort.CanSend() {
		return false
	}

	msg := bs.sendToTopQue[0]
	err := bs.cache.topPort.Send(msg)

	if err != nil {
		return false
	}

	bs.sendToTopQue = bs.sendToTopQue[1:]

	return true
}

func (bs *bottomSender) sendToBottom() bool {
	if len(bs.sendToBottomQue) == 0 {
		return false
	}

	if !bs.cache.bottomPort.CanSend() {
		return false
	}

	msg := bs.sendToBottomQue[0]
	err := bs.cache.bottomPort.Send(msg)

	if err != nil {
		return false
	}

	bs.sendToBottomQue = bs.sendToBottomQue[1:]

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
	bs.sendToTopQue = nil
	bs.sendToBottomQue = nil
}

func (bs *bottomSender) findTransactionByID(ID string, list []*transaction) (int, int) {
	for i, tr := range list {
		for j, req := range tr.reqToBottom {
			if (*req).Meta().ID == ID {
				return i, j
			}
		}
	}
	return -1, -1
}

func (bs *bottomSender) findInvTransactionByID(ID string, list []*transaction) int {
	for i, tr := range list {
		if tr.accessReq().Meta().ID == ID {
			return i
		}
	}
	return -1
}

func (bs *bottomSender) findInvalidationByID(ID string, list []*invTrans) int {
	for i, tr := range list {
		if tr.req.Meta().ID == ID {
			return i
		}
	}
	return -1
}

func (bs *bottomSender) removeInflightRequest(i int) {
	if len(bs.inflightRequest) <= i {
		panic(fmt.Sprintf("Trying to remove inflight request at index %d...", i))
	}
	copy(bs.inflightRequest[i:], bs.inflightRequest[i+1:])
	bs.inflightRequest[len(bs.inflightRequest)-1] = nil // 포인터 명시적 해제
	bs.inflightRequest = bs.inflightRequest[:len(bs.inflightRequest)-1]
}

func (bs *bottomSender) removeInflightInvalidation(i int) {
	if len(bs.inflightInvToBottom) <= i {
		panic(fmt.Sprintf("Trying to remove inflight invalidation at index %d...", i))
	}
	copy(bs.inflightInvToBottom[i:], bs.inflightInvToBottom[i+1:])
	bs.inflightInvToBottom[len(bs.inflightInvToBottom)-1] = nil // 포인터 명시적 해제
	bs.inflightInvToBottom = bs.inflightInvToBottom[:len(bs.inflightInvToBottom)-1]
}
