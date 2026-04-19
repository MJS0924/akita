package optdirectory

import (
	"fmt"
	"log"

	"github.com/sarchlab/akita/v4/pipelining"
	"github.com/sarchlab/akita/v4/sim"
)

type bankStage struct {
	cache  *Comp
	bankID int

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
	s.cache.localDirToBankBuffers[s.bankID].Clear()
	s.cache.remoteDirToBankBuffers[s.bankID].Clear()

	s.localPipeline.Clear()
	s.remotePipeline.Clear()
	s.localPostPipelineBuf.Clear()
	s.remotePostPipelineBuf.Clear()

	s.localInflightTransCount = 0
	s.remoteInflightTransCount = 0
}

func (s *bankStage) pullFromBuf(isLocal bool) bool {
	var pipeline pipelining.Pipeline
	var inBuf sim.Buffer

	// 라우팅
	if isLocal {
		pipeline = s.localPipeline
		inBuf = s.cache.localDirToBankBuffers[s.bankID]
	} else {
		pipeline = s.remotePipeline
		inBuf = s.cache.remoteDirToBankBuffers[s.bankID]
	}

	if !pipeline.CanAccept() {
		s.returnFalse1 = "pipeline cannot accept new trans"
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

	s.returnFalse1 = "There is no trans in dirToBankBuffers"
	return false
}

// [수정] 파라미터로 isLocal 플래그 추가
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
		if !bottomSenderBuf.CanPush() {
			s.returnFalse0 = "Cannot push to bottomSenderBuffer"
		} else {
			s.returnFalse0 = "Cannot push to mshrStageBuffer"
		}
		return false
	}

	for i := 0; i < postBuf.Size(); i++ {
		trans := postBuf.Get(i).(bankPipelineElem).trans
		done := false

		// [수정] 각 Action 함수에 목적지 버퍼(bottomSenderBuf, mshrStageBuf)를 인자로 넘겨줍니다.
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

			if !s.cache.debugProcess && trans.accessReq() != nil && trans.accessReq().GetAddress() == s.cache.debugAddress {
				if isLocal {
					fmt.Printf("[%s][DEBUG]\tReadReq received - 2: %x\n", s.cache.name, trans.accessReq().GetAddress())
				} else {
					fmt.Printf("[%s][DEBUG]\tRemote ReadReq received - 2: %x\n", s.cache.name, trans.accessReq().GetAddress())
				}
			}
			return true
		}
	}

	s.returnFalse0 = "There is no trasaction to be finalized"
	return false
}

func (s *bankStage) InsertNewEntry(trans *transaction, bottomSenderBuffer sim.Buffer, targetMshrBuf sim.Buffer) bool {
	blk := trans.block
	blk.Sharer = []sim.RemotePort{trans.accessReq().GetSrcRDMA()}
	s.cache.recordAccessMask(blk.Tag, trans.accessReq().GetSrcRDMA())
	s.cache.printAccessMask()
	if bit, ok := s.cache.gpuBitFromPort(trans.accessReq().GetSrcRDMA()); ok {
		s.cache.recordSharerRead(blk.Tag>>s.cache.log2BlockSize, bit)
	}

	s.cache.directory.Visit(blk)
	blk.IsLocked = false

	// [수정] 하드코딩된 버퍼 대신 인자로 받은 타겟 버퍼 사용
	bottomSenderBuffer.Push(trans)
	targetMshrBuf.Push(trans)

	return true
}
func (s *bankStage) EvictAndInsertNewEntry(trans *transaction, bottomSenderBuffer sim.Buffer, targetMshrBuf sim.Buffer) bool {
	blk := trans.block
	if trans.victim != nil && trans.victim.IsValid {
		s.cache.recordSharerInvalidate(trans.victim.Tag >> s.cache.log2BlockSize)
	}
	blk.Sharer = []sim.RemotePort{trans.accessReq().GetSrcRDMA()}
	s.cache.recordAccessMask(blk.Tag, trans.accessReq().GetSrcRDMA())
	s.cache.printAccessMask()
	if bit, ok := s.cache.gpuBitFromPort(trans.accessReq().GetSrcRDMA()); ok {
		s.cache.recordSharerRead(blk.Tag>>s.cache.log2BlockSize, bit)
	}

	s.cache.directory.Visit(blk)
	blk.IsLocked = false

	bottomSenderBuffer.Push(trans)
	targetMshrBuf.Push(trans)

	return true
}

func (s *bankStage) UpdateEntry(trans *transaction, bottomSenderBuffer sim.Buffer, targetMshrBuf sim.Buffer) bool {
	blk := trans.block
	blk.Sharer = s.appendSharer(blk.Sharer, trans.accessReq().GetSrcRDMA())
	s.cache.recordAccessMask(blk.Tag, trans.accessReq().GetSrcRDMA())
	s.cache.printAccessMask()
	if bit, ok := s.cache.gpuBitFromPort(trans.accessReq().GetSrcRDMA()); ok {
		s.cache.recordSharerRead(blk.Tag>>s.cache.log2BlockSize, bit)
	}

	s.cache.directory.Visit(blk)
	blk.IsLocked = false
	// trans.action = Nothing

	bottomSenderBuffer.Push(trans)
	targetMshrBuf.Push(trans)

	return true
}

func (s *bankStage) InvalidateAndUpdateEntry(trans *transaction, bottomSenderBuffer sim.Buffer, targetMshrBuf sim.Buffer) bool {
	blk := trans.block
	blk.Sharer = []sim.RemotePort{trans.accessReq().GetSrcRDMA()}
	s.cache.eraseAccessMask(blk.Tag)
	s.cache.recordAccessMask(blk.Tag, trans.accessReq().GetSrcRDMA())
	s.cache.printAccessMask()
	if bit, ok := s.cache.gpuBitFromPort(trans.accessReq().GetSrcRDMA()); ok {
		s.cache.recordSharerWrite(blk.Tag>>s.cache.log2BlockSize, bit)
	}

	s.cache.directory.Visit(blk)
	blk.IsLocked = false

	bottomSenderBuffer.Push(trans)
	targetMshrBuf.Push(trans)

	// fmt.Fprintf(os.Stderr, "[%s] [DEBUG]\tInvalidateAndupdateEntry - 1\n", s.cache.name)

	return true
}

func (s *bankStage) InvalidateEntry(trans *transaction, targetBottomSenderBuf sim.Buffer) bool {
	blk := trans.block
	trans.invalidationList = blk.Sharer
	blk.Sharer = nil
	s.cache.recordSharerInvalidate(blk.Tag >> s.cache.log2BlockSize)

	targetBottomSenderBuf.Push(trans)
	blk.IsLocked = false

	return true
}

func (s *bankStage) appendSharer(list []sim.RemotePort, sh sim.RemotePort) []sim.RemotePort {
	for _, element := range list {
		if element == sh {
			return list
		}
	}

	return append(list, sh)
}

// func (s *bankStage) removeTransaction(trans *transaction) {
// 	for i, t := range s.cache.inFlightTransactions {
// 		if trans == t {
// 			// fmt.Printf("%.10f, %s, trans %s removed in bank stage.\n",
// 			// 	now, s.cache.Name(), t.id)
// 			s.cache.inFlightTransactions = append(
// 				(s.cache.inFlightTransactions)[:i],
// 				(s.cache.inFlightTransactions)[i+1:]...)

// 			return
// 		}
// 	}

// 	now := s.cache.Engine.CurrentTime()

// 	fmt.Printf("%.10f, %s, Transaction %s not found\n",
// 		now, s.cache.Name(), trans.id)

// 	panic("transaction not found")
// }

// func (s *bankStage) processMSHREntry(trans *transaction) bool {
// 	progress := false

// 	blk := trans.block
// 	for _, t := range trans.mshrEntry.Requests {
// 		t = t.(*transaction)
// 		if t == trans {
// 			blk.Sharer = append(blk.Sharer, t.accessReq().GetSrcRDMA())
// 			t.action = Nothing
// 			s.cache.bottomSenderBuffer.Push(t)
// 		}
// 	}

// 	return progress
// }
