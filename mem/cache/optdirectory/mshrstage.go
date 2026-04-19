package optdirectory

import (
	"github.com/sarchlab/akita/v4/sim"
)

type mshrStage struct {
	cache *Comp

	processingTrans   *transaction
	isProcessingLocal bool // [추가] 현재 처리 중인 트랜잭션이 Local 버퍼에서 왔는지 추적

	returnFalse string
}

func (s *mshrStage) Tick() bool {
	// 이미 처리 중인 트랜잭션이 있다면 계속 진행
	if s.processingTrans != nil {
		return s.processOneReq()
	}

	// 1. Remote 버퍼 우선 확인
	item := s.cache.remoteMshrStageBuffer.Peek()
	if item != nil {
		s.processingTrans = item.(*transaction)
		s.isProcessingLocal = false
		return s.processOneReq()
	}

	// 2. Local 버퍼 확인
	item = s.cache.localMshrStageBuffer.Peek()
	if item != nil {
		s.processingTrans = item.(*transaction)
		s.isProcessingLocal = true
		return s.processOneReq()
	}

	s.returnFalse = "There is no item from mshrStageBuffers"
	return false
}

func (s *mshrStage) Reset() {
	s.processingTrans = nil
	s.cache.localMshrStageBuffer.Clear()
	s.cache.remoteMshrStageBuffer.Clear()
}

func (s *mshrStage) processOneReq() bool {
	progress := false
	blk := s.processingTrans.block

	for _, trans := range s.processingTrans.mshrEntry.Requests {
		t := trans.(*transaction)

		// [수정] 트랜잭션 출처에 따라 타겟 버퍼 분기
		var targetBuf sim.Buffer
		if t.fromLocal {
			targetBuf = s.cache.localBottomSenderBuffer
		} else {
			targetBuf = s.cache.remoteBottomSenderBuffer
		}

		if !targetBuf.CanPush() {
			if !progress {
				s.returnFalse = "[processOneReq] Cannot push to bottomSenderBuffer"
			}
			return progress
		}

		blk.Sharer = s.appendSharer(t.accessReq().GetSrcRDMA(), blk.Sharer)
		t.action = Nothing
		targetBuf.Push(t)

		// 처리 완료된 원소 제거
		s.processingTrans.mshrEntry.Requests[0] = nil
		s.processingTrans.mshrEntry.Requests = s.processingTrans.mshrEntry.Requests[1:]

		progress = true
	}

	s.cache.mshr.Remove(s.processingTrans.mshrEntry.PID, s.processingTrans.mshrEntry.Address)

	// [수정] 처리가 완료되었으므로 가져왔던 버퍼에서 Pop
	if s.isProcessingLocal {
		s.cache.localMshrStageBuffer.Pop()
	} else {
		s.cache.remoteMshrStageBuffer.Pop()
	}

	s.processingTrans = nil
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
