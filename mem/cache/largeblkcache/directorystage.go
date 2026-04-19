package largeblkcache

import (
	"fmt"
	"strings"

	"github.com/sarchlab/akita/v4/mem/cache/largeblkcache/internal"
	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
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

	// [수정] Pipeline 및 Buffer를 Local과 Remote로 완전 분리
	localPipeline  pipelining.Pipeline
	remotePipeline pipelining.Pipeline
	localBuf       sim.Buffer
	remoteBuf      sim.Buffer

	activeBuf sim.Buffer // 현재 처리 중인 버퍼를 가리키는 내부 포인터

	lastReturnValue bool
	returnFalse     string
}

func (ds *directoryStage) Tick() (madeProgress bool) {
	ds.returnFalse = ""

	madeProgress = ds.acceptNewTransaction() || madeProgress

	madeProgress = ds.localPipeline.Tick() || madeProgress
	madeProgress = ds.remotePipeline.Tick() || madeProgress

	madeProgress = ds.processTransaction() || madeProgress

	ds.lastReturnValue = madeProgress

	return madeProgress
}

func (ds *directoryStage) processTransaction() bool {
	madeProgress := false

	// 1순위: Remote 버퍼 우선 처리 (교착 상태 방지)
	madeProgress = ds.processSpecificBuffer(ds.remoteBuf) || madeProgress
	// 2순위: Local 버퍼 처리
	madeProgress = ds.processSpecificBuffer(ds.localBuf) || madeProgress

	return madeProgress
}

func (ds *directoryStage) processSpecificBuffer(targetBuf sim.Buffer) bool {
	madeProgress := false
	ds.activeBuf = targetBuf

	for i := 0; i < ds.cache.numReqPerCycle; i++ {
		item := ds.activeBuf.Peek()
		if item == nil {
			break
		}

		if ds.cache.debug > 0 {
			fmt.Printf("[%s]\tDEBUG: Send to bottom - 0\n", ds.cache.name)
			ds.cache.debug--
		}

		trans := item.(dirPipelineItem).trans

		addr := uint64(0)
		if trans.invalidation != nil {
			addr = trans.invalidation.Address
		} else if trans.accessReq() != nil {
			addr = trans.accessReq().GetAddress()
		} else {
			addr = trans.fetchAddress
		}

		cacheLineID, _ := getCacheLineID(addr, ds.cache.log2BlockSize)

		if _, evicting := ds.cache.evictingList[cacheLineID]; evicting {
			break
		}

		if trans.invalidation != nil {
			madeProgress = ds.doInvalidation(trans) || madeProgress
			continue
		}

		if trans.read != nil {
			madeProgress = ds.doRead(trans) || madeProgress
			continue
		}

		if trans.write != nil {
			madeProgress = ds.doWrite(trans) || madeProgress
			continue
		}

		madeProgress = ds.doPrefetch(trans) || madeProgress

		if ds.cache.deviceID == 3 && madeProgress && trans.accessReq() != nil && trans.accessReq().GetAddress() == 12888246464 {
			fmt.Printf("[%s]\tReceived - 4.1: %x\n", ds.cache.name, trans.accessReq().GetAddress())
		}
	}

	return madeProgress
}

func (ds *directoryStage) acceptNewTransaction() bool {
	madeProgress := false

	// 1순위: 외부(Remote) 요청 최우선 파이프라인 진입
	for i := 0; i < ds.cache.numReqPerCycle; i++ {
		if !ds.remotePipeline.CanAccept() {
			break
		}
		item := ds.cache.remoteDirStageBuffer.Peek()
		if item == nil {
			break
		}

		trans := item.(*transaction)
		ds.remotePipeline.Accept(dirPipelineItem{trans})
		ds.cache.remoteDirStageBuffer.Pop()
		madeProgress = true
	}

	// 2순위: 내부(Local) 요청 파이프라인 진입
	for i := 0; i < ds.cache.numReqPerCycle; i++ {
		if !ds.localPipeline.CanAccept() {
			break
		}
		item := ds.cache.dirStageBuffer.Peek()
		if item == nil {
			break
		}

		trans := item.(*transaction)
		ds.localPipeline.Accept(dirPipelineItem{trans})
		ds.cache.dirStageBuffer.Pop()
		madeProgress = true
	}

	return madeProgress
}

func (ds *directoryStage) Reset() {
	ds.localPipeline.Clear()
	ds.remotePipeline.Clear()
	ds.localBuf.Clear()
	ds.remoteBuf.Clear()
	ds.cache.dirStageBuffer.Clear()
	ds.cache.remoteDirStageBuffer.Clear() // 추가
}

func (ds *directoryStage) doInvalidation(trans *transaction) bool {
	ds.returnFalse = "[doInvalidation] "
	req := trans.invalidation

	cachelineID, _ := getCacheLineID(req.Address, ds.cache.log2BlockSize)
	block := ds.cache.directory.Lookup(req.PID, cachelineID)
	mshrEntry := ds.cache.mshr.Query(req.PID, cachelineID)

	// 1. 블록 상태 확인 및 통계 정보 사전 추출 (초기화 전 미리 백업)
	what := "InvalidateInvalidBlock"
	var pid vm.PID
	var vAddr uint64
	accessedCount := 0
	hasBeenRead := false
	hasBeenWritten := false

	if mshrEntry == nil && block != nil { // mshrEntry에 있는 경우는 block이 현재 cache에 없다는 것이므로 바로 response 전송
		if block.IsValid && (block.IsLocked || block.ReadCount > 0) {
			ds.returnFalse += fmt.Sprintf("Block %x is being used, isLocked %v, readCnt %d",
				block.Tag, block.IsLocked, block.ReadCount)

			// temp := ds.activeBuf.Pop()
			// ds.activeBuf.Push(temp)
			return false
		}

		if block.IsValid {
			what = "InvalidateValidBlock"
		}

		// 블록이 지워지기 전에 필요한 정보들을 안전하게 추출
		pid = block.PID
		vAddr = block.VAddr
		for _, accessed := range block.Accessed {
			if accessed {
				accessedCount++
			}
		}
		hasBeenRead = block.HasBeenRead
		hasBeenWritten = block.HasBeenWritten
	}

	// 2. 패킷 전송 가능 여부 확인 (블록을 지우기 "전"에 검사해야 상태 오염이 안 생김)
	port := ds.cache.topPort
	if ds.activeBuf == ds.remoteBuf {
		port = ds.cache.remoteTopPort
	}

	if !port.CanSend() {
		ds.returnFalse += "Cannot send to topPort"
		return false
	}

	// 3. 응답 패킷 생성 및 전송
	rsp := mem.InvRspBuilder{}.
		WithSrc(port.AsRemote()).
		WithDst(req.Src).
		WithRspTo(req.ReqFrom).
		Build()

	if accessedCount > 0 {
		rsp.Accessed = 1
	}

	err := port.Send(rsp)
	if err != nil {
		ds.returnFalse += "Failed to send to topPort"
		return false
	}

	// 4. 전송 성공이 보장된 후 실제 블록 상태 업데이트 및 MSHR 정리
	if mshrEntry == nil && block != nil {
		newBlk := &internal.Block{
			WayID:        block.WayID,
			SetID:        block.SetID,
			CacheAddress: block.CacheAddress,
		}
		*block = *newBlk // 이제 안전하게 덮어씌움

		ds.cache.eraseCacheLineFromRWMask(pid, vAddr)
	}

	ds.activeBuf.Pop()
	ds.cache.mshrStage.removeTransaction(trans)

	// 5. Tracing 기록 (미리 뽑아둔 accessedCount 등 활용)
	if mshrEntry == nil && block != nil {
		whatUsage := fmt.Sprintf("Usage: %d/%d", accessedCount, 1<<(ds.cache.log2BlockSize-6))
		tracing.AddTaskStep(tracing.MsgIDAtReceiver(req, ds.cache), ds.cache, whatUsage)
		whatUsage = fmt.Sprintf("RW: %t/%t", hasBeenRead, hasBeenWritten)
		tracing.AddTaskStep(tracing.MsgIDAtReceiver(req, ds.cache), ds.cache, whatUsage)
	}

	tracing.AddTaskStep(tracing.MsgIDAtReceiver(req, ds.cache), ds.cache, what)
	tracing.TraceReqComplete(req, ds.cache)

	if trans.invalidation.Address == 21475226560 {
		fmt.Printf("[%s]\t[DEBUG] Invalidation - 2: %s\n", ds.cache.name, req.ReqFrom)
	}

	return true
}

func (ds *directoryStage) doRead(trans *transaction) bool {
	ds.returnFalse = "[doRead] "

	cachelineID, _ := getCacheLineID(
		trans.read.Address, ds.cache.log2BlockSize)

	mshrEntry := ds.cache.mshr.Query(trans.read.PID, cachelineID)
	if mshrEntry != nil {
		return ds.handleReadMSHRHit(trans, mshrEntry)
	}

	block := ds.cache.directory.Lookup(
		trans.read.PID, cachelineID)
	if block != nil {
		return ds.handleReadHit(trans, block)
	}

	return ds.handleReadMiss(trans)
}

func (ds *directoryStage) handleReadMSHRHit(
	trans *transaction,
	mshrEntry *internal.MSHREntry,
) bool {
	trans.mshrEntry = mshrEntry
	mshrEntry.Requests = append(mshrEntry.Requests, trans)

	ds.activeBuf.Pop()

	tracing.AddTaskStep(
		tracing.MsgIDAtReceiver(trans.read, ds.cache),
		ds.cache,
		"read-mshr-hit",
	)

	return true
}

func (ds *directoryStage) handleReadHit(
	trans *transaction,
	block *internal.Block,
) bool {
	ds.returnFalse = ds.returnFalse + "[handleReadHit] "

	if block.IsLocked {
		ds.returnFalse = ds.returnFalse + fmt.Sprintf("Block %x is locked", block.Tag)

		return false
	}

	progress := ds.readFromBank(trans, block)
	if progress {
		tracing.AddTaskStep(
			tracing.MsgIDAtReceiver(trans.read, ds.cache),
			ds.cache,
			"read-hit",
		)
		if !trans.toLocal {
			tracing.AddTaskStep(
				tracing.MsgIDAtReceiver(trans.read, ds.cache),
				ds.cache,
				"remote-read-hit",
			)
		}
	}

	return progress
}

func (ds *directoryStage) handleReadMiss(trans *transaction) bool {
	ds.returnFalse = ds.returnFalse + "[handleReadMiss] "

	req := trans.read
	cacheLineID, _ := getCacheLineID(req.Address, ds.cache.log2BlockSize)

	if !ds.isMSHRAvailable(trans) {
		ds.returnFalse = ds.returnFalse + "MSHR is full"
		return false
	}

	victim := ds.cache.directory.FindVictim(cacheLineID)
	if victim.IsLocked || victim.ReadCount > 0 {
		ds.returnFalse = ds.returnFalse + fmt.Sprintf("Victim %x is being used", victim.Tag)

		return false
	}

	// log.Printf("%.10f, %s, dir read miss， %s, %04X, %04X, (%d, %d), %v\n",
	// 	now, ds.cache.Name(),
	// 	trans.read.ID,
	// 	trans.read.Address,
	// 	(trans.read.GetAddress()>>ds.cache.log2BlockSize)<<
	// 	ds.cache.log2BlockSize,
	// 	victim.SetID, victim.WayID,
	// 	nil,
	// )
	what := ""
	if victim.IsValid {
		what = "EvictValidBlock"
	} else {
		what = "EvictInvalidBlock"
	}

	if ds.needEviction(victim) {
		ok := ds.evict(trans, victim)
		if ok {
			tracing.AddTaskStep(
				tracing.MsgIDAtReceiver(trans.read, ds.cache),
				ds.cache,
				"read-miss",
			)
			if !trans.toLocal {
				tracing.AddTaskStep(
					tracing.MsgIDAtReceiver(trans.read, ds.cache),
					ds.cache,
					"remote-read-miss",
				)
			}
			tracing.AddTaskStep(
				tracing.MsgIDAtReceiver(trans.read, ds.cache),
				ds.cache,
				what,
			)
		}

		return ok
	}

	ok := ds.fetch(trans, victim)
	if ok {
		tracing.AddTaskStep(
			tracing.MsgIDAtReceiver(trans.read, ds.cache),
			ds.cache,
			"read-miss",
		)
		if !trans.toLocal {
			tracing.AddTaskStep(
				tracing.MsgIDAtReceiver(trans.read, ds.cache),
				ds.cache,
				"remote-read-miss",
			)
		}
		tracing.AddTaskStep(
			tracing.MsgIDAtReceiver(trans.read, ds.cache),
			ds.cache,
			what,
		)
	}

	return ok
}

func (ds *directoryStage) doWrite(trans *transaction) bool {
	ds.returnFalse = "[doWrite] "

	write := trans.write
	cachelineID, _ := getCacheLineID(write.Address, ds.cache.log2BlockSize)

	mshrEntry := ds.cache.mshr.Query(write.PID, cachelineID)
	if mshrEntry != nil {
		// if !trans.toLocal {
		// 	ds.returnFalse += "MSHR hit for remote write"
		// 	return false
		// }

		ok := ds.doWriteMSHRHit(trans, mshrEntry)
		if ok {
			tracing.AddTaskStep(
				tracing.MsgIDAtReceiver(trans.write, ds.cache),
				ds.cache,
				"write-mshr-hit",
			)
			if !trans.toLocal {
				tracing.AddTaskStep(
					tracing.MsgIDAtReceiver(trans.write, ds.cache),
					ds.cache,
					"remote-read-hit",
				)
			}
		}

		return ok
	}

	block := ds.cache.directory.Lookup(trans.write.PID, cachelineID)
	if block != nil {
		ok := ds.doWriteHit(trans, block)
		if ok {
			tracing.AddTaskStep(
				tracing.MsgIDAtReceiver(trans.write, ds.cache),
				ds.cache,
				"write-hit",
			)
			if !trans.toLocal {
				tracing.AddTaskStep(
					tracing.MsgIDAtReceiver(trans.write, ds.cache),
					ds.cache,
					"remote-write-hit",
				)
			}
		}

		return ok
	}

	ok := ds.doWriteMiss(trans)
	if ok {
		tracing.AddTaskStep(
			tracing.MsgIDAtReceiver(trans.write, ds.cache),
			ds.cache,
			"write-miss",
		)
		if !trans.toLocal {
			tracing.AddTaskStep(
				tracing.MsgIDAtReceiver(trans.write, ds.cache),
				ds.cache,
				"remote-write-miss",
			)
		}
	}

	return ok
}

func (ds *directoryStage) doWriteMSHRHit(
	trans *transaction,
	mshrEntry *internal.MSHREntry,
) bool {
	isFirstWrite := true
	for _, req := range mshrEntry.Requests {
		t := req.(*transaction)
		if t.write != nil {
			isFirstWrite = false
			break // 이미 대기 중인 Write가 있으므로 더 볼 필요 없음
		}
	}

	// 최초 Write인 경우에만 Home Node 판단
	if isFirstWrite {
		port := fmt.Sprintf("%s", ds.cache.addressToPortMapper.Find(trans.accessReq().GetAddress()))
		if strings.Contains(port, "RDMA") {
			trans.writeToHomeNode = true
		} else {
			trans.writeToHomeNode = false
		}
	} else {
		// 이미 앞선 Write가 Home Node로 보낼 예정이므로 묻어감
		trans.writeToHomeNode = false
	}

	trans.mshrEntry = mshrEntry
	mshrEntry.Requests = append(mshrEntry.Requests, trans)

	ds.activeBuf.Pop()

	return true
}

func (ds *directoryStage) doWriteHit(
	trans *transaction,
	block *internal.Block,
) bool {
	ds.returnFalse = ds.returnFalse + "[doWriteHit] "

	if block.IsLocked || block.ReadCount > 0 {
		ds.returnFalse = ds.returnFalse + fmt.Sprintf("Block %x is being used", block.Tag)

		return false
	}

	return ds.writeToBank(trans, block)
}

func (ds *directoryStage) doWriteMiss(trans *transaction) bool {
	ds.returnFalse = ds.returnFalse + "[doWriteMiss] "

	write := trans.write

	if ds.isWritingFullLine(write) {
		return ds.writeFullLineMiss(trans)
	}

	return ds.writePartialLineMiss(trans)
}

func (ds *directoryStage) writeFullLineMiss(trans *transaction) bool {
	ds.returnFalse = ds.returnFalse + "[writeFullLineMiss] "

	write := trans.write
	cachelineID, _ := getCacheLineID(write.Address, ds.cache.log2BlockSize)

	victim := ds.cache.directory.FindVictim(cachelineID)
	if victim.IsLocked || victim.ReadCount > 0 {
		ds.returnFalse = ds.returnFalse + fmt.Sprintf("Victim %x is being used", victim.Tag)

		return false
	}

	what := ""
	if victim.IsValid {
		what = "EvictValidBlock"
	} else {
		what = "EvictInvalidBlock"
	}

	if ds.needEviction(victim) {
		ok := ds.evict(trans, victim)
		if ok {
			tracing.AddTaskStep(
				tracing.MsgIDAtReceiver(trans.write, ds.cache),
				ds.cache,
				what,
			)
		}

		return ok
	}

	ok := ds.writeToBank(trans, victim)
	if ok {
		tracing.AddTaskStep(
			tracing.MsgIDAtReceiver(trans.write, ds.cache),
			ds.cache,
			what,
		)
	}

	return ok
}

func (ds *directoryStage) writePartialLineMiss(trans *transaction) bool {
	ds.returnFalse = ds.returnFalse + "[writePartialLineMiss] "

	write := trans.write
	cachelineID, _ := getCacheLineID(write.Address, ds.cache.log2BlockSize)

	if !ds.isMSHRAvailable(trans) {
		ds.returnFalse = ds.returnFalse + "MSHR is full"

		return false
	}

	victim := ds.cache.directory.FindVictim(cachelineID)
	if victim.IsLocked || victim.ReadCount > 0 {
		ds.returnFalse = ds.returnFalse + fmt.Sprintf("Victim %x is being used", victim.Tag)

		return false
	}

	what := ""
	if victim.IsValid {
		what = "EvictValidBlock"
	} else {
		what = "EvictInvalidBlock"
	}

	if ds.needEviction(victim) {
		ok := ds.evict(trans, victim)
		if ok {
			tracing.AddTaskStep(
				tracing.MsgIDAtReceiver(trans.write, ds.cache),
				ds.cache,
				what,
			)
		}

		return ok
	}

	ok := ds.fetch(trans, victim)
	if ok {
		tracing.AddTaskStep(
			tracing.MsgIDAtReceiver(trans.write, ds.cache),
			ds.cache,
			what,
		)
	}

	return ok
}

func (ds *directoryStage) doPrefetch(trans *transaction) bool {
	ds.returnFalse = "[doPrefetch] "

	addr := trans.fetchAddress
	pid := trans.fetchPID
	cachelineID, _ := getCacheLineID(addr, ds.cache.log2BlockSize)

	block := ds.cache.directory.Lookup(pid, cachelineID)
	victim := &internal.Block{}

	if block != nil {
		// 프리패치는 필수가 아니므로 잠겨있으면 activeBuf에서 폐기(Pop)
		if block.IsLocked {
			ds.activeBuf.Pop()
			ds.returnFalse = ds.returnFalse + "block is locked, discard msg"
			return false
		}

		// 읽기 중이면 Pop하지 않고 기다림 (Stall)
		if block.ReadCount > 0 {
			ds.returnFalse = ds.returnFalse + "block is being read, wait for completion"
			return false
		}

		return ds.prefetchToBank(trans, block)

	} else {
		victim = ds.cache.directory.FindVictim(cachelineID)

		if victim.IsLocked || victim.ReadCount > 0 {
			ds.returnFalse = ds.returnFalse + fmt.Sprintf("Victim %x is being used", victim.Tag)
			return false
		}

		if ds.needEviction(victim) {
			return ds.evictAndPrefetch(trans, victim)
		}

		return ds.prefetchToBank(trans, victim)
	}
}

func (ds *directoryStage) prefetchToBank(trans *transaction, block *internal.Block) bool {
	ds.returnFalse = ds.returnFalse + "[prefetchToBank] "

	numBanks := len(ds.cache.dirToBankBuffers)
	bank := bankID(block, ds.cache.directory.WayAssociativity(), numBanks)
	bankBuf := ds.cache.dirToBankBuffers[bank]

	if !bankBuf.CanPush() {
		ds.returnFalse = ds.returnFalse + "Cannot push to bankBuf "
		return false
	}

	addr := trans.fetchAddress
	pid := trans.fetchPID
	cachelineID, _ := getCacheLineID(addr, ds.cache.log2BlockSize)
	mshrEntry := ds.cache.mshr.Query(pid, addr)

	ds.cache.directory.Visit(block, trans.fetchAddress)
	block.IsLocked = true
	block.Tag = cachelineID
	block.IsValid = true
	block.PID = pid
	trans.block = block
	trans.action = bankWritePrefetched
	trans.mshrEntry = mshrEntry

	ds.activeBuf.Pop()
	bankBuf.Push(trans)

	return true
}

func (ds *directoryStage) evictAndPrefetch(trans *transaction, victim *internal.Block) bool {
	ds.returnFalse = ds.returnFalse + "[evictAndPrefetch] "

	bankNum := bankID(victim, ds.cache.directory.WayAssociativity(), len(ds.cache.dirToBankBuffers))
	bankBuf := ds.cache.dirToBankBuffers[bankNum]

	if !bankBuf.CanPush() {
		ds.returnFalse = ds.returnFalse + "Cannot push to bankBuf "
		return false
	}

	// update trans for eviction before prefetch
	trans.action = bankEvictAndPrefetch
	trans.victim = &internal.Block{
		PID:          victim.PID,
		Tag:          victim.Tag,
		CacheAddress: victim.CacheAddress,
		DirtyMask:    victim.DirtyMask,
	}
	trans.block = victim
	trans.evictingPID = trans.victim.PID
	trans.evictingAddr = trans.victim.Tag
	trans.evictingDirtyMask = victim.DirtyMask

	// update victim block meta data
	// victim.CacheAddress, _ = getCacheLineID(trans.fetchAddress, ds.cache.log2BlockSize)
	victim.Tag = trans.fetchAddress
	victim.PID = trans.fetchPID
	victim.IsLocked = true
	victim.IsDirty = false
	ds.cache.directory.Visit(victim, trans.fetchAddress)

	ds.activeBuf.Pop()
	bankBuf.Push(trans)

	ds.cache.evictingList[trans.victim.Tag] = true

	return true
}

func (ds *directoryStage) readFromBank(
	trans *transaction,
	block *internal.Block,
) bool {
	ds.returnFalse = ds.returnFalse + "[readFromBank] "
	numBanks := len(ds.cache.dirToBankBuffers)
	bank := bankID(block, ds.cache.directory.WayAssociativity(), numBanks)
	bankBuf := ds.cache.dirToBankBuffers[bank]

	if !bankBuf.CanPush() {
		ds.returnFalse = ds.returnFalse + "Cannot push to bankBuf"

		return false
	}

	ds.cache.directory.Visit(block, trans.read.Address)

	block.ReadCount++
	block.HasBeenRead = true
	trans.block = block
	trans.action = bankReadHit

	ds.activeBuf.Pop()
	bankBuf.Push(trans)

	return true
}

func (ds *directoryStage) writeToBank(
	trans *transaction,
	block *internal.Block,
) bool {
	ds.returnFalse = ds.returnFalse + "[writeToBank] "

	numBanks := len(ds.cache.dirToBankBuffers)
	bank := bankID(block, ds.cache.directory.WayAssociativity(), numBanks)
	bankBuf := ds.cache.dirToBankBuffers[bank]

	if !bankBuf.CanPush() {
		ds.returnFalse = ds.returnFalse + "Cannot push to bankBuf "

		return false
	}

	addr := trans.write.Address
	cachelineID, _ := getCacheLineID(addr, ds.cache.log2BlockSize)

	ds.cache.directory.Visit(block, trans.write.Address)
	block.IsLocked = true
	block.Tag = cachelineID
	block.IsValid = true
	block.PID = trans.write.PID
	block.VAddr = trans.accessReq().GetVAddr()
	block.HasBeenWritten = true
	trans.block = block
	trans.action = bankWriteHit

	port := fmt.Sprintf("%s", ds.cache.addressToPortMapper.Find(trans.accessReq().GetAddress()))
	if strings.Contains(port, "RDMA") {
		trans.writeToHomeNode = true // Coherence Directory를 사용하는 경우, write back cache에서도 home node로 write를 보내야 함
		block.IsDirty = false        // remote data에 write 하는 경우, home node로 write 하므로 dirty 표시를 하지 않음
	} else {
		trans.writeToHomeNode = false
	}

	ds.activeBuf.Pop()
	bankBuf.Push(trans)

	return true
}

func (ds *directoryStage) evict(
	trans *transaction,
	victim *internal.Block,
) bool {
	ds.returnFalse = ds.returnFalse + "[evict] "

	bankNum := bankID(victim,
		ds.cache.directory.WayAssociativity(), len(ds.cache.dirToBankBuffers))
	bankBuf := ds.cache.dirToBankBuffers[bankNum]

	if !bankBuf.CanPush() {
		ds.returnFalse = ds.returnFalse + "Cannot push to bankBuf "

		return false
	}

	var (
		addr uint64
		pid  vm.PID
	)

	if trans.read != nil {
		addr = trans.read.Address
		pid = trans.read.PID
	} else {
		addr = trans.write.Address
		pid = trans.write.PID
	}

	cacheLineID, _ := getCacheLineID(addr, ds.cache.log2BlockSize)

	count := 0
	hasBeenRead := victim.HasBeenRead
	hasBeenWritten := victim.HasBeenWritten
	for _, b := range victim.Accessed {
		if b {
			count++
			// cacheline의 사용량 조사
		}
	}

	ds.updateTransForEviction(trans, victim, pid, cacheLineID)
	ds.updateVictimBlockMetaData(trans, victim, cacheLineID, pid)

	ds.activeBuf.Pop()
	bankBuf.Push(trans)

	ds.cache.evictingList[trans.victim.Tag] = true

	ds.cache.eraseCacheLineFromRWMask(victim.PID, victim.VAddr)

	// log.Printf("%.10f, %s, directory evict ， %s, %04X, %04X, (%d, %d), %v\n",
	// 	now, ds.cache.Name(),
	// 	trans.accessReq().Meta().ID,
	// 	trans.accessReq().GetAddress(), trans.victim.Tag,
	// 	victim.SetID, victim.WayID,
	// 	nil,
	// )

	// ds.cache.printRWMask(victim.PID, victim.VAddr)
	what := fmt.Sprintf("Usage: %d/%d", count, 1<<(ds.cache.log2BlockSize-6))
	tracing.AddTaskStep(
		tracing.MsgIDAtReceiver(trans.accessReq(), ds.cache),
		ds.cache,
		what,
	)

	what = fmt.Sprintf("RW: %t/%t", hasBeenRead, hasBeenWritten)
	tracing.AddTaskStep(
		tracing.MsgIDAtReceiver(trans.accessReq(), ds.cache),
		ds.cache,
		what,
	)

	return true
}

func (ds *directoryStage) updateVictimBlockMetaData(
	trans *transaction,
	victim *internal.Block,
	cacheLineID uint64,
	pid vm.PID,
) {
	victim.Tag = cacheLineID
	victim.PID = pid
	victim.IsLocked = true
	victim.IsDirty = false
	victim.HasBeenRead = false
	victim.HasBeenWritten = false
	ds.cache.directory.Visit(victim, trans.accessReq().GetAddress())
	victim.Accessed = make([]bool, 1<<(ds.cache.log2BlockSize-6))
}

func (ds *directoryStage) updateTransForEviction(
	trans *transaction,
	victim *internal.Block,
	pid vm.PID,
	cacheLineID uint64,
) {
	trans.action = bankEvictAndFetch
	trans.victim = &internal.Block{
		PID:          victim.PID,
		Tag:          victim.Tag,
		CacheAddress: victim.CacheAddress,
		DirtyMask:    victim.DirtyMask,
	}
	trans.block = victim
	trans.evictingPID = trans.victim.PID
	trans.evictingAddr = trans.victim.Tag
	trans.evictingDirtyMask = victim.DirtyMask

	if ds.evictionNeedFetch(trans) {
		mshrEntry := ds.cache.mshr.Add(pid, cacheLineID)
		mshrEntry.Block = victim
		mshrEntry.Requests = append(mshrEntry.Requests, trans)
		trans.mshrEntry = mshrEntry
		trans.fetchPID = pid
		trans.fetchAddress = cacheLineID
		trans.action = bankEvictAndFetch
	} else {
		trans.action = bankEvictAndWrite
	}
}

func (ds *directoryStage) evictionNeedFetch(t *transaction) bool {
	if t.write == nil {
		return true
	}

	if ds.isWritingFullLine(t.write) {
		return false
	}

	return true
}

func (ds *directoryStage) fetch(
	trans *transaction,
	block *internal.Block,
) bool {
	ds.returnFalse = ds.returnFalse + "[fetch] "

	var (
		addr uint64
		pid  vm.PID
		req  mem.AccessReq
		va   uint64
	)

	if trans.read != nil {
		req = trans.read
		addr = trans.read.Address
		pid = trans.read.PID
		va = req.GetVAddr()
	} else {
		req = trans.write
		addr = trans.write.Address
		pid = trans.write.PID
		va = req.GetVAddr()
	}

	cacheLineID, _ := getCacheLineID(addr, ds.cache.log2BlockSize)

	// [FIX: head-of-line blocking] writeBufferFetch 항목을 writeBufferFetchBuffer(fetch 전용)로 직접 push.
	// dirToBankBuffers 경유 시 writeBufferFetch가 HEAD를 점유해 bankReadHit/bankWriteHit를 차단하는
	// 교착 상태 발생. writeBufferFetchBuffer를 eviction 전용 writeBufferBuffer와 분리함으로써
	// tooManyInflightFetches()=true 시에도 finalizeBankEviction이 독립적으로 진행 가능해진다.
	if !ds.cache.writeBufferFetchBuffer.CanPush() {
		ds.returnFalse = ds.returnFalse + "Cannot push to writeBufferFetchBuffer"

		return false
	}

	mshrEntry := ds.cache.mshr.Add(pid, cacheLineID)
	trans.mshrEntry = mshrEntry
	trans.block = block
	block.IsLocked = true
	block.Tag = cacheLineID
	block.PID = pid
	block.IsValid = true
	block.VAddr = va
	block.HasBeenRead = false
	block.HasBeenWritten = false
	ds.cache.directory.Visit(block, addr)

	tracing.AddTaskStep(
		tracing.MsgIDAtReceiver(req, ds.cache),
		ds.cache,
		fmt.Sprintf("add-mshr-entry-0x%x-0x%x", mshrEntry.Address, block.Tag),
	)

	ds.activeBuf.Pop()

	trans.action = writeBufferFetch
	trans.fetchPID = pid
	trans.fetchAddress = cacheLineID
	if trans.write != nil {
		port := fmt.Sprintf("%s", ds.cache.addressToPortMapper.Find(trans.accessReq().GetAddress()))
		if strings.Contains(port, "RDMA") {
			trans.writeToHomeNode = true
		} else {
			trans.writeToHomeNode = false
		}
	}

	ds.cache.writeBufferFetchBuffer.Push(trans)

	mshrEntry.Block = block
	mshrEntry.Requests = append(mshrEntry.Requests, trans)

	return true
}

func (ds *directoryStage) isWritingFullLine(write *mem.WriteReq) bool {
	if len(write.Data) != (1 << ds.cache.log2BlockSize) {
		return false
	}

	if write.DirtyMask != nil {
		for _, dirty := range write.DirtyMask {
			if !dirty {
				return false
			}
		}
	}

	return true
}

func (ds *directoryStage) needEviction(victim *internal.Block) bool {
	return victim.IsValid && victim.IsDirty
}

func (ds *directoryStage) isMSHRAvailable(trans *transaction) bool {
	if ds.cache.mshr.IsFull() {
		return false
	}

	if trans.fromLocal {
		entries := ds.cache.mshr.AllEntries()
		count := 0
		for _, entry := range entries {
			if entry.Requests[0].(*transaction).fromLocal {
				count++
			}
		}

		if count >= ds.cache.maxLocalMshr {
			return false
		}
	}

	return true
}
