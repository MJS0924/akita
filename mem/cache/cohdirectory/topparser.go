package coherence

import (
	"fmt"
	"strings"

	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/akita/v4/tracing"
)

type topParser struct {
	cache       *Comp
	returnFalse string
}

// Tick drains topPort + RDMAPort + RDMAInvPort + RDMAInvRspPort
// independently per cycle so that a backlog on one ingress port
// cannot head-of-line block the others (iter19 D1-D3-CD).
func (p *topParser) Tick() bool {
	if p.cache.state != cacheStateRunning {
		p.returnFalse = "cacheStateIsNotRunning"
		return false
	}

	progress := false

	req := p.cache.topPort.PeekIncoming()
	if p.processReq(req, true, p.cache.topPort) {
		p.cache.topPort.RetrieveIncoming()
		progress = true
	}

	if p.cache.RDMAPort != nil {
		req = p.cache.RDMAPort.PeekIncoming()
		if p.processReq(req, false, p.cache.RDMAPort) {
			p.cache.RDMAPort.RetrieveIncoming()
			progress = true
		}
	}

	if p.cache.RDMAInvPort != nil {
		req = p.cache.RDMAInvPort.PeekIncoming()
		if p.processReq(req, false, p.cache.RDMAInvPort) {
			p.cache.RDMAInvPort.RetrieveIncoming()
			progress = true
		}
	}

	if p.cache.RDMAInvRspPort != nil {
		req = p.cache.RDMAInvRspPort.PeekIncoming()
		if p.processReq(req, false, p.cache.RDMAInvRspPort) {
			p.cache.RDMAInvRspPort.RetrieveIncoming()
			progress = true
		}
	}

	return progress
}

func (p *topParser) processReq(req sim.Msg, fromLocal bool, srcPort sim.Port) bool {
	if req == nil {
		return false
	}

	if p.cache.flushLocalAccess && !strings.Contains(fmt.Sprintf("%s", req.Meta().Src), "RDMA") {
		// migration 중에는 local access 버려버리기
		srcPort.RetrieveIncoming()
		p.returnFalse = "Cache is flushing, request from local"
		return false
	}

	trans := &transaction{
		id:        sim.GetIDGenerator().Generate(),
		fromLocal: fromLocal,
	}

	switch req := req.(type) {
	case *mem.InvReq:
		if !p.cache.bottomSenderInvBuffer.CanPush() {
			p.returnFalse = "Cannot push to bottomSenderInvBuffer"
			return false
		}

		p.cache.bottomSenderInvBuffer.Push(req)

		return true

	case *mem.InvRsp:
		if !p.cache.invRspBuffer.CanPush() {
			p.returnFalse = "Cannot push InvRsp to buffer"
			return false
		}

		p.cache.invRspBuffer.Push(req)

		return true

	case *mem.ReadReq:
		tracing.TraceReqReceive(req, p.cache)
		what := ""
		if p.cache.toLocal(req.Address) {
			what = "ToLocalData"
		} else {
			what = "ToRemoteData"
		}
		tracing.AddTaskStep(
			tracing.MsgIDAtReceiver(req, p.cache),
			p.cache,
			what,
		)

		trans.read = req

		if trans.fromLocal || !p.cache.toLocal(req.Address) {
			// local에서 L2 cache를 read하는 경우 또는 remote data를 read 하는 경우는 directory 확인이 필요 없음
			trans.action = Nothing

			if !p.cache.bottomSenderTransBuffer.CanPush() {
				p.returnFalse = "Cannot push to bottomSenderTransBuffer"
				return false
			}
			p.cache.bottomSenderTransBuffer.Push(trans)

			return true
		}

	case *mem.WriteReq:
		tracing.TraceReqReceive(req, p.cache)
		what := ""
		if p.cache.toLocal(req.Address) {
			what = "ToLocalData"
		} else {
			what = "ToRemoteData"
		}
		tracing.AddTaskStep(
			tracing.MsgIDAtReceiver(req, p.cache),
			p.cache,
			what,
		)

		trans.write = req

		if !p.cache.toLocal(req.Address) { // remote data를 write 하는 경우는 directory 확인이 필요 없음
			trans.action = Nothing

			if !p.cache.bottomSenderTransBuffer.CanPush() {
				p.returnFalse = "Cannot push to bottomSenderTransBuffer"
				return false
			}
			p.cache.bottomSenderTransBuffer.Push(trans)

			return true
		}
	}

	if !p.cache.dirStageBuffer.CanPush() {
		p.returnFalse = "Cannot push to dirStageBuffer"
		return false
	}
	p.cache.dirStageBuffer.Push(trans)

	return true
}
