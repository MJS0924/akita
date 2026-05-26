package writebackcoh

import (
	"fmt"
	"strings"

	"github.com/sarchlab/akita/v4/mem/cache/writebackcoh/internal"
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

	// Pipeline 및 Buffer를 Local과 Remote로 완전 분리
	localPipeline  pipelining.Pipeline
	remotePipeline pipelining.Pipeline
	// Optional snoop-latency pipelines for InvReq. When the builder's
	// snoopLatency knob is 0 these are nil and invalidations share the
	// regular local/remote pipelines (historical behavior). When > 0
	// each is a (dirLatency + snoopLatency)-stage pipeline whose post
	// buffer is the same localBuf/remoteBuf — so the downstream commit
	// loop is oblivious to whether an item arrived via the regular or
	// the snoop path. The pipelining package keeps madeProgress=true
	// while items are in flight, which guarantees the cache stays
	// awake until any deferred inv reaches the commit point (no extra
	// queue or shared resource is introduced, hence no new HoL path).
	localSnoopPipeline  pipelining.Pipeline
	remoteSnoopPipeline pipelining.Pipeline
	// Phase F: dedicated InvReq pipeline + post-buffer. ingress is
	// cache.invStageBuffer (separate from dirStageBuffer/remoteDirStageBuffer
	// to avoid HoL blocking by stalled read/write at the queue head).
	// processTransaction drains invBuf BEFORE the regular local/remote
	// bufs every Tick.
	invPipeline pipelining.Pipeline
	localBuf    sim.Buffer
	remoteBuf   sim.Buffer
	invBuf      sim.Buffer

	activeBuf sim.Buffer // 현재 처리 중인 버퍼를 가리키는 내부 포인터

	lastReturnValue bool
	returnFalse0    string
	returnFalse1    string
	activeString    *string
}

func (ds *directoryStage) Tick() (madeProgress bool) {
	ds.returnFalse0 = ""
	ds.returnFalse1 = ""
	if ds.cache.state == cacheStatePreFlushing {
		return false
	}

	madeProgress = ds.acceptNewTransaction() || madeProgress

	madeProgress = ds.localPipeline.Tick() || madeProgress
	madeProgress = ds.remotePipeline.Tick() || madeProgress

	// Tick the optional snoop pipelines every cycle. Empty stages are
	// no-ops that return false, so this only contributes madeProgress
	// while a deferred inv is in flight — exactly the behavior needed
	// to keep the cache awake without spurious wakeups.
	if ds.localSnoopPipeline != nil {
		madeProgress = ds.localSnoopPipeline.Tick() || madeProgress
	}
	if ds.remoteSnoopPipeline != nil {
		madeProgress = ds.remoteSnoopPipeline.Tick() || madeProgress
	}

	// Phase F: dedicated InvReq pipeline. Same dirLatency stages so
	// inv pays tag-lookup time. Items emerge into invBuf which is
	// drained by processTransaction with priority over local/remote.
	madeProgress = ds.invPipeline.Tick() || madeProgress

	madeProgress = ds.processTransaction() || madeProgress

	ds.lastReturnValue = madeProgress

	return madeProgress
}

// invCostInSlots is how many numReqPerCycle slots a single
// invalidation commit consumes in the directory stage. Reads and
// writes consume 1 slot. Invalidations consume more to model the
// extra bank tag-lookup that a real L2 controller would perform on
// an InvReq. We keep invalidations inline (not routed through the
// bank pipeline) to avoid the shared-buffer/port deadlock that
// happens during bursty promotion/demotion-driven InvReq storms in
// SuperDirectory; the cost is captured here as additional commit
// budget instead.
const invCostInSlots = 2

func (ds *directoryStage) processTransaction() bool {
	madeProgress := false

	// Unified per-cycle budget across both pipelines. Each iteration
	// commits at most one transaction. Reads/writes cost 1 slot each;
	// invalidations cost invCostInSlots. The dispatcher peeks remote
	// first to enforce remote-priority arbitration (deadlock
	// avoidance). Loop ends when budget is exhausted, when nothing
	// can commit this cycle, or when both buffers are empty.
	totalBudget := ds.cache.numReqPerCycle
	used := 0
	for used < totalBudget {
		// Phase F: drain invBuf first (preempts read/write so that
		// stalled R/W head in localBuf/remoteBuf cannot block inv
		// processing → cache coherence response always proceeds).
		ds.activeString = &ds.returnFalse0
		ds.activeBuf = ds.invBuf
		ok, cost := ds.processOne()
		if ok {
			used += cost
			madeProgress = true
			continue
		}
		// Try remote.
		ds.activeString = &ds.returnFalse0
		ds.activeBuf = ds.remoteBuf
		ok, cost = ds.processOne()
		if ok {
			used += cost
			madeProgress = true
			continue
		}
		// Then local.
		ds.activeString = &ds.returnFalse1
		ds.activeBuf = ds.localBuf
		ok, cost = ds.processOne()
		if ok {
			used += cost
			madeProgress = true
			continue
		}
		break
	}

	return madeProgress
}

// processOne handles a single transaction from ds.activeBuf. Returns
// (success, cost) where cost is the number of numReqPerCycle slots the
// committed transaction consumed. Cost is meaningful only when
// success is true; on failure the caller treats it as zero.
func (ds *directoryStage) processOne() (bool, int) {
	item := ds.activeBuf.Peek()
	if item == nil {
		return false, 0
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

	// Invalidations bypass the evictingList check. Rationale: when
	// evictingList[cacheLineID] is set, the directory has already been
	// updated to the new tag (see updateVictimBlockMetaData) and the
	// old cacheline is in transit through bankStage. doInvalidation's
	// directory.Lookup returns nil for the old tag and acks the inv
	// without state mutation — safe.  Blocking inv here on
	// evictingList creates a deadlock cycle: writeBuffer full →
	// bankStage can't drain → evictingList lingers → inv stuck at
	// invBuf head → directory's inflightInvToBottom hits cap →
	// directory backpressures L2 evictions → goto start.
	if trans.invalidation != nil {
		return ds.doInvalidation(trans), invCostInSlots
	}

	if _, evicting := ds.cache.evictingList[cacheLineID]; evicting {
		return false, 0
	}

	if trans.read != nil {
		return ds.doRead(trans), 1
	}
	if trans.write != nil {
		return ds.doWrite(trans), 1
	}
	if trans.prefetch != nil {
		return ds.doPrefetch(trans), 1
	}
	panic("unknown transaction type in processOne")
}

// processSpecificBuffer is kept for backward compatibility with any
// remaining callers but is now unused by processTransaction (which
// uses the unified-budget loop above). Left in place to avoid
// breaking other pieces of the codebase that may still reference it.
func (ds *directoryStage) processSpecificBuffer(targetBuf sim.Buffer) bool {
	madeProgress := false
	ds.activeBuf = targetBuf // 현재 처리 중인 버퍼 설정

	for i := 0; i < ds.cache.numReqPerCycle; i++ {
		item := ds.activeBuf.Peek()
		if item == nil {
			break
		}

		trans := item.(dirPipelineItem).trans
		if ds.cache.debugProcess && trans.accessReq() != nil && trans.accessReq().GetAddress() == ds.cache.debugAddress0 {
			fmt.Printf("[%s] [directoryStage]\tReceived req - 1: addr %x, action %d\n", ds.cache.name, trans.accessReq().GetAddress(), trans.action)
		}
		if ds.cache.debugProcess && trans.accessReq() != nil && trans.accessReq().GetAddress() == ds.cache.debugAddress1 {
			fmt.Printf("[%s] [directoryStage]\tReceived req - 1: addr %x, action %d\n", ds.cache.name, trans.accessReq().GetAddress(), trans.action)
		}

		addr := uint64(0)
		if trans.invalidation != nil {
			addr = trans.invalidation.Address
		} else if trans.accessReq() != nil {
			addr = trans.accessReq().GetAddress()
		} else {
			addr = trans.fetchAddress
		}

		cacheLineID, _ := getCacheLineID(addr, ds.cache.log2BlockSize)

		// Invalidations bypass the evictingList check (cycle-breaker;
		// see processOne for rationale).
		if trans.invalidation != nil {
			madeProgress = ds.doInvalidation(trans) || madeProgress
			continue
		}

		if _, evicting := ds.cache.evictingList[cacheLineID]; evicting {
			break
		}

		if trans.read != nil {
			ret := ds.doRead(trans)
			madeProgress = ret || madeProgress
			if !ret && ds.cache.debugProcess && ds.cache.debugAddress0 == addr {
				fmt.Printf("[%s]\tReturnFalse: %s\n", ds.cache.name, *ds.activeString)
			} else if !ret && ds.cache.debugProcess && ds.cache.debugAddress1 == addr {
				fmt.Printf("[%s]\tReturnFalse: %s\n", ds.cache.name, *ds.activeString)
			}
			continue
		}

		if trans.write != nil {
			madeProgress = ds.doWrite(trans) || madeProgress
			continue
		}

		if trans.prefetch != nil {
			madeProgress = ds.doPrefetch(trans) || madeProgress
			continue
		}

		panic("What???")
	}

	return madeProgress
}

func (ds *directoryStage) acceptNewTransaction() bool {
	madeProgress := false

	// [Phase F] 0순위: InvReq 전용 ingress (invStageBuffer) → invPipeline.
	// 분리된 큐를 두어 head-of-line blocking 제거. 정규 큐의 read/write가
	// stall되어도 inv는 자기 큐에서 정상 진행.
	for i := 0; i < ds.cache.numReqPerCycle; i++ {
		item := ds.cache.invStageBuffer.Peek()
		if item == nil {
			break
		}
		if !ds.invPipeline.CanAccept() {
			break
		}
		trans := item.(*transaction)
		ds.invPipeline.Accept(dirPipelineItem{trans})
		ds.cache.invStageBuffer.Pop()
		madeProgress = true
	}

	// [이식 완료] 1순위: 외부(Remote) 요청 최우선 파이프라인 진입.
	// InvReq는 snoopLatency>0인 경우 remoteSnoopPipeline으로 우회.
	// Snoop pipeline이 가득 차면 break — 이 경우 같은 head를 다음
	// 사이클에 다시 시도하므로 deadlock-free (기존 break 패턴과 동일).
	for i := 0; i < ds.cache.numReqPerCycle; i++ {
		item := ds.cache.remoteDirStageBuffer.Peek()
		if item == nil {
			break
		}

		trans := item.(*transaction)

		targetPipeline := ds.remotePipeline
		if trans.invalidation != nil && ds.remoteSnoopPipeline != nil {
			targetPipeline = ds.remoteSnoopPipeline
		}

		if !targetPipeline.CanAccept() {
			break
		}

		targetPipeline.Accept(dirPipelineItem{trans})
		ds.cache.remoteDirStageBuffer.Pop()
		madeProgress = true
	}

	// [이식 완료] 2순위: 내부(Local) 요청 파이프라인 진입.
	for i := 0; i < ds.cache.numReqPerCycle; i++ {
		item := ds.cache.dirStageBuffer.Peek()
		if item == nil {
			break
		}

		trans := item.(*transaction)

		targetPipeline := ds.localPipeline
		if trans.invalidation != nil && ds.localSnoopPipeline != nil {
			targetPipeline = ds.localSnoopPipeline
		}

		if !targetPipeline.CanAccept() {
			break
		}

		targetPipeline.Accept(dirPipelineItem{trans})
		ds.cache.dirStageBuffer.Pop()
		madeProgress = true
	}

	return madeProgress
}

func (ds *directoryStage) Reset() {
	ds.localPipeline.Clear()
	ds.remotePipeline.Clear()
	if ds.localSnoopPipeline != nil {
		ds.localSnoopPipeline.Clear()
	}
	if ds.remoteSnoopPipeline != nil {
		ds.remoteSnoopPipeline.Clear()
	}
	ds.invPipeline.Clear()
	ds.localBuf.Clear()
	ds.remoteBuf.Clear()
	ds.invBuf.Clear()
	ds.cache.dirStageBuffer.Clear()
	ds.cache.remoteDirStageBuffer.Clear() // Comp 구조체에 remoteDirStageBuffer 추가 필요
	ds.cache.invStageBuffer.Clear()
}

func (ds *directoryStage) doPrefetch(trans *transaction) bool {
	// prefetch: writeFullLine처럼 처리
	// i. read count != 0 -> discard
	// ii. isLocked -> discard
	// iii. hit -> discard

	*ds.activeString = "[doPrefetch] "

	addr := trans.fetchAddress
	pid := trans.fetchPID
	cachelineID, _ := getCacheLineID(addr, ds.cache.log2BlockSize)

	// fmt.Printf("[%s]\t[doPrefetch]\t[Lookup]\t%x -> %x\n", ds.cache.name, addr, cachelineID)
	block := ds.cache.directory.Lookup(pid, cachelineID)
	victim := &internal.Block{}

	if block != nil {
		ds.activeBuf.Pop()
		*ds.activeString = *ds.activeString + "block is hit"

		ds.cache.incEvent("PrefetchDiscard - Hit")
		tracing.TraceReqFinalize(trans.prefetch, ds.cache)

		return true

	} else {
		victim = ds.cache.directory.FindVictim(cachelineID)

		if victim.IsLocked || victim.ReadCount > 0 {
			*ds.activeString = *ds.activeString + fmt.Sprintf("Victim %x is being used", victim.Tag)
			if ds.cache.debugProcess {
				fmt.Printf("[%s]\t%s\n", ds.cache.name, *ds.activeString)
			}
			trans.returnFalse = *ds.activeString
			return false
		}

		if ds.needEviction(victim) {
			return ds.evictAndPrefetch(trans, victim)
		}

		return ds.prefetchToBank(trans, victim)
	}
}

// [FIX #2] dirStage push 시 fromLocal 기준으로 Local/Remote 버퍼 분기.
// bankStage.pullFromBuf 가 Remote 먼저 drain 하므로, cross-GPU 데드락의 닫힘
// 고리(remote 요청이 local 가득찬 bankBuf 뒤에 밀려서 처리 못 됨)를 차단.
func (ds *directoryStage) bankBufFor(trans *transaction, bank int) sim.Buffer {
	if trans.fromLocal {
		return ds.cache.dirToBankBuffersLocal[bank]
	}
	return ds.cache.dirToBankBuffersRemote[bank]
}

// [FIX #3] writeBuffer 가 임계 이상 차 있을 때 fromLocal=true 트랜잭션의
// evict() 진입을 막아 자기 GPU L1 으로 backpressure. remote 트랜잭션은
// 무조건 통과시켜 cross-GPU 응답 경로를 살림. capacity 의 7/8(87.5%)을
// 임계로 사용 — soft cap 보다 더 일찍 막아 양쪽 GPU 가 동시에 wB cap 을
// 점유할 수 없게 함.
func (ds *directoryStage) writeBufferReservedForRemote(trans *transaction) bool {
	if !trans.fromLocal {
		return false
	}
	wb := ds.cache.writeBuffer
	used := len(wb.pendingEvictions) + len(wb.inflightEviction)
	return used >= wb.writeBufferCapacity*7/8
}

func (ds *directoryStage) prefetchToBank(trans *transaction, block *internal.Block) bool {
	*ds.activeString = *ds.activeString + "[prefetchToBank] "

	numBanks := len(ds.cache.dirToBankBuffers)
	bank := bankID(block, ds.cache.directory.WayAssociativity(), numBanks)
	bankBuf := ds.bankBufFor(trans, bank)

	if !bankBuf.CanPush() {
		*ds.activeString = *ds.activeString + "Cannot push to bankBuf "
		trans.returnFalse = *ds.activeString
		// fmt.Printf("[%s]\t%s\n", ds.cache.name, ds.returnFalse)

		return false
	}

	addr := trans.fetchAddress
	pid := trans.fetchPID
	cachelineID, _ := getCacheLineID(addr, ds.cache.log2BlockSize)
	mshrEntry := ds.cache.mshr.Query(pid, addr)

	ds.cache.directory.Visit(block)
	block.IsLocked = true
	block.IsValid = true
	block.IsDirty = false
	block.PID = pid
	block.Tag = cachelineID
	trans.block = block
	trans.action = bankWritePrefetched
	trans.mshrEntry = mshrEntry

	ds.activeBuf.Pop()
	bankBuf.Push(trans)

	if trans.prefetch == nil {
		panic("Impossible")
	}

	return true
}

func (ds *directoryStage) evictAndPrefetch(trans *transaction, victim *internal.Block) bool {
	*ds.activeString = *ds.activeString + "[evictAndPrefetch] "

	if ds.writeBufferReservedForRemote(trans) {
		*ds.activeString = *ds.activeString + "wB cap reserved for remote"
		trans.returnFalse = *ds.activeString
		return false
	}

	bankNum := bankID(victim,
		ds.cache.directory.WayAssociativity(), len(ds.cache.dirToBankBuffers))
	bankBuf := ds.bankBufFor(trans, bankNum)

	if !bankBuf.CanPush() {
		*ds.activeString = *ds.activeString + "Cannot push to bankBuf "
		// fmt.Printf("[%s]\t%s\n", ds.cache.name, ds.returnFalse)
		return false
	}

	if victim.IsLocked || victim.ReadCount > 0 {
		// Option 1. wait
		*ds.activeString = *ds.activeString + "Victim is being used, wait"
		trans.returnFalse = *ds.activeString
		return false

		// Option 2. discard
		// ds.activeBuf.Pop()

		// tracing.AddTaskStep(
		// 	trans.prefetch.ID,
		// 	ds.cache,
		// 	"PrefetchDiscard - Busy",
		// )
		// tracing.TraceReqFinalize(trans.prefetch, ds.cache)

		// return true
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
	trans.block.Tag = trans.fetchAddress
	trans.block.PID = trans.fetchPID
	trans.block.IsLocked = true
	trans.block.IsDirty = false
	trans.block.IsValid = true
	ds.cache.directory.Visit(victim)

	ds.activeBuf.Pop()
	bankBuf.Push(trans)

	ds.cache.evictingList[trans.evictingAddr] = true

	return true
}

func (ds *directoryStage) doInvalidation(trans *transaction) bool {
	*ds.activeString = "[doInvalidation] "
	req := trans.invalidation

	cachelineID, _ := getCacheLineID(req.Address, ds.cache.log2BlockSize)
	block := ds.cache.directory.Lookup(req.PID, cachelineID)
	mshrEntry := ds.cache.mshr.Query(req.PID, cachelineID)

	suffix := "-Evict"
	if req.IsWriteInv {
		suffix = "-Write"
	}
	what := "InvalidateInvalidBlock" + suffix
	var pid vm.PID
	var vAddr uint64
	accessedCount := 0
	hasBeenRead := false
	hasBeenWritten := false

	// deferred: when true, the InvRsp is sent now (to free CohDir's
	// inflightInvToBottom slot) but the block zero-out is deferred to
	// bankStage finalize, which clears IsLocked under the in-flight bank
	// op's natural completion path. Breaks the lenet_CD_8 / relu_CD_0
	// class of deadlock where doInvalidation returning false on
	// IsLocked blocks closes a cross-GPU cycle (L2.invStageBuffer →
	// CohDir.RemoteBottomPort.OutgoingBuf → CohDir.inflightInvToBottom
	// cap → CohDir.RDMAInvPort.IncomingBuf → upstream CohDir jam).
	deferred := false

	if mshrEntry == nil && block != nil {
		if block.IsValid && block.ReadCount > 0 {
			// Active readers — cannot defer-invalidate without
			// dropping an in-flight read. Keep blocking semantics for
			// this case. The observed deadlock is the IsLocked path,
			// not this one.
			*ds.activeString += fmt.Sprintf(
				"Block %x has active readers, readCnt %d",
				block.Tag, block.ReadCount)
			trans.returnFalse = *ds.activeString
			return false
		}
		if block.IsValid && block.IsLocked {
			// Cycle-breaker: arm deferred invalidation. InvRsp is
			// still sent below; bank finalize will zero the block
			// when IsLocked clears.
			deferred = true
		}
		if block.IsValid {
			what = "InvalidateValidBlock" + suffix
			reason := evictReasonInvEvict
			if req.IsWriteInv {
				reason = evictReasonInvWrite
			}
			ds.cache.recordEviction(block.PID, block.Tag, reason)
		}
		pid = block.PID
		vAddr = block.VAddr
		if block.Accessed {
			accessedCount++
		}
		hasBeenRead = block.HasBeenRead
		hasBeenWritten = block.HasBeenWritten
	}

	// Latent coherence bug fix: previously the mshrEntry != nil branch
	// sent InvRsp but never zeroed the block (the zero-block below was
	// gated by mshrEntry == nil). When fetch completed the block went
	// IsValid=true while CohDir believed we held no copy. Arm deferred
	// invalidation so bank finalize zeros the block at fetch-complete
	// time.
	if mshrEntry != nil && block != nil {
		deferred = true
	}

	// InvRsp는 InvReq가 도착한 port의 짝으로 돌려보내야 함. InvReq는
	// 항상 remote path (directory의 remoteBottomPort → L2의 remoteTopPort)로
	// 들어오므로 invBuf에서 처리되는 inv는 반드시 remoteTopPort로 응답.
	// 기존 remoteBuf check는 read/write 응답 분기와 호환 위해 유지.
	port := ds.cache.topPort
	if ds.activeBuf == ds.remoteBuf || ds.activeBuf == ds.invBuf {
		port = ds.cache.remoteTopPort
	}
	if !port.CanSend() {
		*ds.activeString += "Cannot send to topPort"
		trans.returnFalse = *ds.activeString
		return false
	}

	rsp := mem.InvRspBuilder{}.
		WithSrc(port.AsRemote()).
		WithDst(req.Src).
		WithRspTo(req.ReqFrom).
		Build()
	if accessedCount > 0 {
		rsp.Accessed = 1
	}
	if err := port.Send(rsp); err != nil {
		*ds.activeString += "Failed to send to topPort"
		trans.returnFalse = *ds.activeString
		return false
	}

	if block != nil {
		if deferred {
			// Defer the zero-out to bankStage finalize. InvRsp has been
			// sent above so CohDir's inflight-inv slot is already freed.
			block.PendingInvalidation = true
			ds.cache.deferredInvArmed++
			ds.cache.incEvent("DeferredInvArmed")
		} else if mshrEntry == nil {
			newBlk := &internal.Block{
				WayID:        block.WayID,
				SetID:        block.SetID,
				CacheAddress: block.CacheAddress,
			}
			*block = *newBlk
			ds.cache.eraseCacheLineFromRWMask(pid, vAddr)
		}
	}

	ds.activeBuf.Pop()
	ds.cache.mshrStage.removeTransaction(trans)

	if mshrEntry == nil && block != nil {
		ds.cache.incEvent(fmt.Sprintf("Usage: %d/%d",
			accessedCount, 1<<(ds.cache.log2BlockSize-6)))
		ds.cache.incEvent(fmt.Sprintf("RW: %t/%t",
			hasBeenRead, hasBeenWritten))
	}
	ds.cache.incEvent(what)
	tracing.TraceReqComplete(req, ds.cache)

	ds.cache.InvokeHook(sim.HookCtx{
		Domain: ds.cache,
		Pos:    HookPosL2Invalidation,
		Detail: L2InvalidationDetail{IsWriteInv: req.IsWriteInv, Addr: cachelineID},
	})

	return true
}

func (ds *directoryStage) doRead(trans *transaction) bool {
	*ds.activeString = "[doRead] "

	cachelineID, _ := getCacheLineID(
		trans.read.Address, ds.cache.log2BlockSize)

	mshrEntry := ds.cache.mshr.Query(trans.read.PID, cachelineID)
	if mshrEntry != nil {
		return ds.handleReadMSHRHit(trans, mshrEntry)
	}

	block := ds.cache.directory.Lookup(
		trans.read.PID, cachelineID)
	if block != nil {
		ok := ds.handleReadHit(trans, block)
		if ok {
			ds.cache.InvokeHook(sim.HookCtx{
				Domain: ds.cache,
				Pos:    HookPosL2Access,
				Detail: L2AccessDetail{Hit: true, Addr: cachelineID},
			})
		}
		return ok
	}

	ok := ds.handleReadMiss(trans)
	if ok {
		ds.cache.InvokeHook(sim.HookCtx{
			Domain: ds.cache,
			Pos:    HookPosL2Access,
			Detail: L2AccessDetail{Hit: false, Addr: cachelineID},
		})
	}
	return ok
}

func (ds *directoryStage) handleReadMSHRHit(
	trans *transaction,
	mshrEntry *internal.MSHREntry,
) bool {
	if ds.cache.debugProcess && trans.read != nil && trans.read.Address == ds.cache.debugAddress0 {
		fmt.Printf("[%s] [directoryStage]\tReceived read req - 1.2: addr %x, action %d\n", ds.cache.name, trans.read.Address, trans.action)
	}
	if ds.cache.debugProcess && trans.read != nil && trans.read.Address == ds.cache.debugAddress1 {
		fmt.Printf("[%s] [directoryStage]\tReceived read req - 1.2: addr %x, action %d\n", ds.cache.name, trans.read.Address, trans.action)
	}
	trans.mshrEntry = mshrEntry
	mshrEntry.Requests = append(mshrEntry.Requests, trans)

	ds.activeBuf.Pop()

	ds.cache.incEvent("read-mshr-hit")
	if !trans.toLocal {
		ds.cache.incEvent("remote-read-mshr-hit")
	}

	return true
}

func (ds *directoryStage) handleReadHit(
	trans *transaction,
	block *internal.Block,
) bool {
	*ds.activeString = *ds.activeString + "[handleReadHit] "

	if block.IsLocked {
		*ds.activeString = *ds.activeString + fmt.Sprintf("Block %x is locked", block.Tag)
		trans.returnFalse = *ds.activeString

		return false
	}

	// log.Printf("%.10f, %s, dir read hit， %s, %04X, %04X, (%d, %d), %v\n",
	// 	now, ds.cache.Name(),
	// 	trans.read.ID,
	// 	trans.read.Address,
	// 	(trans.read.GetAddress()>>ds.cache.log2BlockSize)
	// 	<<ds.cache.log2BlockSize,
	// 	block.SetID, block.WayID,
	// 	nil,
	// )

	progress := ds.readFromBank(trans, block)
	if progress {
		ds.cache.incEvent("read-hit")
		if !trans.toLocal {
			ds.cache.incEvent("remote-read-hit")
		}
	}

	return progress
}

func (ds *directoryStage) handleReadMiss(trans *transaction) bool {
	*ds.activeString = *ds.activeString + "[handleReadMiss] "

	req := trans.read
	cacheLineID, _ := getCacheLineID(req.Address, ds.cache.log2BlockSize)

	// if ds.cache.mshr.IsFull() {
	if !ds.isMSHRAvailable(trans) {
		*ds.activeString = *ds.activeString + "MSHR is full"
		trans.returnFalse = *ds.activeString
		return false
	}

	victim := ds.cache.directory.FindVictim(cacheLineID)
	if victim.IsLocked || victim.ReadCount > 0 {
		*ds.activeString = *ds.activeString + fmt.Sprintf("Victim %x is being used", victim.Tag)
		trans.returnFalse = *ds.activeString

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
			ds.cache.incEvent("read-miss")
			if !trans.toLocal {
				ds.cache.incEvent("remote-read-miss")
			}
			ds.cache.incEvent(what)
			ds.emitMissReason(trans, cacheLineID)
		}

		return ok
	}

	ok := ds.fetch(trans, victim)
	if ok {
		ds.cache.incEvent("read-miss")
		if !trans.toLocal {
			ds.cache.incEvent("remote-read-miss")
		}
		ds.cache.incEvent(what)
		ds.emitMissReason(trans, cacheLineID)
	}

	return ok
}

// emitMissReason classifies the read miss at (pid, cacheLineID) as cold /
// capacity / coh-write / coh-evict / other and emits one (or two) trace
// steps so the metric can be aggregated like read-miss/remote-read-miss.
//
// Names emitted:
//   read-miss-<reason>
//   remote-read-miss-<reason>   (only when trans.toLocal == false)
func (ds *directoryStage) emitMissReason(trans *transaction, cacheLineID uint64) {
	reason := ds.cache.classifyAndRecordReadMiss(trans.read.PID, cacheLineID)
	ds.cache.incEvent("read-miss-" + reason)
	if !trans.toLocal {
		ds.cache.incEvent("remote-read-miss-" + reason)
	}
}

func (ds *directoryStage) doWrite(trans *transaction) bool {
	*ds.activeString = "[doWrite] "

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
			ds.cache.incEvent("write-mshr-hit")
			if !trans.toLocal {
				ds.cache.incEvent("remote-write-mshr-hit")
			}
		}

		return ok
	}

	block := ds.cache.directory.Lookup(trans.write.PID, cachelineID)
	if block != nil {
		ok := ds.doWriteHit(trans, block)
		if ok {
			ds.cache.incEvent("write-hit")
			if !trans.toLocal {
				ds.cache.incEvent("remote-write-hit")
			}
			ds.cache.InvokeHook(sim.HookCtx{
				Domain: ds.cache,
				Pos:    HookPosL2Access,
				Detail: L2AccessDetail{Hit: true, Addr: cachelineID},
			})
		}

		return ok
	}

	ok := ds.doWriteMiss(trans)
	if ok {
		ds.cache.incEvent("write-miss")
		if !trans.toLocal {
			ds.cache.incEvent("remote-write-miss")
		}
		ds.cache.InvokeHook(sim.HookCtx{
			Domain: ds.cache,
			Pos:    HookPosL2Access,
			Detail: L2AccessDetail{Hit: false, Addr: cachelineID},
		})
	}

	return ok
}

func (ds *directoryStage) doWriteMSHRHit(
	trans *transaction,
	mshrEntry *internal.MSHREntry,
) bool {
	if ds.cache.debugProcess && trans.write != nil && trans.write.Address == ds.cache.debugAddress0 {
		fmt.Printf("[%s] [directoryStage]\tReceived write req - 1.3: addr %x, action %d\n", ds.cache.name, trans.write.Address, trans.action)
	}
	if ds.cache.debugProcess && trans.write != nil && trans.write.Address == ds.cache.debugAddress1 {
		fmt.Printf("[%s] [directoryStage]\tReceived write req - 1.3: addr %x, action %d\n", ds.cache.name, trans.write.Address, trans.action)
	}

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
	*ds.activeString = *ds.activeString + "[doWriteHit] "

	if block.IsLocked || block.ReadCount > 0 {
		*ds.activeString = *ds.activeString + fmt.Sprintf("Block %x is being used", block.Tag)
		trans.returnFalse = *ds.activeString

		return false
	}

	return ds.writeToBank(trans, block)
}

func (ds *directoryStage) doWriteMiss(trans *transaction) bool {
	*ds.activeString = *ds.activeString + "[doWriteMiss] "

	write := trans.write

	if ds.isWritingFullLine(write) {
		return ds.writeFullLineMiss(trans)
	}

	return ds.writePartialLineMiss(trans)
}

func (ds *directoryStage) writeFullLineMiss(trans *transaction) bool {
	*ds.activeString = *ds.activeString + "[writeFullLineMiss] "

	write := trans.write
	cachelineID, _ := getCacheLineID(write.Address, ds.cache.log2BlockSize)

	victim := ds.cache.directory.FindVictim(cachelineID)
	if victim.IsLocked || victim.ReadCount > 0 {
		*ds.activeString = *ds.activeString + fmt.Sprintf("Victim %x is being used", victim.Tag)
		trans.returnFalse = *ds.activeString

		return false
	}

	what := ""
	if victim.IsValid {
		what = "EvictValidBlock"
	} else {
		what = "EvictInvalidBlock"
	}

	if ds.needEviction(victim) {
		progress := ds.evict(trans, victim)
		if progress {
			ds.cache.incEvent(what)
		}

		return progress
	}

	progress := ds.writeToBank(trans, victim)
	if progress {
		ds.cache.incEvent(what)
	}

	return progress
}

func (ds *directoryStage) writePartialLineMiss(trans *transaction) bool {
	*ds.activeString = *ds.activeString + "[writePartialLineMiss] "

	write := trans.write
	cachelineID, _ := getCacheLineID(write.Address, ds.cache.log2BlockSize)

	// if ds.cache.mshr.IsFull() {
	if !ds.isMSHRAvailable(trans) {
		*ds.activeString = *ds.activeString + "MSHR is full"
		trans.returnFalse = *ds.activeString

		return false
	}

	victim := ds.cache.directory.FindVictim(cachelineID)
	if victim.IsLocked || victim.ReadCount > 0 {
		*ds.activeString = *ds.activeString + fmt.Sprintf("Victim %x is being used", victim.Tag)

		return false
	}

	// log.Printf("%.10f, %s, write partial line ，"+
	// " %s, %04X, %04X, (%d, %d), %v\n",
	// 	now, ds.cache.Name(),
	// 	trans.write.ID,
	// 	trans.write.Address, cachelineID,
	// 	victim.SetID, victim.WayID,
	// 	write.Data,
	// )
	what := ""
	if victim.IsValid {
		what = "EvictValidBlock"
	} else {
		what = "EvictInvalidBlock"
	}

	if ds.needEviction(victim) {
		progress := ds.evict(trans, victim)
		if progress {
			ds.cache.incEvent(what)
		}

		return progress
	}

	progress := ds.fetch(trans, victim)
	if progress {
		ds.cache.incEvent(what)
	}

	return progress
}

func (ds *directoryStage) readFromBank(
	trans *transaction,
	block *internal.Block,
) bool {
	*ds.activeString = *ds.activeString + "[readFromBank] "
	numBanks := len(ds.cache.dirToBankBuffers)
	bank := bankID(block, ds.cache.directory.WayAssociativity(), numBanks)
	bankBuf := ds.bankBufFor(trans, bank)

	if !bankBuf.CanPush() {
		*ds.activeString = *ds.activeString + "Cannot push to bankBuf"
		trans.returnFalse = *ds.activeString

		return false
	}

	ds.cache.directory.Visit(block)

	block.ReadCount++
	block.Accessed = true
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
	*ds.activeString = *ds.activeString + "[writeToBank] "

	numBanks := len(ds.cache.dirToBankBuffers)
	bank := bankID(block, ds.cache.directory.WayAssociativity(), numBanks)
	bankBuf := ds.bankBufFor(trans, bank)

	if !bankBuf.CanPush() {
		*ds.activeString = *ds.activeString + "Cannot push to bankBuf "
		trans.returnFalse = *ds.activeString

		return false
	}

	addr := trans.write.Address
	cachelineID, _ := getCacheLineID(addr, ds.cache.log2BlockSize)

	// Method D: writeToBank repurposes a victim slot. If the victim
	// previously held a valid line that doesn't match the new key, it
	// is being silently overwritten (the dirty-eviction case goes
	// through evict() instead, which records on its own path). Stamp
	// the LRU reason here for the silent-overwrite case only.
	if block.IsValid && (block.Tag != cachelineID || block.PID != trans.write.PID) {
		ds.cache.recordEviction(block.PID, block.Tag, evictReasonLRU)
	}

	ds.cache.directory.Visit(block)
	block.IsLocked = true
	block.Tag = cachelineID
	block.IsValid = true
	block.PID = trans.write.PID
	block.VAddr = trans.accessReq().GetVAddr()
	block.Accessed = true
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

	if ds.cache.debugProcess && trans.write != nil && trans.write.Address == ds.cache.debugAddress0 {
		fmt.Printf("[%s] [directoryStage]\tReceived write req - 1.4: addr %x, action %d\n", ds.cache.name, trans.write.Address, trans.action)
	}
	if ds.cache.debugProcess && trans.write != nil && trans.write.Address == ds.cache.debugAddress1 {
		fmt.Printf("[%s] [directoryStage]\tReceived write req - 1.4: addr %x, action %d\n", ds.cache.name, trans.write.Address, trans.action)
	}

	return true
}

func (ds *directoryStage) evict(
	trans *transaction,
	victim *internal.Block,
) bool {
	*ds.activeString = *ds.activeString + "[evict] "

	if ds.writeBufferReservedForRemote(trans) {
		*ds.activeString = *ds.activeString + "wB cap reserved for remote"
		trans.returnFalse = *ds.activeString
		return false
	}

	bankNum := bankID(victim,
		ds.cache.directory.WayAssociativity(), len(ds.cache.dirToBankBuffers))
	bankBuf := ds.bankBufFor(trans, bankNum)

	if !bankBuf.CanPush() {
		*ds.activeString = *ds.activeString + "Cannot push to bankBuf "
		trans.returnFalse = *ds.activeString

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
	if victim.Accessed {
		count++
		// cacheline의 사용량 조사
	}

	// Method D: stamp the victim's pre-overwrite identity as LRU-evicted so
	// a future re-fetch of the same line can be classified as a capacity
	// miss. Must run BEFORE updateVictimBlockMetaData (which overwrites
	// victim.Tag with cacheLineID).
	if victim.IsValid {
		ds.cache.recordEviction(victim.PID, victim.Tag, evictReasonLRU)
	}

	ds.updateTransForEviction(trans, victim, pid, cacheLineID)
	ds.updateVictimBlockMetaData(victim, cacheLineID, pid)

	ds.activeBuf.Pop()
	bankBuf.Push(trans)

	ds.cache.evictingList[trans.victim.Tag] = true

	// log.Printf("%.10f, %s, directory evict ， %s, %04X, %04X, (%d, %d), %v\n",
	// 	now, ds.cache.Name(),
	// 	trans.accessReq().Meta().ID,
	// 	trans.accessReq().GetAddress(), trans.victim.Tag,
	// 	victim.SetID, victim.WayID,
	// 	nil,
	// )

	// ds.cache.printRWMask(victim.PID, victim.VAddr)
	ds.cache.eraseCacheLineFromRWMask(victim.PID, victim.VAddr)

	ds.cache.incEvent(fmt.Sprintf("Usage: %d/%d", count, 1<<(ds.cache.log2BlockSize-6)))
	ds.cache.incEvent(fmt.Sprintf("RW: %t/%t", hasBeenRead, hasBeenWritten))

	return true
}

func (ds *directoryStage) updateVictimBlockMetaData(
	victim *internal.Block,
	cacheLineID uint64,
	pid vm.PID,
) {
	victim.Tag = cacheLineID
	victim.PID = pid
	victim.IsLocked = true
	victim.IsDirty = false
	victim.Accessed = false
	victim.HasBeenRead = false
	victim.HasBeenWritten = false
	ds.cache.directory.Visit(victim)
}

func (ds *directoryStage) updateTransForEviction(
	trans *transaction,
	victim *internal.Block,
	pid vm.PID,
	cacheLineID uint64,
) {
	var copiedDirtyMask []bool
	if victim.DirtyMask != nil {
		copiedDirtyMask = make([]bool, len(victim.DirtyMask))
		copy(copiedDirtyMask, victim.DirtyMask)
	}

	trans.action = bankEvictAndFetch
	trans.victim = &internal.Block{
		PID:          victim.PID,
		Tag:          victim.Tag,
		CacheAddress: victim.CacheAddress,
		DirtyMask:    copiedDirtyMask,
	}
	trans.block = victim
	trans.evictingPID = trans.victim.PID
	trans.evictingAddr = trans.victim.Tag
	trans.evictingDirtyMask = copiedDirtyMask

	if ds.evictionNeedFetch(trans) {
		mshrEntry := ds.cache.mshr.Add(pid, cacheLineID)
		mshrEntry.Block = victim
		mshrEntry.Requests = append(mshrEntry.Requests, trans)
		trans.mshrEntry = mshrEntry
		trans.fetchPID = pid
		trans.fetchAddress = cacheLineID
		trans.action = bankEvictAndFetch
		if trans.fromLocal {
			ds.cache.mshrLocalAdded++
		} else {
			ds.cache.mshrRemoteAdded++
		}
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
	*ds.activeString = *ds.activeString + "[fetch] "

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

	// [FIX: head-of-line blocking v2] writeBufferFetch 항목을 writeBufferBuffer(eviction 전용)와
	// 분리된 writeBufferFetchBuffer로 직접 push한다.
	// writeBufferBuffer와 writeBufferFetchBuffer를 분리함으로써:
	//   - tooManyInflightFetches()=true 시 writeBufferFetchBuffer의 HEAD만 블로킹되고,
	//   - writeBufferBuffer(eviction)는 독립적으로 drain 가능 → finalizeBankEviction 진행 가능
	//   - downwardInflightTransCount 감소 → dirToBankBuffers drain → WriteDoneRsp 발송
	//   - 순환 의존성 해제.
	if !ds.cache.writeBufferFetchBuffer.CanPush() {
		*ds.activeString = *ds.activeString + "Cannot push to writeBufferFetchBuffer"
		trans.returnFalse = *ds.activeString

		return false
	}

	// Method D: if the slot held a valid (clean) line, this fetch
	// silently evicts it (needEviction=false skips evict()). Record the
	// LRU eviction so a future re-fetch is classified as capacity.
	if block.IsValid {
		ds.cache.recordEviction(block.PID, block.Tag, evictReasonLRU)
	}

	mshrEntry := ds.cache.mshr.Add(pid, cacheLineID)
	if trans.fromLocal {
		ds.cache.mshrLocalAdded++
	} else {
		ds.cache.mshrRemoteAdded++
	}
	trans.mshrEntry = mshrEntry
	trans.block = block
	block.IsLocked = true
	block.Tag = cacheLineID
	block.PID = pid
	block.IsValid = true
	block.VAddr = va
	block.Accessed = false       // fetch 이후 아직 사용하진 않은
	block.HasBeenRead = false    // fetch 이후 아직 사용하진 않은
	block.HasBeenWritten = false // fetch 이후 아직 사용하진 않은
	ds.cache.directory.Visit(block)

	ds.cache.incEvent(fmt.Sprintf("add-mshr-entry-0x%x-0x%x", mshrEntry.Address, block.Tag))

	ds.cache.InvokeHook(sim.HookCtx{
		Domain: ds.cache,
		Pos:    HookPosRegionFetch,
		Detail: RegionFetchDetail{
			RegionTag:       cacheLineID,
			RegionSizeBytes: uint64(1) << ds.cache.log2BlockSize,
		},
	})

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
	mshrEntry.Requests = append(mshrEntry.Requests, trans) // mshrEntry의 1st entry는 항상 1st request

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
		ds.cache.stallMSHRTotalFull++
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
			ds.cache.stallMSHRLocalCap++
			return false
		}
	}

	return true
}
