package optdirectory

import (
	"log"

	"github.com/sarchlab/akita/v4/pipelining"
	"github.com/sarchlab/akita/v4/sim"
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

		s.returnFalse1 = "pipeline cannot accept new trans"
		return false
	}

	inBuf := s.cache.dirToBankBuffers[s.bankID]
	trans := inBuf.Pop()

	if trans != nil {
		s.pipeline.Accept(bankPipelineElem{trans: trans.(*transaction)})
		s.inflightTransCount++

		return true
	}

	s.returnFalse1 = "There is no trans in dirToBankBuffers"
	return false
}

func (s *bankStage) finalizeTrans() bool {
	if !s.cache.bottomSenderBuffer.CanPush() || !s.cache.mshrStageBuffer.CanPush() {

		if !s.cache.bottomSenderBuffer.CanPush() {
			s.returnFalse0 = "Cannot push to bottomSenderBuffer"
		} else {
			s.returnFalse0 = "Cannot push to mshrStageBuffer"
		}
		return false
	}

	for i := 0; i < s.postPipelineBuf.Size(); i++ {
		trans := s.postPipelineBuf.Get(i).(bankPipelineElem).trans

		done := false

		switch trans.action {
		case InsertNewEntry:
			done = s.InsertNewEntry(trans)
		case EvictAndInsertNewEntry:
			done = s.EvictAndInsertNewEntry(trans)
		case UpdateEntry:
			done = s.UpdateEntry(trans)
		case InvalidateAndUpdateEntry:
			done = s.InvalidateAndUpdateEntry(trans)
		case InvalidateEntry:
			done = s.InvalidateEntry(trans)
		default:
			panic("bank action not supported")
		}

		if done {
			s.postPipelineBuf.Remove(i)
			s.inflightTransCount--

			return true
		}
	}

	s.returnFalse0 = "There is no trasaction to be finalized"
	return false
}

func (s *bankStage) InsertNewEntry(trans *transaction) bool {
	blk := trans.block
	blk.Sharer = s.appendSharer(blk.Sharer[:0], trans.accessReq().GetSrcRDMA())
	s.cache.recordAccessMask(blk.Tag, trans.accessReq().GetSrcRDMA())
	s.cache.printAccessMask()

	s.cache.directory.Visit(blk)
	blk.IsLocked = false
	// trans.action = Nothing
	// s.cache.bottomSenderBuffer.Push(trans)
	s.cache.mshrStageBuffer.Push(trans)

	return true
}

func (s *bankStage) EvictAndInsertNewEntry(trans *transaction) bool {
	blk := trans.block
	blk.Sharer = s.appendSharer(blk.Sharer[:0], trans.accessReq().GetSrcRDMA())
	s.cache.recordAccessMask(blk.Tag, trans.accessReq().GetSrcRDMA())
	s.cache.printAccessMask()

	s.cache.directory.Visit(blk)
	blk.IsLocked = false
	// s.cache.bottomSenderBuffer.Push(trans)
	s.cache.mshrStageBuffer.Push(trans)

	return true
}

func (s *bankStage) UpdateEntry(trans *transaction) bool {
	blk := trans.block
	blk.Sharer = s.appendSharer(blk.Sharer, trans.accessReq().GetSrcRDMA())
	s.cache.recordAccessMask(blk.Tag, trans.accessReq().GetSrcRDMA())
	s.cache.printAccessMask()

	s.cache.directory.Visit(blk)
	blk.IsLocked = false
	// trans.action = Nothing
	// s.cache.bottomSenderBuffer.Push(trans)
	s.cache.mshrStageBuffer.Push(trans)

	return true
}

func (s *bankStage) InvalidateAndUpdateEntry(trans *transaction) bool {
	blk := trans.block
	blk.Sharer = s.appendSharer(blk.Sharer[:0], trans.accessReq().GetSrcRDMA())
	s.cache.eraseAccessMask(blk.Tag)
	s.cache.recordAccessMask(blk.Tag, trans.accessReq().GetSrcRDMA())
	s.cache.printAccessMask()

	s.cache.directory.Visit(blk)
	blk.IsLocked = false
	// s.cache.bottomSenderBuffer.Push(trans)
	s.cache.mshrStageBuffer.Push(trans)

	return true
}

func (s *bankStage) InvalidateEntry(trans *transaction) bool {
	blk := trans.block
	trans.invalidationList = blk.Sharer
	blk.Sharer = blk.Sharer[:0]

	s.cache.bottomSenderBuffer.Push(trans)
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
