package REC

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

	progress := false

	req := p.cache.topPort.PeekIncoming()
	if p.processReq(req, true) {
		p.cache.topPort.RetrieveIncoming()
		progress = true
	}

	req = p.cache.RDMAPort.PeekIncoming()
	if p.processReq(req, false) {
		p.cache.RDMAPort.RetrieveIncoming()
		progress = true
	}

	req = p.cache.RDMAInvPort.PeekIncoming()
	if p.processReq(req, false) {
		p.cache.RDMAInvPort.RetrieveIncoming()
		progress = true
	}

	return progress
}

func (p *topParser) processReq(req sim.Msg, fromLocal bool) bool {
	if req == nil {
		return false
	}

	if p.cache.flushLocalAccess && !strings.Contains(fmt.Sprintf("%s", req.Meta().Src), "RDMA") {
		p.cache.topPort.RetrieveIncoming()

		return false
		// migration 중에는 local access 버려버리기
	}

	trans := &transaction{
		id:        sim.GetIDGenerator().Generate(),
		fromLocal: fromLocal, // 수신 포트 기반으로 결정: topPort→true, RDMAPort→false
	}

	needsTracing := false
	traceWhat0 := ""
	traceWhat1 := ""

	switch req := req.(type) {
	case *mem.InvReq:
		if !p.cache.invReqBuffer.CanPush() {

			p.returnFalse = "Cannot push to bottomSenderBuffer"
			return false
		}

		if p.cache.debugProcess && req.Address == p.cache.debugAddress {
			fmt.Printf("[%s]\tReceive Invalidation Req: addr %x\n", p.cache.name, req.Address)
		}
		p.cache.invReqBuffer.Push(req)

		return true

	case *mem.InvRsp:
		if !p.cache.invRspBuffer.CanPush() {

			p.returnFalse = "Cannot push InvRsp to buffer"
			return false
		}

		// fmt.Printf("[%s]\tReceive Inv Rsp - 3.0: %s\n", p.cache.name, req.RespondTo)
		p.cache.invRspBuffer.Push(req)

		return true

	case *mem.ReadReq:
		trans.toLocal = p.cache.toLocal(req.Address)
		trans.read = req

		if p.cache.debugProcess && req.Address == p.cache.debugAddress {
			if trans.fromLocal {
				fmt.Printf("[%s] [topparser]\tReceived read req - 0: addr %x\n", p.cache.name, req.Address)
			} else {
				fmt.Printf("[%s] [topparser]\tReceived remote read req - 0: addr %x, src %s\n", p.cache.name, req.Address, req.SrcRDMA)
			}
		}

		needsTracing = true
		traceWhat0 = "ToRemoteData"
		traceWhat1 = "FromRemote"
		if trans.toLocal {
			traceWhat0 = "ToLocalData"
		}
		if trans.fromLocal {
			traceWhat1 = "FromLocal"
		}

		// 1. [Bypass 대상] Local에서 발생한 Local 데이터 Read 요청
		if trans.fromLocal || !trans.toLocal {
			trans.action = BypassingDirectory
			if !p.cache.localBypassBuffer.CanPush() {
				p.returnFalse = "Cannot push to localBypassBuffer"
				return false
			}
			p.cache.localBypassBuffer.Push(trans)

			tracing.TraceReqReceive(req, p.cache)
			tracing.AddTaskStep(
				tracing.MsgIDAtReceiver(req, p.cache),
				p.cache,
				traceWhat0,
			)
			tracing.TraceReqReceive(req, p.cache)
			tracing.AddTaskStep(
				tracing.MsgIDAtReceiver(req, p.cache),
				p.cache,
				traceWhat1,
			)
			return true
		}

	case *mem.WriteReq:
		trans.toLocal = p.cache.toLocal(req.Address)
		trans.write = req

		if p.cache.debugProcess && req.Address == p.cache.debugAddress {
			if trans.fromLocal {
				fmt.Printf("[%s] [topparser]\tReceived write req - 0: addr %x\n", p.cache.name, req.Address)
			} else {
				fmt.Printf("[%s] [topparser]\tReceived remote write req - 0: addr %x\n", p.cache.name, req.Address)
			}
		}

		needsTracing = true
		traceWhat0 = "ToRemoteData"
		traceWhat1 = "FromRemote"
		if trans.toLocal {
			traceWhat0 = "ToLocalData"
		}
		if trans.fromLocal {
			traceWhat1 = "FromLocal"
		}

		if !trans.toLocal { // remote data를 write 하는 경우는 directory 확인이 필요 없음
			trans.action = BypassingDirectory
			if !p.cache.localBypassBuffer.CanPush() {
				p.returnFalse = "Cannot push to localBypassBuffer"
				return false
			}
			p.cache.localBypassBuffer.Push(trans)

			tracing.TraceReqReceive(req, p.cache)
			tracing.AddTaskStep(
				tracing.MsgIDAtReceiver(req, p.cache),
				p.cache,
				traceWhat0,
			)
			tracing.TraceReqReceive(req, p.cache)
			tracing.AddTaskStep(
				tracing.MsgIDAtReceiver(req, p.cache),
				p.cache,
				traceWhat1,
			)

			return true
		}
	}

	var targetBuf sim.Buffer
	if trans.fromLocal {
		targetBuf = p.cache.localDirStageBuffer
	} else {
		targetBuf = p.cache.remoteDirStageBuffer
	}

	if !targetBuf.CanPush() {
		p.returnFalse = "Cannot push to target dirStageBuffer"
		return false
	}

	targetBuf.Push(trans)

	if needsTracing {
		tracing.TraceReqReceive(req, p.cache)
		tracing.AddTaskStep(
			tracing.MsgIDAtReceiver(req, p.cache),
			p.cache,
			traceWhat0,
		)
		tracing.TraceReqReceive(req, p.cache)
		tracing.AddTaskStep(
			tracing.MsgIDAtReceiver(req, p.cache),
			p.cache,
			traceWhat1,
		)
	}

	return true
}
