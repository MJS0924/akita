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

	// progress = s.wakeupGatheredPromotions() || progress
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
	progress := true
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
		if req == nil {
			trans.mshrEntry.Requests[0] = nil
			trans.mshrEntry.Requests = trans.mshrEntry.Requests[1:]

			continue
		}

		t := req.(*transaction)
		index := t.blockIdx
		e := &blk.SubEntry[index]

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

		if e.IsValid && s.sharerExist(t.accessReq().GetSrcRDMA(), e.Sharer) {
			t.action = Nothing
		} else {
			t.action = UpdateEntry
			e.Sharer = append(
				e.Sharer,
				t.accessReq().GetSrcRDMA(),
			)
			e.IsValid = true
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

	// if trans.action == InvalidateAndUpdateEntry && trans.bankID != s.cache.numBanks-1 && len(trans.invalidationList) != 0 {
	if trans.action == InvalidateAndUpdateEntry && trans.needToDemotion {
		// mshr.Add는 regionLen[bankID]를 mask로 사용하므로, QueryWithMask도 동일한 값을 써야
		// 충돌 검사가 일치한다. regionLen[bankID+1](하위 뱅크, 더 작은 값)을 쓰면 Add가
		// 잡아내는 충돌을 QueryWithMask가 놓쳐 "entry already in mshr" panic이 발생한다.
		list := s.cache.mshr.QueryWithMask(trans.mshrEntry.PID, trans.mshrEntry.Address, uint64(s.cache.regionLen[trans.bankID]))
		if len(list) == 0 {
			s.demotionQueue = append(s.demotionQueue, trans)

			trans.block.SubEntry[trans.blockIdx].IsValid = false
			s.updateBloomFilter(trans.mshrEntry.Address, trans.bankID, false)
			entry := s.cache.mshr.Add(trans.mshrEntry.PID, trans.mshrEntry.Address, uint64(s.cache.regionLen[trans.bankID]), trans.bankID+1)
			// demotion 이전 구간 길이로 entry를 생성하지 않으면 4개의 entry를 삽입해야 함..
			entry.IsDemotion = true
			trans.mshrEntry = entry

			if s.cache.debugPromotion {
				fmt.Printf("[%s]\tStart Demotion: Addr %x, from local %t\n", s.cache.name, entry.Address, trans.fromLocal)
			}
		} else {
			fmt.Printf("[%s]\t[WARNING] Waiting for mshr entry before demotion\n", s.cache.name)
		}

	} else if trans.block.AbleToPromotion() && trans.bankID != 0 {
		list := s.cache.mshr.QueryWithMask(trans.mshrEntry.PID, trans.mshrEntry.Address, uint64(s.cache.regionLen[trans.bankID-1]))
		if len(list) == 0 {
			s.promotionQueue = append(s.promotionQueue, trans)

			trans.block.IsValid = false
			for i := range trans.block.SubEntry {
				trans.block.SubEntry[i].IsValid = false
			}
			s.updateBloomFilter(trans.block.Tag, trans.bankID, true)
			entry := s.cache.mshr.Add(trans.mshrEntry.PID, trans.mshrEntry.Address, uint64(s.cache.regionLen[trans.bankID-1]), trans.bankID-1)
			entry.IsPromotion = true
			trans.mshrEntry = entry

			if s.cache.debugPromotion {
				fmt.Printf("[%s]\tStart Promotion: Addr %x\n", s.cache.name, entry.Address)
			}
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

		if t.action == InsertPromotionEntry || t.action == EvictAndPromotionEntry {
			// [수정 3] DirectoryStage에서 걸어둔 Lock을 안전하게 해제
			blk.SubEntry[t.blockIdx].IsLocked = false

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
		newRegionLen := uint64(s.cache.regionLen[trans.bankID])
		addr := t.accessReq().GetAddress()
		index := int((addr >> newRegionLen) % uint64(1<<s.cache.log2NumSubEntry))
		blk.SubEntry[index].Sharer = s.appendSharer(
			t.accessReq().GetSrcRDMA(),
			blk.SubEntry[index].Sharer,
		)
		blk.SubEntry[index].IsValid = true

		// 4. 다음 파이프라인으로 Push
		t.action = Nothing
		targetBuf.Push(t)

		// 5. 메모리 누수 방지 및 큐에서 요소 제거
		trans.mshrEntry.Requests[0] = nil
		trans.mshrEntry.Requests = trans.mshrEntry.Requests[1:]

		progress = true

		if s.cache.debugProcess && t.read != nil && s.cache.debugAddress == t.read.Address {
			fmt.Printf("[%s]\tSend read request after processing mshr entry: addr %x, action %d\n",
				s.cache.name, t.read.Address, t.action)
		}
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

	if s.cache.debugPromotion {
		fmt.Printf("[%s] [mshrStage]\tProcess Req After Promotion: addr %x, bankID %d valid ", s.cache.name, trans.mshrEntry.Address, trans.bankID)
		for i := range trans.block.SubEntry {
			fmt.Printf(" %t", trans.block.SubEntry[i].IsValid)
		}
		fmt.Printf("\n")
	}

	if trans.block.AbleToPromotion() && trans.bankID != 0 {
		list := s.cache.mshr.QueryWithMask(trans.mshrEntry.PID, trans.mshrEntry.Address, uint64(s.cache.regionLen[trans.bankID-1]))
		if len(list) == 0 {
			s.promotionQueue = append(s.promotionQueue, trans)

			trans.block.IsValid = false
			for i := range trans.block.SubEntry {
				trans.block.SubEntry[i].IsValid = false
			}
			s.updateBloomFilter(trans.block.Tag, trans.bankID, true)
			entry := s.cache.mshr.Add(trans.mshrEntry.PID, trans.mshrEntry.Address, uint64(s.cache.regionLen[trans.bankID-1]), trans.bankID-1)
			entry.IsPromotion = true
			trans.mshrEntry = entry

			if s.cache.debugPromotion {
				fmt.Printf("[%s]\tStart Promotion: Addr %x\n", s.cache.name, entry.Address)
			}
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

		if t.action == InsertDemotionEntry || t.action == EvictAndDemotionEntry { // [수정 1] DirectoryStage에서 걸어둔 Lock을 안전하게 일괄 해제
			// Demotion은 여러 SubEntry가 복사되어 잠겨있을 수 있으므로 전체를 순회하며 해제합니다.
			for i := range blk.SubEntry {
				if blk.SubEntry[i].IsLocked {
					blk.SubEntry[i].IsLocked = false
				}
			}

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
		blk.SubEntry[t.blockIdx].IsValid = true

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

	if s.cache.debugPromotion {
		fmt.Printf("[%s] [bottomSender]\tProcess Req After Demotion: addr %x\n", s.cache.name, trans.evictingAddr)
	}

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
		return true
	}

	if trans.block.IsLockedEntry() || trans.block.GetReadCount() > 0 { // 누가 사용 중임
		return false
	}

	originalSharers := trans.block.SubEntry[0].Sharer
	copiedSharers := make([]sim.RemotePort, len(originalSharers))
	copy(copiedSharers, originalSharers)

	trans.block.Reset()

	newTrans := *trans.DeepCopy()
	newTrans.bankID = trans.bankID - 1
	newTrans.action = InsertPromotionEntry
	newTrans.fromLocal = true
	newTrans.reqToBottom = nil
	newTrans.sharers = copiedSharers
	newTrans.fetchingAddr = trans.block.Tag
	newTrans.fetchingPID = trans.block.PID

	// reqs := newTrans.mshrEntry.Requests // mshrEntry.Request의 맨 앞에 newTrans 삽입하기
	// reqs = append(reqs, nil)
	// copy(reqs[1:], reqs)
	// reqs[0] = &newTrans
	// newTrans.mshrEntry.Requests = reqs

	// pid := trans.block.PID
	addr := trans.block.Tag
	mask := s.cache.regionLen[trans.bankID-1]
	newTrans.blockIdx = int((addr >> uint(mask)) % uint64(1<<s.cache.log2NumSubEntry))

	// mshrEntry := s.cache.mshr.Query(pid, addr)
	// if mshrEntry != nil {
	// 	return false
	// }
	// mshrEntry = s.cache.mshr.Add(pid, addr, uint64(mask), bankID-1)
	// mshrEntry.Requests = append(mshrEntry.Requests, &newTrans)
	// newTrans.mshrEntry = mshrEntry

	if s.cache.debugPromotion {
		bankID := trans.bankID
		fmt.Printf("[%s]\tPromotion Entry: Addr %x, bank %d -> %d, Sharer %6.6s",
			s.cache.name, addr, bankID, bankID-1, newTrans.sharers)
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
			if !trans.fromLocal {
				e.Sharer = make([]sim.RemotePort, 1)
				e.Sharer[0] = owner
				e.IsValid = true
				e.IsLocked = false
				e.ReadCount = 0
			} else {
				e.IsValid = false
			}
		} else {
			copiedSh := make([]sim.RemotePort, len(sh))
			copy(copiedSh, sh)

			e.Sharer = copiedSh
			e.IsValid = true
			e.IsLocked = false
			e.ReadCount = 0
		}

		currAddr += 1 << currRegionLen
		i++
	}

	newTrans := *trans.DeepCopy()
	newTrans.bankID = bankID + 1
	newTrans.block = newBlk
	newTrans.action = InsertDemotionEntry
	newTrans.fromLocal = true
	newTrans.reqToBottom = nil

	s.cache.dirStageMotionBuffer.Push(&newTrans)

	if s.cache.debugPromotion {
		fmt.Printf("[%s]\tDemotion Entry: Addr %x, bank %d -> %d, Owner %s, Sharer [ ",
			s.cache.name, addr, bankID, bankID+1, owner)
		for i := range newTrans.block.SubEntry {
			fmt.Printf("%6.6s ", newTrans.block.SubEntry[i].Sharer)
		}
		fmt.Printf("]\n")
	}

	s.demotionQueue[0] = nil
	s.demotionQueue = s.demotionQueue[1:]
	return true
}

func (s *mshrStage) updateBloomFilter(tag uint64, prevBankID int, forPromotion bool) {
	if forPromotion && prevBankID == 0 {
		return
	}

	if !forPromotion && prevBankID == s.cache.numBanks-1 {
		return
	}

	if forPromotion {
		prevRegionLen := s.cache.regionLen[prevBankID]
		currRegionLen := s.cache.regionLen[prevBankID-1]
		addr := tag >> currRegionLen << currRegionLen
		endAddr := addr + 1<<currRegionLen
		diff := 1 << prevRegionLen

		s.cache.directory.InsertBloomfilter(prevBankID-1, tag) // 이게 맞나??
		for addr < endAddr {
			s.cache.directory.EvictBloomfilter(prevBankID, addr)
			addr += uint64(diff)
		}
		s.cache.regionSizeBuffer.InvalidateForPromotion(tag, prevBankID)
	} else {
		prevRegionLen := s.cache.regionLen[prevBankID]
		// currRegionLen := s.cache.regionLen[prevBankID+1]
		addr := tag >> prevRegionLen << prevRegionLen
		// endAddr := addr + 1<<prevRegionLen
		// diff := 1 << currRegionLen

		s.cache.directory.EvictBloomfilter(prevBankID, addr)
		// for addr < endAddr {
		// 	s.cache.directory.InsertBloomfilter(prevBankID+1, addr)
		// 	addr += uint64(diff)
		// }
	}
}
