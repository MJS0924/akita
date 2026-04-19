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
	cache        *Comp
	pipeline     pipelining.Pipeline
	buf          sim.Buffer
	returnFalse0 string
	returnFalse1 string
}

func (ds *directoryStage) Tick() (madeProgress bool) {
	madeProgress = ds.acceptNewTransaction() || madeProgress

	madeProgress = ds.pipeline.Tick() || madeProgress

	madeProgress = ds.processTransaction() || madeProgress

	return madeProgress
}

func (ds *directoryStage) acceptNewTransaction() bool {
	madeProgress := false

	for i := 0; i < ds.cache.numReqPerCycle; i++ {
		if !ds.pipeline.CanAccept() {
			break
		}

		item := ds.cache.dirStageBuffer.Peek()
		if item == nil {
			break
		}

		trans := item.(*transaction)
		ds.pipeline.Accept(dirPipelineItem{trans})
		ds.cache.dirStageBuffer.Pop()

		madeProgress = true
	}

	if !madeProgress {
		ds.returnFalse0 = "There is no new trasaction which can be accepted"
	}
	return madeProgress
}

func (ds *directoryStage) Reset() {
	ds.pipeline.Clear()
	ds.buf.Clear()
	ds.cache.dirStageBuffer.Clear()
}

func (ds *directoryStage) processTransaction() bool {
	madeProgress := false

	for i := 0; i < ds.cache.numReqPerCycle; i++ {
		item := ds.buf.Peek()
		if item == nil {
			break
		}

		trans := item.(dirPipelineItem).trans

		addr := trans.accessReq().GetAddress()
		byteSize := trans.accessReq().GetByteSize()
		cacheLineID, offset := getCacheLineID(addr, ds.cache.log2BlockSize+ds.cache.log2UnitSize)
		if offset+byteSize > 1<<ds.cache.log2BlockSize {
			fmt.Printf("[%s][processTransaction]\tERROR: addr %x, offset %d, bytesize %d, blkSize %d, from %s\n",
				ds.cache.name, addr, offset, byteSize, ds.cache.log2BlockSize,
				trans.accessReq().Meta().Src)
		}

		if _, evicting := ds.cache.evictingList[cacheLineID]; evicting {
			break
		}

		if ds.doWrite(trans) {
			ds.buf.Pop()
			madeProgress = madeProgress || true
		}
	}

	if !madeProgress {
		ds.returnFalse1 = "There is no transaction can be processed"
	}

	return madeProgress
}

func (ds *directoryStage) doWrite(trans *transaction) bool {
	req := trans.accessReq()
	cachelineID, _ := getCacheLineID(req.GetAddress(), ds.cache.log2BlockSize+ds.cache.log2UnitSize)

	mshrEntry := ds.cache.mshr.Query(req.GetPID(), cachelineID)
	if mshrEntry != nil {
		if trans.write != nil { // write 인 경우, MSHR hit이 발생하면 처리 x
			return false
		}

		ok := ds.doWriteMSHRHit(trans, mshrEntry)
		tracing.AddTaskStep(
			tracing.MsgIDAtReceiver(trans.accessReq(), ds.cache),
			ds.cache,
			"read-mshr-hit",
		)

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
		return false
	}

	if trans.isReadTrans() {
		if ds.readPermission(trans, block.Sharer) {
			trans.action = Nothing
		} else {
			trans.action = UpdateEntry
		}
		return ds.writeToBank(trans, block)
	}

	if ds.writePermission(trans, block.Sharer) {
		trans.action = Nothing
	} else {
		trans.action = InvalidateAndUpdateEntry
		trans.invalidationList = block.Sharer
		trans.evictingAddr = block.Tag
		// trans.evictingAddr = block.CacheAddress
		trans.evictingPID = block.PID
	}

	if trans.action == Nothing {
		// if ds.isFromLocal(trans) {
		if !ds.cache.bottomSenderBuffer.CanPush() {
			return false
		}

		ds.cache.bottomSenderBuffer.Push(trans)
		return true
	}

	return ds.writeToBank(trans, block)
}

func (ds *directoryStage) doWriteMiss(trans *transaction) bool {
	// if ds.isFromLocal(trans) { // local write request에 대해 directory miss 발생 시, entry 추가 안 함
	if trans.fromLocal { // local write request에 대해 directory miss 발생 시, entry 추가 안 함
		trans.action = Nothing
		if !ds.cache.bottomSenderBuffer.CanPush() {
			return false
		}
		ds.cache.bottomSenderBuffer.Push(trans)
		return true
	}

	if ds.cache.mshr.IsFull() {
		return false
	}

	cachelineID, _ := getCacheLineID(trans.accessReq().GetAddress(), ds.cache.log2BlockSize)

	victim := ds.cache.directory.FindVictim(cachelineID)
	if victim.IsLocked || victim.ReadCount > 0 {
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
	numBanks := len(ds.cache.dirToBankBuffers)
	bank := bankID(block, ds.cache.directory.WayAssociativity(), numBanks)
	bankBuf := ds.cache.dirToBankBuffers[bank]

	if !bankBuf.CanPush() {
		return false
	}

	if ds.cache.mshr.IsFull() {
		return false
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

func (ds *directoryStage) readPermission(trans *transaction, sharer []sim.RemotePort) bool {
	// if !ds.isFromLocal(trans) { // remote access
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
	if !ds.isFromLocal(trans) { // remote access
		if len(sharer) > 1 {
			return false
		} else if sharer[0] != trans.accessReq().Meta().Src {
			return false
		}

		return true
	} else { // local access
		if len(sharer) > 1 {
			return false
		}

		return true
	}
}
