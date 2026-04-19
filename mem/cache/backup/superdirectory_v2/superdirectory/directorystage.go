package superdirectory

import (
	"fmt"

	"github.com/sarchlab/akita/v4/mem/cache/superdirectory/internal"
	"github.com/sarchlab/akita/v4/pipelining"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/akita/v4/tracing"
)

type dirPipelineItem struct {
	trans *transaction
}

func (i dirPipelineItem) TaskID() string {
	return i.trans.id + "_dir_pipeline"
}

type directoryStage struct {
	cache *Comp

	localPipeline  []pipelining.Pipeline
	remotePipeline []pipelining.Pipeline
	localBuf       []sim.Buffer
	remoteBuf      []sim.Buffer

	motionPipeline []pipelining.Pipeline
	motionBuf      []sim.Buffer

	returnFalse0 string
	returnFalse1 string
	returnFalse  *string
}

func (ds *directoryStage) Tick() (madeProgress bool) {
	// 양쪽 모두 Tick 진행
	madeProgress = ds.acceptNewTransaction(true) || madeProgress  // Local
	madeProgress = ds.acceptNewTransaction(false) || madeProgress // Remote
	madeProgress = ds.acceptMotionTransaction() || madeProgress   // Local Motion

	for i, _ := range ds.localPipeline {
		madeProgress = ds.localPipeline[i].Tick() || madeProgress
		madeProgress = ds.remotePipeline[i].Tick() || madeProgress
		madeProgress = ds.motionPipeline[i].Tick() || madeProgress
	}

	madeProgress = ds.processTransaction(true) || madeProgress
	madeProgress = ds.processTransaction(false) || madeProgress
	madeProgress = ds.processMotionTransaction() || madeProgress

	return madeProgress
}

func (ds *directoryStage) acceptNewTransaction(fromLocal bool) bool {
	madeProgress := false

	targetBuf := ds.cache.localDirStageBuffer
	targetPipeline := ds.localPipeline
	if !fromLocal {
		targetBuf = ds.cache.remoteDirStageBuffer
		targetPipeline = ds.remotePipeline
	}

	// 사이클 당 최대 처리량(numReqPerCycle)만큼 단일 루프 실행
	for i := 0; i < ds.cache.numReqPerCycle; i++ {
		// 1. 버퍼 폴링 (Ack 버퍼를 먼저 확인하여 교착 상태 방지, 이후 일반 버퍼 확인)
		item := targetBuf.Peek()
		if item == nil {
			break
		}
		trans := item.(*transaction)
		req := trans.accessReq()

		if req == nil {
			fmt.Printf("[%s][ERROR] Transaction %s has nil AccessReq. action: %d\n", ds.cache.name, trans.id, trans.action)
		}

		list := ds.cache.directory.GetBank(trans.accessReq().GetAddress())
		if len(list) > 0 {
			trans.bankID = list[0]
			trans.bankList = list[1:]
		} else {
			trans.bankID = ds.cache.numBanks - 1
		}

		if !targetPipeline[trans.bankID].CanAccept() {
			return madeProgress // 파이프라인 정체 시 조기 종료
		}

		targetPipeline[trans.bankID].Accept(dirPipelineItem{trans})
		targetBuf.Pop()
		madeProgress = true

	}

	return madeProgress
}

func (ds *directoryStage) acceptMotionTransaction() bool {
	madeProgress := false

	// 주의: cache 컴포넌트에 localDirStageMotionBuffer 를 추가해야 합니다.
	targetBuf := ds.cache.dirStageMotionBuffer

	for i := 0; i < ds.cache.numReqPerCycle; i++ {
		var trans *transaction
		var activeBuf sim.Buffer

		// Ack 버퍼를 최우선으로 폴링 (Ack는 Local에서만 온다고 가정)
		if ds.cache.dirStageAckBuffer.Peek() != nil {
			trans = ds.cache.dirStageAckBuffer.Peek().(*transaction)
			activeBuf = ds.cache.dirStageAckBuffer
		} else if item := targetBuf.Peek(); item != nil {
			trans = item.(*transaction)
			activeBuf = targetBuf
		} else {
			break
		}

		switch trans.action {
		case PromotionGatherAck:
			origTrans := trans.mshrEntry.Requests[0].(*transaction)

			if !origTrans.isGatherDone {
				if len(origTrans.gatheredSharers) == 0 {
					origTrans.gatheredSharers = make([][]sim.RemotePort, len(trans.gatheredSharers))
				}
				for j, sharers := range trans.gatheredSharers {
					origTrans.gatheredSharers[j] = append(origTrans.gatheredSharers[j], sharers...)
				}

				origTrans.gatherWaitCnt--

				if origTrans.gatherWaitCnt <= 0 {
					origTrans.isGatherDone = true
				}
			}

			if origTrans.isGatherDone {
				if !ds.motionPipeline[origTrans.bankID].CanAccept() {
					return madeProgress
				}
				ds.motionPipeline[origTrans.bankID].Accept(dirPipelineItem{trans: origTrans})
			}

			activeBuf.Pop()
			madeProgress = true

		case InsertPromotionEntry, InsertDemotionEntry:
			trans.fromLocal = true
			if !ds.motionPipeline[trans.bankID].CanAccept() {
				return madeProgress
			}

			ds.motionPipeline[trans.bankID].Accept(dirPipelineItem{trans})
			activeBuf.Pop()
			madeProgress = true

		default:
			fmt.Printf("[%s][ERROR] Invalid action in Motion Buffer: %d\n", ds.cache.name, trans.action)
			activeBuf.Pop() // 비정상 메시지 폐기
		}
	}

	return madeProgress
}

func (ds *directoryStage) Reset() {
	for i := range ds.localPipeline {
		ds.localPipeline[i].Clear()
		ds.remotePipeline[i].Clear()
		ds.motionPipeline[i].Clear()
	}

	for i := range ds.localBuf {
		ds.localBuf[i].Clear()
		ds.remoteBuf[i].Clear()
		ds.motionBuf[i].Clear()
	}

	ds.cache.localDirStageBuffer.Clear()
	ds.cache.remoteDirStageBuffer.Clear()

	if ds.cache.dirStageMotionBuffer != nil {
		ds.cache.dirStageMotionBuffer.Clear()
	}
}

func (ds *directoryStage) processTransaction(isLocal bool) bool {
	madeProgress := false
	buf := ds.localBuf
	ds.returnFalse = &ds.returnFalse0
	if !isLocal {
		buf = ds.remoteBuf
		ds.returnFalse = &ds.returnFalse1
	}
	*ds.returnFalse = ""

	for bankID := range buf {
		for i := 0; i < ds.cache.numReqPerCycle; i++ {
			*ds.returnFalse += "."
			item := buf[bankID].Peek()
			if item == nil {
				break
			}

			trans := item.(dirPipelineItem).trans

			addr := trans.accessReq().GetAddress()
			cacheLineID, _ := getCacheLineID(addr, ds.cache.log2BlockSize)

			if _, evicting := ds.cache.evictingList[cacheLineID]; evicting {
				break
			}

			if ds.doWrite(trans, isLocal) {
				buf[bankID].Pop()
				madeProgress = true
			} else {
				break // 처리가 막히면 해당 Bank의 루프 중단
			}
		}
	}

	if madeProgress {
		*ds.returnFalse = ""
	}
	return madeProgress
}

func (ds *directoryStage) processMotionTransaction() bool {
	madeProgress := false
	buf := ds.motionBuf

	for bankID := range buf {
		for i := 0; i < ds.cache.numReqPerCycle; i++ {
			item := buf[bankID].Peek()
			if item == nil {
				break
			}

			trans := item.(dirPipelineItem).trans

			if trans.action == InsertPromotionEntry {
				if ds.doPromotion(trans, true) {
					buf[bankID].Pop()
					madeProgress = true
				} else {
					break
				}
			} else if trans.action == InsertDemotionEntry {
				if ds.doDemotion(trans, true) {
					buf[bankID].Pop()
					madeProgress = true
				} else {
					break
				}
			}
		}
	}
	return madeProgress
}

func (ds *directoryStage) doPromotion(trans *transaction, isLocal bool) bool {
	*ds.returnFalse += "[doPromotion] "
	targetBankID := trans.bankID // 승격될 타겟 상위 bank (예: n-1)
	targetBuffer := ds.cache.localDirToBankBuffers
	if !isLocal {
		targetBuffer = ds.cache.remoteDirToBankBuffers
	}

	// =========================================================================
	// Phase 1: 하위 Bank들에 Sharer 탐색 요청 (Gathering Initiation)
	// =========================================================================
	if !trans.isGatherDone {
		// 1. 하위 뱅크 리스트업 (targetBankID 보다 큰 ID들)
		lowerBanks := []int{}
		for b := targetBankID + 1; b < ds.cache.numBanks; b++ {
			lowerBanks = append(lowerBanks, b)
		}

		for _, lb := range lowerBanks {
			if !targetBuffer[lb].CanPush() {
				*ds.returnFalse += "Cannot push to target bank buffer"
				return false
			}
		}

		upperRegionLen := uint64(ds.cache.regionLen[targetBankID])
		maskLen := upperRegionLen + uint64(ds.cache.log2NumSubEntry)
		baseAddr := trans.block.Tag >> maskLen << maskLen
		trans.gatherWaitCnt = len(lowerBanks) // 기다려야 할 응답 수 설정

		// 4. (기본 포함) 본인을 트리거한 하위 Block의 Sharer들은 미리 담아둠
		// [방어 코드] 슬라이스가 nil일 경우를 대비해 초기화
		if len(trans.gatheredSharers) == 0 {
			trans.gatheredSharers = make([][]sim.RemotePort, 1<<ds.cache.log2NumSubEntry)
		}
		trans.gatheredSharers[trans.blockIdx] = append(trans.gatheredSharers[trans.blockIdx], trans.block.SubEntry[0].Sharer...)

		// 5. 하위 뱅크들로 Query 트랜잭션 복제 발송
		for _, lb := range lowerBanks {
			queryTrans := &transaction{
				action:       QueryLowerBankForPromotion,
				id:           trans.id + fmt.Sprintf("_query_bank_%d", lb),
				bankID:       lb,
				block:        trans.block,
				mshrEntry:    trans.mshrEntry,
				fetchingAddr: baseAddr, // [핵심] 상위 Bank 기준의 Base Address 전달
			}
			targetBuffer[lb].Push(queryTrans)
		}

		return true // 초기화 완료, 버퍼에서 꺼냄(Pop)
	}

	// =========================================================================
	// Phase 2: 응답 취합 완료 후 실제 Promotion 반영 (isGatherDone == true)
	// =========================================================================

	// 타겟 상위 뱅크 버퍼에 여유가 없으면 대기
	if !targetBuffer[targetBankID].CanPush() {
		*ds.returnFalse += "Cannot push to target dir to bank buffer"
		return false
	}

	regionLen := uint64(ds.cache.regionLen[targetBankID])
	maskLen := regionLen + uint64(ds.cache.log2NumSubEntry)
	addr := trans.block.Tag
	pid := trans.block.PID
	cachelineID, _ := getCacheLineID(addr, regionLen)

	// Target Block 찾기 및 할당 (이전 로직과 동일)
	targetBlock, index := ds.cache.directory.Lookup(targetBankID, pid, cachelineID)
	var alloc bool
	if targetBlock == nil {
		targetBlock, alloc = ds.cache.directory.FindVictim(targetBankID, pid, cachelineID)
		if alloc && (targetBlock.IsLockedEntry() || targetBlock.GetReadCount() > 0) {
			*ds.returnFalse += "Target block is used"
			return false
		}
		if alloc && ds.needEviction(targetBlock) {
			trans.action = EvictAndPromotionEntry
			trans.victim = *targetBlock
		}
		index = int((cachelineID >> regionLen) % uint64(1<<ds.cache.log2NumSubEntry))
	}

	// --- 수집된 Sharer 중복 제거 ---
	numSubEntries := 1 << ds.cache.log2NumSubEntry
	mergedMap := make([]map[sim.RemotePort]bool, numSubEntries)

	// [수정] 슬라이스 내부의 Map 초기화 필수
	for i := 0; i < numSubEntries; i++ {
		mergedMap[i] = make(map[sim.RemotePort]bool)
	}

	for i, sharers := range trans.gatheredSharers {
		if i >= numSubEntries {
			break
		} // 안전장치
		for _, sh := range sharers {
			mergedMap[i][sh] = true
		}
	}

	// [수정] 2차원 슬라이스 크기 할당
	finalSharers := make([][]sim.RemotePort, numSubEntries)
	for i, sharers := range mergedMap {
		for sh, b := range sharers {
			if b {
				finalSharers[i] = append(finalSharers[i], sh)
			}
		}
	}

	// Target Block 상태 일괄 업데이트
	targetBlock.PID = pid
	targetBlock.Tag = cachelineID >> maskLen << maskLen
	targetBlock.IsValid = true
	targetBlock.SubEntry[index].IsValid = true
	targetBlock.SubEntry[index].IsLocked = true
	for i, sharers := range finalSharers {
		targetBlock.SubEntry[i].Sharer = sharers // 취합된 결과 할당
		targetBlock.SubEntry[i].IsValid = true   // 취합된 결과 할당
		targetBlock.SubEntry[i].IsLocked = true  // 취합된 결과 할당
	}

	trans.block = targetBlock
	trans.blockIdx = index

	// MSHR 정리 및 Bank로 전송
	ds.cache.directory.InsertBloomfilter(targetBankID, cachelineID)

	// MSHR에서 안전하게 제거 (구현된 MSHR Remove 로직에 맞게 호출)
	// ds.cache.mshr.Remove(pid, cachelineID)

	targetBuffer[targetBankID].Push(trans)

	return true
}

func (ds *directoryStage) doDemotion(trans *transaction, isLocal bool) bool {
	*ds.returnFalse += "[doDemotion] "
	bankID := trans.bankID
	targetBuffer := ds.cache.localDirToBankBuffers
	if !isLocal {
		targetBuffer = ds.cache.remoteDirToBankBuffers
	}

	// 1. Early Exit: 버퍼 확인
	if !targetBuffer[bankID].CanPush() {
		*ds.returnFalse += "Cannot push to target dir to bank buffer"
		return false
	}

	if ds.cache.debugProcess && trans.evictingAddr == ds.cache.debugAddress {
		fmt.Printf("[%s] [bottomSender]\tProcessing Demotion Request - 1: addr %x\n", ds.cache.name, trans.evictingAddr)
	}

	regionLen := ds.cache.regionLen[bankID]
	maskLen := regionLen + ds.cache.log2NumSubEntry
	addr := trans.block.Tag
	pid := trans.block.PID
	cachelineID, _ := getCacheLineID(addr, uint64(regionLen))

	// 2. Target Block 찾기
	targetBlock, _ := ds.cache.directory.Lookup(bankID, pid, cachelineID)

	if targetBlock != nil { // Hit
		fmt.Printf("[%s][WARNING] Hit about demotion entry\n", ds.cache.name)
	} else { // Miss
		var alloc bool
		targetBlock, alloc = ds.cache.directory.FindVictim(bankID, pid, cachelineID)
		if alloc && (targetBlock.IsLockedEntry() || targetBlock.GetReadCount() > 0) {
			*ds.returnFalse += "Target block is being used"
			return false
		}

		if alloc && ds.needEviction(targetBlock) {
			trans.action = EvictAndDemotionEntry
			trans.victim = *targetBlock
		}
	}

	// 3. Target Block 상태 일괄 업데이트
	targetBlock.PID = pid
	targetBlock.Tag = cachelineID >> maskLen << maskLen
	targetBlock.IsValid = true

	for i, e := range trans.block.SubEntry { // bankStage에서 생성한 block을 저장하는 과정
		if e.IsValid {
			targetBlock.SubEntry[i] = trans.block.SubEntry[i]
			targetBlock.SubEntry[i].IsLocked = true
			ds.cache.directory.InsertBloomfilter(bankID, cachelineID)
		}
		cachelineID += 1 << regionLen
	}

	trans.block = targetBlock

	// 4. 전송 및 마무리
	targetBuffer[bankID].Push(trans)

	return true
}

func (ds *directoryStage) doWrite(trans *transaction, isLocal bool) bool {
	*ds.returnFalse += "[doWrite] "
	bankID := trans.bankID
	regionLen := ds.cache.regionLen[bankID]
	req := trans.accessReq()
	cachelineID, _ := getCacheLineID(req.GetAddress(), uint64(regionLen)) // 하위 영역은 0으로 지우기

	mshrEntry := ds.cache.mshr.Query(req.GetPID(), cachelineID) // TODO: MSHR query 방식 바꿔야됨
	if mshrEntry != nil {
		if trans.write != nil { // write 인 경우, MSHR hit이 발생하면 처리 x
			*ds.returnFalse += "MSHR hit for write request"
			return false
		}

		if trans.read.FetchForWriteMiss {
			*ds.returnFalse += "MSHR hit for fetch for write miss request"
			return false
		}

		ok := ds.doWriteMSHRHit(trans, mshrEntry)
		if ok {
			tracing.AddTaskStep(
				tracing.MsgIDAtReceiver(trans.accessReq(), ds.cache),
				ds.cache,
				"read-mshr-hit",
			)
		}

		return ok
	}

	block, index := ds.cache.directory.Lookup(bankID, trans.accessReq().GetPID(), cachelineID)
	// if block != nil {
	// 	fmt.Printf("[%s]\tDirectory Lookup: Addr %x -> %x, bank %d, set %d, way %d, index %d\n",
	// 		ds.cache.name, req.GetAddress(), block.Tag, bankID, block.SetID, block.WayID, index)
	// }

	if block != nil && block.SubEntry[index].IsValid {
		ok := ds.doWriteHit(trans, block, index, isLocal)
		if ok {
			what := "read-hit"
			if trans.write != nil {
				what = "write-hit"
			}

			tracing.AddTaskStep(
				tracing.MsgIDAtReceiver(trans.accessReq(), ds.cache),
				ds.cache,
				what,
			)
		}

		return ok
	}

	pipeline := ds.localPipeline
	if !isLocal {
		pipeline = ds.remotePipeline
	}
	if len(trans.bankList) != 0 {
		bankID := trans.bankList[0]

		if !pipeline[bankID].CanAccept() {
			// fmt.Printf("[%s]\t[WARNING] Next pipeline[%d] is full, waiting...\n", ds.cache.name, bankID)
			*ds.returnFalse += "Pipeline is full"
			return false
		}

		trans.bankID = bankID
		trans.bankList = trans.bankList[1:]
		pipeline[bankID].Accept(dirPipelineItem{trans})
		return true
	}

	ok := ds.doWriteMiss(trans, isLocal)
	if ok {
		what := "read-miss"
		if trans.write != nil {
			what = "write-miss"
		}

		tracing.AddTaskStep(
			tracing.MsgIDAtReceiver(trans.accessReq(), ds.cache),
			ds.cache,
			what,
		)
	}

	return ok
}

func (ds *directoryStage) doWriteMSHRHit(
	trans *transaction,
	mshrEntry *internal.MSHREntry,
) bool {
	trans.mshrEntry = mshrEntry

	regionLen := mshrEntry.RegionLen
	log2NumSubEntry := uint64(ds.cache.log2NumSubEntry)
	_, offset := getCacheLineID(trans.accessReq().GetAddress(), regionLen+log2NumSubEntry)
	offset = offset >> regionLen
	trans.blockIdx = int(offset)

	mshrEntry.Requests = append(mshrEntry.Requests, trans)

	return true
}

func (ds *directoryStage) doWriteHit(
	trans *transaction,
	block *internal.CohEntry,
	index int,
	isLocal bool,
) bool {
	*ds.returnFalse += "[doWriteHit] "
	subEntry := block.SubEntry[index]
	if subEntry.IsLocked || subEntry.ReadCount > 0 {
		*ds.returnFalse += "Subentry is being used"
		return false
	}

	if trans.isReadTrans() && !trans.read.FetchForWriteMiss {
		if ds.readPermission(trans, subEntry.Sharer) {
			trans.action = Nothing
		} else {
			trans.action = UpdateEntry
		}

		return ds.writeToBank(trans, block, index, isLocal)
	}

	if ds.writePermission(trans, subEntry.Sharer) {
		trans.action = Nothing
	} else {
		trans.action = InvalidateAndUpdateEntry
		trans.invalidationList = subEntry.Sharer
		trans.evictingAddr = block.Tag + uint64(index)*(1<<ds.cache.log2NumSubEntry)
		trans.evictingPID = block.PID
	}

	targetBuffer := ds.cache.localBottomSenderBuffer
	if !isLocal {
		targetBuffer = ds.cache.remoteBottomSenderBuffer
	}
	if trans.action == Nothing {
		if !targetBuffer.CanPush() {
			*ds.returnFalse += "Cannot push to target bottom sender buffer"
			return false
		}

		targetBuffer.Push(trans)
		return true
	}

	return ds.writeToBank(trans, block, index, isLocal)
}

func (ds *directoryStage) doWriteMiss(trans *transaction, isLocal bool) bool {
	*ds.returnFalse += "[doWriteMiss] "
	targetBuffer := ds.cache.localBottomSenderBuffer
	if !isLocal {
		targetBuffer = ds.cache.remoteBottomSenderBuffer
	}
	if trans.fromLocal { // local write request에 대해 directory miss 발생 시, entry 추가 안 함
		trans.action = Nothing
		if !targetBuffer.CanPush() {
			*ds.returnFalse += "Cannot push to bottom sender buffer"
			return false
		}
		targetBuffer.Push(trans)
		return true
	}

	if ds.cache.mshr.IsFull() {
		*ds.returnFalse += "MSHR is full"
		return false
	}

	// bankID를 PTE 또는 regionSizeBuffer에서 확인하기
	bankID := ds.cache.numBanks - 1
	e := ds.cache.regionSizeBuffer.Search(trans.accessReq().GetAddress())
	if e.RegionID != -1 {
		bankID = e.RegionID
		ds.cache.regionSizeBuffer.Delete(e)
	}
	trans.bankID = bankID
	regionLen := ds.cache.regionLen[bankID]

	pid := trans.accessReq().GetPID()
	addr := trans.accessReq().GetAddress()
	cachelineID, _ := getCacheLineID(addr, uint64(regionLen))
	index := (addr >> regionLen) % uint64(1<<ds.cache.log2NumSubEntry)

	victim, alloc := ds.cache.directory.FindVictim(bankID, pid, cachelineID)

	if !alloc {
		trans.action = UpdateEntry
		return ds.writeToBank(trans, victim, int(index), isLocal)
	}

	if victim.IsLockedEntry() || victim.GetReadCount() > 0 {
		*ds.returnFalse += "Victim is being used"
		return false
	}

	if ds.needEviction(victim) {
		trans.action = EvictAndInsertNewEntry
		trans.victim = *victim
	} else {
		trans.action = InsertNewEntry
	}

	// victim은 bloomfilter 감소, 추가되는 entry는 증가
	if victim.IsValidEntry() {
		addr := victim.Tag
		diff := uint64(1 << regionLen)
		for i, e := range victim.SubEntry {
			if e.IsValid {
				ds.cache.directory.EvictBloomfilter(bankID, addr)
				victim.SubEntry[i].IsValid = false
			}
			addr += diff
		}

		if bankID != ds.cache.numBanks-1 {
			ds.cache.regionSizeBuffer.Update(victim.Tag, bankID)
		}
	}

	return ds.writeToBank(trans, victim, int(index), isLocal)
}

func (ds *directoryStage) writeToBank(
	trans *transaction,
	block *internal.CohEntry,
	index int,
	isLocal bool,
) bool {
	*ds.returnFalse += "[writeToBank] "
	// numBanks := len(ds.cache.dirToBankBuffers)
	// bank := bankID(block, ds.cache.directory.WayAssociativity(), numBanks)

	bankID := trans.bankID
	bankBuf := ds.cache.localDirToBankBuffers[bankID]
	if !isLocal {
		bankBuf = ds.cache.remoteDirToBankBuffers[bankID]
	}

	if !bankBuf.CanPush() {
		*ds.returnFalse += "Cannot push to bank buffer"
		return false
	}

	if ds.cache.mshr.IsFull() {
		*ds.returnFalse += "MSHR is full"
		return false
	}

	regionLen := ds.cache.regionLen[bankID]
	maskLen := regionLen + ds.cache.log2NumSubEntry
	addr := trans.accessReq().GetAddress()
	cachelineID, _ := getCacheLineID(addr, uint64(ds.cache.regionLen[bankID]))

	if !block.SubEntry[index].IsValid {
		ds.cache.directory.InsertBloomfilter(bankID, cachelineID)
	}

	subEntry := &block.SubEntry[index]
	ds.cache.directory.Visit(trans.bankID, block)
	block.Tag = cachelineID >> maskLen << maskLen
	block.IsValid = true
	block.PID = trans.accessReq().GetPID()
	subEntry.IsLocked = true
	subEntry.IsValid = true
	trans.block = block
	trans.blockIdx = index

	bankBuf.Push(trans)

	mshrEntry := ds.cache.mshr.Add(block.PID, cachelineID, uint64(ds.cache.regionLen[bankID]), bankID)
	mshrEntry.Block = block
	mshrEntry.BlockIdx = index
	trans.mshrEntry = mshrEntry
	mshrEntry.Requests = nil

	return true
}

func (ds *directoryStage) needEviction(victim *internal.CohEntry) bool {
	if !victim.IsValidEntry() {
		return false
	}

	for _, e := range victim.SubEntry {
		if e.IsValid && len(e.Sharer) > 0 {
			return true
		}
	}

	return false
}

func (ds *directoryStage) readPermission(trans *transaction, sharer []sim.RemotePort) bool {
	if !trans.fromLocal { // remote access
		for _, sh := range sharer {
			if sh == trans.accessReq().Meta().Src {
				return true
			}
		}

		return false
	} else { // local access
		return true
	}
}

func (ds *directoryStage) writePermission(trans *transaction, sharer []sim.RemotePort) bool {
	if !trans.fromLocal { // remote access
		// 1. Sharer가 없거나(0명) 여러 명(2명 이상)인 경우 권한 없음
		if len(sharer) != 1 {
			return false
		}

		// 2. Sharer가 딱 1명이지만, 그게 요청자 본인이 아닌 경우 권한 없음
		if sharer[0] != trans.accessReq().Meta().Src {
			return false
		}

		// 유일한 Sharer가 본인일 때만 권한 있음
		return true

	} else { // local access
		if len(sharer) > 1 {
			return false
		}

		return true
	}
}
