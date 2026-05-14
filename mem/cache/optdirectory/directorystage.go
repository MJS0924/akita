package optdirectory

import (
	"fmt"
	"strings"

	"github.com/sarchlab/akita/v4/mem/cache/optdirectory/internal"
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

	// [파이프라인 및 버퍼 분리]
	localPipeline  pipelining.Pipeline
	remotePipeline pipelining.Pipeline
	localBuf       sim.Buffer
	remoteBuf      sim.Buffer

	returnFalse0 string
	returnFalse1 string
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
	var targetBuf sim.Buffer
	var targetPipeline pipelining.Pipeline

	if fromLocal {
		targetBuf = ds.cache.localDirStageBuffer
		targetPipeline = ds.localPipeline
	} else {
		targetBuf = ds.cache.remoteDirStageBuffer
		targetPipeline = ds.remotePipeline
	}

	for i := 0; i < ds.cache.numReqPerCycle; i++ {
		if !targetPipeline.CanAccept() {
			break
		}
		item := targetBuf.Peek()
		if item == nil {
			break
		}

		trans := item.(*transaction)
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
	if !isLocal {
		buf = ds.remoteBuf
	}

	for i := 0; i < ds.cache.numReqPerCycle; i++ {
		item := buf.Peek()
		if item == nil {
			break
		}

		trans := item.(dirPipelineItem).trans

		addr := trans.accessReq().GetAddress()
		byteSize := trans.accessReq().GetByteSize()
		cacheLineID, offset := getCacheLineID(addr, ds.cache.log2BlockSize)
		if offset+byteSize > 1<<ds.cache.log2BlockSize {
			fmt.Printf("[%s][processTransaction]\tERROR: addr %x, offset %d, bytesize %d, blkSize %d, from %s\n",
				ds.cache.name, addr, offset, byteSize, ds.cache.log2BlockSize,
				trans.accessReq().Meta().Src)
		}

		if _, evicting := ds.cache.evictingList[cacheLineID]; evicting {
			ds.cache.stallEvictingList++
			break
		}

		if ds.doWrite(trans) {
			buf.Pop()
			madeProgress = madeProgress || true
			if ds.cache.debugProcess && addr == ds.cache.debugAddress {
				if trans.fromLocal {
					fmt.Printf("[%s][DEBUG]\tReadReq received - 1: %x\n", ds.cache.name, addr)
				} else {
					fmt.Printf("[%s][DEBUG]\tRemote ReadReq received - 1: %x\n", ds.cache.name, addr)
				}
			}
		} else {
			// Match REC's break-on-failure semantics so stall counters
			// reflect actual back-pressure events, not the
			// simulator-internal inner-loop retry amplification that
			// previously inflated CD_0's stall_bottom_buf_full to ~230M.
			break
		}
	}

	if !madeProgress {
		ds.returnFalse1 = "There is no transaction can be processed"
	}

	return madeProgress
}

func (ds *directoryStage) doWrite(trans *transaction) bool {
	ds.cache.totalDoWriteCalls++
	req := trans.accessReq()
	cachelineID, _ := getCacheLineID(req.GetAddress(), ds.cache.log2BlockSize+ds.cache.log2UnitSize)

	mshrEntry := ds.cache.mshr.Query(req.GetPID(), cachelineID)
	if mshrEntry != nil {
		if trans.write != nil { // write 인 경우, MSHR hit이 발생하면 처리 x
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

	block := ds.cache.directory.Lookup(trans.accessReq().GetPID(), cachelineID)
	if block != nil {
		ok := ds.doWriteHit(trans, block)
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

	ok := ds.doWriteMiss(trans)
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
	mshrEntry.Requests = append(mshrEntry.Requests, trans)

	return true
}

func (ds *directoryStage) doWriteHit(
	trans *transaction,
	block *internal.Block,
) bool {
	if block.IsLocked || block.ReadCount > 0 {
		ds.cache.stallBlockLocked++
		return false
	}

	if trans.isReadTrans() {
		// Read-hit fast-path. Both branches (already-sharer / new-sharer)
		// avoid writeToBank because the directory state mutation is a
		// trivial sharer-list append (no MSHR, no block lock, no bank
		// pipeline traversal). Mirrors REC's read-hit fast-path so the
		// CD vs REC comparison stays apples-to-apples.
		var buf sim.Buffer
		if trans.fromLocal {
			buf = ds.cache.localBottomSenderBuffer
		} else {
			buf = ds.cache.remoteBottomSenderBuffer
		}
		if !buf.CanPush() {
			ds.cache.stallBottomBufFull++
			return false
		}

		if ds.readPermission(trans, block.Sharer) {
			// Already a sharer — no directory mutation needed.
			trans.action = Nothing
		} else {
			// Append sharer atomically here; bottomSender forwards the
			// read to L2 with action=UpdateEntry so per-action counters
			// remain accurate.
			ds.cache.directory.Visit(block)
			block.Sharer = appendSharerInline(
				block.Sharer, trans.accessReq().GetSrcRDMA())
			trans.action = UpdateEntry
		}
		trans.bottomEnterTime = ds.cache.Engine.CurrentTime()
		trans.pathCategory = "fast"
		buf.Push(trans)
		return true
	}

	if ds.writePermission(trans, block.Sharer) {
		trans.action = Nothing
	} else {
		trans.action = InvalidateAndUpdateEntry
		trans.invalidationList = block.Sharer
		trans.evictingAddr = block.Tag
		// trans.evictingAddr = block.CacheAddress
		trans.evictingPID = block.PID
		// fmt.Fprintf(os.Stderr, "[%s] [DEBUG]\tInvalidateAndupdateEntry - 0\n", ds.cache.name)
	}

	if trans.action == Nothing {
		var buf sim.Buffer
		if trans.fromLocal {
			buf = ds.cache.localBottomSenderBuffer
		} else {
			buf = ds.cache.remoteBottomSenderBuffer
		}

		if !buf.CanPush() {
			ds.cache.stallBottomBufFull++
			return false
		}

		trans.bottomEnterTime = ds.cache.Engine.CurrentTime()
		trans.pathCategory = "fast"
		buf.Push(trans)
		return true
	}

	return ds.writeToBank(trans, block)
}

func (ds *directoryStage) doWriteMiss(trans *transaction) bool {
	// if ds.isFromLocal(trans) { // local write request에 대해 directory miss 발생 시, entry 추가 안 함
	if trans.fromLocal { // local write request에 대해 directory miss 발생 시, entry 추가 안 함
		var buf sim.Buffer
		if trans.fromLocal {
			buf = ds.cache.localBottomSenderBuffer
		} else {
			buf = ds.cache.remoteBottomSenderBuffer
		}

		trans.action = Nothing
		if !buf.CanPush() {
			ds.cache.stallBottomBufFull++
			return false
		}

		trans.bottomEnterTime = ds.cache.Engine.CurrentTime()
		trans.pathCategory = "fast"
		buf.Push(trans)
		return true
	}

	if ds.cache.mshr.IsFull() {
		ds.cache.stallMSHRFull++
		return false
	}

	cachelineID, _ := getCacheLineID(trans.accessReq().GetAddress(), ds.cache.log2BlockSize+ds.cache.log2UnitSize)

	victim := ds.cache.directory.FindVictim(cachelineID)
	if victim.IsLocked || victim.ReadCount > 0 {
		ds.cache.stallVictimLocked++
		return false
	}

	if ds.needEviction(victim) {
		trans.action = EvictAndInsertNewEntry
		trans.invalidationList = victim.Sharer
		// trans.evictingAddr = victim.CacheAddress
		trans.evictingAddr = victim.Tag
		trans.evictingPID = victim.PID
	} else {
		trans.action = InsertNewEntry
	}

	return ds.writeToBank(trans, victim)
}

func (ds *directoryStage) writeToBank(
	trans *transaction,
	block *internal.Block,
) bool {
	numBanks := len(ds.cache.localDirToBankBuffers) // 길이 동일 가정
	bank := bankID(block, ds.cache.directory.WayAssociativity(), numBanks)

	var bankBuf sim.Buffer
	if trans.fromLocal {
		bankBuf = ds.cache.localDirToBankBuffers[bank]
	} else {
		bankBuf = ds.cache.remoteDirToBankBuffers[bank]
	}

	if !bankBuf.CanPush() {
		ds.cache.stallBankFull++
		return false
	}

	if ds.cache.mshr.IsFull() {
		ds.cache.stallMSHRFull++
		return false
	}

	// local soft cap: remote 요청을 위해 항상 16개 슬롯 예약.
	// remote 요청(fromLocal=false)은 전체 numMSHREntry까지 사용 가능.
	if trans.fromLocal && ds.cache.maxLocalMshr > 0 {
		if ds.cache.localMshrCount >= ds.cache.maxLocalMshr {
			ds.cache.stallMSHRFull++
			return false
		}
		ds.cache.localMshrCount++
	}

	addr := trans.accessReq().GetAddress()
	cachelineID, _ := getCacheLineID(addr, ds.cache.log2BlockSize+ds.cache.log2UnitSize)

	ds.cache.directory.Visit(block)
	block.IsLocked = true
	block.Tag = cachelineID
	block.IsValid = true
	block.PID = trans.accessReq().GetPID()
	trans.block = block

	bankBuf.Push(trans)

	mshrEntry := ds.cache.mshr.Add(block.PID, cachelineID)
	mshrEntry.Block = block
	trans.mshrEntry = mshrEntry
	mshrEntry.Requests = nil

	return true
}

func (ds *directoryStage) needEviction(victim *internal.Block) bool {
	return victim.IsValid && len(victim.Sharer) != 0
}

func (ds *directoryStage) isFromLocal(trans *transaction) bool {
	if strings.Contains(fmt.Sprintf("%s", trans.accessReq().Meta().Src), "RDMA") { // remote access
		return false
	}
	return true
}

// appendSharerInline appends sh to the sharer list if not already present.
// Used by the read-hit fast-path (UpdateEntry) which mutates directory
// state in directorystage instead of routing through the bank pipeline.
// String-formatting equality matches bankstage.sharerExist's semantics.
func appendSharerInline(list []sim.RemotePort, sh sim.RemotePort) []sim.RemotePort {
	target := fmt.Sprintf("%s", sh)
	for _, e := range list {
		if fmt.Sprintf("%s", e) == target {
			return list
		}
	}
	return append(list, sh)
}

func (ds *directoryStage) readPermission(trans *transaction, sharer []sim.RemotePort) bool {
	// if !ds.isFromLocal(trans) { // remote access
	if !trans.fromLocal { // remote access
		// Sharer is stored as the requester's RDMA port
		// (bankstage InsertNewEntry/UpdateEntry use GetSrcRDMA()).
		// Comparing against Meta().Src would be the immediate sender
		// (e.g. the RDMA engine port) and never match the stored value,
		// so the action=Nothing fast-path would never fire and existing
		// sharers would re-traverse the full pipeline on every re-read.
		src := trans.accessReq().GetSrcRDMA()
		for _, sh := range sharer {
			if sh == src {
				return true
			}
		}

		return false
	} else { // local access
		return true
	}
}

func (ds *directoryStage) writePermission(trans *transaction, sharer []sim.RemotePort) bool {
	if !ds.isFromLocal(trans) { // remote access
		if len(sharer) > 1 || len(sharer) == 0 {
			return false
		} else if fmt.Sprintf("%s", sharer[0]) != fmt.Sprintf("%s", trans.accessReq().GetSrcRDMA()) {
			return false
		}

		return true
	} else { // local access
		// Spec (HMG/CD baseline coherence): local writes to remotely-shared
		// data must propagate invalidations to every recorded sharer. Home
		// GPU is excluded from the sharer list, so any present sharer is a
		// remote GPU that holds a stale copy.
		//
		// Pre-fix code returned true here when len(sharer) <= 1, silently
		// dropping the invalidation to a lone remote sharer.
		// op5aShortcutWithRemoteSharer (Comp field) is the regression slot
		// for that bug; if a future change reintroduces the shortcut, also
		// reintroduce the increment site here so the post-C-2 sanity dump
		// catches it.
		return len(sharer) == 0
	}
}
