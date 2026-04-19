package writeback

import (
	"fmt"
	"strings"

	"github.com/sarchlab/akita/v4/mem/cache"
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
	cache    *Comp
	pipeline pipelining.Pipeline
	buf      sim.Buffer

	lastReturnValue bool
	returnFalse     string
}

func (ds *directoryStage) Tick() (madeProgress bool) {
	ds.returnFalse = ""

	madeProgress = ds.acceptNewTransaction() || madeProgress

	madeProgress = ds.pipeline.Tick() || madeProgress

	madeProgress = ds.processTransaction() || madeProgress

	ds.lastReturnValue = madeProgress

	return madeProgress
}

func (ds *directoryStage) processTransaction() bool {
	madeProgress := false

	for i := 0; i < ds.cache.numReqPerCycle; i++ {
		item := ds.buf.Peek()
		if item == nil {
			break
		}

		trans := item.(dirPipelineItem).trans

		addr := uint64(0)
		if trans.invalidation != nil {
			addr = trans.invalidation.Address
		} else {
			addr = trans.accessReq().GetAddress()
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

		madeProgress = ds.doWrite(trans) || madeProgress
	}

	// if !madeProgress && ds.returnFalse != "" && ds.cache.deviceID == 5 {
	// 	fmt.Printf("[%s]\t%s\n", ds.cache.name, ds.returnFalse)
	// }
	if !madeProgress {
		temp := ds.buf.Pop()
		ds.buf.Push(temp)
		// 이지랄 해도 되는지 잘 모르겠음
	}
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

	return madeProgress
}

func (ds *directoryStage) Reset() {
	ds.pipeline.Clear()
	ds.buf.Clear()
	ds.cache.dirStageBuffer.Clear()
}

func (ds *directoryStage) doInvalidation(trans *transaction) bool {
	ds.returnFalse = "[doInvalidation] "

	cachelineID, _ := getCacheLineID(
		trans.invalidation.Address, ds.cache.log2BlockSize)

	block := ds.cache.directory.Lookup(
		trans.invalidation.PID, cachelineID)

	if block != nil {
		if block.IsValid && (block.IsLocked || block.ReadCount > 0) {
			ds.returnFalse = ds.returnFalse + fmt.Sprintf("Block %x is being used, isLocked %v, readCnt %d", block.Tag, block.IsLocked, block.ReadCount)
			// temp := ds.buf.Pop()
			// ds.buf.Push(temp)

			// 이랬는데도 지랄나면 이거 따로 버퍼에 저장하는 식으로 해서 빼놔야 될 듯
			return false
		}

		block.IsValid = false
	}

	if !ds.cache.topPort.CanSend() {
		ds.returnFalse = ds.returnFalse + "Cannot send to topPort"

		return false
	}

	req := trans.invalidation
	rsp := mem.InvRspBuilder{}.
		WithSrc(ds.cache.topPort.AsRemote()).
		WithDst(req.Src).
		WithRspTo(req.ReqFrom).
		Build()
	if block != nil && block.Accessed { // eviction 되어서 사라진 경우는 count 안 하기
		rsp.Accessed = 1
	}

	err := ds.cache.topPort.Send(rsp)
	if err != nil {
		ds.returnFalse = ds.returnFalse + "Failed to send to topPort"

		return false
	}

	ds.buf.Pop()
	ds.cache.mshrStage.removeTransaction(trans)

	// fmt.Printf("[%s]\tC.0. (%s -> %s) Processed invalidation for %x\n",
	// 	ds.cache.Name(), req.ReqFrom, rsp.Meta().ID, req.Address)
	if block != nil {
		// ds.cache.printRWMask(block.PID, block.VAddr)
		ds.cache.eraseCacheLineFromRWMask(block.PID, block.VAddr)
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
	mshrEntry *cache.MSHREntry,
) bool {
	trans.mshrEntry = mshrEntry
	mshrEntry.Requests = append(mshrEntry.Requests, trans)

	ds.buf.Pop()

	tracing.AddTaskStep(
		tracing.MsgIDAtReceiver(trans.read, ds.cache),
		ds.cache,
		"read-mshr-hit",
	)

	return true
}

func (ds *directoryStage) handleReadHit(
	trans *transaction,
	block *cache.Block,
) bool {
	ds.returnFalse = ds.returnFalse + "[handleReadHit] "

	if block.IsLocked {
		ds.returnFalse = ds.returnFalse + fmt.Sprintf("Block %x is locked", block.Tag)

		return false
	}

	tracing.AddTaskStep(
		tracing.MsgIDAtReceiver(trans.read, ds.cache),
		ds.cache,
		"read-hit",
	)

	// log.Printf("%.10f, %s, dir read hit， %s, %04X, %04X, (%d, %d), %v\n",
	// 	now, ds.cache.Name(),
	// 	trans.read.ID,
	// 	trans.read.Address,
	// 	(trans.read.GetAddress()>>ds.cache.log2BlockSize)
	// 	<<ds.cache.log2BlockSize,
	// 	block.SetID, block.WayID,
	// 	nil,
	// )

	return ds.readFromBank(trans, block)
}

func (ds *directoryStage) handleReadMiss(trans *transaction) bool {
	ds.returnFalse = ds.returnFalse + "[handleReadMiss] "

	req := trans.read
	cacheLineID, _ := getCacheLineID(req.Address, ds.cache.log2BlockSize)

	if ds.cache.mshr.IsFull() {
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

	if ds.needEviction(victim) {
		ok := ds.evict(trans, victim)
		if ok {
			tracing.AddTaskStep(
				tracing.MsgIDAtReceiver(trans.read, ds.cache),
				ds.cache,
				"read-miss",
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
	}

	return ok
}

func (ds *directoryStage) doWrite(trans *transaction) bool {
	ds.returnFalse = "[doWrite] "

	write := trans.write
	cachelineID, _ := getCacheLineID(write.Address, ds.cache.log2BlockSize)

	mshrEntry := ds.cache.mshr.Query(write.PID, cachelineID)
	if mshrEntry != nil {
		ok := ds.doWriteMSHRHit(trans, mshrEntry)
		tracing.AddTaskStep(
			tracing.MsgIDAtReceiver(trans.write, ds.cache),
			ds.cache,
			"write-mshr-hit",
		)

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
	}

	return ok
}

func (ds *directoryStage) doWriteMSHRHit(
	trans *transaction,
	mshrEntry *cache.MSHREntry,
) bool {
	trans.mshrEntry = mshrEntry
	mshrEntry.Requests = append(mshrEntry.Requests, trans)

	ds.buf.Pop()

	return true
}

func (ds *directoryStage) doWriteHit(
	trans *transaction,
	block *cache.Block,
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

	if ds.needEviction(victim) {
		return ds.evict(trans, victim)
	}

	return ds.writeToBank(trans, victim)
}

func (ds *directoryStage) writePartialLineMiss(trans *transaction) bool {
	ds.returnFalse = ds.returnFalse + "[writePartialLineMiss] "

	write := trans.write
	cachelineID, _ := getCacheLineID(write.Address, ds.cache.log2BlockSize)

	if ds.cache.mshr.IsFull() {
		ds.returnFalse = ds.returnFalse + "MSHR is full"

		return false
	}

	victim := ds.cache.directory.FindVictim(cachelineID)
	if victim.IsLocked || victim.ReadCount > 0 {
		ds.returnFalse = ds.returnFalse + fmt.Sprintf("Victim %x is being used", victim.Tag)

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

	if ds.needEviction(victim) {
		return ds.evict(trans, victim)
	}

	return ds.fetch(trans, victim)
}

func (ds *directoryStage) readFromBank(
	trans *transaction,
	block *cache.Block,
) bool {
	ds.returnFalse = ds.returnFalse + "[readFromBank] "
	numBanks := len(ds.cache.dirToBankBuffers)
	bank := bankID(block, ds.cache.directory.WayAssociativity(), numBanks)
	bankBuf := ds.cache.dirToBankBuffers[bank]

	if !bankBuf.CanPush() {
		ds.returnFalse = ds.returnFalse + "Cannot push to bankBuf"

		return false
	}

	ds.cache.directory.Visit(block)

	block.ReadCount++
	trans.block = block
	trans.action = bankReadHit

	ds.buf.Pop()
	bankBuf.Push(trans)

	return true
}

func (ds *directoryStage) writeToBank(
	trans *transaction,
	block *cache.Block,
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

	ds.cache.directory.Visit(block)
	block.IsLocked = true
	block.Tag = cachelineID
	block.IsValid = true
	block.PID = trans.write.PID
	block.VAddr = trans.accessReq().GetVAddr()
	trans.block = block
	trans.action = bankWriteHit

	port := fmt.Sprintf("%s", ds.cache.addressToPortMapper.Find(trans.accessReq().GetAddress()))
	if strings.Contains(port, "RDMA") {
		trans.writeToHomeNode = true // Coherence Directory를 사용하는 경우, write back cache에서도 home node로 write를 보내야 함
		block.IsDirty = false        // remote data에 write 하는 경우, home node로 write 하므로 dirty 표시를 하지 않음
	} else {
		trans.writeToHomeNode = false
	}

	ds.buf.Pop()
	bankBuf.Push(trans)

	return true
}

func (ds *directoryStage) evict(
	trans *transaction,
	victim *cache.Block,
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

	ds.updateTransForEviction(trans, victim, pid, cacheLineID)
	ds.updateVictimBlockMetaData(victim, cacheLineID, pid)

	ds.buf.Pop()
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

	return true
}

func (ds *directoryStage) updateVictimBlockMetaData(
	victim *cache.Block,
	cacheLineID uint64,
	pid vm.PID,
) {
	victim.Tag = cacheLineID
	victim.PID = pid
	victim.IsLocked = true
	victim.IsDirty = false
	ds.cache.directory.Visit(victim)
	victim.Accessed = false
}

func (ds *directoryStage) updateTransForEviction(
	trans *transaction,
	victim *cache.Block,
	pid vm.PID,
	cacheLineID uint64,
) {
	trans.action = bankEvictAndFetch
	trans.victim = &cache.Block{
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
	block *cache.Block,
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

	bankNum := bankID(block,
		ds.cache.directory.WayAssociativity(), len(ds.cache.dirToBankBuffers))
	bankBuf := ds.cache.dirToBankBuffers[bankNum]

	if !bankBuf.CanPush() {
		ds.returnFalse = ds.returnFalse + "Cannot push to bankBuf"

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
	ds.cache.directory.Visit(block)

	tracing.AddTaskStep(
		tracing.MsgIDAtReceiver(req, ds.cache),
		ds.cache,
		fmt.Sprintf("add-mshr-entry-0x%x-0x%x", mshrEntry.Address, block.Tag),
	)

	ds.buf.Pop()

	trans.action = writeBufferFetch
	trans.fetchPID = pid
	trans.fetchAddress = cacheLineID
	bankBuf.Push(trans)

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

func (ds *directoryStage) needEviction(victim *cache.Block) bool {
	return victim.IsValid && victim.IsDirty
}
