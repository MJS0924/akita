package optdirectory

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

	// localInflightBypassRequest []*transaction
	// [수정] Inflight 트랜잭션을 Local과 Remote로 분리
	localInflightRequest       []*transaction
	localInflightBypassRequest []*transaction
	remoteInflightRequest      []*transaction

	inflightInvToOutside []*transaction
	inflightInvToBottom  []*invTrans

	pendingWriteAfterInv []*transaction // write transactions waiting for L2 after all InvRsps received

	sendToBottomQue       []sim.Msg
	remoteSendToBottomQue []sim.Msg
	sendToTopQue          []sim.Msg
	sendToRemoteTopQue    []sim.Msg // remote(RDMAPort)로 나가야 하는 응답 전용 (Src에 RDMA 없는 쓰기 eviction 등)
	bypassRspQue          []sim.Msg

	returnFalse0 string
	returnFalse1 string
	returnFalse2 string
}

func (bs *bottomSender) Tick() bool {
	madeProgress := false

	temp := false
	// [추가] Bypass 버퍼를 가장 먼저(또는 병렬로) 확인하여 빠른 처리 보장
	temp = bs.processBypassReq()
	madeProgress = madeProgress || temp
	if bs.cache.printReturn {
		fmt.Printf("[DEBUG CohDir %d]\treturn 1.3.0: %v\n", bs.cache.deviceID, temp)
	}

	temp = bs.processReturnRsp()
	madeProgress = madeProgress || temp
	if bs.cache.printReturn {
		fmt.Printf("[DEBUG CohDir %d]\treturn 1.3.1: %v\n", bs.cache.deviceID, temp)
	}

	temp = bs.processInputReq()
	madeProgress = madeProgress || temp
	if bs.cache.printReturn {
		fmt.Printf("[DEBUG CohDir %d]\treturn 1.3.2: %v\n", bs.cache.deviceID, temp)
	}

	temp = bs.processInvalidationReq()
	madeProgress = madeProgress || temp
	if bs.cache.printReturn {
		fmt.Printf("[DEBUG CohDir %d]\treturn 1.3.3: %v\n", bs.cache.deviceID, temp)
	}

	temp = bs.processInvalidationRsp()
	madeProgress = madeProgress || temp
	if bs.cache.printReturn {
		fmt.Printf("[DEBUG CohDir %d]\treturn 1.3.3: %v\n", bs.cache.deviceID, temp)
	}

	temp = bs.processPendingWriteAfterInv()
	madeProgress = madeProgress || temp

	temp = bs.sendToBottom()
	madeProgress = madeProgress || temp
	if bs.cache.printReturn {
		fmt.Printf("[DEBUG CohDir %d]\treturn 1.3.4: %v\n", bs.cache.deviceID, temp)
	}

	// [추가] 일반 응답보다 Bypass 응답을 최우선으로 전송 (우선순위 라우팅)
	temp = bs.sendBypassRspToTop()
	madeProgress = madeProgress || temp

	// [FIX] Dst에 "RDMA"가 없는 remote 응답(write eviction 등)을 RDMAPort로 전송
	temp = bs.sendRemoteRspToTop()
	madeProgress = madeProgress || temp

	temp = bs.sendToTop()
	madeProgress = madeProgress || temp
	if bs.cache.printReturn {
		// fmt.Printf("[DEBUG CohDir %d]\treturn 1.3.5: %v\n", bs.cache.deviceID, temp)
	}

	return madeProgress
}

// [추가] Bypass 전용 처리 함수
func (bs *bottomSender) processBypassReq() bool {
	item := bs.cache.localBypassBuffer.Peek()
	if item == nil {
		return false
	}

	trans := item.(*transaction)

	req := bs.cache.cloneReq(trans.accessReq())
	req.Meta().Src = bs.cache.bottomPort.AsRemote()
	if trans.fromLocal {
		req.Meta().Dst = bs.cache.addressToPortMapper.Find(trans.accessReq().GetAddress())
	} else {
		req.Meta().Dst = bs.cache.addressToPortMapperForRemoteReq.Find(trans.accessReq().GetAddress())
	}
	req.SetReqFrom(trans.accessReq().Meta().ID)

	bs.sendToBottomQue = append(bs.sendToBottomQue, req)

	// Bypass 버퍼에서 제거
	bs.cache.localBypassBuffer.Pop()

	// bs.localInflightBypassRequest = append(bs.localInflightBypassRequest, trans)
	bs.localInflightBypassRequest = append(bs.localInflightBypassRequest, trans)
	trans.reqToBottom = append(trans.reqToBottom, &req)
	trans.ack++

	tracing.AddTaskStep(tracing.MsgIDAtReceiver(trans.accessReq(), bs.cache), bs.cache, "BypassToLocalL2")
	tracing.TraceReqFinalize(trans.accessReq(), bs.cache)

	return true
}

// [수정] 양쪽 큐를 모두 확인하여 데드락 방지
func (bs *bottomSender) processInputReq() bool {
	progress := false

	// 1. Remote 버퍼 우선 확인 (원격 응답/요청을 먼저 빼주어 네트워크 데드락 완화)
	item := bs.cache.remoteBottomSenderBuffer.Peek()
	if item != nil {
		if bs.processItem(item, false) {
			bs.cache.remoteBottomSenderBuffer.Pop()
			progress = true
		}
	}

	// 2. Local 버퍼 확인
	item = bs.cache.localBottomSenderBuffer.Peek()
	if item != nil {
		if bs.processItem(item, true) {
			bs.cache.localBottomSenderBuffer.Pop()
			progress = true
		}
	}

	return progress
}

func (bs *bottomSender) processItem(item interface{}, isLocal bool) bool {
	switch req := item.(type) {
	case *transaction:
		return bs.processNewTransaction(req, isLocal)
	}
	return false
}

func (bs *bottomSender) processNewTransaction(trans *transaction, isLocal bool) bool {

	progress := false
	switch trans.action {
	case Nothing:
		progress = bs.sendRequestToBottom(trans, isLocal)
	case InsertNewEntry, UpdateEntry:
		if bs.cache.fetchSingleCacheLine {
			progress = bs.sendRequestToBottom(trans, isLocal)
		} else {
			progress = bs.sendMultipleRequest(trans, isLocal)
		}
	case EvictAndInsertNewEntry, InvalidateAndUpdateEntry, InvalidateEntry:
		progress = bs.sendInvalidationRequest(trans, isLocal)
	default:
		panic("unknown transaction action")
	}

	if progress {
		if bs.cache.debugProcess && trans.accessReq() != nil && trans.accessReq().GetAddress() == bs.cache.debugAddress {
			if !isLocal {
				fmt.Printf("[%s][DEBUG]\tReadReq received - 3, action %d: %x\n", bs.cache.name, trans.action, trans.accessReq().GetAddress())
			} else {
				fmt.Printf("[%s][DEBUG]\tReadReq received - -1: %x\n", bs.cache.name, trans.accessReq().GetAddress())
			}
		}
	}
	return progress
}

// [수정] Local/Remote 배열 분리 저장
func (bs *bottomSender) sendRequestToBottom(trans *transaction, isLocal bool) bool {
	if bs.tooManyInflightRequest(isLocal) {
		return false
	}

	req := bs.cache.cloneReq(trans.accessReq())

	// 1. 포트 및 Dst 설정
	var srcPort sim.Port
	if isLocal {
		srcPort = bs.cache.bottomPort
		req.Meta().Dst = bs.cache.addressToPortMapper.Find(trans.accessReq().GetAddress())
	} else {
		srcPort = bs.cache.remoteBottomPort
		req.Meta().Dst = bs.cache.addressToPortMapperForRemoteReq.Find(trans.accessReq().GetAddress())
	}
	req.Meta().Src = srcPort.AsRemote()
	req.SetReqFrom(trans.accessReq().Meta().ID)

	// 2. 물리적 큐와 Inflight 분기 삽입
	if isLocal {
		bs.sendToBottomQue = append(bs.sendToBottomQue, req)
		bs.localInflightRequest = append(bs.localInflightRequest, trans)
	} else {
		bs.remoteSendToBottomQue = append(bs.remoteSendToBottomQue, req)
		bs.remoteInflightRequest = append(bs.remoteInflightRequest, trans)
	}

	trans.reqToBottom = append(trans.reqToBottom, &req)
	trans.ack++

	what := "Nothing"
	if trans.action != Nothing {
		what = "UpdateEntry"
	}
	tracing.AddTaskStep(tracing.MsgIDAtReceiver(trans.accessReq(), bs.cache), bs.cache, what)
	tracing.TraceReqFinalize(trans.accessReq(), bs.cache)

	if bs.cache.debugProcess && trans.accessReq() != nil && trans.accessReq().GetAddress() == bs.cache.debugAddress {
		if isLocal {
			fmt.Printf("[%s][DEBUG]\tReadReq received - 3.1: %x\n", bs.cache.name, trans.accessReq().GetAddress())
		} else {
			fmt.Printf("[%s][DEBUG]\tRemote ReadReq received - 3.1: %x\n", bs.cache.name, trans.accessReq().GetAddress())
		}
	}
	return true
}

func (bs *bottomSender) sendMultipleRequest(trans *transaction, isLocal bool) bool {
	if bs.tooManyInflightRequest(isLocal) {
		return false
	}

	req := bs.cache.cloneReq(trans.accessReq())

	var srcPort sim.Port
	if isLocal {
		srcPort = bs.cache.bottomPort
		req.Meta().Dst = bs.cache.addressToPortMapper.Find(trans.accessReq().GetAddress())
	} else {
		srcPort = bs.cache.remoteBottomPort
		req.Meta().Dst = bs.cache.addressToPortMapperForRemoteReq.Find(trans.accessReq().GetAddress())
	}
	req.Meta().Src = srcPort.AsRemote()
	req.SetReqFrom(trans.accessReq().Meta().ID)

	if req.GetAddress()%(1<<bs.cache.log2BlockSize)+req.GetByteSize() > 1<<bs.cache.log2BlockSize {
		fmt.Printf("[%s][sendMultipleRequest]\tERROR: addr %x, offset %x, bytesize %d, blkSize %d\n",
			bs.cache.name, req.GetAddress(), req.GetAddress()%(1<<bs.cache.log2BlockSize), req.GetByteSize(), bs.cache.log2BlockSize)
		panic("ERR")
	}

	if isLocal {
		bs.sendToBottomQue = append(bs.sendToBottomQue, req)
		bs.localInflightRequest = append(bs.localInflightRequest, trans)
	} else {
		bs.remoteSendToBottomQue = append(bs.remoteSendToBottomQue, req)
		bs.remoteInflightRequest = append(bs.remoteInflightRequest, trans)
	}

	trans.reqToBottom = append(trans.reqToBottom, &req)
	trans.ack++

	originAddr := trans.accessReq().GetAddress()
	regionLen := bs.cache.log2BlockSize + bs.cache.log2UnitSize
	blkSize := bs.cache.log2BlockSize
	addr := originAddr >> regionLen << regionLen
	endAddr := addr + 1<<regionLen

	for addr < endAddr {
		if addr>>blkSize == originAddr>>blkSize {
			addr += 1 << blkSize
			continue
		}

		req := bs.cache.cloneReq(trans.accessReq())
		req.SetAddress(addr)
		req.Meta().Src = srcPort.AsRemote()

		if isLocal {
			req.Meta().Dst = bs.cache.addressToPortMapper.Find(addr)
			req.SetNoNeedToReply(true)
		} else {
			req.Meta().Dst = bs.cache.addressToPortMapperForRemoteReq.Find(addr)
			req.SetNoNeedToReply(false)
			trans.reqToBottom = append(trans.reqToBottom, &req)
			trans.ack++
		}

		if isLocal {
			bs.sendToBottomQue = append(bs.sendToBottomQue, req)
		} else {
			bs.remoteSendToBottomQue = append(bs.remoteSendToBottomQue, req)
		}
		addr += 1 << blkSize
	}

	what := ""
	if trans.action != Nothing {
		what = "UpdateEntry"
	}
	tracing.AddTaskStep(tracing.MsgIDAtReceiver(trans.accessReq(), bs.cache), bs.cache, what)
	tracing.TraceReqFinalize(trans.accessReq(), bs.cache)

	if bs.cache.debugProcess && trans.accessReq() != nil && trans.accessReq().GetAddress() == bs.cache.debugAddress {
		if isLocal {
			fmt.Printf("[%s][DEBUG]\tReadReq received - 3.2: %x\n", bs.cache.name, trans.accessReq().GetAddress())
		} else {
			fmt.Printf("[%s][DEBUG]\tRemote ReadReq received - 3.2: %x\n", bs.cache.name, trans.accessReq().GetAddress())
		}
	}
	return true
}

func (bs *bottomSender) sendInvalidationRequest(trans *transaction, isLocal bool) bool {
	if trans.action != InvalidateEntry && bs.tooManyInflightRequest(isLocal) {
		// return bs.sendRequestToBottom(trans, isLocal) || progress에서
		// snedRequestToBottom이 false를 반환하는 경우가 있음, 이때 transaction이 버려지는 현상 방지
		return false
	}

	if bs.tooManyInflightInvalidation() {
		return false
	}

	// 1. [수정] Inflight 리스트에 무작정 넣기 전에, 보낼 대상을 먼저 선별합니다.
	var validTargets []sim.RemotePort
	for i := 0; i < len(trans.invalidationList); i++ {
		sh := trans.invalidationList[i]
		// 나 자신이거나 유효하지 않은 포트면 건너뜀
		if sh == trans.accessReq().GetSrcRDMA() || sh == "" {
			continue
		}
		validTargets = append(validTargets, sh)
	}

	progress := false

	// 2. [수정] 진짜로 무효화 메시지를 보낼 외부 노드가 있을 때만 Inflight에 등록합니다.
	// if trans.action == InvalidateAndUpdateEntry {
	// 	fmt.Fprintf(os.Stderr, "[%s] [DEBUG]\tInvalidateAndupdateEntry(%6.6s)(%6.6s) - 2.1\n", bs.cache.name, trans.invalidationList, validTargets)
	// }
	if len(validTargets) > 0 {
		i := bs.findInvTransactionByID(trans.accessReq().Meta().ID, bs.inflightInvToOutside)
		if i == -1 {
			bs.inflightInvToOutside = append(bs.inflightInvToOutside, trans)
			// fmt.Printf("[%s]\tStart Invalidation: %x\n", bs.cache.name, trans.evictingAddr)
			progress = true
		}

		for _, sh := range validTargets {
			req := mem.InvReqBuilder{}.
				WithSrc(bs.cache.topPort.AsRemote()).
				WithDst(bs.cache.ToRDMAInv).
				WithAddress(trans.evictingAddr).
				WithPID(trans.evictingPID).
				WithReqFrom(trans.accessReq().Meta().ID).
				WithDstRDMA(sh).
				Build()

			bs.sendToTopQue = append(bs.sendToTopQue, req)
			trans.pendingEviction = append(trans.pendingEviction, sh)
			progress = true

			what := ""
			if trans.action == EvictAndInsertNewEntry {
				what = "InvalidateByEviction"
			} else if trans.action == InvalidateAndUpdateEntry {
				// fmt.Fprintf(os.Stderr, "[%s] [DEBUG]\tInvalidateAndupdateEntry - 2.2\n", bs.cache.name)
				what = "InvalidateByWrite"
			}
			if what != "" {
				tracing.AddTaskStep(tracing.MsgIDAtReceiver(trans.accessReq(), bs.cache), bs.cache, what)
			}
		}
	}

	tracing.TraceReqFinalize(trans.accessReq(), bs.cache)

	// [중요] InvalidationList 갱신 (선별된 타겟들만 남김, 혹은 비움)
	trans.invalidationList = nil

	if trans.action != InvalidateEntry {
		if bs.cache.debugProcess && progress && !isLocal && trans.accessReq() != nil && trans.accessReq().GetAddress() == bs.cache.debugAddress {
			fmt.Printf("[%s][DEBUG]\tReadReq received - 3.3.0: %x\n", bs.cache.name, trans.accessReq().GetAddress())
		}
		return bs.sendRequestToBottom(trans, isLocal) || progress
	}

	if progress && bs.cache.debugProcess && trans.accessReq() != nil && trans.accessReq().GetAddress() == bs.cache.debugAddress {
		if isLocal {
			fmt.Printf("[%s][DEBUG]\tReadReq received - 3.3.1: %x\n", bs.cache.name, trans.accessReq().GetAddress())
		} else {
			fmt.Printf("[%s][DEBUG]\tRemote ReadReq received - 3.3.1: %x\n", bs.cache.name, trans.accessReq().GetAddress())
		}
	}
	return progress
}

func (bs *bottomSender) processReturnRsp() bool {
	madeProgress := false

	// 1. 데드락 방지를 위해 Remote 포트를 우선 처리
	msg := bs.cache.remoteBottomPort.PeekIncoming()
	if msg != nil {
		madeProgress = bs.processRspMsg(msg, bs.cache.remoteBottomPort) || madeProgress
	}

	// 2. Local 포트 처리
	msg = bs.cache.bottomPort.PeekIncoming()
	if msg != nil {
		madeProgress = bs.processRspMsg(msg, bs.cache.bottomPort) || madeProgress
	}

	if !madeProgress {
		bs.returnFalse0 = "There is no msg from bottomPort"
	}

	return madeProgress
}

// [추가] 공통 라우팅 로직 (포트를 인자로 받음)
func (bs *bottomSender) processRspMsg(msg sim.Msg, port sim.Port) bool {
	switch msg := msg.(type) {
	case *mem.DataReadyRsp:
		return bs.processDataReadyRsp(msg, port)
	case *mem.WriteDoneRsp:
		return bs.processWriteDoneRsp(msg, port)
	case *mem.InvRsp:
		return bs.processInvRspFromBottom(msg, port)
	default:
		panic("unknown msg type")
	}
}

func (bs *bottomSender) processDataReadyRsp(msg *mem.DataReadyRsp, port sim.Port) bool {
	isBypass := false
	isLocal := false

	// 1. Bypass Inflight 배열에서 먼저 검색
	i, j := bs.findTransactionByID(msg.GetRspTo(), bs.localInflightBypassRequest)

	if i != -1 {
		isBypass = true
	} else {
		// 2. Local Inflight 배열 검색
		i, j = bs.findTransactionByID(msg.GetRspTo(), bs.localInflightRequest)
		if i != -1 {
			isLocal = true
		} else {
			// 3. Remote Inflight 배열 검색
			i, j = bs.findTransactionByID(msg.GetRspTo(), bs.remoteInflightRequest)
		}
	}

	if i == -1 {
		port.RetrieveIncoming()
		return true
	}

	// 타겟 트랜잭션 포인터 획득
	var trans *transaction
	if isBypass {
		trans = bs.localInflightBypassRequest[i]
	} else if isLocal {
		trans = bs.localInflightRequest[i]
	} else {
		trans = bs.remoteInflightRequest[i]
	}

	trans.reqToBottom[j] = nil
	trans.reqToBottom = append(trans.reqToBottom[:j], trans.reqToBottom[j+1:]...)

	if len(trans.reqToBottom) == 0 {
		// [수정] 출처에 맞는 삭제 함수 호출
		if isBypass {
			bs.removeInflightBypassRequest(i)
		} else {
			bs.removeInflightRequest(i, isLocal)
		}
	}

	msg.RespondTo = trans.accessReq().Meta().ID
	msg.Src = bs.cache.topPort.AsRemote()
	msg.Dst = trans.accessReq().Meta().Src
	msg.WaitFor = trans.ack

	// [수정] Bypass 트랜잭션이거나 Bypass 액션인 경우 우회 큐로 삽입
	if isBypass || trans.action == BypassingDirectory {
		bs.bypassRspQue = append(bs.bypassRspQue, msg)
	} else if !trans.fromLocal && !strings.Contains(fmt.Sprintf("%s", msg.Meta().Dst), "RDMA") {
		bs.sendToRemoteTopQue = append(bs.sendToRemoteTopQue, msg)
	} else {
		bs.sendToTopQue = append(bs.sendToTopQue, msg)
	}

	port.RetrieveIncoming()

	if bs.cache.debugProcess && trans.accessReq() != nil && trans.accessReq().GetAddress() == bs.cache.debugAddress {
		if trans.fromLocal {
			fmt.Printf("[CohDir %s][DEBUG]\tReadReq received - 5: %x\n", bs.cache.name, trans.accessReq().GetAddress())
		} else {
			fmt.Printf("[CohDir %s][DEBUG]\tReadReq received - 4: %x\n", bs.cache.name, trans.accessReq().GetAddress())
		}
	}

	return true
}

func (bs *bottomSender) processWriteDoneRsp(msg *mem.WriteDoneRsp, port sim.Port) bool {
	isBypass := false
	isLocal := false

	// 1. Bypass Inflight 배열에서 먼저 검색
	i, j := bs.findTransactionByID(msg.GetRspTo(), bs.localInflightBypassRequest)

	if i != -1 {
		isBypass = true
	} else {
		// 2. Local Inflight 배열 검색
		i, j = bs.findTransactionByID(msg.GetRspTo(), bs.localInflightRequest)
		if i != -1 {
			isLocal = true
		} else {
			// 3. Remote Inflight 배열 검색
			i, j = bs.findTransactionByID(msg.GetRspTo(), bs.remoteInflightRequest)
		}
	}

	if i == -1 {
		port.RetrieveIncoming()
		return true
	}

	var trans *transaction
	if isBypass {
		trans = bs.localInflightBypassRequest[i]
	} else if isLocal {
		trans = bs.localInflightRequest[i]
	} else {
		trans = bs.remoteInflightRequest[i]
	}

	trans.reqToBottom[j] = nil
	trans.reqToBottom = append(trans.reqToBottom[:j], trans.reqToBottom[j+1:]...)

	if len(trans.reqToBottom) == 0 {
		// [수정] 출처에 맞는 삭제 함수 호출
		if isBypass {
			bs.removeInflightBypassRequest(i)
		} else {
			bs.removeInflightRequest(i, isLocal)
		}
	}

	msg.RespondTo = trans.accessReq().Meta().ID
	msg.Src = bs.cache.topPort.AsRemote()
	msg.Dst = trans.accessReq().Meta().Src
	msg.WaitFor = trans.ack

	// [수정] Bypass 트랜잭션이거나 Bypass 액션인 경우 우회 큐로 삽입
	if isBypass || trans.action == BypassingDirectory {
		bs.bypassRspQue = append(bs.bypassRspQue, msg)
	} else if !trans.fromLocal && !strings.Contains(fmt.Sprintf("%s", msg.Meta().Dst), "RDMA") {
		bs.sendToRemoteTopQue = append(bs.sendToRemoteTopQue, msg)
	} else {
		bs.sendToTopQue = append(bs.sendToTopQue, msg)
	}

	port.RetrieveIncoming()
	return true
}

func (bs *bottomSender) processInvRspFromBottom(rsp *mem.InvRsp, port sim.Port) bool {
	i := bs.findInvalidationByID(rsp.RespondTo, bs.inflightInvToBottom)
	if i == -1 {
		port.RetrieveIncoming()
		return true
	}

	inflightInv := bs.inflightInvToBottom[i]
	inflightInv.ack--
	if inflightInv.ack > 0 {
		port.RetrieveIncoming()
		return true
	}

	req := inflightInv.req
	rspToOutside := mem.InvRspBuilder{}.
		WithSrc(bs.cache.topPort.AsRemote()).
		WithDst(req.Meta().Src).
		WithRspTo(req.ReqFrom).
		Build()

	bs.sendToTopQue = append(bs.sendToTopQue, rspToOutside)
	port.RetrieveIncoming()

	// [핵심 추가] 처리가 완료된 트랜잭션을 Inflight 배열에서 안전하게 삭제합니다.
	bs.removeInflightInvalidation(i)
	// fmt.Printf("[%s]\tFinalize Inv Req - 0: %s\n", bs.cache.name, req.ReqFrom)
	// if req.Address == 21475226560 {
	// 	fmt.Printf("[%s]\t[DEBUG] Invalidation - 3: %s\n", bs.cache.name, rsp.RespondTo)
	// }

	return true
}

func (bs *bottomSender) processInvalidationReq() bool {
	item := bs.cache.invReqBuffer.Peek()
	if item == nil {
		bs.returnFalse1 = "There is no invalidation request from invReqBuffer"
		return false
	}

	if bs.tooManyInflightInvalidationToBottom() {
		bs.returnFalse1 = "Too many inflight invalidation to bottom"
		return false
	}

	req := item.(*mem.InvReq)

	regionLen := bs.cache.log2BlockSize + bs.cache.log2UnitSize
	blockSize := bs.cache.log2BlockSize
	addr := req.GetAddress() >> regionLen << regionLen
	endAddr := addr + 1<<regionLen

	tr := invTrans{}
	tr.req = req

	for addr < endAddr {
		// [수정] InvReq는 외부 개입(Intervention)이므로 Remote 포트로 송신
		reqToBottom := mem.InvReqBuilder{}.
			WithSrc(bs.cache.remoteBottomPort.AsRemote()).
			WithDst(bs.cache.addressToPortMapperForRemoteReq.Find(req.Address)).
			WithPID(req.PID).
			WithAddress(req.Address).
			WithReqFrom(req.Meta().ID).
			Build()

		bs.remoteSendToBottomQue = append(bs.remoteSendToBottomQue, reqToBottom)

		addr += 1 << blockSize
		tr.ack++
	}

	bs.inflightInvToBottom = append(bs.inflightInvToBottom, &tr)
	bs.cache.invReqBuffer.Pop()

	// if req.Address == 21475226560 {
	// 	fmt.Printf("[%s]\t[DEBUG] Invalidation - 1: %x\n", bs.cache.name, req.Meta().ID)
	// }
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
	i := bs.findInvTransactionByID(rsp.RespondTo, bs.inflightInvToOutside)
	if i == -1 {
		return true
	}
	trans := bs.inflightInvToOutside[i]

	for j, sh := range trans.pendingEviction {
		if fmt.Sprintf("%s", sh) == fmt.Sprintf("%s", rsp.SrcRDMA) {
			trans.pendingEviction = append(trans.pendingEviction[:j], trans.pendingEviction[j+1:]...)
			break
		}
	}

	// [수정] 대기 목록이 비워지면 안전하게 Inflight에서 트랜잭션 제거
	// fmt.Printf("[%s]\tFinalize Inv Req - 3.1: %s\n", bs.cache.name, rsp.RespondTo)
	if len(trans.pendingEviction) == 0 {
		bs.removeInflightInvToOutside(i)
		// fmt.Printf("[%s]\tFinalize Inv Req - 3.2: %x\n", bs.cache.name, trans.evictingAddr)

		// write에 의한 invalidation 처리:
		// 모든 InvRsp 수신 완료 후 실제 write를 L2로 전송해야 함
		if trans.write != nil {
			trans.action = Nothing
			bs.pendingWriteAfterInv = append(bs.pendingWriteAfterInv, trans)
		}
	}

	return true
}

func (bs *bottomSender) processPendingWriteAfterInv() bool {
	if len(bs.pendingWriteAfterInv) == 0 {
		return false
	}

	trans := bs.pendingWriteAfterInv[0]
	if bs.sendRequestToBottom(trans, trans.fromLocal) {
		bs.pendingWriteAfterInv = bs.pendingWriteAfterInv[1:]
		return true
	}
	return false
}

func (bs *bottomSender) sendBypassRspToTop() bool {
	if len(bs.bypassRspQue) == 0 {
		return false
	}

	if !bs.cache.topPort.CanSend() {
		return false
	}

	msg := bs.bypassRspQue[0]
	err := bs.cache.topPort.Send(msg)

	if err != nil {
		return false
	}

	bs.bypassRspQue[0] = nil
	bs.bypassRspQue = bs.bypassRspQue[1:]

	return true
}

// / sendRemoteRspToTop은 Dst에 "RDMA"가 없는 remote 응답을 RDMAPort를 통해 전송한다.
func (bs *bottomSender) sendRemoteRspToTop() bool {
	if len(bs.sendToRemoteTopQue) == 0 {
		return false
	}
	if !bs.cache.RDMAPort.CanSend() {
		return false
	}
	msg := bs.sendToRemoteTopQue[0]
	msg.Meta().Src = bs.cache.RDMAPort.AsRemote()
	err := bs.cache.RDMAPort.Send(msg)
	if err != nil {
		return false
	}
	bs.sendToRemoteTopQue[0] = nil
	bs.sendToRemoteTopQue = bs.sendToRemoteTopQue[1:]
	return true
}

// [FIX: head-of-line blocking] RDMAPort 혼잡 시 뒤에 있는 topPort 응답까지 막히는 문제 수정.
// RDMAPort(데이터 채널 WriteDoneRsp)만 혼잡 시 skip; RDMAInvPort·topPort는 순서 보장을 위해 원래대로 return false.
// 원래 코드(return false)로 되돌리려면 아래 루프를 제거하고 sendToTopQue[0]만 처리하는 원래 로직으로 교체.
func (bs *bottomSender) sendToTop() bool {
	for i := 0; i < len(bs.sendToTopQue); i++ {
		msg := bs.sendToTopQue[i]
		dst := fmt.Sprintf("%s", msg.Meta().Dst)
		port := bs.cache.topPort
		isRDMADataPort := false
		if strings.Contains(dst, "RDMAInv") {
			port = bs.cache.RDMAInvPort
			msg.Meta().Src = port.AsRemote()
		} else if strings.Contains(dst, "RDMA") {
			port = bs.cache.RDMAPort
			msg.Meta().Src = port.AsRemote()
			isRDMADataPort = true
		}

		if !port.CanSend() {
			if isRDMADataPort {
				continue // safe to skip: RDMAPort(데이터 채널) WriteDoneRsp만 건너뜀
			}
			return false // RDMAInvPort·topPort는 순서 보장 필요 — 원래 동작 유지
		}

		err := port.Send(msg)
		if err != nil {
			continue
		}

		bs.sendToTopQue[i] = nil
		bs.sendToTopQue = append(bs.sendToTopQue[:i], bs.sendToTopQue[i+1:]...)
		return true
	}
	return false
}

func (bs *bottomSender) sendToBottom() bool {
	madeProgress := false

	// 1. Remote Bottom 전송
	if len(bs.remoteSendToBottomQue) > 0 {
		if bs.cache.remoteBottomPort.CanSend() {
			msg := bs.remoteSendToBottomQue[0]
			err := bs.cache.remoteBottomPort.Send(msg)
			if err == nil {
				bs.remoteSendToBottomQue[0] = nil
				bs.remoteSendToBottomQue = bs.remoteSendToBottomQue[1:]
				madeProgress = true
			}
		}
	}

	// 2. Local Bottom 전송
	if len(bs.sendToBottomQue) > 0 {
		if bs.cache.bottomPort.CanSend() {
			msg := bs.sendToBottomQue[0]
			err := bs.cache.bottomPort.Send(msg)
			if err == nil {
				bs.sendToBottomQue[0] = nil
				bs.sendToBottomQue = bs.sendToBottomQue[1:]
				madeProgress = true
			}
		}
	}

	return madeProgress
}

func (bs *bottomSender) writeBufferFull() bool {
	numEntry := len(bs.inflightInvToOutside) + len(bs.localInflightRequest) + len(bs.remoteInflightRequest)
	return numEntry >= bs.writeBufferCapacity
}

func (bs *bottomSender) tooManyInflightInvalidation() bool {
	return len(bs.inflightInvToOutside) >= bs.maxInflightInvalidation
}

func (bs *bottomSender) tooManyInflightInvalidationToBottom() bool {
	return len(bs.inflightInvToBottom) >= bs.maxInflightInvalidation
}

// [수정] 슬롯(Capacity) 분리
func (bs *bottomSender) tooManyInflightRequest(isLocal bool) bool {
	if isLocal {
		// Local 요청은 전체 쿼터의 75%
		limit := bs.maxInflightRequest - (bs.maxInflightRequest / 4)
		return len(bs.localInflightRequest) >= limit
	}
	// Remote 요청은 전체 쿼터의 25%
	limit := bs.maxInflightRequest / 4
	return len(bs.remoteInflightRequest) >= limit
}

func (bs *bottomSender) Reset() {
	bs.cache.localBottomSenderBuffer.Clear()
	bs.cache.remoteBottomSenderBuffer.Clear()
	bs.cache.localBypassBuffer.Clear()

	bs.localInflightRequest = nil
	bs.remoteInflightRequest = nil

	bs.inflightInvToBottom = nil
	bs.inflightInvToOutside = nil
	bs.pendingWriteAfterInv = nil
	bs.sendToTopQue = nil
	bs.sendToRemoteTopQue = nil
	bs.sendToBottomQue = nil
	bs.remoteSendToBottomQue = nil
	bs.bypassRspQue = nil
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

// [수정] 배열에서 제거하는 헬퍼 함수
func (bs *bottomSender) removeInflightRequest(i int, isLocal bool) {
	if isLocal {
		copy(bs.localInflightRequest[i:], bs.localInflightRequest[i+1:])
		bs.localInflightRequest[len(bs.localInflightRequest)-1] = nil
		bs.localInflightRequest = bs.localInflightRequest[:len(bs.localInflightRequest)-1]
	} else {
		copy(bs.remoteInflightRequest[i:], bs.remoteInflightRequest[i+1:])
		bs.remoteInflightRequest[len(bs.remoteInflightRequest)-1] = nil
		bs.remoteInflightRequest = bs.remoteInflightRequest[:len(bs.remoteInflightRequest)-1]
	}
}

// [추가] Bypass Inflight 배열에서 트랜잭션을 안전하게 삭제하는 헬퍼 함수
func (bs *bottomSender) removeInflightBypassRequest(i int) {
	if i < 0 || i >= len(bs.localInflightBypassRequest) {
		panic(fmt.Sprintf("Trying to remove localInflightBypassRequest at out of bounds index %d", i))
	}

	// 뒤의 원소들을 앞으로 당김
	copy(bs.localInflightBypassRequest[i:], bs.localInflightBypassRequest[i+1:])
	// 마지막 원소 포인터 명시적 해제 (메모리 누수 방지)
	bs.localInflightBypassRequest[len(bs.localInflightBypassRequest)-1] = nil
	// 슬라이스 길이 축소
	bs.localInflightBypassRequest = bs.localInflightBypassRequest[:len(bs.localInflightBypassRequest)-1]
}

func (bs *bottomSender) removeInflightInvalidation(i int) {
	if len(bs.inflightInvToBottom) <= i {
		panic(fmt.Sprintf("Trying to remove inflight invalidation at index %d...", i))
	}
	copy(bs.inflightInvToBottom[i:], bs.inflightInvToBottom[i+1:])
	bs.inflightInvToBottom[len(bs.inflightInvToBottom)-1] = nil
	bs.inflightInvToBottom = bs.inflightInvToBottom[:len(bs.inflightInvToBottom)-1]
}

// [추가] 외부 무효화 리스트에서 트랜잭션을 안전하게 삭제하는 헬퍼 함수
func (bs *bottomSender) removeInflightInvToOutside(i int) {
	if i < 0 || i >= len(bs.inflightInvToOutside) {
		panic(fmt.Sprintf("Trying to remove inflightInvToOutside at out of bounds index %d", i))
	}

	// 뒤의 원소들을 앞으로 당김
	copy(bs.inflightInvToOutside[i:], bs.inflightInvToOutside[i+1:])
	// 마지막 원소 포인터 명시적 해제 (메모리 누수 방지)
	bs.inflightInvToOutside[len(bs.inflightInvToOutside)-1] = nil
	// 슬라이스 길이 축소
	bs.inflightInvToOutside = bs.inflightInvToOutside[:len(bs.inflightInvToOutside)-1]
}
