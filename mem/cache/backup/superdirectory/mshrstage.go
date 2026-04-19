package superdirectory

import (
	"fmt"

	"github.com/sarchlab/akita/v4/mem/cache/superdirectory/internal"
	"github.com/sarchlab/akita/v4/sim"
)

type mshrStage struct {
	cache *Comp

	processingTrans   *transaction
	isProcessingLocal bool // [추가] 현재 처리 중인 트랜잭션의 출처 추적

	promotionQueue []*transaction
	demotionQueue  []*transaction

	pendingGatherQueue []*transaction
}

func (s *mshrStage) Tick() bool {
	progress := false

	progress = s.wakeupGatheredPromotions() || progress
	progress = s.insertPromotionEntry() || progress
	progress = s.insertDemotionEntry() || progress

	// 0. 이미 처리 중인 트랜잭션이 있다면 이어서 진행
	if s.processingTrans != nil {
		return s.processOneReq() || progress
	}

	// 1. [수정] Remote 버퍼 우선 확인 (네트워크 트래픽 우선 해소)
	item := s.cache.remoteMshrStageBuffer.Peek()
	if item != nil {
		s.processingTrans = item.(*transaction)
		s.isProcessingLocal = false
		return s.processOneReq() || progress
	}

	// 2. [수정] Local 버퍼 확인
	item = s.cache.localMshrStageBuffer.Peek()
	if item != nil {
		s.processingTrans = item.(*transaction)
		s.isProcessingLocal = true
		return s.processOneReq() || progress
	}

	return progress
}

func (s *mshrStage) Reset() {
	s.processingTrans = nil
	s.promotionQueue = nil
	s.demotionQueue = nil
	s.pendingGatherQueue = nil

	// [수정] 분리된 버퍼 초기화
	s.cache.localMshrStageBuffer.Clear()
	s.cache.remoteMshrStageBuffer.Clear()
}

func (s *mshrStage) processOneReq() bool {
	progress := false
	trans := s.processingTrans

	if trans.action == InsertPromotionEntry {
		return s.processOneReqAfterPromotion()
	} else if trans.action == InsertDemotionEntry {
		return s.processOneReqAfterDemotion()
	}

	blk := trans.block

	// [버그 수정 및 최적화] 처리된 요청을 슬라이스에서 즉시 제거하여 중복 Push 방지
	for len(trans.mshrEntry.Requests) > 0 {
		req := trans.mshrEntry.Requests[0]
		t := req.(*transaction)

		// [수정] 트랜잭션 출처에 따라 전송할 타겟 버퍼 분기
		var targetBuf sim.Buffer
		if t.fromLocal {
			targetBuf = s.cache.localBottomSenderBuffer
		} else {
			targetBuf = s.cache.remoteBottomSenderBuffer
		}

		if !targetBuf.CanPush() {
			return progress
		}

		if s.sharerExist(t.accessReq().GetSrcRDMA(), blk.SubEntry[trans.blockIdx].Sharer) {
			t.action = Nothing
		} else {
			t.action = UpdateEntry
			blk.SubEntry[trans.blockIdx].Sharer = append(
				blk.SubEntry[trans.blockIdx].Sharer,
				t.accessReq().GetSrcRDMA(),
			)
		}

		targetBuf.Push(t)

		// 처리 완료된 원소를 배열에서 안전하게 제거
		trans.mshrEntry.Requests[0] = nil
		trans.mshrEntry.Requests = trans.mshrEntry.Requests[1:]

		progress = true
	}

	s.cache.mshr.Remove(trans.mshrEntry.PID, trans.mshrEntry.Address)

	// [수정] 처리가 완료되었으므로 알맞은 버퍼에서 Pop
	if s.isProcessingLocal {
		s.cache.localMshrStageBuffer.Pop()
	} else {
		s.cache.remoteMshrStageBuffer.Pop()
	}
	s.processingTrans = nil

	if trans.action == InvalidateAndUpdateEntry && trans.bankID != s.cache.numBanks-1 {
		list := s.cache.mshr.QueryWithMask(trans.mshrEntry.PID, trans.mshrEntry.Address, uint64(s.cache.regionLen[trans.bankID+1]))
		if len(list) == 0 {
			s.demotionQueue = append(s.demotionQueue, trans)

			trans.block.IsValid = false
			s.cache.directory.EvictBloomfilter(trans.bankID, trans.block.Tag)
			s.cache.directory.InsertBloomfilter(trans.bankID+1, trans.block.Tag)
			s.cache.mshr.Add(trans.mshrEntry.PID, trans.mshrEntry.Address, uint64(s.cache.regionLen[trans.bankID+1]), trans.bankID+1)
		} else {
			fmt.Printf("[%s]\t[WARNING] Waiting for mshr entry before demotion\n", s.cache.name)
		}

	} else if trans.block.AbleToPromotion() && trans.bankID != 0 {
		list := s.cache.mshr.QueryWithMask(trans.mshrEntry.PID, trans.mshrEntry.Address, uint64(s.cache.regionLen[trans.bankID-1]))
		if len(list) == 0 {
			s.promotionQueue = append(s.promotionQueue, trans)

			trans.block.IsValid = false
			s.cache.directory.EvictBloomfilter(trans.bankID, trans.block.Tag)
			s.cache.directory.InsertBloomfilter(trans.bankID-1, trans.block.Tag)
			s.cache.mshr.Add(trans.mshrEntry.PID, trans.mshrEntry.Address, uint64(s.cache.regionLen[trans.bankID-1]), trans.bankID-1)
		}
	}

	return progress
}

func (s *mshrStage) processOneReqAfterPromotion() bool {
	progress := false
	trans := s.processingTrans

	// [수정됨] 대기 중인 요청들을 하나씩 꺼내서 targetBuffer로 안전하게 전송
	for len(trans.mshrEntry.Requests) > 0 {
		req := trans.mshrEntry.Requests[0]
		t := req.(*transaction)
		blk := trans.block

		if t.action == InsertPromotionEntry || t.action == EvictAndPromotionEntry ||
			t.action == InsertDemotionEntry || t.action == EvictAndDemotionEntry {
			progress = true
			trans.mshrEntry.Requests[0] = nil
			trans.mshrEntry.Requests = trans.mshrEntry.Requests[1:]
			continue
		}

		// 1. 출처에 따른 타겟 버퍼 라우팅
		var targetBuf sim.Buffer
		if t.fromLocal {
			targetBuf = s.cache.localBottomSenderBuffer
		} else {
			targetBuf = s.cache.remoteBottomSenderBuffer
		}

		// 2. 버퍼 여유 공간 확인 (Backpressure 처리)
		if !targetBuf.CanPush() {
			return progress // 버퍼가 꽉 찼으면 현재까지의 진행 상황 반환 후 다음 틱 대기
		}

		// 3. Sharer 상태 업데이트
		subRegionLen := trans.mshrEntry.RegionLen - uint64(s.cache.log2NumSubEntry)
		offset := t.accessReq().GetAddress() % (1 << trans.mshrEntry.RegionLen)
		index := offset >> subRegionLen
		blk.SubEntry[index].Sharer = s.appendSharer(
			t.accessReq().GetSrcRDMA(),
			blk.SubEntry[index].Sharer,
		)

		// 4. 다음 파이프라인으로 Push
		t.action = Nothing
		targetBuf.Push(t)

		// 5. 메모리 누수 방지 및 큐에서 요소 제거
		trans.mshrEntry.Requests[0] = nil
		trans.mshrEntry.Requests = trans.mshrEntry.Requests[1:]

		progress = true
	}

	// 모든 대기 요청을 처리한 뒤에만 MSHR에서 안전하게 제거
	s.cache.mshr.Remove(trans.mshrEntry.PID, trans.mshrEntry.Address)

	// 출처에 맞는 MSHR Stage 버퍼 Pop
	if s.isProcessingLocal {
		s.cache.localMshrStageBuffer.Pop()
	} else {
		s.cache.remoteMshrStageBuffer.Pop()
	}
	s.processingTrans = nil

	if trans.block.AbleToPromotion() && trans.bankID != 0 {
		list := s.cache.mshr.QueryWithMask(trans.mshrEntry.PID, trans.mshrEntry.Address, uint64(s.cache.regionLen[trans.bankID-1]))
		if len(list) == 0 {
			s.promotionQueue = append(s.promotionQueue, trans)

			trans.block.IsValid = false
			s.cache.directory.EvictBloomfilter(trans.bankID, trans.block.Tag)
			s.cache.directory.InsertBloomfilter(trans.bankID-1, trans.block.Tag)
			s.cache.mshr.Add(trans.mshrEntry.PID, trans.mshrEntry.Address, uint64(s.cache.regionLen[trans.bankID-1]), trans.bankID-1)
		}
	}

	return progress
}

func (s *mshrStage) processOneReqAfterDemotion() bool {
	progress := false
	trans := s.processingTrans

	// [수정됨] 대기 중인 요청들을 하나씩 꺼내서 targetBuffer로 안전하게 전송
	for len(trans.mshrEntry.Requests) > 0 {
		req := trans.mshrEntry.Requests[0]
		t := req.(*transaction)
		blk := trans.block

		if t.action == InsertDemotionEntry || t.action == InsertDemotionEntry {
			progress = true
			trans.mshrEntry.Requests[0] = nil
			trans.mshrEntry.Requests = trans.mshrEntry.Requests[1:]
			continue
		}

		// 1. 출처에 따른 타겟 버퍼 라우팅
		var targetBuf sim.Buffer
		if t.fromLocal {
			targetBuf = s.cache.localBottomSenderBuffer
		} else {
			targetBuf = s.cache.remoteBottomSenderBuffer
		}

		// 2. 버퍼 여유 공간 확인 (Backpressure 처리)
		if !targetBuf.CanPush() {
			return progress // 버퍼가 꽉 찼으면 다음 틱 대기
		}

		// 3. Sharer 상태 업데이트
		blk.SubEntry[t.blockIdx].Sharer = s.appendSharer(
			t.accessReq().GetSrcRDMA(),
			blk.SubEntry[t.blockIdx].Sharer,
		)

		// 4. 다음 파이프라인으로 Push
		t.action = Nothing
		targetBuf.Push(t)

		// 5. 메모리 누수 방지 및 큐에서 요소 제거
		trans.mshrEntry.Requests[0] = nil
		trans.mshrEntry.Requests = trans.mshrEntry.Requests[1:]

		progress = true
	}

	// 모든 대기 요청을 처리한 뒤에만 MSHR에서 안전하게 제거
	s.cache.mshr.Remove(trans.mshrEntry.PID, trans.mshrEntry.Address)

	// 출처에 맞는 MSHR Stage 버퍼 Pop
	if s.isProcessingLocal {
		s.cache.localMshrStageBuffer.Pop()
	} else {
		s.cache.remoteMshrStageBuffer.Pop()
	}
	s.processingTrans = nil

	return progress
}

func (s *mshrStage) appendSharer(sh sim.RemotePort, list []sim.RemotePort) []sim.RemotePort {
	// for _, p := range list {
	// 	if p == sh {
	// 		return list
	// 	}
	// }

	if s.sharerExist(sh, list) {
		return list
	}

	return append(list, sh)
}

func (s *mshrStage) sharerExist(sh sim.RemotePort, list []sim.RemotePort) bool {
	for _, p := range list {
		if fmt.Sprintf("%s", p) == fmt.Sprintf("%s", sh) {
			return true
		}
	}

	return false
}

func (s *mshrStage) insertPromotionEntry() bool {
	if s.cache.mshr.IsFull() {
		return false
	}

	if !s.cache.dirStageMotionBuffer.CanPush() {
		return false
	}

	if len(s.promotionQueue) == 0 {
		return false
	}

	trans := s.promotionQueue[0]

	if trans.read != nil && trans.read.FetchForWriteMiss {
		s.promotionQueue[0] = nil
		s.promotionQueue = s.promotionQueue[1:]
		return true
	}

	if trans == nil || trans.bankID == 0 { // 이미 reigon 길이가 최대가 된 경우 promotion을 할 수 없음
		s.promotionQueue[0] = nil
		s.promotionQueue = s.promotionQueue[1:]
		return false
	}

	if trans.block.IsLockedEntry() || trans.block.GetReadCount() > 0 { // 누가 사용 중임
		return false
	}

	trans.block.IsValid = false

	bankID := trans.bankID

	newTrans := *trans
	newTrans.bankID = bankID - 1
	newTrans.block = trans.block
	newTrans.action = InsertPromotionEntry
	newTrans.fromLocal = true
	newTrans.reqToBottom = nil

	reqs := newTrans.mshrEntry.Requests // mshrEntry.Request의 맨 앞에 newTrans 삽입하기
	reqs = append(reqs, nil)
	copy(reqs[1:], reqs)
	reqs[0] = &newTrans
	newTrans.mshrEntry.Requests = reqs

	// pid := trans.block.PID
	addr := trans.block.Tag
	mask := s.cache.regionLen[trans.bankID-1]
	newTrans.blockIdx = int(addr) % (mask + s.cache.log2NumSubEntry) >> mask

	// mshrEntry := s.cache.mshr.Query(pid, addr)
	// if mshrEntry != nil {
	// 	return false
	// }
	// mshrEntry = s.cache.mshr.Add(pid, addr, uint64(mask), bankID-1)
	// mshrEntry.Requests = append(mshrEntry.Requests, &newTrans)
	// newTrans.mshrEntry = mshrEntry

	if s.cache.debugPromotion {
		fmt.Printf("[%s]\tPromotion Entry: Addr %x, bank %d -> %d, Sharer %6.6s",
			s.cache.name, addr, bankID, bankID-1, trans.block.SubEntry[0].Sharer)
		// for i := range trans.block.SubEntry[0].Sharer {
		// 	fmt.Printf("%6.6s ", trans.block.SubEntry[0].Sharer[i])
		// }
		fmt.Printf("\n")
	}

	s.cache.dirStageMotionBuffer.Push(&newTrans)
	s.promotionQueue[0] = nil
	s.promotionQueue = s.promotionQueue[1:]

	return true
}

func (s *mshrStage) insertDemotionEntry() bool {
	if s.cache.mshr.IsFull() {
		return false
	}

	if !s.cache.dirStageMotionBuffer.CanPush() {
		return false
	}

	if len(s.demotionQueue) == 0 {
		return false
	}

	trans := s.demotionQueue[0]
	if trans == nil {
		s.demotionQueue = s.demotionQueue[1:]
		return false
	}

	if trans.bankID == s.cache.numBanks-1 { // 이미 reigon 길이가 최소가 된 경우 demotion을 할 수 없음
		s.demotionQueue[0] = nil
		s.demotionQueue = s.demotionQueue[1:]
		return false
	}

	if trans.block.SubEntry[trans.blockIdx].IsLocked || trans.block.GetReadCount() > 0 { // 누가 사용 중임
		return false
	}

	trans.block.SubEntry[trans.blockIdx].IsValid = false

	bankID := trans.bankID
	prevRegionLen := s.cache.regionLen[bankID]
	currRegionLen := s.cache.regionLen[bankID+1]

	pid := trans.evictingPID
	addr := trans.evictingAddr
	sh := trans.invalidationList
	owner := trans.accessReq().GetSrcRDMA()

	currAddr := addr >> prevRegionLen << prevRegionLen
	endAddr := currAddr + (1 << prevRegionLen)
	newBlk := &internal.CohEntry{
		PID: pid,
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

	newTrans := *trans
	newTrans.bankID = bankID + 1
	newTrans.block = newBlk
	newTrans.action = InsertDemotionEntry
	newTrans.fromLocal = true
	newTrans.reqToBottom = nil

	mshrEntry := s.cache.mshr.Query(pid, addr)
	if mshrEntry != nil {
		return false
	}
	mshrEntry = s.cache.mshr.Add(pid, addr, uint64(prevRegionLen), bankID) // currRegionLen으로 저장하려면 entry 4개를 저장해야됨
	mshrEntry.Requests = append(mshrEntry.Requests, &newTrans)
	newTrans.mshrEntry = mshrEntry

	s.cache.dirStageMotionBuffer.Push(&newTrans)

	if s.cache.debugPromotion {
		fmt.Printf("[%s]\tDemotion Entry: Addr %x, bank %d -> %d, Sharer %v\n", s.cache.name, addr, bankID, bankID+1, sh)
	}

	s.demotionQueue[0] = nil
	s.demotionQueue = s.demotionQueue[1:]
	return true
}

func (s *mshrStage) wakeupGatheredPromotions() bool {
	progress := false

	for i := 0; i < len(s.pendingGatherQueue); i++ {
		trans := s.pendingGatherQueue[i]

		// 모든 하위 뱅크로부터 Ack가 도착하여 취합이 완료된 경우
		if trans.isGatherDone {

			// Directory Stage로 다시 넣을 수 있는지 여유 공간 확인
			if s.cache.dirStageMotionBuffer.CanPush() {
				s.cache.dirStageMotionBuffer.Push(trans)

				// 큐에서 깨운 트랜잭션 제거 (순서 유지하며 중간 요소 삭제)
				s.pendingGatherQueue = append(s.pendingGatherQueue[:i], s.pendingGatherQueue[i+1:]...)
				i-- // 슬라이스 길이가 줄었으므로 인덱스 보정

				progress = true
			}
		}
	}

	return progress
}
