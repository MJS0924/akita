package writebackcoh

import (
	"fmt"
	"log"

	"github.com/sarchlab/akita/v4/mem/cache/writebackcoh/internal"
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
	// Commit-only budget for finalize: each iteration consumes one
	// slot ONLY when a transaction actually commits out of the
	// post-pipeline buffer. If no item in postPipelineBuf can finalize
	// this cycle, the loop terminates early — a stalled head item
	// shouldn't keep burning per-cycle budget.
	for i := 0; i < s.cache.numReqPerCycle; i++ {
		if !s.finalizeTrans() {
			break
		}
		madeProgress = true
	}

	madeProgress = s.pipeline.Tick() || madeProgress

	// Commit-only budget for pull: pullFromBuf already arbitrates
	// between writeBufferToBankBuffers (upward, priority) and
	// dirToBankBuffers (downward) internally and returns true only on
	// commit. If both queues are empty/blocked this cycle, terminate.
	for i := 0; i < s.cache.numReqPerCycle; i++ {
		if !s.pullFromBuf() {
			break
		}
		madeProgress = true
	}

	return madeProgress
}

func (s *bankStage) Reset() {
	s.cache.dirToBankBuffers[s.bankID].Clear()
	s.cache.dirToBankBuffersLocal[s.bankID].Clear()
	s.cache.dirToBankBuffersRemote[s.bankID].Clear()
	s.cache.writeBufferToBankBuffers[s.bankID].Clear()
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

		return true
	}

	// Always reserve one lane for up-going transactions
	if s.downwardInflightTransCount >= s.pipelineWidth-1 {
		return false
	}

	// [FIX #2] Remote 먼저, Local 그 다음. dirStage 의 invBuf→remoteBuf→localBuf
	// priority 가 bank 까지 전파되도록. local 트랜잭션이 bankBuf head 에 쌓여도
	// remote 가 뒤에서 영원히 밀리는 일이 없게 함 (cross-GPU 데드락 차단).
	for _, inBuf := range []sim.Buffer{
		s.cache.dirToBankBuffersRemote[s.bankID],
		s.cache.dirToBankBuffersLocal[s.bankID],
	} {
		trans = inBuf.Pop()
		if trans == nil {
			continue
		}

		t := trans.(*transaction)
		s.pipeline.Accept(bankPipelineElem{trans: t})
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
		if s.cache.debugProcess && trans.accessReq() != nil && trans.accessReq().GetAddress() == s.cache.debugAddress0 {
			fmt.Printf("[%s] [bankStage]\tReceived req - 2: addr %x, action %d\n", s.cache.name, trans.accessReq().GetAddress(), trans.action)
		}
		if s.cache.debugProcess && trans.accessReq() != nil && trans.accessReq().GetAddress() == s.cache.debugAddress1 {
			fmt.Printf("[%s] [bankStage]\tReceived req - 2: addr %x, action %d\n", s.cache.name, trans.accessReq().GetAddress(), trans.action)
		}
		if s.cache.debugProcess && trans.responsing {
			fmt.Printf("[%s]\tTransaction %x is responsing, discard.\n", s.cache.name, trans.accessReq().GetAddress())
			s.postPipelineBuf.Remove(i)
			continue
		}

		done := false

		switch trans.action {
		case bankReadHit: // action 1
			done = s.finalizeReadHit(trans)
		case bankWriteHit: // action 2
			done = s.finalizeWriteHit(trans)
		case bankWriteFetched: // action 6
			done = s.finalizeBankWriteFetched(trans)
		case bankWritePrefetched, bankEvictAndPrefetch:
			done = s.finalizeBankWritePrefetched(trans)
		case bankEvictAndFetch, bankEvictAndWrite, bankEvict: // action 5, 4, 3
			done = s.finalizeBankEviction(trans)
		default:
			panic("bank action not supported")
		}

		if done {
			s.postPipelineBuf.Remove(i)

			return true
		}
	}

	return false
}

func (s *bankStage) finalizeReadHit(trans *transaction) bool {
	read := trans.read

	// NoNeedToReply: sub-block prefetch request from CohDir.
	// Skip response to avoid flooding topPort and causing pipeline deadlock.
	// if read.GetNoNeedToReply() {
	// 	_, offset := getCacheLineID(read.Address, s.cache.log2BlockSize)
	// 	block := trans.block

	// 	_, err := s.cache.storage.Read(block.CacheAddress+offset, read.AccessByteSize)
	// 	if err != nil {
	// 		panic(err)
	// 	}

	// 	s.removeTransaction(trans)
	// 	s.inflightTransCount--
	// 	block.ReadCount--
	// 	block.Accessed = true

	// 	tracing.TraceReqComplete(read, s.cache)
	// 	return true
	// }

	if trans.fromLocal && !s.cache.topPort.CanSend() {
		return false
	} else if !trans.fromLocal && !s.cache.remoteTopPort.CanSend() {
		return false
	}

	addr := read.Address
	_, offset := getCacheLineID(addr, s.cache.log2BlockSize)
	block := trans.block

	data, err := s.cache.storage.Read(
		block.CacheAddress+offset, read.AccessByteSize)
	if err != nil {
		panic(err)
	}

	s.removeTransaction(trans)

	// if read.ReqFrom == "GPU[1].SA[0].L1VCache[0]" {
	// 	fmt.Printf("[%s]\tSend data ready response: Addr %x\n", s.cache.name, addr)
	// }

	var error *sim.SendError
	dataReady := mem.DataReadyRspBuilder{}.
		WithSrc(s.cache.topPort.AsRemote()).
		WithDst(read.Meta().Src).
		WithRspTo(read.Meta().ID).
		WithData(data).
		WithOrigin(read).
		Build()

	if !trans.fromLocal {
		dataReady.Src = s.cache.remoteTopPort.AsRemote()
		error = s.cache.remoteTopPort.Send(dataReady)
	} else {
		error = s.cache.topPort.Send(dataReady)
	}
	if error != nil {
		return false
	}

	s.inflightTransCount--
	block.ReadCount--
	block.Accessed = true

	tracing.TraceReqComplete(read, s.cache)

	if s.cache.debugProcess && trans.accessReq() != nil && trans.accessReq().GetAddress() == s.cache.debugAddress0 {
		fmt.Printf("[%s] [bankStage]\tSend rsp - 2.1: addr %x, dst %s, src %s, action %d, ID %s\n",
			s.cache.name, dataReady.Origin.GetAddress(), dataReady.Dst, dataReady.Src, trans.action, dataReady.Meta().ID)
	}
	if s.cache.debugProcess && trans.accessReq() != nil && trans.accessReq().GetAddress() == s.cache.debugAddress1 {
		fmt.Printf("[%s] [bankStage]\tSend rsp - 2.1: addr %x, dst %s, src %s, action %d\n, ID %s",
			s.cache.name, dataReady.Origin.GetAddress(), dataReady.Dst, dataReady.Src, trans.action, dataReady.Meta().ID)
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

	dirtyMask := s.writeData(block, write, offset)

	block.IsValid = true
	block.IsLocked = false
	block.IsDirty = true
	block.Accessed = true
	block.DirtyMask = dirtyMask

	s.removeTransaction(trans)

	s.inflightTransCount--

	done := mem.WriteDoneRspBuilder{}.
		WithSrc(s.cache.topPort.AsRemote()).
		WithDst(write.Meta().Src).
		// WithRspTo(write.ID).
		WithRspTo(write.Meta().ID).
		WithOrigin(write).
		Build()
	if !trans.fromLocal {
		done.Meta().Src = s.cache.remoteTopPort.AsRemote()
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
		trans.evictingAddr = trans.accessReq().GetAddress()
		trans.evictingData = data
		trans.evictingDirtyMask = block.DirtyMask

		trans.action = writeBufferFlush
		s.cache.writeBufferBuffer.Push(trans)

		if s.cache.debugProcess && trans.action == bankWritePrefetched {
			fmt.Printf("[%s]\t[WARNING]\twrong trans action\n", s.cache.name)
		}
	}

	// Deferred-invalidation: an InvReq arrived while this block was
	// IsLocked by the write-hit op. It has already been acked upstream;
	// the writeToHomeNode flush above (if any) has already captured
	// block metadata, so zero the local copy now.
	s.applyPendingInvalidation(block)

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

	// Deferred-invalidation: see directoryStage.doInvalidation. The
	// L1 read miss responder has already been served via mshrStage drain;
	// the home-node flush above (if any) has captured block metadata.
	// Safe to zero the local L2 copy now.
	s.applyPendingInvalidation(block)

	if s.cache.debugProcess && trans.accessReq() != nil && trans.accessReq().GetAddress() == s.cache.debugAddress0 {
		fmt.Printf("[%s] [bankStage]\tReceived rsp - 2.2: addr %x, action %d, writeToHomenode %t\n",
			s.cache.name, trans.accessReq().GetAddress(), trans.action, trans.writeToHomeNode)
	}
	if s.cache.debugProcess && trans.accessReq() != nil && trans.accessReq().GetAddress() == s.cache.debugAddress1 {
		fmt.Printf("[%s] [bankStage]\tReceived rsp - 2.2: addr %x, action %d, writeToHomenode %t\n",
			s.cache.name, trans.accessReq().GetAddress(), trans.action, trans.writeToHomeNode)
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
	// 1. MSHR 버퍼와 WriteBuffer 버퍼 모두 여유 공간이 있는지 확인
	if !s.cache.mshrStageBuffer.CanPush() {
		return false
	}
	if trans.action == bankEvictAndPrefetch && !s.cache.writeBufferBuffer.CanPush() {
		return false
	}

	block := trans.block
	data := trans.fetchedData

	// 2. 덮어쓰기 전에 기존 데이터를 읽어내어 Eviction 준비 (EvictAndPrefetch인 경우만)
	var oldData []byte
	var err error
	if trans.action == bankEvictAndPrefetch {
		oldData, err = s.cache.storage.Read(block.CacheAddress, 1<<s.cache.log2BlockSize)
		if err != nil {
			panic(err)
		}
	}

	// 3. MSHR 깨우기 (필요 시)
	if trans.mshrEntry != nil {
		s.cache.mshrStageBuffer.Push(trans.mshrEntry)
	}

	// 4. 새로운 Prefetch 데이터를 뱅크에 기록
	err = s.cache.storage.Write(block.CacheAddress, data)
	if err != nil {
		panic(err)
	}

	// ====================================================================
	// 5. [핵심 수정] 블록 상태 갱신은 조건문 "밖"에 있어야 합니다!
	// ====================================================================
	block.IsLocked = false
	block.IsValid = true
	block.IsDirty = false // (필요하다면 활성화)

	// 6. 기존 데이터를 writeBufferStage로 넘겨 하위로 Flush(Write)
	what := "Prefetch"
	if trans.action == bankEvictAndPrefetch {
		if trans.evictingAddr == 0 {
			trans.evictingAddr = block.Tag
			trans.evictingPID = block.PID
			trans.evictingDirtyMask = block.DirtyMask
		}
		trans.evictingData = oldData
		trans.action = writeBufferFlush // writeBufferStage가 이 데이터를 하단으로 내려보내도록 액션 지정

		// evictAndPrefetch()에서 추가한 evictingList 항목 제거.
		// 제거하지 않으면 해당 주소에 대한 이후 모든 요청이 영구적으로 차단됨.
		delete(s.cache.evictingList, trans.evictingAddr)

		// WriteBufferStage로 방출(Push)
		s.cache.writeBufferBuffer.Push(trans)

		// [핵심 수정] 하위로 보냈으므로 Downward 카운터 차감
		s.downwardInflightTransCount--
		what = "EvictAndPrefetch"

		if s.cache.debugProcess {
			fmt.Printf("[Prefetch Profiling] [%s]\tEvict And Prefetch: evict %x, prefetch %x\n", s.cache.name, trans.evictingAddr, block.Tag)
		}
	}

	s.inflightTransCount--

	if s.cache.debugProcess && trans.accessReq() != nil && trans.accessReq().GetAddress() == s.cache.debugAddress0 {
		fmt.Printf("[%s] [bankStage]\tReceived rsp - 2.3: addr %x, action %d\n", s.cache.name, trans.accessReq().GetAddress(), trans.action)
	}
	if s.cache.debugProcess && trans.accessReq() != nil && trans.accessReq().GetAddress() == s.cache.debugAddress1 {
		fmt.Printf("[%s] [bankStage]\tReceived rsp - 2.3: addr %x, action %d\n", s.cache.name, trans.accessReq().GetAddress(), trans.action)
	}

	// Deferred-invalidation: prefetched data was never returned to any
	// L1, so dropping the local L2 copy here is invisible to consumers.
	// For bankEvictAndPrefetch, the OLD victim was already pushed to
	// writeBufferBuffer above so its flush is unaffected.
	s.applyPendingInvalidation(block)

	s.cache.incEvent(what)
	tracing.TraceReqFinalize(trans.prefetch, s.cache)
	return true
}

// applyPendingInvalidation finalizes a deferred InvReq that was acked
// upstream by directoryStage.doInvalidation while this block was still
// IsLocked. The InvRsp has already been sent; CohDir considers this L2
// a non-sharer. Must be called AFTER the bank op consumes any block
// metadata it needs (e.g. PID/Tag/DirtyMask for the writeToHomeNode
// flush) and AFTER IsLocked has been cleared.
func (s *bankStage) applyPendingInvalidation(block *internal.Block) {
	if !block.PendingInvalidation {
		return
	}
	s.cache.eraseCacheLineFromRWMask(block.PID, block.VAddr)
	newBlk := &internal.Block{
		WayID:        block.WayID,
		SetID:        block.SetID,
		CacheAddress: block.CacheAddress,
	}
	*block = *newBlk
	s.cache.deferredInvApplied++
	s.cache.incEvent("DeferredInvApplied")
}

func (s *bankStage) removeTransaction(trans *transaction) {
	for i, t := range s.cache.inFlightTransactions {
		if trans == t {
			// fmt.Printf("%.10f, %s, trans %s removed in bank stage.\n",
			// 	now, s.cache.Name(), t.id)
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
