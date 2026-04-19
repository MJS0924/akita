package largeblkcache

import (
	"fmt"
	"log"

	"github.com/sarchlab/akita/v4/mem/cache/largeblkcache/internal"
	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/pipelining"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/akita/v4/tracing"
)

type bankStage struct {
	cache  *Comp
	bankID int

	pipeline           pipelining.Pipeline
	pipelineWidth      int
	postPipelineBuf    *bufferImpl
	inflightTransCount int

	// Count the trans that needs to be sent to the write buffer.
	downwardInflightTransCount int
}

type bufferImpl struct {
	sim.HookableBase

	name     string
	capacity int
	elements []interface{}
}

func (b *bufferImpl) Name() string {
	return b.name
}

func (b *bufferImpl) CanPush() bool {
	return len(b.elements) < b.capacity
}

func (b *bufferImpl) Push(e interface{}) {
	if len(b.elements) >= b.capacity {
		log.Panic("buffer overflow")
	}

	b.elements = append(b.elements, e)

	if b.NumHooks() > 0 {
		b.InvokeHook(sim.HookCtx{
			Domain: b,
			Pos:    sim.HookPosBufPush,
			Item:   e,
			Detail: nil,
		})
	}
}

func (b *bufferImpl) Pop() interface{} {
	if len(b.elements) == 0 {
		return nil
	}

	e := b.elements[0]
	b.elements = b.elements[1:]

	if b.NumHooks() > 0 {
		b.InvokeHook(sim.HookCtx{
			Domain: b,
			Pos:    sim.HookPosBufPush,
			Item:   e,
			Detail: nil,
		})
	}

	return e
}

func (b *bufferImpl) Peek() interface{} {
	if len(b.elements) == 0 {
		return nil
	}

	return b.elements[0]
}

func (b *bufferImpl) Capacity() int {
	return b.capacity
}

func (b *bufferImpl) Size() int {
	return len(b.elements)
}

func (b *bufferImpl) Clear() {
	b.elements = nil
}

func (b *bufferImpl) Get(i int) interface{} {
	return b.elements[i]
}

func (b *bufferImpl) Remove(i int) {
	element := b.elements[i]

	b.elements = append(b.elements[:i], b.elements[i+1:]...)

	if b.NumHooks() > 0 {
		b.InvokeHook(sim.HookCtx{
			Domain: b,
			Pos:    sim.HookPosBufPush,
			Item:   element,
			Detail: nil,
		})
	}
}

type bankPipelineElem struct {
	trans *transaction
}

func (e bankPipelineElem) TaskID() string {
	return e.trans.req().Meta().ID + "_write_back_bank_pipeline"
}

func (s *bankStage) Tick() (madeProgress bool) {
	for i := 0; i < s.cache.numReqPerCycle; i++ {
		madeProgress = s.finalizeTrans() || madeProgress
	}

	madeProgress = s.pipeline.Tick() || madeProgress

	for i := 0; i < s.cache.numReqPerCycle; i++ {
		madeProgress = s.pullFromBuf() || madeProgress
	}

	return madeProgress
}

func (s *bankStage) Reset() {
	s.cache.dirToBankBuffers[s.bankID].Clear()
	s.pipeline.Clear()
	s.postPipelineBuf.Clear()
	s.inflightTransCount = 0
}

func (s *bankStage) pullFromBuf() bool {
	if !s.pipeline.CanAccept() {
		return false
	}

	inBuf := s.cache.writeBufferToBankBuffers[s.bankID]

	trans := inBuf.Pop()
	if trans != nil {
		s.pipeline.Accept(bankPipelineElem{trans: trans.(*transaction)})

		s.inflightTransCount++

		if s.cache.debug > 0 {
			fmt.Printf("[%s]\tDEBUG: Send to bottom - 1\n", s.cache.name)
			s.cache.debug--
		}
		return true
	}

	// [FIX: head-of-line blocking] writeBufferBuffer.CanPush() blanket guard 제거.
	// writeBufferFetch 항목은 이제 dirToBankBuffers를 통해 오지 않으므로
	// writeBufferBuffer 포화 여부와 무관하게 dirToBankBuffers를 drain할 수 있어야 한다.
	// writeToHomeNode 케이스의 writeBufferBuffer 사용은 finalizeTrans()에서 개별 확인한다.

	// Always reserve one lane for up-going transactions
	if s.downwardInflightTransCount >= s.pipelineWidth-1 {
		return false
	}

	inBuf = s.cache.dirToBankBuffers[s.bankID]
	trans = inBuf.Pop()

	if trans != nil {
		t := trans.(*transaction)

		// [FIX] writeBufferFetch 항목은 더 이상 dirToBankBuffers를 통해 오지 않는다.
		// dirStage.fetch()에서 writeBufferFetchBuffer로 직접 push하도록 변경됨.

		s.pipeline.Accept(bankPipelineElem{trans: trans.(*transaction)})

		s.inflightTransCount++

		switch t.action {
		case bankEvict, bankEvictAndFetch, bankEvictAndWrite, bankEvictAndPrefetch:
			s.downwardInflightTransCount++
		}

		return true
	}

	return false
}

func (s *bankStage) finalizeTrans() bool {
	for i := 0; i < s.postPipelineBuf.Size(); i++ {
		trans := s.postPipelineBuf.Get(i).(bankPipelineElem).trans

		done := false

		switch trans.action {
		case bankReadHit: // action 1
			done = s.finalizeReadHit(trans)
		case bankWriteHit: // action 2
			done = s.finalizeWriteHit(trans)
		case bankWriteFetched: // action 6
			done = s.finalizeBankWriteFetched(trans)
		case bankWritePrefetched: // [추가] 프리패치 쓰기 완료 액션 처리
			done = s.finalizeBankWritePrefetched(trans)
		case bankEvictAndFetch, bankEvictAndWrite, bankEvict: // action 5, 4, 3
			done = s.finalizeBankEviction(trans)
		default:
			panic("bank action not supported")
		}

		if done {
			s.postPipelineBuf.Remove(i)

			if s.cache.debug > 0 {
				fmt.Printf("[%s]\tDEBUG: Send to bottom - 2\n", s.cache.name)
				s.cache.debug--
			}

			if s.cache.deviceID == 3 && trans.action != bankReadHit && trans.accessReq() != nil && trans.accessReq().GetAddress() == 12888246464 {
				fmt.Printf("[%s][DEBUG]\tReadReq received - 4.2: %d\n", s.cache.name, trans.accessReq().GetAddress())
			}
			return true
		}
	}

	return false
}

func (s *bankStage) finalizeReadHit(trans *transaction) bool {
	if trans.fromLocal && !s.cache.topPort.CanSend() {
		return false
	} else if !trans.fromLocal && !s.cache.remoteTopPort.CanSend() {
		return false
	}

	read := trans.read
	addr := read.Address
	_, offset := getCacheLineID(addr, s.cache.log2BlockSize)
	block := trans.block

	data, err := s.cache.storage.Read(
		block.CacheAddress+offset, read.AccessByteSize)
	if err != nil {
		panic(err)
	}

	s.removeTransaction(trans)

	s.inflightTransCount--
	s.downwardInflightTransCount--
	block.ReadCount--

	// if read.ReqFrom == "GPU[1].SA[0].L1VCache[0]" {
	// 	fmt.Printf("[%s]\tSend data ready response: Addr %x\n", s.cache.name, addr)
	// }

	dataReady := mem.DataReadyRspBuilder{}.
		WithSrc(s.cache.topPort.AsRemote()).
		WithDst(read.Src).
		WithRspTo(read.Meta().ID).
		WithData(data).
		WithOrigin(read).
		Build()
	if !trans.fromLocal {
		dataReady.Src = s.cache.remoteTopPort.AsRemote()
		s.cache.remoteTopPort.Send(dataReady)
	} else {
		s.cache.topPort.Send(dataReady)
	}

	tracing.TraceReqComplete(read, s.cache)

	if s.cache.deviceID == 3 && trans.accessReq() != nil && trans.accessReq().GetAddress() == 12888246464 {
		fmt.Printf("[%s][DEBUG]\tReadReq received - 4.9: %d\n", s.cache.name, trans.accessReq().GetAddress())
	}
	// fmt.Printf("[%s]\tSend Data Ready Response %x to %s for reqID %s\n", s.cache.name, read.Address, read.Src, read.ID)

	// log.Printf("%.10f, %s, bank read hit finalize，"+
	// " %s, %04X, %04X, (%d, %d), %v\n",
	// 	now, s.cache.Name(),
	// 	trans.read.ID,
	// 	trans.read.Address, block.Tag,
	// 	block.SetID, block.WayID,
	// 	dataReady.Data,
	// )

	return true
}

func (s *bankStage) finalizeWriteHit(trans *transaction) bool {
	if trans.fromLocal && !s.cache.topPort.CanSend() {
		return false
	} else if !trans.fromLocal && !s.cache.remoteTopPort.CanSend() {
		return false
	}

	if trans.writeToHomeNode && !s.cache.writeBufferBuffer.CanPush() {
		return false
	}

	write := trans.write
	addr := write.Address
	_, offset := getCacheLineID(addr, s.cache.log2BlockSize)
	block := trans.block

	if offset+uint64(len(write.Data)) > 1<<s.cache.log2BlockSize {
		fmt.Printf("[%s]\tERR: not aligned to large block: addr %x, offset %d, len %d, blockSize %d, reqFrom %s\n",
			s.cache.name, addr, offset, len(write.Data), s.cache.log2BlockSize, trans.accessReq().Meta().Src)
	}

	dirtyMask := s.writeData(block, write, offset)

	block.IsValid = true
	block.IsLocked = false
	block.IsDirty = true
	block.DirtyMask = dirtyMask

	s.removeTransaction(trans)

	s.inflightTransCount--
	s.downwardInflightTransCount--

	done := mem.WriteDoneRspBuilder{}.
		WithSrc(s.cache.topPort.AsRemote()).
		WithDst(write.Src).
		// WithRspTo(write.ID).
		WithRspTo(write.Meta().ID).
		WithOrigin(write).
		Build()
	if !trans.fromLocal {
		done.Src = s.cache.remoteTopPort.AsRemote()
		s.cache.remoteTopPort.Send(done)
	} else {
		s.cache.topPort.Send(done)
	}
	// fmt.Printf("[%s]\tSend Write Done Response %x to %s for reqID %s\n", s.cache.name, write.Address, write.Src, write.ID)

	if trans.writeToHomeNode {
		data, err := s.cache.storage.Read(block.CacheAddress, 1<<s.cache.log2BlockSize)
		if err != nil {
			panic(err)
		}

		block.IsDirty = false // remote data에 write 하는 경우, home node로 write 하므로 dirty 표시를 하지 않음
		trans.evictingPID = block.PID
		trans.evictingAddr = block.Tag
		trans.evictingData = data
		trans.evictingDirtyMask = block.DirtyMask

		trans.action = writeBufferFlush
		s.cache.writeBufferBuffer.Push(trans)
	}

	tracing.TraceReqComplete(write, s.cache)

	// log.Printf("%.10f, %s, bank write hit finalize， "+
	// "%s, %04X, %04X, (%d, %d), %v\n",
	// 	now, s.cache.Name(),
	// 	trans.write.ID,
	// 	trans.write.Address, block.Tag,
	// 	block.SetID, block.WayID,
	// 	write.Data,
	// )

	return true
}

func (s *bankStage) writeData(
	block *internal.Block,
	write *mem.WriteReq,
	offset uint64,
) []bool {
	data, err := s.cache.storage.Read(
		block.CacheAddress, 1<<s.cache.log2BlockSize)
	if err != nil {
		panic(err)
	}

	dirtyMask := block.DirtyMask
	if dirtyMask == nil {
		dirtyMask = make([]bool, 1<<s.cache.log2BlockSize)
	}

	for i := 0; i < len(write.Data); i++ {
		if write.DirtyMask == nil || write.DirtyMask[i] {
			index := offset + uint64(i)
			data[index] = write.Data[i]
			dirtyMask[index] = true
		}
	}

	err = s.cache.storage.Write(block.CacheAddress, data)
	if err != nil {
		panic(err)
	}

	return dirtyMask
}

func (s *bankStage) finalizeBankWriteFetched(
	trans *transaction,
) bool {
	if !s.cache.mshrStageBuffer.CanPush() {
		return false
	}

	if trans.writeToHomeNode && !s.cache.writeBufferBuffer.CanPush() {
		return false
	}

	mshrEntry := trans.mshrEntry
	block := mshrEntry.Block
	s.cache.mshrStageBuffer.Push(mshrEntry)

	err := s.cache.storage.Write(block.CacheAddress, mshrEntry.Data)
	if err != nil {
		panic(err)
	}

	block.IsLocked = false
	block.IsValid = true

	s.inflightTransCount--

	if trans.writeToHomeNode {
		trans.evictingPID = block.PID
		trans.evictingAddr = block.Tag
		trans.evictingData = mshrEntry.Data
		trans.evictingDirtyMask = block.DirtyMask

		trans.action = writeBufferFlush
		s.cache.writeBufferBuffer.Push(trans)
	}

	// if trans.accessReq() != nil {
	// 	log.Printf("%.10f, %s, write fetched, "+
	// 		"%s, %04X, %04X, (%d, %d), %v\n",
	// 		now, s.cache.Name(),
	// 		trans.accessReq().Meta().ID,
	// 		trans.accessReq().GetAddress(), block.Tag,
	// 		block.SetID, block.WayID,
	// 		mshrEntry.Data,
	// 	)
	// }

	return true
}

func (s *bankStage) finalizeBankWritePrefetched(
	trans *transaction,
) bool {
	if !s.cache.mshrStageBuffer.CanPush() {
		return false
	}

	block := trans.block
	data := trans.fetchedData

	// MSHR에 기록이 있다면 MSHR 스테이지로 넘겨 깨움
	if trans.mshrEntry != nil {
		s.cache.mshrStageBuffer.Push(trans.mshrEntry)
	}

	err := s.cache.storage.Write(block.CacheAddress, data)
	if err != nil {
		panic(err)
	}

	block.IsLocked = false
	block.IsValid = true

	s.inflightTransCount--

	return true
}

func (s *bankStage) removeTransaction(trans *transaction) {
	for i, t := range s.cache.inFlightTransactions {
		if trans == t {
			s.cache.inFlightTransactions = append(
				(s.cache.inFlightTransactions)[:i],
				(s.cache.inFlightTransactions)[i+1:]...)

			return
		}
	}

	now := s.cache.Engine.CurrentTime()

	fmt.Printf("%.10f, %s, Transaction %s not found\n",
		now, s.cache.Name(), trans.id)

	// panic("transaction not found")
}

func (s *bankStage) finalizeBankEviction(
	trans *transaction,
) bool {
	if !s.cache.writeBufferBuffer.CanPush() {
		return false
	}

	victim := trans.victim

	data, err := s.cache.storage.Read(
		victim.CacheAddress, 1<<s.cache.log2BlockSize)
	if err != nil {
		panic(err)
	}

	trans.evictingData = data

	switch trans.action {
	case bankEvict:
		trans.action = writeBufferFlush
	case bankEvictAndFetch:
		trans.action = writeBufferEvictAndFetch
	case bankEvictAndWrite:
		trans.action = writeBufferEvictAndWrite
	case bankEvictAndPrefetch:
		trans.action = writeBufferEvictAndPrefetch
	default:
		panic("unsupported action")
	}

	// if trans.accessReq() != nil {
	// 	log.Printf("%.10f, %s, bank read for eviction， "+
	// 		"%s, %04X, %04X, (%d, %d), %v\n",
	// 		now, s.cache.Name(),
	// 		trans.accessReq().Meta().ID,
	// 		trans.accessReq().GetAddress(), victim.Tag,
	// 		victim.SetID, victim.WayID,
	// 		data,
	// 	)
	// }

	delete(s.cache.evictingList, trans.evictingAddr)
	s.cache.writeBufferBuffer.Push(trans)

	s.inflightTransCount--
	s.downwardInflightTransCount--

	return true
}
