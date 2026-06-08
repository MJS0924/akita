package optdirectory

import (
	"fmt"

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

	// D1 (ported from SD 307dbe6): drain the dedicated InvRsp ingress.
	// Isolated from RDMAInvPort's InvReq backlog so it cannot be
	// head-blocked by a full invReqBuffer.
	req = p.cache.RDMAInvRspPort.PeekIncoming()
	if p.processReq(req, false) {
		p.cache.RDMAInvRspPort.RetrieveIncoming()
		progress = true
	}

	return progress
}

func (p *topParser) processReq(req sim.Msg, fromLocal bool) bool {
	if req == nil {
		p.returnFalse = "request is nil"
		return false
	}

	// if p.cache.flushLocalAccess && !strings.Contains(fmt.Sprintf("%s", req.Meta().Src), "RDMA") {
	// 	p.cache.topPort.RetrieveIncoming()

	// 	p.returnFalse = "Cache is flushing, request from local"
	// 	return false
	// 	// migration 중에는 local access 버려버리기
	// }

	trans := &transaction{
		id:        sim.GetIDGenerator().Generate(),
		fromLocal: fromLocal, // 수신 포트 기반으로 결정: topPort→true, RDMAPort→false
		enterTime: p.cache.Engine.CurrentTime(),
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
				fmt.Printf("[%s] [topparser]\tReceived remote read req - 0: addr %x\n", p.cache.name, req.Address)
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

		// 1. [Bypass 대상] Local-origin requests only. Bypass is a fast-path
		// for this GPU's own L1 traffic; external incoming (fromLocal=false)
		// must take the directory path so its responses are correctly
		// routed back to the sender's RDMA via sendToRemoteTopQue →
		// sendRemoteRspToTop → RDMAPort. The previous condition
		// (fromLocal || !toLocal) admitted external incoming with stale
		// home mapping (post-page-migration window) into bypass, whose
		// responses are forwarded only via topPort → never reach the
		// sender GPU's RDMA → causing transactionsFromOutside to stuck.
		if trans.fromLocal {
			trans.action = BypassingDirectory
			if !p.cache.localBypassBuffer.CanPush() {
				p.returnFalse = "Cannot push to localBypassBuffer"
				return false
			}
			trans.bottomEnterTime = p.cache.Engine.CurrentTime()
			trans.pathCategory = "bypass"
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

		trans.write = req

		// Bypass only this GPU's own L1 writes to remote data (fast-path);
		// external incoming writes (fromLocal=false) MUST take the
		// directory path. See ReadReq comment above for the rationale —
		// bypass response routing assumes local origin.
		if trans.fromLocal && !trans.toLocal {
			trans.action = BypassingDirectory
			if !p.cache.localBypassBuffer.CanPush() {
				p.returnFalse = "Cannot push to localBypassBuffer"
				return false
			}
			trans.bottomEnterTime = p.cache.Engine.CurrentTime()
			trans.pathCategory = "bypass"
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

	// [Deadlock 방지] 자원이 분리되었으므로, 기존의 인위적인 RDMA 예약 로직은 제거해도 안전합니다.
	if !targetBuf.CanPush() {
		p.returnFalse = "Cannot push to target dirStageBuffer"
		return false
	}

	if !trans.fromLocal && trans.accessReq() != nil && trans.accessReq().GetAddress() == 12888246464 {
		fmt.Printf("[CohDir %d][DEBUG]\tReadReq received - 0: %d\n", p.cache.deviceID, trans.accessReq().GetAddress())
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
