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

		addr := req.GetAddress()

		sel := ds.selectBank(addr)
		trans.bankID = sel.bankID
		trans.bankList = sel.bankList
		trans.bfEagerInserted = sel.bfEager

		if !targetPipeline[trans.bankID].CanAccept() {
			return madeProgress // 상태 미변경 — 다음 cycle에 동일 로직 재실행 가능
		}

		targetPipeline[trans.bankID].Accept(dirPipelineItem{trans})
		if sel.onCommit != nil {
			sel.onCommit() // ★ dispatch 성공 후에만 state mutation
		}
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

	// 타겟 상위 뱅크 버퍼에 여유가 없으면 대기
	if !targetBuffer[targetBankID].CanPush() {
		*ds.returnFalse += "Cannot push to target dir to bank buffer"
		return false
	}

	regionLen := uint64(ds.cache.regionLen[targetBankID])
	maskLen := regionLen + uint64(ds.cache.log2NumSubEntry)
	addr := trans.fetchingAddr
	pid := trans.fetchingPID
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
			trans.victim = *targetBlock.DeepCopy()
		}

		if alloc {
			evictAddr := targetBlock.Tag
			diff := uint64(1 << ds.cache.regionLen[targetBankID])
			for _, e := range targetBlock.SubEntry {
				if e.IsValid {
					ds.cache.directory.EvictBloomfilter(targetBankID, evictAddr)
				}
				evictAddr += diff
			}

			targetBlock.Reset()
		}

		index = int((cachelineID >> regionLen) % uint64(1<<ds.cache.log2NumSubEntry))
	}

	// Target Block 상태 일괄 업데이트
	targetBlock.PID = pid
	targetBlock.Tag = cachelineID >> maskLen << maskLen
	targetBlock.IsValid = true

	targetBlock.SubEntry[index].IsValid = true
	targetBlock.SubEntry[index].IsLocked = true
	copiedSh := make([]sim.RemotePort, len(trans.sharers))
	copy(copiedSh, trans.sharers)
	targetBlock.SubEntry[index].Sharer = copiedSh

	trans.block = targetBlock
	trans.blockIdx = index

	ds.cache.directory.InsertBloomfilter(targetBankID, cachelineID)

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
		fmt.Printf("[%s] [directoryStage]\tProcessing Demotion Request - 1: addr %x\n", ds.cache.name, trans.evictingAddr)
	}

	regionLen := ds.cache.regionLen[bankID]
	maskLen := regionLen + ds.cache.log2NumSubEntry
	addr := trans.block.Tag
	pid := trans.block.PID
	cachelineID, _ := getCacheLineID(addr, uint64(regionLen))

	// 2. Target Block 찾기
	targetBlock, _ := ds.cache.directory.Lookup(bankID, pid, cachelineID)

	if targetBlock != nil { // Hit
		fmt.Printf("[%s][WARNING] Hit about demotion entry: bank %d, addr %x\n", ds.cache.name, bankID, cachelineID)
	} else { // Miss
		var alloc bool
		targetBlock, alloc = ds.cache.directory.FindVictim(bankID, pid, cachelineID)
		if alloc && (targetBlock.IsLockedEntry() || targetBlock.GetReadCount() > 0) {
			*ds.returnFalse += "Target block is being used"
			return false
		}

		if alloc && ds.needEviction(targetBlock) {
			trans.action = EvictAndDemotionEntry
			trans.victim = *targetBlock.DeepCopy()
		}

		if alloc {
			evictAddr := targetBlock.Tag
			diff := uint64(1 << ds.cache.regionLen[bankID]) // doDemotion이면 bankID 사용
			for _, e := range targetBlock.SubEntry {
				if e.IsValid {
					ds.cache.directory.EvictBloomfilter(bankID, evictAddr)
				}
				evictAddr += diff
			}

			targetBlock.Reset()
		}
	}

	// 3. Target Block 상태 일괄 업데이트
	targetBlock.PID = pid
	targetBlock.Tag = cachelineID >> maskLen << maskLen
	targetBlock.IsValid = true

	for i, e := range trans.block.SubEntry { // mshrStage에서 생성한 block을 저장하는 과정
		if e.IsValid {
			targetBlock.SubEntry[i] = trans.block.SubEntry[i]
			targetBlock.SubEntry[i].IsLocked = true
		} else {
			targetBlock.SubEntry[i].IsValid = false
		}
		// FinalizeDemotionEntry가 IsValid=false인 subentry까지 포함해 모두 IsValid=true로
		// 덮어쓰므로, BF 삽입은 IsValid 여부와 무관하게 항상 수행해야 false negative를 막을 수 있다.
		ds.cache.directory.InsertBloomfilter(bankID, cachelineID)
		cachelineID += 1 << regionLen
	}

	trans.block = targetBlock

	// 4. 전송 및 마무리
	targetBuffer[bankID].Push(trans)

	return true
}

func (ds *directoryStage) doWrite(trans *transaction, isLocal bool) bool {
	*ds.returnFalse += "[doWrite] "
	trans.banksChecked++ // 이 bank를 실제로 확인함
	bankID := trans.bankID
	regionLen := ds.cache.regionLen[bankID]
	req := trans.accessReq()
	cachelineID, _ := getCacheLineID(req.GetAddress(), uint64(regionLen)) // 하위 영역은 0으로 지우기

	// Cross-granularity-aware MSHR check.
	// mshr.Add uses max(e.RegionLen, newRegionLen) so it detects conflicts across
	// granularities.  The old mshr.Query only used e.RegionLen, making fine-grained
	// entries (e.g. bank-4, RegionLen=6) invisible to coarser requests (bank-2,
	// RegionLen=10) that share the same 1KB region — leading to a panic in mshr.Add.
	//
	// New logic:
	//  • If a *finer* MSHR entry (e.RegionLen < regionLen) overlaps → stall until it
	//    completes; otherwise mshr.Add would panic with "entry already in mshr".
	//  • If a *coarser or same* MSHR entry overlaps → normal MSHR hit (same as before).
	var mshrEntry *internal.MSHREntry
	for _, e := range ds.cache.mshr.QueryWithMask(req.GetPID(), req.GetAddress(), uint64(regionLen)) {
		if e.RegionLen < uint64(regionLen) {
			// Finer-grained entry overlaps this coarser request → must wait for it
			*ds.returnFalse += "stall: finer MSHR entry overlaps coarser request"
			return false
		}
		if mshrEntry == nil {
			mshrEntry = e // first coarser-or-same entry becomes the MSHR hit
		}
	}
	if mshrEntry != nil {
		if trans.write != nil { // write 인 경우, MSHR hit이 발생하면 처리 x
			*ds.returnFalse += "MSHR hit for write request"
			return false
		}

		// if trans.read.FetchForWriteMiss {
		// 	*ds.returnFalse += "MSHR hit for fetch for write miss request"
		// 	return false
		// }

		ok := ds.doWriteMSHRHit(trans, mshrEntry)
		if ok {
			tracing.AddTaskStep(
				tracing.MsgIDAtReceiver(trans.accessReq(), ds.cache),
				ds.cache,
				"read-mshr-hit",
			)
			tracing.AddTaskStep(
				tracing.MsgIDAtReceiver(trans.accessReq(), ds.cache),
				ds.cache,
				fmt.Sprintf("BankChecked - %d", trans.banksChecked),
			)

			if ds.cache.debugProcess && trans.read != nil && trans.read.Address == ds.cache.debugAddress {
				fmt.Printf("[%s] [directoryStage]\tReceived remote read req - 1: mshr hit addr %x\n", ds.cache.name, trans.read.Address)
			}
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
			tracing.AddTaskStep(
				tracing.MsgIDAtReceiver(trans.accessReq(), ds.cache),
				ds.cache,
				fmt.Sprintf("BankChecked - %d", trans.banksChecked),
			)

			if ds.cache.debugProcess && trans.read != nil && trans.read.Address == ds.cache.debugAddress {
				fmt.Printf("[%s] [directoryStage]\tReceived remote read req - 2: write hit: bank %d addr %x, action %d\n", ds.cache.name, trans.bankID, trans.read.Address, trans.action)
			}
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
		tracing.AddTaskStep(
			tracing.MsgIDAtReceiver(trans.accessReq(), ds.cache),
			ds.cache,
			fmt.Sprintf("BankChecked - %d", trans.banksChecked),
		)

		if ds.cache.debugProcess && trans.read != nil && trans.read.Address == ds.cache.debugAddress {
			fmt.Printf("[%s] [directoryStage]\tReceived remote read req - 3: write miss: bank %d addr %x, action %d\n", ds.cache.name, trans.bankID, trans.read.Address, trans.action)
		}
	}

	return ok
}

func (ds *directoryStage) doWriteMSHRHit(
	trans *transaction,
	mshrEntry *internal.MSHREntry,
) bool {
	trans.mshrEntry = mshrEntry

	regionLen := ds.cache.regionLen[trans.bankID] // demotion 중인 entry는 bankID와 regionLen이 맞지 않으므로 새로 계산하여 사용
	log2NumSubEntry := ds.cache.log2NumSubEntry
	_, offset := getCacheLineID(trans.accessReq().GetAddress(), uint64(regionLen+log2NumSubEntry))
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
		trans.invalidationList = make([]sim.RemotePort, len(subEntry.Sharer))
		copy(trans.invalidationList, subEntry.Sharer)
		trans.evictingAddr = block.Tag + uint64(index<<ds.cache.regionLen[trans.bankID])
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

	// bankID를 PTE 또는 regionSizeBuffer에서 확인하기 -> bloomfilter 확인하기 전으로 이동
	bankID := ds.cache.numBanks - 1
	// e := ds.cache.regionSizeBuffer.Search(trans.accessReq().GetAddress())
	// if e.RegionID != -1 {
	// 	bankID = e.RegionID
	// 	ds.cache.regionSizeBuffer.Delete(e)
	// }
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
		trans.victim = *victim.DeepCopy()
		trans.evictingAddr = victim.Tag
		trans.evictingPID = victim.PID
	} else {
		trans.action = InsertNewEntry
	}

	// Pre-flight check: ensure bank buffer has space BEFORE modifying BF/directory state.
	// Without this check, BF decrements on the victim happen even if writeToBank later
	// fails (buffer full), leaving the BF with a false negative.  That false negative
	// causes future requests to miss the coarser bank and fall through to a finer bank,
	// creating MSHR entries at two different granularities for the same address region,
	// which triggers the "entry already in mshr" panic in mshr.Add.
	{
		bankBuf := ds.cache.localDirToBankBuffers[bankID]
		if !isLocal {
			bankBuf = ds.cache.remoteDirToBankBuffers[bankID]
		}
		if !bankBuf.CanPush() {
			*ds.returnFalse += "pre-check: bank buffer full before victim eviction"
			return false
		}
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

			if bankID != ds.cache.numBanks-1 {
				ds.cache.regionSizeBuffer.Update(addr, bankID)
			}

			addr += diff
		}

	}

	victim.Reset()

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

	// [BUG FIX] mshr.Query in doWrite uses e.RegionLen as mask and may miss fine-grained MSHR
	// entries when a coarse-grained (large regionLen) operation is about to be added.
	// Example: bank-4 entry (RegionLen=6) for addr=0x2000CF000 is invisible to Query when
	// checking a bank-0 access (addr=0x2000CC040), because 0x2000CF000>>6 != 0x2000CC040>>6.
	// But mshr.Add uses max(6,14)=14 and both >>14 match → panic.
	// Fix: stall if any overlapping entry exists at the actual Add granularity.
	// if conflictList := ds.cache.mshr.QueryWithMask(trans.accessReq().GetPID(), cachelineID, uint64(regionLen)); len(conflictList) > 0 {
	// 	*ds.returnFalse += "Stall: cross-granularity MSHR conflict"
	// 	fmt.Printf("[%s] [WARNING]\t%s: Addr %x\n", *ds.returnFalse, cachelineID)
	// 	return false
	// }

	if !block.SubEntry[index].IsValid && !trans.bfEagerInserted {
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
	mshrEntry.IsAccessReq = true
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
		if fmt.Sprintf("%s", sharer[0]) != fmt.Sprintf("%s", trans.accessReq().GetSrcRDMA()) {
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

// bankSelection holds the routing decision produced by selectBank.
// onCommit must be called exactly once, immediately after the pipeline Accept
// succeeds — never before, and never on early return.
type bankSelection struct {
	bankID   int
	bankList []int
	// onCommit executes all state mutations (RSB.Delete, BF.Insert).
	// Nil for RSB-miss paths that require no state change.
	onCommit func()
	// bfEager is true when onCommit will call InsertBloomfilter.
	// writeToBank checks this to avoid double-counting the same BF counter.
	bfEager bool
}

// selectBank computes the routing decision for addr without mutating any shared
// state.  All state changes are deferred to bankSelection.onCommit, which the
// caller must invoke only after the pipeline Accept succeeds ([S1] guarantee).
//
// GetBank returns banks finest-first ([0]=finest, [last]=coarsest).
func (ds *directoryStage) selectBank(addr uint64) bankSelection {
	e := ds.cache.regionSizeBuffer.Search(addr)
	if e.RegionID != -1 {
		bfList := ds.cache.directory.GetBank(addr)
		n := len(bfList)
		hasCoarserBF := n > 0 && bfList[n-1] < e.RegionID
		if hasCoarserBF {
			// stale RSB → BF결과를 finest-first 순으로 사용
			// BF는 이미 해당 bank의 entry를 가리키고 있으므로 eager insert 불필요
			rsbEntry := e
			return bankSelection{
				bankID:   bfList[0],
				bankList: bfList[1:],
				onCommit: func() { ds.cache.regionSizeBuffer.Delete(rsbEntry) },
				bfEager:  false,
			}
		}
		// normal RSB hit → eager BF insert로 후속 요청이 동일 bank로 라우팅되도록 보장
		rsbEntry := e
		routedBank := e.RegionID
		return bankSelection{
			bankID:  routedBank,
			onCommit: func() {
				ds.cache.directory.InsertBloomfilter(routedBank, addr)
				ds.cache.regionSizeBuffer.Delete(rsbEntry)
			},
			bfEager: true,
		}
	}

	// RSB miss: BF finest-first 그대로 사용; 상태 변경 없음
	list := ds.cache.directory.GetBank(addr)
	if len(list) > 0 {
		return bankSelection{bankID: list[0], bankList: list[1:]}
	}
	return bankSelection{bankID: ds.cache.numBanks - 1}
}
