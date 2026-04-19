package REC

import (
	"fmt"

	"github.com/sarchlab/akita/v4/sim"
)

type mshrStage struct {
	cache *Comp

	processingTrans   *transaction
	isProcessingLocal bool // [추가] 현재 처리 중인 트랜잭션의 출처 추적
}

func (s *mshrStage) Tick() bool {
	progress := false

	// 0. 이미 처리 중인 트랜잭션이 있다면 이어서 진행
	if s.processingTrans != nil {
		return s.processOneReq() || progress
	}

	// 1. [수정] Remote 버퍼 우선 확인 (네트워크 트래픽 우선 해소)
	item := s.cache.remoteMshrStageBuffer.Peek()
	if item != nil {
		s.processingTrans = item.(*transaction)
		s.isProcessingLocal = false
		return s.processOneReq() || progress
	}

	// 2. [수정] Local 버퍼 확인
	item = s.cache.localMshrStageBuffer.Peek()
	if item != nil {
		s.processingTrans = item.(*transaction)
		s.isProcessingLocal = true
		return s.processOneReq() || progress
	}

	return progress
}

func (s *mshrStage) Reset() {
	s.processingTrans = nil

	// [수정] 분리된 버퍼 초기화
	s.cache.localMshrStageBuffer.Clear()
	s.cache.remoteMshrStageBuffer.Clear()
}

func (s *mshrStage) processOneReq() bool {
	progress := false
	trans := s.processingTrans
	blk := trans.block

	// [버그 수정 및 최적화] 처리된 요청을 슬라이스에서 즉시 제거하여 중복 Push 방지
	for len(trans.mshrEntry.Requests) > 0 {
		req := trans.mshrEntry.Requests[0]
		t := req.(*transaction)

		// [수정] 트랜잭션 출처에 따라 전송할 타겟 버퍼 분기
		var targetBuf sim.Buffer
		if t.fromLocal {
			targetBuf = s.cache.localBottomSenderBuffer
		} else {
			targetBuf = s.cache.remoteBottomSenderBuffer
		}

		if !targetBuf.CanPush() {
			return progress
		}

		if s.sharerExist(t.accessReq().GetSrcRDMA(), blk.SubEntry[trans.blockIdx].Sharer) {
			t.action = Nothing
		} else {
			t.action = UpdateEntry
			blk.SubEntry[trans.blockIdx].Sharer = append(
				blk.SubEntry[trans.blockIdx].Sharer,
				t.accessReq().GetSrcRDMA(),
			)
		}

		targetBuf.Push(t)

		// 처리 완료된 원소를 배열에서 안전하게 제거
		trans.mshrEntry.Requests[0] = nil
		trans.mshrEntry.Requests = trans.mshrEntry.Requests[1:]

		progress = true
	}

	s.cache.mshr.Remove(trans.mshrEntry.PID, trans.mshrEntry.Address)

	// [수정] 처리가 완료되었으므로 알맞은 버퍼에서 Pop
	if s.isProcessingLocal {
		s.cache.localMshrStageBuffer.Pop()
	} else {
		s.cache.remoteMshrStageBuffer.Pop()
	}
	s.processingTrans = nil

	return progress
}

func (s *mshrStage) appendSharer(sh sim.RemotePort, list []sim.RemotePort) []sim.RemotePort {
	// for _, p := range list {
	// 	if p == sh {
	// 		return list
	// 	}
	// }

	if s.sharerExist(sh, list) {
		return list
	}

	return append(list, sh)
}

func (s *mshrStage) sharerExist(sh sim.RemotePort, list []sim.RemotePort) bool {
	for _, p := range list {
		if fmt.Sprintf("%s", p) == fmt.Sprintf("%s", sh) {
			return true
		}
	}

	return false
}
