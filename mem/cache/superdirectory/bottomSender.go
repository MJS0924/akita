package superdirectory

import (
	"fmt"
	"strings"

	"github.com/sarchlab/akita/v4/mem/cache/superdirectory/internal"
	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/akita/v4/tracing"
)

type bottomSender struct {
	cache *Comp

	writeBufferCapacity     int
	maxInflightRequest      int
	maxInflightInvalidation int

	localInflightRequest       []*transaction
	localInflightBypassRequest []*transaction
	remoteInflightRequest      []*transaction

	inflightInvToOutside []*transaction
	inflightInvToBottom  []*invTrans

	pendingWriteAfterInv []*transaction // write transactions waiting for L2 after all InvRsps received

	sendToBottomQue       []sim.Msg
	sendToRemoteBottomQue []sim.Msg
	sendToTopQue          []sim.Msg
	sendToRemoteTopQue    []sim.Msg // remote(RDMAPort)로 나가야 하는 응답 전용 (Src에 RDMA 없는 쓰기 eviction 등)
	sendToDirQue          []*transaction
	bypassRspQue          []sim.Msg

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
		fmt.Printf("[DEBUG CohDir %d]\treturn 1.3.5: %v\n", bs.cache.deviceID, temp)
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
	req.Meta().Dst = bs.cache.addressToPortMapper.Find(trans.accessReq().GetAddress())
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
			progress = bs.sendMultipleRequestToBottom(trans, isLocal)
		}
	case EvictAndInsertNewEntry, InvalidateEntry, EvictAndPromotionEntry, EvictAndDemotionEntry: // entry 전체에 대한 invalidation, invalidation ack에서 사용량을 확인하여 demotion 결정
		progress = bs.sendInvalidationRequest(trans, isLocal)
	case InvalidateAndUpdateEntry: // subentry 하나에 대한 invalidation
		progress = bs.sendInvalidationRequestByWrite(trans, isLocal)
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

func (bs *bottomSender) sendRequestToBottom( // 단일 request만 전송
	trans *transaction,
	isLocal bool,
) bool {
	if bs.tooManyInflightRequest(trans.fromLocal) {
		return false
	}

	if bs.cache.debugProcess && trans.accessReq() != nil && trans.accessReq().GetAddress() == bs.cache.debugAddress {
		if trans.fromLocal {
			fmt.Printf("[%s] [bottomSender]\tReceived req - 3.1.1: addr %x\n", bs.cache.name, trans.accessReq().GetAddress())
		} else {
			fmt.Printf("[%s] [bottomSender]\tReceived remote req - 3.1.1: addr %x\n", bs.cache.name, trans.accessReq().GetAddress())
		}
	}

	srcPort := bs.cache.bottomPort
	portMapper := bs.cache.addressToPortMapper
	if !isLocal {
		srcPort = bs.cache.remoteBottomPort
		portMapper = bs.cache.addressToPortMapperForRemoteReq
	}

	req := bs.cache.cloneReq(trans.accessReq())
	req.Meta().Src = srcPort.AsRemote()
	req.Meta().Dst = portMapper.Find(trans.accessReq().GetAddress())
	req.SetReqFrom(trans.accessReq().Meta().ID)

	trans.reqToBottom = append(trans.reqToBottom, &req)
	trans.ack++

	// [수정] 전송 큐 분리 삽입
	if isLocal {
		bs.sendToBottomQue = append(bs.sendToBottomQue, req)
		bs.localInflightRequest = append(bs.localInflightRequest, trans)
	} else {
		bs.sendToRemoteBottomQue = append(bs.sendToRemoteBottomQue, req)
		bs.remoteInflightRequest = append(bs.remoteInflightRequest, trans)
	}

	// 동일한 region에 속한 영역에 대해 read request 전송
	if trans.read == nil {
		return true
	}

	what := "Nothing"
	if trans.action != Nothing {
		what = ""
	}
	what += fmt.Sprintf(" - %d", trans.bankID)
	tracing.AddTaskStep(
		tracing.MsgIDAtReceiver(trans.accessReq(), bs.cache),
		bs.cache,
		what,
	)

	tracing.TraceReqFinalize(trans.accessReq(), bs.cache)

	return true
}

func (bs *bottomSender) sendMultipleRequestToBottom( // TODO: local에서 remote로 보내는 요청인지 아닌지에 따라 구분해야 함
	trans *transaction,
	isLocal bool,
) bool {
	// region 길이에 따라 여러 개의 request를 bottom으로 보내야할 듯
	// 단, response 전송 시, local로는 단일 cache line 응답만, remote로는 region에 해당하는 cache line 전체 전송
	// 또는 L2 cache에서 기존 request에 해당하지 않는 request는 response를 보내지 않게 할 수도??

	if bs.tooManyInflightRequest(trans.fromLocal) {
		return false
	}

	if bs.cache.debugProcess && trans.accessReq() != nil && trans.accessReq().GetAddress() == bs.cache.debugAddress {
		if trans.fromLocal {
			fmt.Printf("[%s] [bottomSender]\tReceived req - 3.1.2: addr %x\n", bs.cache.name, trans.accessReq().GetAddress())
		} else {
			fmt.Printf("[%s] [bottomSender]\tReceived remote req - 3.1.2: addr %x\n", bs.cache.name, trans.accessReq().GetAddress())
		}
	}

	srcPort := bs.cache.bottomPort
	portMapper := bs.cache.addressToPortMapper
	if !isLocal {
		srcPort = bs.cache.remoteBottomPort
		portMapper = bs.cache.addressToPortMapperForRemoteReq
	}

	req := bs.cache.cloneReq(trans.accessReq())
	req.Meta().Src = srcPort.AsRemote()
	req.Meta().Dst = portMapper.Find(trans.accessReq().GetAddress())
	req.SetReqFrom(trans.accessReq().Meta().ID)

	trans.reqToBottom = append(trans.reqToBottom, &req)
	trans.ack++

	// [수정] 전송 큐 분리 삽입
	if isLocal {
		bs.sendToBottomQue = append(bs.sendToBottomQue, req)
		bs.localInflightRequest = append(bs.localInflightRequest, trans)
	} else {
		bs.sendToRemoteBottomQue = append(bs.sendToRemoteBottomQue, req)
		bs.remoteInflightRequest = append(bs.remoteInflightRequest, trans)
	}

	// 동일한 region에 속한 영역에 대해 read request 전송
	if trans.read == nil {
		return true
	}

	bankID := trans.bankID
	regionLen := uint64(bs.cache.regionLen[bankID])
	blockSize := bs.cache.log2BlockSize
	numCacheLine := 1 << (regionLen - blockSize)

	addr := trans.accessReq().GetAddress() >> regionLen << regionLen
	for i := 0; i < numCacheLine; i++ {
		if addr == req.GetAddress()>>blockSize<<blockSize { // 이미 보낸 request는 건너뛰기
			addr += 1 << blockSize
			continue
		}

		// [수정] req 변수 섀도잉 방지! subReq로 명명
		subReq := bs.cache.cloneReq(trans.accessReq())
		subReq.Meta().ID = sim.GetIDGenerator().Generate() // [수정] 새로운 고유 ID 필수 발급!
		subReq.SetAddress(addr)
		subReq.Meta().Src = srcPort.AsRemote()
		subReq.Meta().Dst = portMapper.Find(addr)
		subReq.SetReqFrom(trans.accessReq().Meta().ID) // [수정] 응답 매핑을 위해 원본 ID 기록

		// if trans.fromLocal { // L1 cache로 응답할 예정인 경우, L2 cache에서 response를 전송하지 않을 예정
		// 	subReq.SetNoNeedToReply(true)
		// } else { // RDMA로 응답할 예정인 경우, L2 cache에서 response를 전송해야 함
		subReq.SetNoNeedToReply(false)
		tempReq := subReq
		trans.reqToBottom = append(trans.reqToBottom, &tempReq)
		trans.ack++
		// }

		// [수정] 전송 큐 분리 삽입
		if isLocal {
			bs.sendToBottomQue = append(bs.sendToBottomQue, subReq)
		} else {
			bs.sendToRemoteBottomQue = append(bs.sendToRemoteBottomQue, subReq)
		}
		addr += 1 << blockSize
	}

	what := ""
	if trans.action != Nothing {
		what = "UpdateEntry"
		tracing.AddTaskStep(
			tracing.MsgIDAtReceiver(trans.accessReq(), bs.cache),
			bs.cache,
			what,
		)
	}
	what += fmt.Sprintf(" - %d", trans.bankID)
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
	isLocal bool,
) bool {
	// 1. [사전 검사] Bottom으로 요청을 내려보내야 하는 액션인데 여유 공간이 없다면 조기 리턴 (트랜잭션 증발 방지)
	if trans.action == EvictAndInsertNewEntry && bs.tooManyInflightRequest(isLocal) {
		return false
	}

	if bs.tooManyInflightInvalidation() {
		return false
	}

	// 2. [대상 선별] victim.SubEntry를 순회하며 실제로 무효화 메시지를 보낼 외부 노드가 있는지 검사
	hasValidTargets := false
	victim := &trans.victim
	for i := 0; i < len(victim.SubEntry); i++ {
		for _, sh := range victim.SubEntry[i].Sharer {
			if sh != trans.accessReq().GetSrcRDMA() && sh != "" {
				hasValidTargets = true
				break
			}
		}
		if hasValidTargets {
			break
		}
	}

	progress := false

	if bs.cache.debugProcess && trans.accessReq() != nil && trans.accessReq().GetAddress() == bs.cache.debugAddress {
		if trans.fromLocal {
			fmt.Printf("[%s] [bottomSender]\tReceived req - 3.1.3: addr %x\n", bs.cache.name, trans.accessReq().GetAddress())
		} else {
			fmt.Printf("[%s] [bottomSender]\tReceived remote req - 3.1.3: addr %x\n", bs.cache.name, trans.accessReq().GetAddress())
		}
	}

	// 3. 무효화 대상이 있을 때만 Inflight 큐에 등록하고 메시지 생성
	if hasValidTargets {
		// (참고: superdirectory의 findTransactionByID는 반환값이 1개(int)이므로 i만 받습니다)
		i := bs.findInvTransactionByID(trans.accessReq().Meta().ID, bs.inflightInvToOutside)
		if i == -1 {
			bs.inflightInvToOutside = append(bs.inflightInvToOutside, trans)
			progress = true
		}

		addr := victim.Tag
		regionLen := bs.cache.regionLen[trans.bankID]
		for i := 0; i < len(victim.SubEntry); i++ {
			e := &victim.SubEntry[i]
			addr = victim.Tag + uint64(i<<regionLen)

			for j := 0; j < len(e.Sharer); j++ {
				sh := e.Sharer[j]

				if sh == trans.accessReq().GetSrcRDMA() || sh == "" {
					continue
				}

				// [핵심 변경] topPort.Send()로 직결하지 않고, sendToTopQue에 삽입하여 중간 실패(네트워크 블로킹) 방지
				req := mem.InvReqBuilder{}.
					WithSrc(bs.cache.topPort.AsRemote()).
					WithDst(bs.cache.ToRDMAInv).
					WithAddress(addr).
					WithPID(trans.victim.PID).
					WithReqFrom(trans.accessReq().Meta().ID).
					WithDstRDMA(sh).
					WithRegionID(trans.bankID).
					Build()

				bs.sendToTopQue = append(bs.sendToTopQue, req)

				// Sharer 리스트에서 제거 및 pending 처리
				e.Sharer = append(e.Sharer[:j], e.Sharer[j+1:]...)
				j-- // 요소 삭제로 인한 인덱스 밀림 보정

				trans.pendingEviction = append(trans.pendingEviction, sh)
				progress = true

				what := ""
				if trans.action == EvictAndInsertNewEntry {
					what = "InvalidateByEviction"
				} else if trans.action == InvalidateAndUpdateEntry {
					what = "InvalidateByWrite"
				} else if trans.action == EvictAndPromotionEntry {
					what = "InvalidateByPromotion"
				} else if trans.action == EvictAndDemotionEntry {
					what = "InvalidateByDemotion"
				}
				if what != "" {
					tracing.AddTaskStep(tracing.MsgIDAtReceiver(trans.accessReq(), bs.cache), bs.cache, what)
				}
				what += fmt.Sprintf(" - %d", trans.bankID)
				if what != "" {
					tracing.AddTaskStep(tracing.MsgIDAtReceiver(trans.accessReq(), bs.cache), bs.cache, what)
				}
				if bs.cache.debugProcess {
					regionLen := bs.cache.regionLen[trans.bankID]
					if addr>>regionLen == bs.cache.debugAddress>>regionLen {
						fmt.Printf("[%s]\tSend Invalidation Request - 0.0: addr %x, dst %s\n", bs.cache.name, addr, sh)
					}
				}
			}

			fmt.Printf("[%s]\tEvict entry from bank %d: Addr %x\n", bs.cache.name, trans.bankID, addr)
		}
	} else {
		// [Deadlock 방지] 보낼 대상이 없으면 성공(true)한 것으로 간주하여 Pop 되도록 유도
		progress = true
	}

	tracing.TraceReqFinalize(trans.accessReq(), bs.cache)

	// 4. Bottom으로의 추가 요청 하달
	// EvictAndInsertNewEntry만 실제 데이터 요청이 발생하므로 Bottom으로 보냄
	if trans.action == EvictAndInsertNewEntry {
		return bs.sendRequestToBottom(trans, isLocal) || progress
	}

	return progress
}

func (bs *bottomSender) sendInvalidationRequestByWrite(
	trans *transaction,
	isLocal bool,
) bool {
	// 1. Inflight Invalidation 제한 검사
	if bs.tooManyInflightInvalidation() {
		return false
	}

	// 2. [수정] 보낼 대상(Target) 사전 선별
	var validTargets []sim.RemotePort
	for i := 0; i < len(trans.invalidationList); i++ {
		sh := trans.invalidationList[i]
		// 나 자신이거나 빈 포트면 제외
		if sh == trans.accessReq().GetSrcRDMA() || sh == "" {
			continue
		}
		validTargets = append(validTargets, sh)
	}

	progress := false

	i := bs.findInvTransactionByID(trans.accessReq().Meta().ID, bs.inflightInvToOutside)
	if i == -1 {
		// [핵심 변경 2] 자원이 꽉 차서 Demoted Entry 생성에 실패하면 즉시 조기 리턴.
		// false를 반환하므로 processInputReq에서 Pop() 되지 않고, 다음 Tick에 재시도합니다.
		// if !bs.insertDemotedEntry(trans) {
		// 	return false
		// }

		// (이전 답변의 좀비 트랜잭션 방지 로직: 타겟이 있을 때만 Inflight 큐에 넣음)
		if len(validTargets) > 0 {
			bs.inflightInvToOutside = append(bs.inflightInvToOutside, trans)
		}
		progress = true
	}

	if bs.cache.debugProcess && trans.accessReq() != nil && trans.accessReq().GetAddress() == bs.cache.debugAddress {
		if trans.fromLocal {
			fmt.Printf("[%s] [bottomSender]\tReceived req - 3.1.4: addr %x\n", bs.cache.name, trans.accessReq().GetAddress())
		} else {
			fmt.Printf("[%s] [bottomSender]\tReceived remote req - 3.1.4: addr %x\n", bs.cache.name, trans.accessReq().GetAddress())
		}
	}

	// 4. [수정] 선별된 타겟들에 대해 무효화 메시지 생성 및 안전한 큐잉
	if len(validTargets) > 0 {
		for _, sh := range validTargets {
			bankID := min(trans.bankID, bs.cache.numBanks-1)
			req := mem.InvReqBuilder{}.
				WithSrc(bs.cache.topPort.AsRemote()).
				WithDst(bs.cache.ToRDMAInv).
				WithAddress(trans.write.Address).
				WithPID(trans.write.PID).
				WithReqFrom(trans.accessReq().Meta().ID).
				WithDstRDMA(sh).
				WithRegionID(bankID).
				Build()

			bs.sendToTopQue = append(bs.sendToTopQue, req)

			trans.pendingEviction = append(trans.pendingEviction, sh)
			progress = true

			tracing.AddTaskStep(tracing.MsgIDAtReceiver(trans.accessReq(), bs.cache), bs.cache, "InvalidateByWrite")
			what := fmt.Sprintf("InvalidateByWrite - %d", bankID)
			tracing.AddTaskStep(tracing.MsgIDAtReceiver(trans.accessReq(), bs.cache), bs.cache, what)

			if bs.cache.debugProcess {
				regionLen := bs.cache.regionLen[bankID]
				if trans.write.Address>>regionLen == bs.cache.debugAddress>>regionLen {
					fmt.Printf("[%s]\tSend Invalidation Request - 0.1: addr %x, dst %s\n", bs.cache.name, trans.write.Address, sh)
				}
			}
		}
	} else if trans.write != nil {
		// [FIX] 무효화할 sharer가 없는 경우: invalidation 단계를 건너뛰고 즉시 L2 쓰기 진행.
		// validTargets == 0이면 transaction이 버퍼에서 pop된 뒤 소멸되어
		// WriteDoneRsp가 영원히 돌아오지 않는 데드락이 발생한다.
		trans.action = Nothing
		bs.pendingWriteAfterInv = append(bs.pendingWriteAfterInv, trans)
	}

	// 5. 메시지 생성이 모두 끝났으므로 invalidationList 비움
	trans.invalidationList = nil

	tracing.TraceReqFinalize(trans.accessReq(), bs.cache)

	return progress
}

func (bs *bottomSender) insertDemotedEntry(
	trans *transaction,
) bool {
	if trans.bankID == bs.cache.numBanks-1 { // 이미 reigon 길이가 최소가 된 경우 demotion을 할 수 없음
		return true
	}

	bankID := trans.bankID
	prevRegionLen := bs.cache.regionLen[bankID]
	currRegionLen := bs.cache.regionLen[bankID+1]

	pid := trans.evictingPID
	addr := trans.evictingAddr
	sh := make([]sim.RemotePort, len(trans.invalidationList))
	copy(sh, trans.invalidationList)
	owner := trans.accessReq().GetSrcRDMA()

	list := bs.cache.mshr.QueryWithMask(pid, addr, uint64(currRegionLen))
	var mshrEntry *internal.MSHREntry
	if len(list) > 0 {
		mshrEntry = list[0]
	}

	if mshrEntry == nil {
		if bs.cache.mshr.IsFull() {
			// MSHR이 꽉 차서 자원 할당이 불가능하므로 false 반환 (Tick 지연)
			return false
		}
	}

	currAddr := addr >> prevRegionLen << prevRegionLen
	endAddr := currAddr + (1 << prevRegionLen)
	newBlk := &internal.CohEntry{
		Tag: currAddr,
	}

	i := 0
	for currAddr < endAddr {
		nxtAddr := currAddr + (1 << currRegionLen)
		e := &newBlk.SubEntry[i]

		if currAddr <= addr && addr < nxtAddr {
			e.Sharer = append(e.Sharer, owner)
			e.IsValid = true
			e.IsLocked = false
			e.ReadCount = 0
		} else {
			e.Sharer = sh
			e.IsValid = true
			e.IsLocked = false
			e.ReadCount = 0
		}

		currAddr += 1 << currRegionLen
		i++
	}

	newTrans := *trans.DeepCopy()
	newTrans.block = newBlk
	newTrans.action = InsertDemotionEntry
	newTrans.fromLocal = true

	// [추가] MSHR Entry 등록 로직
	if mshrEntry == nil {
		mshrEntry = bs.cache.mshr.Add(pid, addr, uint64(currRegionLen), bankID)
	}
	newTrans.mshrEntry = mshrEntry
	mshrEntry.Requests = append(mshrEntry.Requests, &newTrans)

	bs.sendToDirQue = append(bs.sendToDirQue, &newTrans)

	if bs.cache.debugProcess && newTrans.evictingAddr == bs.cache.debugAddress {
		fmt.Printf("[%s] [bottomSender]\tInsert Demotion request - 0: addr %x\n", bs.cache.name, newTrans.evictingAddr)
	}

	return true
}

func (bs *bottomSender) processInvalidationReq() bool {
	item := bs.cache.invReqBuffer.Peek()
	if item == nil {
		bs.returnFalse1 = "There is no invalidation request from invReqBuffer"
		return false
	}

	if bs.tooManyInflightInvalidationToBottom() {
		return false
	}

	req := item.(*mem.InvReq)

	regionLen := uint64(bs.cache.directory.GetRegionLen(req.RegionID))
	blockSize := bs.cache.log2BlockSize
	numCacheLine := 1 << (regionLen - blockSize)

	tr := invTrans{}
	tr.req = req

	addr := req.GetAddress() >> regionLen << regionLen
	for i := 0; i < numCacheLine; i++ {
		reqToBottom := mem.InvReqBuilder{}.
			WithSrc(bs.cache.remoteBottomPort.AsRemote()).
			WithDst(bs.cache.addressToPortMapperForRemoteReq.Find(addr)).
			WithPID(req.PID).
			WithAddress(addr).
			WithReqFrom(req.Meta().ID).
			Build()
		bs.sendToRemoteBottomQue = append(bs.sendToRemoteBottomQue, reqToBottom)

		tr.ack++

		if bs.cache.debugProcess && addr == bs.cache.debugAddress {
			fmt.Printf("[%s]\tSend Invalidation Req to Bottom - 1: addr %x, dst %s\n", bs.cache.name, addr, reqToBottom.Dst)
		}

		addr += 1 << blockSize
	}

	bs.inflightInvToBottom = append(bs.inflightInvToBottom, &tr)
	bs.cache.invReqBuffer.Pop()

	return true
}

// [수정] 양쪽 Bottom 포트를 모두 폴링하도록 개편
func (bs *bottomSender) processReturnRsp() bool {
	madeProgress := false

	// 1. Remote 응답 포트 최우선 처리 (네트워크 정체 해소)
	msg := bs.cache.remoteBottomPort.PeekIncoming()
	if msg != nil {
		madeProgress = bs.processRspMsg(msg, bs.cache.remoteBottomPort) || madeProgress
	}

	// 2. Local 응답 포트 처리
	msg = bs.cache.bottomPort.PeekIncoming()
	if msg != nil {
		madeProgress = bs.processRspMsg(msg, bs.cache.bottomPort) || madeProgress
	}

	if !madeProgress {
		bs.returnFalse0 = "There is no msg from bottomPort"
	}

	return madeProgress
}

// [추가] 공통 라우팅 로직
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

	if msg.Origin == nil {
		fmt.Printf("[%s]\t[WARNING] Origin field is not valid\n", bs.cache.name)
	} else if msg.Origin.GetAddress() == 0 {
		fmt.Printf("[%s]\t[WARNING] Origin.Address field is not valid\n", bs.cache.name)
	}

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
		// superdirectory 환경에서 트랜잭션 유실을 추적하기 위해 기존 로그 유지
		// if bs.cache.debugProcess && msg.Origin.GetAddress() == bs.cache.debugAddress {
		fmt.Printf("[%s] [bottomSender]\tDiscard read rsp - 3.2: addr %x\n", bs.cache.name, msg.Origin.GetAddress())
		// }
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

	// [핵심 변경 1] 여러 개의 하위 요청 중 완료된 것만 리스트에서 제거
	trans.reqToBottom[j] = nil
	trans.reqToBottom = append(trans.reqToBottom[:j], trans.reqToBottom[j+1:]...)

	// 하위 요청을 모두 응답받았을 때만 Inflight 큐에서 최종 삭제
	if len(trans.reqToBottom) == 0 {
		if isBypass {
			bs.removeInflightBypassRequest(i)
		} else {
			bs.removeInflightRequest(i, isLocal)
		}
	}

	// 응답 메시지 헤더 조작
	msg.RespondTo = trans.accessReq().Meta().ID
	msg.Src = bs.cache.topPort.AsRemote()
	msg.Dst = trans.accessReq().Meta().Src
	msg.WaitFor = trans.ack // [추가] 병합 처리를 위한 Ack 개수 전달

	// [핵심 변경 2] 직접 Send() 하지 않고, 용도에 맞는 전송 큐에 삽입 (블로킹 방지)
	if isBypass || trans.action == BypassingDirectory {
		bs.bypassRspQue = append(bs.bypassRspQue, msg)
	} else if !trans.fromLocal && !strings.Contains(fmt.Sprintf("%s", msg.Meta().Dst), "RDMA") {
		bs.sendToRemoteTopQue = append(bs.sendToRemoteTopQue, msg)
	} else {
		bs.sendToTopQue = append(bs.sendToTopQue, msg)
	}

	port.RetrieveIncoming()

	if bs.cache.debugProcess && msg.Origin.GetAddress() == bs.cache.debugAddress {
		fmt.Printf("[%s] [bottomSender]\tSend read rsp - 3.3: addr %x, dst %s\n", bs.cache.name, trans.read.Address, msg.Dst)
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
		// superdirectory의 디버깅 로그 유지
		if bs.cache.debugProcess && msg.Origin.GetAddress() == bs.cache.debugAddress {
			fmt.Printf("[%s] [bottomSender]\tDiscard write rsp - 3.4: addr %x\n", bs.cache.name, msg.Origin.GetAddress())
		}
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

	// [핵심 1] 여러 개의 하위 요청 중 완료된 것만 리스트에서 제거
	trans.reqToBottom[j] = nil
	trans.reqToBottom = append(trans.reqToBottom[:j], trans.reqToBottom[j+1:]...)

	// 하위 요청을 모두 응답받았을 때만 Inflight 큐에서 최종 삭제
	if len(trans.reqToBottom) == 0 {
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

	// [핵심 2] 포트(topPort)에 직접 Send하지 않고 용도에 맞는 전송 큐에 삽입 (블로킹 방지)
	if isBypass || trans.action == BypassingDirectory {
		bs.bypassRspQue = append(bs.bypassRspQue, msg)
	} else if !trans.fromLocal && !strings.Contains(fmt.Sprintf("%s", msg.Meta().Dst), "RDMA") {
		bs.sendToRemoteTopQue = append(bs.sendToRemoteTopQue, msg)
	} else {
		bs.sendToTopQue = append(bs.sendToTopQue, msg)
	}

	port.RetrieveIncoming()

	if bs.cache.debugProcess && trans.write != nil && trans.write.Address == bs.cache.debugAddress {
		fmt.Printf("[%s] [bottomSender]\tSend write rsp - 3.5: addr %x, dst %s\n", bs.cache.name, trans.write.Address, msg.Dst)
	}
	return true
}

func (bs *bottomSender) processInvRspFromBottom(rsp *mem.InvRsp, port sim.Port) bool {
	i := bs.findInvalidationByID(rsp.RespondTo, bs.inflightInvToBottom)
	if i == -1 {
		fmt.Printf("[%s]\tCannot find transaction for InvRsp with RspTo %s\n", bs.cache.Name(), rsp.RespondTo)
		port.RetrieveIncoming()
		return true
	}

	inflightInv := bs.inflightInvToBottom[i]
	inflightInv.ack--
	// superdirectory 고유 통계 데이터 누적 유지
	inflightInv.numInv = inflightInv.numInv + rsp.NumInv
	inflightInv.accessed = inflightInv.accessed + rsp.Accessed

	if inflightInv.ack > 0 {
		// [중요 버그 수정] 처리가 끝난 메시지는 반드시 버퍼에서 꺼내주어야(RetrieveIncoming) 데드락에 빠지지 않습니다.
		port.RetrieveIncoming()
		return true
	}

	req := inflightInv.req
	rspToOutside := mem.InvRspBuilder{}.
		WithSrc(bs.cache.topPort.AsRemote()).
		WithDst(req.Meta().Src).
		WithRspTo(req.ReqFrom).
		WithNumInv(inflightInv.numInv).
		WithAccessed(inflightInv.accessed).
		WithSrcRDMA(req.DstRDMA).
		Build()

	// [핵심 변경] 직접 Send() 하지 않고 상단 큐에 적재하여 포트 블로킹 우회
	bs.sendToTopQue = append(bs.sendToTopQue, rspToOutside)

	port.RetrieveIncoming()

	// [핵심 추가] 처리가 완료된 트랜잭션을 Inflight 배열에서 안전하게 삭제
	bs.removeInflightInvalidation(i)

	return true
}

func (bs *bottomSender) processInvalidationRsp() bool {
	rsp := bs.cache.invRspBuffer.Pop()
	if rsp == nil {
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

		// [수정] directory에서 적용한 안전한 문자열 변환 기반 포트 비교 적용
		if fmt.Sprintf("%s", sh) == fmt.Sprintf("%s", rsp.SrcRDMA) {
			trans.pendingEviction = append(trans.pendingEviction[:j], trans.pendingEviction[j+1:]...)

			// superdirectory 고유 로직 유지 (통계 누적)
			trans.numInv += rsp.NumInv
			trans.accessed += rsp.Accessed

			// fmt.Printf("[%s]\tF.1.1. Remove pending Eviction: %s\n", bs.cache.Name(), rsp.SrcRDMA)
			break
		}
	}

	// [수정] 대기 목록이 비워지면 안전한 헬퍼 함수를 사용하여 Inflight에서 트랜잭션 제거
	if len(trans.pendingEviction) == 0 {
		bs.removeInflightInvToOutside(i)
		// fmt.Printf("[%s]\tF.2. Remove inflight invalidation to outside\n", bs.cache.Name())

		// superdirectory 고유 로직 유지: eviction에 의한 invalidation은 사용량을 보고 demotion 여부 결정
		if trans.action == EvictAndInsertNewEntry && trans.accessed*8 < trans.numInv*7 { // threshold: 7/8
			if trans.bankID+1 < bs.cache.numBanks-1 {
				bs.cache.regionSizeBuffer.Update(trans.evictingAddr, trans.bankID+1)
			}
		} else {
			bs.cache.regionSizeBuffer.Update(trans.evictingAddr, trans.bankID)
		}

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

/// sendRemoteRspToTop은 Dst에 "RDMA"가 없는 remote 응답을 RDMAPort를 통해 전송한다.
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

// [수정] 분할된 2개의 Bottom 포트 및 큐 처리
func (bs *bottomSender) sendToBottom() bool {
	madeProgress := false

	// 1. Remote Bottom 전송 (우선)
	if len(bs.sendToRemoteBottomQue) > 0 {
		if bs.cache.remoteBottomPort.CanSend() {
			msg := bs.sendToRemoteBottomQue[0]
			err := bs.cache.remoteBottomPort.Send(msg)
			if err == nil {
				bs.sendToRemoteBottomQue[0] = nil
				bs.sendToRemoteBottomQue = bs.sendToRemoteBottomQue[1:]
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

func (bs *bottomSender) sendToDir() bool {
	if len(bs.sendToDirQue) == 0 {
		return false
	}

	if !bs.cache.dirStageMotionBuffer.CanPush() {
		return false
	}

	msg := bs.sendToDirQue[0]
	bs.cache.dirStageMotionBuffer.Push(msg)

	bs.sendToDirQue = bs.sendToDirQue[1:]
	return true
}

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

func (bs *bottomSender) tooManyInflightInvalidation() bool {
	return len(bs.inflightInvToOutside) >= bs.maxInflightInvalidation
}

func (bs *bottomSender) tooManyInflightInvalidationToBottom() bool {
	return len(bs.inflightInvToBottom) >= bs.maxInflightInvalidation
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
	bs.sendToRemoteBottomQue = nil
	bs.sendToDirQue = nil
	bs.bypassRspQue = nil
}

// func (bs *bottomSender) findTransactionByReqIDToBottom(ID string, list []*transaction) int {
// 	for i, tr := range list {
// 		if tr.reqIDToBottom == ID {
// 			return i
// 		}
// 	}
// 	return -1
// }

func (bs *bottomSender) findTransactionByID(ID string, list []*transaction) (int, int) {
	for i, tr := range list {
		for j, req := range tr.reqToBottom {
			if req == nil {
				continue
			}

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

func (bs *bottomSender) removeInflightInvalidation(i int) {
	if len(bs.inflightInvToBottom) <= i {
		panic(fmt.Sprintf("Trying to remove inflight invalidation at index %d...", i))
	}
	copy(bs.inflightInvToBottom[i:], bs.inflightInvToBottom[i+1:])
	bs.inflightInvToBottom[len(bs.inflightInvToBottom)-1] = nil
	bs.inflightInvToBottom = bs.inflightInvToBottom[:len(bs.inflightInvToBottom)-1]
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
