package REC

import (
	"fmt"

	"github.com/sarchlab/akita/v4/mem/cache/REC/internal"
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

	localPipeline  pipelining.Pipeline
	remotePipeline pipelining.Pipeline
	localBuf       sim.Buffer
	remoteBuf      sim.Buffer

	returnFalse0 string
	returnFalse1 string
	returnFalse  *string
}

func (ds *directoryStage) Tick() (madeProgress bool) {
	// 양쪽 모두 Tick 진행
	madeProgress = ds.acceptNewTransaction(true) || madeProgress  // Local
	madeProgress = ds.acceptNewTransaction(false) || madeProgress // Remote

	madeProgress = ds.localPipeline.Tick() || madeProgress
	madeProgress = ds.remotePipeline.Tick() || madeProgress

	madeProgress = ds.processTransaction(true) || madeProgress
	madeProgress = ds.processTransaction(false) || madeProgress

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
		if !targetPipeline.CanAccept() {
			return madeProgress // 파이프라인 정체 시 조기 종료
		}

		targetPipeline.Accept(dirPipelineItem{trans})
		targetBuf.Pop()
		madeProgress = true

	}

	return madeProgress
}

func (ds *directoryStage) Reset() {
	ds.localPipeline.Clear()
	ds.remotePipeline.Clear()

	ds.localBuf.Clear()
	ds.remoteBuf.Clear()

	ds.cache.localDirStageBuffer.Clear()
	ds.cache.remoteDirStageBuffer.Clear()
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

	for i := 0; i < ds.cache.numReqPerCycle; i++ {
		*ds.returnFalse += "."
		item := buf.Peek()
		if item == nil {
			break
		}

		trans := item.(dirPipelineItem).trans

		addr := trans.accessReq().GetAddress()
		cacheLineID, _ := getCacheLineID(addr, ds.cache.log2BlockSize+uint64(ds.cache.log2NumSubEntry))

		if _, evicting := ds.cache.evictingList[cacheLineID]; evicting {
			break
		}

		if ds.doWrite(trans, isLocal) {
			buf.Pop()
			madeProgress = true
		} else {
			break // 처리가 막히면 해당 Bank의 루프 중단
		}
	}

	if madeProgress {
		*ds.returnFalse = ""
	}
	return madeProgress
}

func (ds *directoryStage) doWrite(trans *transaction, isLocal bool) bool {
	*ds.returnFalse += "[doWrite] "
	// regionLen := ds.cache.log2BlockSize + uint64(ds.cache.log2NumSubEntry)
	req := trans.accessReq()
	// cachelineID, _ := getCacheLineID(req.GetAddress(), uint64(regionLen)) // 하위 영역은 0으로 지우기

	mshrEntry := ds.cache.mshr.Query(req.GetPID(), req.GetAddress())
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
		}

		return ok
	}

	block, index := ds.cache.directory.Lookup(trans.accessReq().GetPID(), req.GetAddress())
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

	// regionLen := ds.cache.log2BlockSize + uint64(ds.cache.log2NumSubEntry)
	// _, offset := getCacheLineID(trans.accessReq().GetAddress(), regionLen)
	// offset = offset >> ds.cache.log2BlockSize
	// trans.blockIdx = int(offset)

	addr := trans.accessReq().GetAddress()
	trans.blockIdx = int((addr >> ds.cache.log2BlockSize) % uint64(1<<ds.cache.log2NumSubEntry))

	mshrEntry.Requests = append(mshrEntry.Requests, trans)

	if ds.cache.debugProcess && trans.read != nil && trans.read.Address == ds.cache.debugAddress {
		fmt.Printf("[%s] [directoryStage]\tReceived remote read req - 1: addr %x\n", ds.cache.name, trans.read.Address)
	}

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
	blkSize := ds.cache.log2BlockSize

	pid := trans.accessReq().GetPID()
	addr := trans.accessReq().GetAddress()
	cachelineID, _ := getCacheLineID(addr, uint64(blkSize))
	index := (addr >> blkSize) % uint64(1<<ds.cache.log2NumSubEntry)

	victim, alloc := ds.cache.directory.FindVictim(pid, cachelineID)

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

	// victim은 bloomfilter 감소, 추가되는 entry는 증가
	// if victim.IsValidEntry() {
	// 	addr := victim.Tag
	// 	diff := uint64(1 << ds.cache.log2BlockSize)
	// 	for i, e := range victim.SubEntry {
	// 		if e.IsValid {
	// 			victim.SubEntry[i].IsValid = false
	// 		}
	// 		addr += diff
	// 	}
	// }
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

	bankBuf := ds.cache.localDirToBankBuffers
	if !isLocal {
		bankBuf = ds.cache.remoteDirToBankBuffers
	}

	if !bankBuf.CanPush() {
		*ds.returnFalse += "Cannot push to bank buffer"
		return false
	}

	if ds.cache.mshr.IsFull() {
		*ds.returnFalse += "MSHR is full"
		return false
	}

	maskLen := int(ds.cache.log2BlockSize) + ds.cache.log2NumSubEntry
	addr := trans.accessReq().GetAddress()
	cachelineID, _ := getCacheLineID(addr, uint64(ds.cache.log2BlockSize))

	subEntry := &block.SubEntry[index]
	ds.cache.directory.Visit(block)
	block.Tag = cachelineID >> maskLen << maskLen
	block.IsValid = true
	block.PID = trans.accessReq().GetPID()
	subEntry.IsLocked = true
	subEntry.IsValid = true
	trans.block = block
	trans.blockIdx = index

	bankBuf.Push(trans)

	mshrEntry := ds.cache.mshr.Add(block.PID, cachelineID)
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
