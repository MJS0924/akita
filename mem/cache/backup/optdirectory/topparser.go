package optdirectory

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

func (p *topParser) Tick() bool {
	if p.cache.state != cacheStateRunning {
		p.returnFalse = "cacheStateIsNotRunning"
		return false
	}

	req := p.cache.topPort.PeekIncoming()
	if req == nil {
		p.returnFalse = "request is nil"
		return false
	}

	if p.cache.flushLocalAccess && !strings.Contains(fmt.Sprintf("%s", req.Meta().Src), "RDMA") {
		p.cache.topPort.RetrieveIncoming()

		p.returnFalse = "Cache is flushing, request from local"
		return false
		// migration 중에는 local access 버려버리기
	}

	trans := &transaction{
		id:        sim.GetIDGenerator().Generate(),
		fromLocal: p.cache.fromLocal(req), // remote에서 발생한 request인지, local에서 발생한 request인지에 따라 동작이 달라질 수 있으므로 구분이 필요함
	}

	switch req := req.(type) {
	case *mem.InvReq:
		if !p.cache.bottomSenderBuffer.CanPush() {

			p.returnFalse = "Cannot push to bottomSenderBuffer"
			return false
		}

		p.cache.bottomSenderBuffer.Push(req)
		p.cache.topPort.RetrieveIncoming()

		return true

	case *mem.InvRsp:
		if !p.cache.invRspBuffer.CanPush() {

			p.returnFalse = "Cannot push InvRsp to buffer"
			return false
		}

		p.cache.invRspBuffer.Push(req)
		p.cache.topPort.RetrieveIncoming()

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

		if trans.fromLocal || !p.cache.toLocal(req.Address) { // local에서 L2 cache를 read하는 경우 또는 remote data를 read 하는 경우는 directory 확인이 필요 없음
			trans.action = Nothing

			if !p.cache.bottomSenderBuffer.CanPush() {

				p.returnFalse = "Cannot push to bottomSenderBuffer"
				return false
			}
			p.cache.bottomSenderBuffer.Push(trans)
			p.cache.topPort.RetrieveIncoming()

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

			if !p.cache.bottomSenderBuffer.CanPush() {

				p.returnFalse = "Cannot push to bottomSenderBuffer"
				return false
			}
			p.cache.bottomSenderBuffer.Push(trans)
			p.cache.topPort.RetrieveIncoming()

			return true
		}
	}

	if !p.cache.dirStageBuffer.CanPush() {
		p.returnFalse = "Cannot push to dirStageBuffer"
		return false
	}
	p.cache.dirStageBuffer.Push(trans)
	p.cache.topPort.RetrieveIncoming()

	// p.cache.inFlightTransactions = append(p.cache.inFlightTransactions, trans)

	return true
}
