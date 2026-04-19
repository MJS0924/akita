package writebackcoh

import (
	"fmt"
	"log"
	"os"

	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/akita/v4/tracing"
)

type topParser struct {
	cache *Comp
}

func (p *topParser) Tick() bool {
	if p.cache.state != cacheStateRunning {
		return false
	}

	// 2. [핵심] 두 개의 입력 포트 확인 (Remote 포트 최우선 처리)
	var req sim.Msg
	var isLocal bool
	var targetPort sim.Port

	// 네트워크 데드락을 막기 위해 외부 패킷(RDMA)을 우선적으로 낚아챔
	req = p.cache.remoteTopPort.PeekIncoming()
	if req != nil {
		isLocal = false
		targetPort = p.cache.remoteTopPort
	} else {
		// 외부 패킷이 없을 때만 내부(Local L1) 패킷 확인
		req = p.cache.topPort.PeekIncoming()
		if req != nil {
			isLocal = true
			targetPort = p.cache.topPort
		} else {
			return false // 두 포트 모두 요청 없음
		}
	}

	trans := &transaction{
		id:        sim.GetIDGenerator().Generate(),
		fromLocal: isLocal,
	}

	switch req := req.(type) {
	case *mem.ReadReq:
		trans.toLocal = p.cache.toLocal(req.Address)
		trans.read = req
	case *mem.WriteReq:
		trans.toLocal = p.cache.toLocal(req.Address)
		trans.write = req
	case *mem.InvReq:
		trans.invalidation = req
		if isLocal {
			fmt.Fprintf(os.Stderr, "[%s]\t[WARNING]\tInvalidation from local\n", p.cache.name)
		}
	default:
		log.Panic(fmt.Sprintf("[%s]\nErr: Cannot handle req type\n", p.cache.name))
	}

	// 4. 버퍼 진입 가능 여부 확인 및 라우팅
	if p.cache.debugProcess && trans.accessReq() != nil && trans.accessReq().GetAddress() == p.cache.debugAddress0 {
		fmt.Printf("[%s] [topparser]\tReceived req - 0: addr %x\n", p.cache.name, trans.accessReq().GetAddress())
	}
	if p.cache.debugProcess && trans.accessReq() != nil && trans.accessReq().GetAddress() == p.cache.debugAddress1 {
		fmt.Printf("[%s] [topparser]\tReceived req - 0: addr %x\n", p.cache.name, trans.accessReq().GetAddress())
	}

	if isLocal {
		if !p.cache.dirStageBuffer.CanPush() {
			return false // 버퍼가 꽉 찼으면 해당 포트의 처리를 멈춤 (Stall)
		}
		p.cache.dirStageBuffer.Push(trans)
	} else {
		if !p.cache.remoteDirStageBuffer.CanPush() {
			return false
		}
		p.cache.remoteDirStageBuffer.Push(trans)
	}

	p.cache.inFlightTransactions = append(p.cache.inFlightTransactions, trans)
	tracing.TraceReqReceive(req, p.cache)

	// 5. 성공적으로 큐에 넣었을 때만 해당 포트에서 패킷을 완전히 제거 (Pop)
	targetPort.RetrieveIncoming()

	return true
}
