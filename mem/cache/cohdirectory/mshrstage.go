package coherence

import "github.com/sarchlab/akita/v4/sim"

type mshrStage struct {
	cache *Comp

	processingTrans *transaction

	returnFalse string
}

func (s *mshrStage) Tick() bool {
	item := s.cache.mshrStageBuffer.Peek()
	if item == nil {
		s.returnFalse = "There is no item from mshrStageBuffer"
		return false
	}

	s.processingTrans = item.(*transaction)

	return s.processOneReq()
}

func (s *mshrStage) Reset() {
	s.processingTrans = nil
	s.cache.mshrStageBuffer.Clear()
}

func (s *mshrStage) processOneReq() bool {
	progress := false

	blk := s.processingTrans.block
	for _, trans := range s.processingTrans.mshrEntry.Requests {
		if !s.cache.bottomSenderBuffer.CanPush() {
			if !progress {
				s.returnFalse = "[processOneReq] Cannot push to bottomSenderBuffer"
			}

			return progress
		}

		t := trans.(*transaction)
		blk.Sharer = s.appendSharer(t.accessReq().GetSrcRDMA(), blk.Sharer)
		t.action = Nothing
		s.cache.bottomSenderBuffer.Push(t)
		progress = true
	}

	s.cache.mshr.Remove(s.processingTrans.mshrEntry.PID, s.processingTrans.mshrEntry.Address)
	s.cache.mshrStageBuffer.Pop()
	s.processingTrans = nil

	// return progress
	return true
}

func (s *mshrStage) appendSharer(sh sim.RemotePort, list []sim.RemotePort) []sim.RemotePort {
	for _, p := range list {
		if p == sh {
			return list
		}
	}

	return append(list, sh)
}
