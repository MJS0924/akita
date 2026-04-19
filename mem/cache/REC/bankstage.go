package REC

import (
	"fmt"
	"log"

	"github.com/sarchlab/akita/v4/pipelining"
	"github.com/sarchlab/akita/v4/sim"
)

type bankStage struct {
	cache *Comp

	localPipeline         pipelining.Pipeline
	remotePipeline        pipelining.Pipeline
	localPostPipelineBuf  *bufferImpl
	remotePostPipelineBuf *bufferImpl

	pipelineWidth int

	localInflightTransCount  int
	remoteInflightTransCount int

	// Count the trans that needs to be sent to the write buffer.
	downwardInflightTransCount int

	returnFalse0 string
	returnFalse1 string
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
	return e.trans.req().Meta().ID + "_coherence_directory_bank_pipeline"
}

func (s *bankStage) Tick() (madeProgress bool) {
	for i := 0; i < s.cache.numReqPerCycle; i++ {
		// [수정] Local과 Remote 각각 Finalize 수행
		madeProgress = s.finalizeTrans(true) || madeProgress  // Local 처리
		madeProgress = s.finalizeTrans(false) || madeProgress // Remote 처리
	}

	// [수정] 양쪽 파이프라인 모두 Tick
	madeProgress = s.localPipeline.Tick() || madeProgress
	madeProgress = s.remotePipeline.Tick() || madeProgress

	for i := 0; i < s.cache.numReqPerCycle; i++ {
		// [수정] 양쪽 큐에서 각각 Pull 시도
		madeProgress = s.pullFromBuf(true) || madeProgress  // Local 당겨오기
		madeProgress = s.pullFromBuf(false) || madeProgress // Remote 당겨오기
	}

	return madeProgress
}

func (s *bankStage) Reset() {
	s.cache.localDirToBankBuffers.Clear()
	s.cache.remoteDirToBankBuffers.Clear()

	s.localPipeline.Clear()
	s.remotePipeline.Clear()
	s.localPostPipelineBuf.Clear()
	s.remotePostPipelineBuf.Clear()

	s.localInflightTransCount = 0
	s.remoteInflightTransCount = 0
}

func (s *bankStage) pullFromBuf(isLocal bool) bool {
	inBuf := s.cache.localDirToBankBuffers
	pipeline := s.localPipeline
	if !isLocal {
		inBuf = s.cache.remoteDirToBankBuffers
		pipeline = s.remotePipeline
	}

	if !pipeline.CanAccept() {
		return false
	}

	trans := inBuf.Pop()

	if trans != nil {
		pipeline.Accept(bankPipelineElem{trans: trans.(*transaction)})
		if isLocal {
			s.localInflightTransCount++
		} else {
			s.remoteInflightTransCount++
		}

		return true
	}

	return false
}

func (s *bankStage) finalizeTrans(isLocal bool) bool {
	var postBuf *bufferImpl
	var bottomSenderBuf sim.Buffer
	var mshrStageBuf sim.Buffer

	// 목적지 버퍼 라우팅
	if isLocal {
		postBuf = s.localPostPipelineBuf
		bottomSenderBuf = s.cache.localBottomSenderBuffer
		mshrStageBuf = s.cache.localMshrStageBuffer
	} else {
		postBuf = s.remotePostPipelineBuf
		bottomSenderBuf = s.cache.remoteBottomSenderBuffer
		mshrStageBuf = s.cache.remoteMshrStageBuffer
	}

	if !bottomSenderBuf.CanPush() || !mshrStageBuf.CanPush() {
		return false
	}

	for i := 0; i < postBuf.Size(); i++ {
		trans := postBuf.Get(i).(bankPipelineElem).trans

		if s.cache.debugProcess && trans.accessReq() != nil && trans.accessReq().GetAddress() == s.cache.debugAddress {
			if trans.fromLocal {
				fmt.Printf("[%s] [bankStage]\tReceived req - 2: addr %x\n", s.cache.name, trans.accessReq().GetAddress())
			} else {
				fmt.Printf("[%s] [bankStage]\tReceived remote req - 2: addr %x, action %d\n", s.cache.name, trans.accessReq().GetAddress(), trans.action)
			}
		}

		done := false

		switch trans.action {
		case InsertNewEntry:
			done = s.InsertNewEntry(trans, bottomSenderBuf, mshrStageBuf)
		case EvictAndInsertNewEntry:
			done = s.EvictAndInsertNewEntry(trans, bottomSenderBuf, mshrStageBuf)
		case UpdateEntry:
			done = s.UpdateEntry(trans, bottomSenderBuf, mshrStageBuf)
		case InvalidateAndUpdateEntry:
			done = s.InvalidateAndUpdateEntry(trans, bottomSenderBuf, mshrStageBuf)
		case InvalidateEntry:
			done = s.InvalidateEntry(trans, bottomSenderBuf)
		default:
			panic("bank action not supported")
		}

		if done {
			postBuf.Remove(i)
			if isLocal {
				s.localInflightTransCount--
			} else {
				s.remoteInflightTransCount--
			}

			return true
		}
	}

	return false
}

func (s *bankStage) InsertNewEntry(trans *transaction, bottomSenderBuffer sim.Buffer, targetMshrBuf sim.Buffer) bool {
	blk := trans.block
	entry := &blk.SubEntry[trans.blockIdx]
	entry.Sharer = nil
	entry.Sharer = append(entry.Sharer, trans.accessReq().GetSrcRDMA())

	if s.cache.debugPromotion {
		fmt.Printf("[%s]\tInsert New Entry: Addr %x, block %x, index %d, sharer ",
			s.cache.name, trans.accessReq().GetAddress(), blk.Tag, trans.blockIdx)
		for i := range entry.Sharer {
			fmt.Printf("%6.6s ", entry.Sharer[i])
		}
		fmt.Printf("\n")
	}

	entry.IsLocked = false
	if trans.read != nil && trans.read.FetchForWriteMiss {
		trans.action = Nothing
		// write miss를 위한 fetch는 cacheline 1개만 fetch해야 함
	}

	bottomSenderBuffer.Push(trans)
	targetMshrBuf.Push(trans)

	return true
}

func (s *bankStage) EvictAndInsertNewEntry(trans *transaction, bottomSenderBuffer sim.Buffer, targetMshrBuf sim.Buffer) bool {
	blk := trans.block
	entry := &blk.SubEntry[trans.blockIdx]
	entry.Sharer = nil
	entry.Sharer = append(entry.Sharer, trans.accessReq().GetSrcRDMA())

	entry.IsLocked = false

	bottomSenderBuffer.Push(trans)
	targetMshrBuf.Push(trans)

	return true
}

func (s *bankStage) UpdateEntry(trans *transaction, bottomSenderBuffer sim.Buffer, targetMshrBuf sim.Buffer) bool {
	blk := trans.block
	entry := &blk.SubEntry[trans.blockIdx]
	if s.sharerExist(entry.Sharer, trans.accessReq().GetSrcRDMA()) {
		trans.action = Nothing
	} else {
		entry.Sharer = s.appendSharer(entry.Sharer, trans.accessReq().GetSrcRDMA())
	}

	if s.cache.debugPromotion {
		fmt.Printf("[%s]\tUpdate Entry: Addr %x, block %x, index %d, sharer ",
			s.cache.name, trans.accessReq().GetAddress(), blk.Tag, trans.blockIdx)
		for i := range entry.Sharer {
			fmt.Printf("%6.6s ", entry.Sharer[i])
		}
		fmt.Printf("\n")
	}

	entry.IsLocked = false
	if trans.read != nil && trans.read.FetchForWriteMiss {
		trans.action = Nothing
		// write miss를 위한 fetch는 cacheline 1개만 fetch해야 함
	}

	bottomSenderBuffer.Push(trans)
	targetMshrBuf.Push(trans)

	if s.cache.debugProcess && trans.accessReq() != nil && trans.accessReq().GetAddress() == s.cache.debugAddress {
		if trans.fromLocal {
			fmt.Printf("[%s] [bankStage]\tReceived req - 2.1: addr %x\n", s.cache.name, trans.accessReq().GetAddress())
		} else {
			fmt.Printf("[%s] [bankStage]\tReceived remote req - 2.1: addr %x, action %d\n", s.cache.name, trans.accessReq().GetAddress(), trans.action)
		}
	}

	return true
}

func (s *bankStage) InvalidateAndUpdateEntry(trans *transaction, bottomSenderBuffer sim.Buffer, targetMshrBuf sim.Buffer) bool { // subentry 하나에 대해 invalidate
	blk := trans.block
	entry := &blk.SubEntry[trans.blockIdx]
	entry.Sharer = nil

	entry.IsLocked = false
	entry.IsValid = false
	entry.ReadCount = 0

	bottomSenderBuffer.Push(trans)
	targetMshrBuf.Push(trans)

	if !blk.IsValidEntry() {
		blk.IsValid = true
		for i := range blk.SubEntry { // 혹시 몰라서
			if i == trans.blockIdx {
				continue
			}

			e := &blk.SubEntry[i]
			e.Sharer = e.Sharer[:0]
			e.IsDirty = false
			e.IsLocked = false
			e.ReadCount = 0
		}
	}

	return true
}

func (s *bankStage) InvalidateEntry(trans *transaction, targetBottomSenderBuf sim.Buffer) bool {
	victim := trans.block

	victim.IsValid = false
	for i := range victim.SubEntry {
		victim.SubEntry[i].Sharer = nil
		victim.SubEntry[i].IsValid = false
		victim.SubEntry[i].IsLocked = false
		victim.SubEntry[i].ReadCount = 0
	}

	targetBottomSenderBuf.Push(trans)

	return true
}

func (s *bankStage) sharerExist(list []sim.RemotePort, sh sim.RemotePort) bool {
	for _, element := range list {
		if fmt.Sprintf("%s", element) == fmt.Sprintf("%s", sh) {
			return true
		}
	}

	return false
}

func (s *bankStage) appendSharer(list []sim.RemotePort, sh sim.RemotePort) []sim.RemotePort {
	if s.sharerExist(list, sh) {
		return list
	}

	return append(list, sh)
}
