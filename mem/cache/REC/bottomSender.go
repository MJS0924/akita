package REC

import (
	"fmt"
	"strings"

	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/mempath"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/akita/v4/tracing"
)

type bottomSender struct {
	cache *Comp

	writeBufferCapacity      int
	maxInflightBypassRequest int
	maxInflightRequest       int
	maxInflightInvalidation  int
	// [OUTGOING-REMOTE CAP FIX, REC layer] Sub-cap of
	// maxInflightRequest that ONLY bounds the remote branch
	// (remoteInflightRequest). Keeps the remote inflight from
	// consuming the full maxInflightRequest budget so local-origin
	// reverse-flow (ack-return, bypass response, etc.) always has
	// guaranteed headroom. <= 0 disables. Mirrors the L2
	// outgoing-remote pending cap pattern at the bottomSender layer.
	maxOutgoingRemoteInflight int

	// [RDMA-LAYER DIAG] Counters for responses that arrived but
	// matched no inflight (bypass / local / remote) — silently
	// discarded in processWriteDoneRsp / processDataReadyRsp. The
	// counterpart of RDMA's lostRspFromL2Count, but at the REC layer
	// (responses arriving from peer GPU via the RDMA → REC.bottomPort
	// path). Non-zero suggests responses are dropping somewhere
	// upstream of REC's bookkeeping.
	lostWriteDoneRspCount    uint64
	lostDataReadyRspCount    uint64
	lostWriteDoneRspSampleID string
	lostDataReadyRspSampleID string
	// [ITER20 DIAG C] eviction-acks (WriteDoneRsp routed to sendToRemoteTopQue)
	// successfully egressed via RDMADataRspPort toward the original sender.
	evictAckEgressedCount uint64

	// [C2 observability] Per-PID counter of flushable local-origin
	// transactions silently dropped during a flush window
	// (flushLocalAccess=true). The topparser drops non-RDMA ingress
	// during migration to prevent in-flight local accesses from racing
	// the directory reset; counting them per-PID makes the drop volume
	// observable for post-mortem analysis. No semantic change.
	flushDropCount map[vm.PID]uint64

	// Phase 2 inv-emit budget (see cohdirectory/bottomSender.go for
	// rationale). Filtered by message type when draining the shared
	// queues: only InvReqs count against the cap.
	maxInvEmitPerCycle          int
	invEmittedToRDMAThisCycle   int
	invEmittedToBottomThisCycle int
	// [INV-FIDELITY C4] Third budget lane: dir→peer-dir InvReq fan-out via
	// RDMAInvPort (sendToRDMAInvQue), previously drained unbudgeted.
	invEmittedToPeerThisCycle int
	lastEmitCycleTime         sim.VTimeInSec

	localInflightRequest       []*transaction
	localInflightBypassRequest []*transaction
	remoteInflightRequest      []*transaction

	// [ITER17 F5b] Cap on the peer-incoming bypass lane added in
	// iter15. iter15's `if !isLocal { return false }` admitted peer
	// reqs unconditionally — bounded only by upstream peer's sender
	// cap, which is fine in symmetric workloads but pathological when
	// one GPU becomes a hot dest. Track numPeerInflightRequest
	// separately and cap at maxPeerInflightRequest (default 256).
	maxPeerInflightRequest int
	numPeerInflightRequest int
	// [PEER R/W SPLIT] peer-inflight READS only (subset of numPeerInflightRequest).
	// Reads are held to maxPeerInflightRequest-reserve so incoming WRITES
	// (evictions → DRAM sink) always retain a forwarding slot.
	numPeerInflightRead int

	// [DIAG] peerRspRecvCount = DataReadyRsp messages the dir received back
	// from the L2 (response path). peerRspClearedRemote = how many cleared a
	// remoteInflightRequest entry (decremented numPeerInflightRequest). If
	// the L2's peerReadServedCount is high but peerRspRecvCount stays low,
	// the L2's DataReadyRsp never reaches the dir. If recv is high but
	// cleared is low, the responses arrive but don't match the 256 stuck.
	peerRspRecvCount     uint64
	peerRspClearedRemote uint64

	// [ORIGIN-SPLIT] inflightInvToOutside partitioned by ORIGIN
	// (trans.fromLocal). Previously a single slice + single
	// maxInflightInvalidation bounded BOTH own-origin (fromLocal=true,
	// evict/write-driven) invalidations AND peer-serve-originated
	// (fromLocal=false, peer write/evict via RDMA) invalidations. An
	// own-origin inv-storm saturating all slots blocked every peer-serve
	// invalidation from being admitted → the peer's write/evict never
	// completes → no InvRsp/ack → the cross-GPU cycle holds. This is the
	// inv-path analogue of the already-fixed tooManyInflightRequest. The
	// cap is now an asymmetric soft split (own = max - max/4; peer keeps
	// the full cap incl. reserve), so an own inv-storm always leaves
	// headroom for peer-serve invalidations.
	inflightInvToOutsideOwn    []*transaction
	inflightInvToOutsideRemote []*transaction
	inflightInvToBottom        []*invTrans

	// Split pendingWriteAfterInv by trans.fromLocal direction to avoid
	// head-of-line stall under asymmetric soft cap (see superdirectory
	// for full rationale).
	pendingLocalWriteAfterInv  []*transaction
	pendingRemoteWriteAfterInv []*transaction

	sendToBottomQue       []sim.Msg
	sendToRemoteBottomQue []sim.Msg
	// [R3] Local bottom egress for InvReq (to own L2). Mirrors the
	// remote-side iter17 split: invalidation traffic gets its own
	// outbound lane so a data-path backpressure burst on bottomPort
	// cannot HoL-block invalidations into our own L2. Drained BEFORE
	// sendToBottomQue at the egress.
	sendToBottomInvQue []sim.Msg
	// Phase F equivalent for REC path. Inv send queue separated from
	// read/write so a backpressure burst on the data path cannot
	// HoL-block invalidation traffic. processInvalidationReq() pushes
	// here; sendToBottom() drains this BEFORE sendToRemoteBottomQue so
	// invalidations preempt regular requests at the egress.
	sendToRemoteBottomInvQue []sim.Msg
	// [ITER17 F4 / D7] sendToTopQue split by EGRESS PORT to eliminate
	// head-of-line blocking between unrelated traffic classes.  Before
	// the split: a single FIFO mixed outbound InvReq (→ RDMAInvPort),
	// outbound InvRsp (→ RDMAInvPort), local L1 RSPs (→ topPort), and
	// peer data RSPs (→ RDMAPort).  When ANY destination was jammed,
	// the head item could not drain and ALL types behind it HoL-blocked
	// — even though the other ports were idle.  Observed in iter16
	// stencil2d hang where invReqBuffer=16/16, localBottomSenderBuffer=
	// 16/16, RDMAInvPort.incomingBuf=32/32 all simultaneously full.
	//
	// New typed queues drain INDEPENDENTLY each Tick; a stalled port no
	// longer prevents progress on the other ports.
	sendToTopRspQue      []sim.Msg // local L1 RSPs → topPort
	sendToRDMADataRspQue []sim.Msg // peer data RSPs → RDMAPort
	// [R3] Split iter17's sendToRDMAInvQue into request vs response
	// lanes. Mixing them at the egress queue forced InvReq backpressure
	// (peer's RDMAInvPort.incomingBuf full) to HoL-block our outbound
	// InvRsp — which is exactly the traffic the peer needs to drain its
	// own inflightInvToBottom and free that very buffer. Split + drained
	// independently breaks that mutual-block cycle.
	sendToRDMAInvQue    []sim.Msg // outbound InvReq → RDMAInvPort
	sendToRDMAInvRspQue []sim.Msg // outbound InvRsp → RDMAInvPort
	sendToRemoteTopQue  []sim.Msg // remote(RDMAPort)로 나가야 하는 응답 전용 (Src에 RDMA 없는 쓰기 eviction 등)
	sendToDirQue        []*transaction
	bypassRspQue        []sim.Msg

	returnFalse0 string
	returnFalse1 string
	returnFalse2 string
}

func (bs *bottomSender) refreshEmitBudget() {
	now := bs.cache.Engine.CurrentTime()
	if now > bs.lastEmitCycleTime {
		bs.invEmittedToRDMAThisCycle = 0
		bs.invEmittedToBottomThisCycle = 0
		bs.invEmittedToPeerThisCycle = 0
		bs.lastEmitCycleTime = now
	}
}

func (bs *bottomSender) canEmitInvToRDMA() bool {
	return bs.maxInvEmitPerCycle <= 0 ||
		bs.invEmittedToRDMAThisCycle < bs.maxInvEmitPerCycle
}

func (bs *bottomSender) canEmitInvToPeer() bool {
	return bs.maxInvEmitPerCycle <= 0 ||
		bs.invEmittedToPeerThisCycle < bs.maxInvEmitPerCycle
}

// drainRDMAInvQueuePeerBudget drains one message from sendToRDMAInvQue with
// the per-cycle inv-emit budget applied to InvReq heads only. Responses
// routed into this queue (Dst contains "RDMAInv") flow unbudgeted — the
// iter19 invariant that rsp lanes are never throttled. A budget-deferred
// head retries next cycle; bounded pacing adds no wait-for edge.
// [INV-FIDELITY C4]
func (bs *bottomSender) drainRDMAInvQueuePeerBudget() bool {
	if len(bs.sendToRDMAInvQue) == 0 {
		return false
	}
	head := bs.sendToRDMAInvQue[0]
	_, headIsInv := head.(*mem.InvReq)
	if headIsInv && !bs.canEmitInvToPeer() {
		bs.cache.stallInvEmitPeer++
		return false
	}
	if !bs.cache.RDMAInvPort.CanSend() {
		bs.cache.stallTopPortBusy++
		return false
	}
	head.Meta().Src = bs.cache.RDMAInvPort.AsRemote()
	if err := bs.cache.RDMAInvPort.Send(head); err != nil {
		return false
	}
	bs.sendToRDMAInvQue[0] = nil
	bs.sendToRDMAInvQue = bs.sendToRDMAInvQue[1:]
	if headIsInv {
		bs.invEmittedToPeerThisCycle++
		bs.cache.invEmittedPeer++
	}
	return true
}

func (bs *bottomSender) canEmitInvToBottom() bool {
	return bs.maxInvEmitPerCycle <= 0 ||
		bs.invEmittedToBottomThisCycle < bs.maxInvEmitPerCycle
}

func (bs *bottomSender) Tick() bool {
	bs.refreshEmitBudget()

	madeProgress := false

	// [ITER6 RESPONSE PRIORITY] Tick re-ordered to drain RESPONSES first
	// (sendBypassRspToTop, sendRemoteRspToTop, sendToTop, processReturnRsp,
	// processInvalidationRsp) before sending new requests (processInputReq,
	// processBypassReq, processInvalidationReq, sendToBottom). This
	// ensures that within each tick the upstream sees the freshest
	// state (lower inflict counts) and is more likely to make
	// forward progress, breaking the closed wait cycle.
	temp := false

	// === RESPONSE PHASE: drain all responses first ===
	temp = bs.sendBypassRspToTop()
	madeProgress = madeProgress || temp
	temp = bs.sendRemoteRspToTop()
	madeProgress = madeProgress || temp
	temp = bs.sendToTop()
	madeProgress = madeProgress || temp
	if bs.cache.printReturn {
		fmt.Printf("[DEBUG CohDir %d]\treturn 1.3.5: %v\n", bs.cache.deviceID, temp)
	}

	temp = bs.processReturnRsp()
	madeProgress = madeProgress || temp
	if bs.cache.printReturn {
		fmt.Printf("[DEBUG CohDir %d]\treturn 1.3.1: %v\n", bs.cache.deviceID, temp)
	}

	temp = bs.processInvalidationRsp()
	madeProgress = madeProgress || temp
	if bs.cache.printReturn {
		fmt.Printf("[DEBUG CohDir %d]\treturn 1.3.3: %v\n", bs.cache.deviceID, temp)
	}

	// === REQUEST PHASE: process new requests after responses ===
	// [ITER14 workflow fix #6] processInputReq (peer-driven) runs
	// BEFORE processBypassReq (local L1 only). processInputReq drains
	// both remote+local BSB which carry peer + local mixed; doing it
	// first lets peer-incoming trans (already at remote BSB head) make
	// progress before local L1's bypass eats sendToBottomQue capacity.
	temp = bs.processInputReq()
	madeProgress = madeProgress || temp
	if bs.cache.printReturn {
		fmt.Printf("[DEBUG CohDir %d]\treturn 1.3.2: %v\n", bs.cache.deviceID, temp)
	}

	temp = bs.processBypassReq()
	madeProgress = madeProgress || temp
	if bs.cache.printReturn {
		fmt.Printf("[DEBUG CohDir %d]\treturn 1.3.0: %v\n", bs.cache.deviceID, temp)
	}

	temp = bs.processInvalidationReq()
	madeProgress = madeProgress || temp
	if bs.cache.printReturn {
		fmt.Printf("[DEBUG CohDir %d]\treturn 1.3.3: %v\n", bs.cache.deviceID, temp)
	}

	temp = bs.processPendingWriteAfterInv()
	madeProgress = madeProgress || temp

	temp = bs.sendToBottom()
	madeProgress = madeProgress || temp
	if bs.cache.printReturn {
		fmt.Printf("[DEBUG CohDir %d]\treturn 1.3.4: %v\n", bs.cache.deviceID, temp)
	}

	return madeProgress
}

// [추가] Bypass 전용 처리 함수
func (bs *bottomSender) processBypassReq() bool {
	// [FIX] bypass 경로에도 inflight 제한 적용 — but use the dedicated
	// bypass cap (1024 in builder), not the fetch cap (128). Previously
	// this used maxInflightRequest=128, which throttled bypass to ~100x
	// of the intended bandwidth and accounted for the entire ~78ns
	// dir_avg_latency vs CD's 1ns (Method E2 wait-tracker confirmed).
	// CD's processBypassReq has no such cap.
	if len(bs.localInflightBypassRequest) >= bs.maxInflightBypassRequest {
		return false // L2가 느릴 때 backpressure 전파
	}

	item := bs.cache.localBypassBuffer.Peek()
	if item == nil {
		return false
	}

	trans := item.(*transaction)

	req := bs.cache.cloneReq(trans.accessReq())
	req.Meta().Src = bs.cache.bottomPort.AsRemote()
	req.Meta().Dst = bs.cache.addressToPortMapper.Find(trans.accessReq().GetAddress())
	req.SetReqFrom(trans.accessReq().Meta().ID)

	bs.sendToBottomQue = append(bs.sendToBottomQue, req)

	// Bypass 버퍼에서 제거
	bs.cache.localBypassBuffer.Pop()

	// bs.localInflightBypassRequest = append(bs.localInflightBypassRequest, trans)
	bs.localInflightBypassRequest = append(bs.localInflightBypassRequest, trans)
	trans.reqToBottom = append(trans.reqToBottom, &req)
	trans.ack++

	bs.cache.actBypass++
	bs.cache.bottomSendCount++

	tracing.AddTaskStep(tracing.MsgIDAtReceiver(trans.accessReq(), bs.cache), bs.cache, "BypassToLocalL2")

	// Method E2: queueing-delay accounting for bypass trans.
	// For bypass, bottomEnterTime is stamped at topparser push to
	// localBypassBuffer, so waitDir is essentially the buffer-residency
	// before processBypassReq picks it up.
	now := bs.cache.Engine.CurrentTime()
	bs.cache.waitDirSum_bypass += trans.bottomEnterTime - trans.enterTime
	bs.cache.waitBottomSum_bypass += now - trans.bottomEnterTime
	bs.cache.waitCount_bypass++

	tracing.TraceReqComplete(trans.accessReq(), bs.cache)
	tracing.TraceReqFinalize(trans.accessReq(), bs.cache)

	return true
}

// [수정] 양쪽 큐를 모두 확인하여 데드락 방지
func (bs *bottomSender) processInputReq() bool {
	progress := false

	// [ITER14 instrumentation fix] Do NOT overwrite returnFalse2 in the
	// "Peek returned nil" branches when the OTHER buffer's processItem
	// already failed with a precise reason. The previous code masked
	// the actual send*-fail reason set by processNewTransaction, making
	// hang diagnosis impossible. Now: if buffer was non-empty and
	// processItem failed, returnFalse2 preserves the exact send* reason.

	// [R4] BSB split into Data + Inv classes (per trans.action). Drain
	// all 4 lanes each tick so an Inv-class HoL-stall (e.g. inflight cap
	// reached) cannot block Data-class trans behind it, and vice versa.
	// Order: Remote-Inv, Remote-Data, Local-Inv, Local-Data — remote
	// first preserves the iter6 cross-GPU forward-progress rule; inv
	// first within each side preserves the egress-priority rule.
	drainOne := func(buf sim.Buffer, isLocal bool) (sawItem, popped bool) {
		item := buf.Peek()
		if item == nil {
			return false, false
		}
		if bs.processItem(item, isLocal) {
			buf.Pop()
			return true, true
		}
		return true, false
	}

	anySaw := false

	rInvSaw, rInvPop := drainOne(bs.cache.remoteBSBInv, false)
	anySaw = anySaw || rInvSaw
	progress = progress || rInvPop

	rDataSaw, rDataPop := drainOne(bs.cache.remoteBSBData, false)
	anySaw = anySaw || rDataSaw
	progress = progress || rDataPop

	lInvSaw, lInvPop := drainOne(bs.cache.localBSBInv, true)
	anySaw = anySaw || lInvSaw
	progress = progress || lInvPop

	lDataSaw, lDataPop := drainOne(bs.cache.localBSBData, true)
	anySaw = anySaw || lDataSaw
	progress = progress || lDataPop

	// Only mark "all empty" when all four Peeks returned nil; otherwise
	// preserve the send*-failure reason already in returnFalse2.
	if !progress && !anySaw {
		bs.returnFalse2 = "all BSB lanes empty (no items to process)"
	}

	return progress
}

func (bs *bottomSender) processItem(item interface{}, isLocal bool) bool {
	switch req := item.(type) {
	case *transaction:
		return bs.processNewTransaction(req, isLocal)
	}
	bs.returnFalse2 = fmt.Sprintf("processItem: unknown type %T", item)
	return false
}

func (bs *bottomSender) processNewTransaction(trans *transaction, isLocal bool) bool {
	progress := false
	switch trans.action {
	case Nothing, InsertNewEntry, UpdateEntry:
		progress = bs.sendRequestToBottom(trans, isLocal)
		if !progress {
			bs.returnFalse2 = fmt.Sprintf("processNewTransaction: sendRequestToBottom returned false (action=%v, isLocal=%v, fromLocal=%v)", trans.action, isLocal, trans.fromLocal)
		}
	case EvictAndInsertNewEntry, InvalidateEntry: // entry 전체에 대한 invalidation, invalidation ack에서 사용량을 확인하여 demotion 결정
		progress = bs.sendInvalidationRequest(trans, isLocal)
		if !progress {
			bs.returnFalse2 = fmt.Sprintf("processNewTransaction: sendInvalidationRequest returned false (action=%v, isLocal=%v, fromLocal=%v)", trans.action, isLocal, trans.fromLocal)
		}
	case InvalidateAndUpdateEntry: // subentry 하나에 대한 invalidation
		progress = bs.sendInvalidationRequestByWrite(trans, isLocal)
		if !progress {
			bs.returnFalse2 = fmt.Sprintf("processNewTransaction: sendInvalidationRequestByWrite returned false (action=%v, isLocal=%v, fromLocal=%v)", trans.action, isLocal, trans.fromLocal)
		}
	case RemoteWriteHitPreserveWriter:
		// OP5b/REC fix: same wire behavior as InvalidateAndUpdateEntry
		// (send invalidations to non-writer sharers carried in
		// trans.invalidationList) but the directory state mutation in
		// bankstage preserves the writer instead of clearing all sharers.
		progress = bs.sendInvalidationRequestByWrite(trans, isLocal)
		if !progress {
			bs.returnFalse2 = fmt.Sprintf("processNewTransaction: sendInvalidationRequestByWrite (RemoteWriteHitPreserveWriter) returned false (isLocal=%v, fromLocal=%v)", isLocal, trans.fromLocal)
		}
	default:
		panic("unknown transaction action")
	}
	if progress {
		switch trans.action {
		case Nothing:
			bs.cache.actNothing++
		case InsertNewEntry:
			bs.cache.actInsertNew++
		case UpdateEntry:
			bs.cache.actUpdate++
		case EvictAndInsertNewEntry:
			bs.cache.actEvictInsert++
		case InvalidateEntry:
			bs.cache.actInvalidateEnt++
		case InvalidateAndUpdateEntry:
			bs.cache.actInvUpdate++
		}
	}

	// if progress {
	// 	temp := bs.cache.bottomSenderBuffer.Pop().(*transaction)
	// 	if temp.accessReq().Meta().ID != trans.accessReq().Meta().ID {
	// 		panic("Popped transaction mismatch")
	// 	}
	// }
	return progress
}

func (bs *bottomSender) sendRequestToBottom( // 단일 request만 전송
	trans *transaction,
	isLocal bool,
) bool {
	if bs.tooManyInflightRequest(trans.fromLocal, trans.write != nil) {
		bs.cache.stallInflightFetch++
		bs.returnFalse2 = fmt.Sprintf("sendRequestToBottom: tooManyInflightRequest=true (localInflight=%d/%d, fromLocal=%v)", len(bs.localInflightRequest), bs.maxInflightRequest, trans.fromLocal)
		return false
	}

	if bs.cache.debugProcess && trans.accessReq() != nil && trans.accessReq().GetAddress() == bs.cache.debugAddress {
		if trans.fromLocal {
			fmt.Printf("[%s] [bottomSender]\tReceived req - 3.1.1: addr %x\n", bs.cache.name, trans.accessReq().GetAddress())
		} else {
			fmt.Printf("[%s] [bottomSender]\tReceived remote req - 3.1.1: addr %x\n", bs.cache.name, trans.accessReq().GetAddress())
		}
	}

	srcPort := bs.cache.bottomPort
	portMapper := bs.cache.addressToPortMapper
	if !isLocal {
		srcPort = bs.cache.remoteBottomPort
		portMapper = bs.cache.addressToPortMapperForRemoteReq
	}

	req := bs.cache.cloneReq(trans.accessReq())
	req.Meta().Src = srcPort.AsRemote()
	req.Meta().Dst = portMapper.Find(trans.accessReq().GetAddress())
	req.SetReqFrom(trans.accessReq().Meta().ID)

	trans.reqToBottom = append(trans.reqToBottom, &req)
	trans.ack++

	// [수정] 전송 큐 분리 삽입
	if isLocal {
		bs.sendToBottomQue = append(bs.sendToBottomQue, req)
		bs.localInflightRequest = append(bs.localInflightRequest, trans)
	} else {
		bs.sendToRemoteBottomQue = append(bs.sendToRemoteBottomQue, req)
		bs.remoteInflightRequest = append(bs.remoteInflightRequest, trans)
	}
	// [DOWRITE-TRACE] traced cache line reached the L2 forward (past doWrite).
	if bs.cache.debugProcess {
		tLine, _ := getCacheLineID(bs.cache.debugAddress, bs.cache.log2BlockSize)
		if l, _ := getCacheLineID(trans.accessReq().GetAddress(), bs.cache.log2BlockSize); l == tLine {
			fmt.Printf("[%s][TRACE] sendRequestToBottom FORWARD-TO-L2 addr=%x isLocal=%v fromLocal=%v write=%v\n",
				bs.cache.name, trans.accessReq().GetAddress(), isLocal, trans.fromLocal, trans.write != nil)
		}
	}
	// [ITER17 F5b] Track peer-bypass inflight count so tooManyInflightRequest
	// can cap it. Origin = trans.fromLocal (peer if !fromLocal).
	if !trans.fromLocal {
		bs.numPeerInflightRequest++
		if trans.read != nil { // [PEER R/W SPLIT] track peer reads separately
			bs.numPeerInflightRead++
		}
	}

	bs.cache.bottomSendCount++

	// 동일한 region에 속한 영역에 대해 read request 전송
	if trans.read == nil {
		return true
	}

	what := "Nothing"
	if trans.action != Nothing {
		what = "UpdateEntry"
	}
	tracing.AddTaskStep(
		tracing.MsgIDAtReceiver(trans.accessReq(), bs.cache),
		bs.cache,
		what,
	)

	// Method E2: queueing-delay accumulation. pathCategory is "fast" or
	// "bank" depending on whether the trans came from directorystage's
	// fast-path or via the bank pipeline. For "bypass" trans this site
	// is not reached (they go through processBypassReq).
	now := bs.cache.Engine.CurrentTime()
	switch trans.pathCategory {
	case "fast":
		bs.cache.waitDirSum_fast += trans.bottomEnterTime - trans.enterTime
		bs.cache.waitBottomSum_fast += now - trans.bottomEnterTime
		bs.cache.waitCount_fast++
	case "bank":
		bs.cache.waitDirSum_bank += trans.bottomEnterTime - trans.enterTime
		bs.cache.waitBottomSum_bank += now - trans.bottomEnterTime
		bs.cache.waitCount_bank++
	}

	tracing.TraceReqComplete(trans.accessReq(), bs.cache)
	tracing.TraceReqFinalize(trans.accessReq(), bs.cache)

	return true
}

func (bs *bottomSender) sendInvalidationRequest(
	trans *transaction,
	isLocal bool,
) bool {
	// 1. [사전 검사] Bottom으로 요청을 내려보내야 하는 액션인데 여유 공간이 없다면 조기 리턴 (트랜잭션 증발 방지)
	// Cross-variant fairness: match CD (optdirectory)'s strict
	// semantics — every non-InvalidateEntry action is throttled by the
	// shared fetch-cap. Previously REC throttled only
	// EvictAndInsertNewEntry, letting InvalidateAndUpdateEntry slip
	// through and hide inv pressure on the fetch path.
	// [ITER17 F5a] Pass trans.fromLocal to match the semantic of the
	// iter15 peer-incoming bypass. The local `isLocal` parameter here
	// is the BSB-side flag (which BSB the trans was popped from), NOT
	// the request-origin flag. The bypass in tooManyInflightRequest is
	// keyed on origin (fromLocal=false → peer-incoming → don't gate on
	// own sender cap), so using `isLocal` here was a semantic mismatch
	// that let local sender flood the cap without throttling. Mirror
	// of sendRequestToBottom:334 which already uses trans.fromLocal.
	if trans.action != InvalidateEntry && bs.tooManyInflightRequest(trans.fromLocal, trans.write != nil) {
		bs.cache.stallInflightFetch++
		bs.returnFalse2 = fmt.Sprintf("sendInvalidationRequest: tooManyInflightRequest=true (localInflight=%d/%d, action=%v, fromLocal=%v)", len(bs.localInflightRequest), bs.maxInflightRequest, trans.action, trans.fromLocal)
		return false
	}

	if bs.tooManyInflightInvalidation(trans.fromLocal) {
		bs.cache.stallInflightInv++
		bs.returnFalse2 = fmt.Sprintf("sendInvalidationRequest: tooManyInflightInvalidation=true (own=%d remote=%d max=%d fromLocal=%v)", len(bs.inflightInvToOutsideOwn), len(bs.inflightInvToOutsideRemote), bs.maxInflightInvalidation, trans.fromLocal)
		return false
	}

	// 2. [대상 선별] victim.SubEntry를 순회하며 실제로 무효화 메시지를 보낼 외부 노드가 있는지 검사.
	// Self-filter는 write-driven inv (InvalidateAndUpdateEntry)에만 적용.
	// evict-driven inv는 self 포함 모든 sharer가 invalidate해야 함.
	isWriteDriven := trans.action == InvalidateAndUpdateEntry
	hasValidTargets := false
	hasAnySharer := false
	victim := &trans.victim
	for i := 0; i < len(victim.SubEntry); i++ {
		for _, sh := range victim.SubEntry[i].Sharer {
			if sh == "" {
				continue
			}
			hasAnySharer = true
			if isWriteDriven && sh == trans.accessReq().GetSrcRDMA() {
				continue
			}
			hasValidTargets = true
			break
		}
		if hasValidTargets {
			break
		}
	}
	if trans.action == EvictAndInsertNewEntry || trans.action == InvalidateAndUpdateEntry {
		if hasValidTargets {
			bs.cache.invSentCount++
		} else if hasAnySharer {
			// sharer existed but every one was the requester itself → silently dropped
			bs.cache.invSkippedSelfOnlyCount++
		}
	}

	// sample utilization once per eviction transaction
	if !trans.utilRecorded {
		numSub := 1 << bs.cache.log2NumSubEntry
		if numSub > 0 {
			validCount := 0
			for k := 0; k < numSub; k++ {
				if victim.SubEntry[k].IsValid {
					validCount++
				}
			}
			util := float64(validCount) / float64(numSub)
			bs.cache.evictEntryUtilSum += util
			bs.cache.evictEntryCount++
		}
		trans.utilRecorded = true
	}

	progress := false

	if bs.cache.debugProcess && trans.accessReq() != nil && trans.accessReq().GetAddress() == bs.cache.debugAddress {
		if trans.fromLocal {
			fmt.Printf("[%s] [bottomSender]\tReceived req - 3.1.3: addr %x\n", bs.cache.name, trans.accessReq().GetAddress())
		} else {
			fmt.Printf("[%s] [bottomSender]\tReceived remote req - 3.1.3: addr %x\n", bs.cache.name, trans.accessReq().GetAddress())
		}
	}

	// 3. 무효화 대상이 있을 때만 Inflight 큐에 등록하고 메시지 생성
	if hasValidTargets {
		// [ORIGIN-SPLIT] duplicate guard across BOTH origin lists; register
		// into the list matching trans.fromLocal.
		if !bs.inflightInvToOutsideContains(trans.accessReq().Meta().ID) {
			bs.appendInflightInvToOutside(trans)
			progress = true
		}

		addr := victim.Tag
		blkSize := bs.cache.log2BlockSize
		for i := 0; i < len(victim.SubEntry); i++ {
			e := &victim.SubEntry[i]
			addr = victim.Tag + uint64(i<<blkSize)

			for j := 0; j < len(e.Sharer); j++ {
				sh := e.Sharer[j]

				if sh == "" {
					continue
				}
				if isWriteDriven && sh == trans.accessReq().GetSrcRDMA() {
					continue
				}

				// [핵심 변경] topPort.Send()로 직결하지 않고, sendToTopQue에 삽입하여 중간 실패(네트워크 블로킹) 방지
				req := mem.InvReqBuilder{}.
					WithSrc(bs.cache.topPort.AsRemote()).
					WithDst(bs.cache.ToRDMAInv).
					WithAddress(addr).
					WithPID(trans.victim.PID).
					WithReqFrom(trans.accessReq().Meta().ID).
					WithDstRDMA(sh).
					Build()

				// [ITER17 F4/D7] outbound InvReq → RDMAInvPort.
				bs.sendToRDMAInvQue = append(bs.sendToRDMAInvQue, req)

				// Sharer 리스트에서 제거 및 pending 처리
				e.Sharer = append(e.Sharer[:j], e.Sharer[j+1:]...)
				j-- // 요소 삭제로 인한 인덱스 밀림 보정

				trans.pendingEviction = append(trans.pendingEviction, sh)
				progress = true

				what := ""
				if trans.action == EvictAndInsertNewEntry {
					what = "InvalidateByEviction"
				} else if trans.action == InvalidateAndUpdateEntry {
					what = "InvalidateByWrite"
				} else if trans.action == EvictAndPromotionEntry {
					what = "InvalidateByPromotion"
				} else if trans.action == EvictAndDemotionEntry {
					what = "InvalidateByDemotion"
				}
				if what != "" {
					tracing.AddTaskStep(tracing.MsgIDAtReceiver(trans.accessReq(), bs.cache), bs.cache, what)
				}
				if bs.cache.debugProcess && addr == bs.cache.debugAddress {
					fmt.Printf("[%s]\tSend Invalidation Request - 0.0: addr %x, dst %s\n", bs.cache.name, addr, sh)
				}
			}
		}
	} else {
		// [Deadlock 방지] 보낼 대상이 없으면 성공(true)한 것으로 간주하여 Pop 되도록 유도
		progress = true
	}

	tracing.TraceReqComplete(trans.accessReq(), bs.cache)
	tracing.TraceReqFinalize(trans.accessReq(), bs.cache)

	// 4. Bottom으로의 추가 요청 하달
	// EvictAndInsertNewEntry만 실제 데이터 요청이 발생하므로 Bottom으로 보냄
	if trans.action == EvictAndInsertNewEntry {
		if bs.sendRequestToBottom(trans, isLocal) {
			return true
		}
		// [ACK-LEAK FIX sister] The invalidation half already progressed
		// (InvReqs queued above), so the caller will pop this trans from the
		// BSB. If the data-write half is rejected here (inflight cap), the
		// write must NOT be lost with it — park it in the same bounded
		// pendingWriteAfterInv retry queue used by the InvalidateAndUpdate
		// path. processPendingWriteAfterInv re-issues it when the cap frees.
		if progress {
			trans.action = Nothing
			if trans.fromLocal {
				bs.pendingLocalWriteAfterInv = append(bs.pendingLocalWriteAfterInv, trans)
			} else {
				bs.pendingRemoteWriteAfterInv = append(bs.pendingRemoteWriteAfterInv, trans)
			}
		}
		return progress
	}

	return progress
}

func (bs *bottomSender) sendInvalidationRequestByWrite(
	trans *transaction,
	isLocal bool,
) bool {
	// Cross-variant fairness: write-induced inv path also respects the
	// fetch cap (matches CD strict semantics).
	// [ITER17 F5a] Pass trans.fromLocal (origin) instead of isLocal
	// (BSB side) — mirror of sendInvalidationRequest:421.
	if bs.tooManyInflightRequest(trans.fromLocal, trans.write != nil) {
		bs.cache.stallInflightFetch++
		bs.returnFalse2 = fmt.Sprintf("sendInvalidationRequestByWrite: tooManyInflightRequest=true (localInflight=%d/%d)", len(bs.localInflightRequest), bs.maxInflightRequest)
		return false
	}

	// 1. Inflight Invalidation 제한 검사
	if bs.tooManyInflightInvalidation(trans.fromLocal) {
		bs.cache.stallInflightInv++
		bs.returnFalse2 = fmt.Sprintf("sendInvalidationRequestByWrite: tooManyInflightInvalidation=true (own=%d remote=%d max=%d fromLocal=%v)", len(bs.inflightInvToOutsideOwn), len(bs.inflightInvToOutsideRemote), bs.maxInflightInvalidation, trans.fromLocal)
		return false
	}

	// 2. [수정] 보낼 대상(Target) 사전 선별
	var validTargets []sim.RemotePort
	for i := 0; i < len(trans.invalidationList); i++ {
		sh := trans.invalidationList[i]
		// 나 자신이거나 빈 포트면 제외
		if sh == trans.accessReq().GetSrcRDMA() || sh == "" {
			continue
		}
		validTargets = append(validTargets, sh)
	}

	progress := false

	// [ORIGIN-SPLIT] duplicate guard across BOTH origin lists.
	if !bs.inflightInvToOutsideContains(trans.accessReq().Meta().ID) {
		// [핵심 변경 2] 자원이 꽉 차서 Demoted Entry 생성에 실패하면 즉시 조기 리턴.
		// false를 반환하므로 processInputReq에서 Pop() 되지 않고, 다음 Tick에 재시도합니다.
		// if !bs.insertDemotedEntry(trans) {
		// 	return false
		// }

		// (이전 답변의 좀비 트랜잭션 방지 로직: 타겟이 있을 때만 Inflight 큐에 넣음)
		// [ORIGIN-SPLIT] register into the list matching trans.fromLocal.
		if len(validTargets) > 0 {
			bs.appendInflightInvToOutside(trans)
		}
		progress = true
	}

	if bs.cache.debugProcess && trans.accessReq() != nil && trans.accessReq().GetAddress() == bs.cache.debugAddress {
		if trans.fromLocal {
			fmt.Printf("[%s] [bottomSender]\tReceived req - 3.1.4: addr %x\n", bs.cache.name, trans.accessReq().GetAddress())
		} else {
			fmt.Printf("[%s] [bottomSender]\tReceived remote req - 3.1.4: addr %x\n", bs.cache.name, trans.accessReq().GetAddress())
		}
	}

	// 4. [수정] 선별된 타겟들에 대해 무효화 메시지 생성 및 안전한 큐잉
	if len(validTargets) > 0 {
		for _, sh := range validTargets {
			req := mem.InvReqBuilder{}.
				WithSrc(bs.cache.topPort.AsRemote()).
				WithDst(bs.cache.ToRDMAInv).
				WithAddress(trans.write.Address).
				WithPID(trans.write.PID).
				WithReqFrom(trans.accessReq().Meta().ID).
				WithDstRDMA(sh).
				WithIsWriteInv(true).
				Build()

			// [ITER17 F4/D7] outbound write-induced InvReq → RDMAInvPort.
			bs.sendToRDMAInvQue = append(bs.sendToRDMAInvQue, req)

			trans.pendingEviction = append(trans.pendingEviction, sh)
			progress = true

			what := fmt.Sprintf("InvalidateByWrite")
			tracing.AddTaskStep(tracing.MsgIDAtReceiver(trans.accessReq(), bs.cache), bs.cache, what)

			if bs.cache.debugProcess {
				if trans.write.Address == bs.cache.debugAddress {
					fmt.Printf("[%s]\tSend Invalidation Request - 0.1: addr %x, dst %s\n", bs.cache.name, trans.write.Address, sh)
				}
			}
		}
	} else if trans.write != nil {
		// [FIX] 무효화할 sharer가 없는 경우: invalidation 단계를 건너뛰고 즉시 L2 쓰기 진행.
		// validTargets == 0이면 transaction이 버퍼에서 pop된 뒤 소멸되어
		// WriteDoneRsp가 영원히 돌아오지 않는 데드락이 발생한다.
		trans.action = Nothing
		if trans.fromLocal {
			bs.pendingLocalWriteAfterInv = append(bs.pendingLocalWriteAfterInv, trans)
		} else {
			bs.pendingRemoteWriteAfterInv = append(bs.pendingRemoteWriteAfterInv, trans)
		}
		// [ITER11 FIX] missing progress=true. When i != -1 (already in
		// inflightInvToOutside) AND validTargets == 0, the original code
		// reached this branch with progress=false (init), causing the BSB
		// head to never pop. Setting progress=true here ensures the trans
		// is consumed once it's been moved to pendingWriteAfterInv.
		progress = true
	}

	// 5. 메시지 생성이 모두 끝났으므로 invalidationList 비움
	trans.invalidationList = nil

	tracing.TraceReqComplete(trans.accessReq(), bs.cache)
	tracing.TraceReqFinalize(trans.accessReq(), bs.cache)

	return progress
}

func (bs *bottomSender) processInvalidationReq() bool {
	item := bs.cache.invReqBuffer.Peek()
	if item == nil {
		bs.returnFalse1 = "There is no invalidation request from invReqBuffer"
		return false
	}

	if bs.tooManyInflightInvalidationToBottom() {
		bs.cache.stallInflightInv++
		return false
	}

	req := item.(*mem.InvReq)

	tr := invTrans{}
	tr.req = req

	addr := req.GetAddress()
	reqToBottom := mem.InvReqBuilder{}.
		WithSrc(bs.cache.remoteBottomPort.AsRemote()).
		WithDst(bs.cache.addressToPortMapperForRemoteReq.Find(addr)).
		WithPID(req.PID).
		WithAddress(addr).
		WithReqFrom(req.Meta().ID).
		WithIsWriteInv(req.IsWriteInv).
		Build()
	// Phase F equivalent: dedicated inv egress queue, drained before
	// sendToRemoteBottomQue to avoid HoL block by data-path stalls.
	bs.sendToRemoteBottomInvQue = append(bs.sendToRemoteBottomInvQue, reqToBottom)

	tr.ack++

	if bs.cache.debugProcess && addr == bs.cache.debugAddress {
		fmt.Printf("[%s]\tSend Invalidation Req to Bottom - 1: addr %x, dst %s\n", bs.cache.name, addr, reqToBottom.Dst)
	}

	bs.inflightInvToBottom = append(bs.inflightInvToBottom, &tr)
	bs.cache.invReqBuffer.Pop()

	return true
}

// [수정] 양쪽 Bottom 포트를 모두 폴링하도록 개편
func (bs *bottomSender) processReturnRsp() bool {
	madeProgress := false

	// 1. Remote 응답 포트 최우선 처리 (네트워크 정체 해소)
	msg := bs.cache.remoteBottomPort.PeekIncoming()
	if msg != nil {
		madeProgress = bs.processRspMsg(msg, bs.cache.remoteBottomPort) || madeProgress
	}

	// 2. Local 응답 포트 처리
	msg = bs.cache.bottomPort.PeekIncoming()
	if msg != nil {
		madeProgress = bs.processRspMsg(msg, bs.cache.bottomPort) || madeProgress
	}

	if !madeProgress {
		bs.returnFalse0 = "There is no msg from bottomPort"
	}

	return madeProgress
}

// [추가] 공통 라우팅 로직
func (bs *bottomSender) processRspMsg(msg sim.Msg, port sim.Port) bool {
	switch msg := msg.(type) {
	case *mem.DataReadyRsp:
		return bs.processDataReadyRsp(msg, port)
	case *mem.WriteDoneRsp:
		return bs.processWriteDoneRsp(msg, port)
	case *mem.InvRsp:
		return bs.processInvRspFromBottom(msg, port)
	default:
		panic("unknown msg type")
	}
}

func (bs *bottomSender) processDataReadyRsp(msg *mem.DataReadyRsp, port sim.Port) bool {
	bs.peerRspRecvCount++ // [DIAG] a DataReadyRsp reached the dir from the L2
	isBypass := false
	isLocal := false

	if msg.Origin == nil {
		fmt.Printf("[%s]\t[WARNING] Origin field is not valid\n", bs.cache.name)
	} else if msg.Origin.GetAddress() == 0 {
		fmt.Printf("[%s]\t[WARNING] Origin.Address field is not valid\n", bs.cache.name)
	}

	// 1. Bypass Inflight 배열에서 먼저 검색
	i, j := bs.findTransactionByID(msg.GetRspTo(), bs.localInflightBypassRequest)

	if i != -1 {
		isBypass = true
	} else {
		// 2. Local Inflight 배열 검색
		i, j = bs.findTransactionByID(msg.GetRspTo(), bs.localInflightRequest)
		if i != -1 {
			isLocal = true
		} else {
			// 3. Remote Inflight 배열 검색
			i, j = bs.findTransactionByID(msg.GetRspTo(), bs.remoteInflightRequest)
		}
	}

	if i == -1 {
		// superdirectory 환경에서 트랜잭션 유실을 추적하기 위해 기존 로그 유지
		// if bs.cache.debugProcess && msg.Origin.GetAddress() == bs.cache.debugAddress {
		// fmt.Printf("[%s] [bottomSender]\tDiscard read rsp - 3.2: addr %x\n", bs.cache.name, msg.Origin.GetAddress())
		// }
		// if msg.ID == "14861018" {
		// 	fmt.Fprintf(os.Stderr, "\tDiscard\n")
		// }
		// [RDMA-LAYER DIAG] Silent drop counter.
		bs.lostDataReadyRspCount++
		bs.lostDataReadyRspSampleID = msg.GetRspTo()
		port.RetrieveIncoming()
		return true
	}

	// 타겟 트랜잭션 포인터 획득
	var trans *transaction
	if isBypass {
		trans = bs.localInflightBypassRequest[i]
	} else if isLocal {
		trans = bs.localInflightRequest[i]
	} else {
		trans = bs.remoteInflightRequest[i]
	}

	// [핵심 변경 1] 여러 개의 하위 요청 중 완료된 것만 리스트에서 제거
	trans.reqToBottom[j] = nil
	trans.reqToBottom = append(trans.reqToBottom[:j], trans.reqToBottom[j+1:]...)

	// 하위 요청을 모두 응답받았을 때만 Inflight 큐에서 최종 삭제
	if len(trans.reqToBottom) == 0 {
		if isBypass {
			bs.removeInflightBypassRequest(i)
		} else {
			bs.removeInflightRequest(i, isLocal)
		}
	}

	// 응답 메시지 헤더 조작
	msg.RespondTo = trans.accessReq().Meta().ID
	msg.Src = bs.cache.topPort.AsRemote()
	msg.Dst = trans.accessReq().Meta().Src
	msg.WaitFor = trans.ack // [추가] 병합 처리를 위한 Ack 개수 전달
	if mempath.Enabled {
		msg.PathProbe = trans.accessReq().GetPathProbe()
	}

	// [핵심 변경 2] 직접 Send() 하지 않고, 용도에 맞는 전송 큐에 삽입 (블로킹 방지)
	if isBypass || trans.action == BypassingDirectory {
		bs.bypassRspQue = append(bs.bypassRspQue, msg)
	} else if !trans.fromLocal && !strings.Contains(fmt.Sprintf("%s", msg.Meta().Dst), "RDMA") {
		// remote 요청(RDMAPort 수신)의 응답인데 Dst에 "RDMA"가 없는 경우
		// (예: GPU[X].L2Cache.bottomPort 로부터 온 write eviction)
		// topPort로 보내면 도달 불가 → RDMAPort 전용 큐를 통해 전송
		bs.sendToRemoteTopQue = append(bs.sendToRemoteTopQue, msg)
	} else {
		// [ITER17 F4/D7] classify DataReadyRsp by egress port.
		dst := fmt.Sprintf("%s", msg.Meta().Dst)
		if strings.Contains(dst, "RDMAInv") {
			bs.sendToRDMAInvQue = append(bs.sendToRDMAInvQue, msg)
		} else if strings.Contains(dst, "RDMA") {
			bs.sendToRDMADataRspQue = append(bs.sendToRDMADataRspQue, msg)
		} else {
			bs.sendToTopRspQue = append(bs.sendToTopRspQue, msg)
		}
	}

	port.RetrieveIncoming()

	if bs.cache.debugProcess && msg.Origin.GetAddress() == bs.cache.debugAddress {
		fmt.Printf("[%s] [bottomSender]\tSend read rsp - 3.3: addr %x, dst %s\n", bs.cache.name, trans.read.Address, msg.Dst)
	}
	// if msg.ID == "14861018" {
	// 	fmt.Fprintf(os.Stderr, "\tSend read rsp: addr %x, dst %s, dstRDMA %s\n", trans.read.Address, msg.Dst)
	// }
	return true
}

func (bs *bottomSender) processWriteDoneRsp(msg *mem.WriteDoneRsp, port sim.Port) bool {
	isBypass := false
	isLocal := false

	// 1. Bypass Inflight 배열에서 먼저 검색
	i, j := bs.findTransactionByID(msg.GetRspTo(), bs.localInflightBypassRequest)

	if i != -1 {
		isBypass = true
	} else {
		// 2. Local Inflight 배열 검색
		i, j = bs.findTransactionByID(msg.GetRspTo(), bs.localInflightRequest)
		if i != -1 {
			isLocal = true
		} else {
			// 3. Remote Inflight 배열 검색
			i, j = bs.findTransactionByID(msg.GetRspTo(), bs.remoteInflightRequest)
		}
	}

	if i == -1 {
		// superdirectory의 디버깅 로그 유지
		if bs.cache.debugProcess && msg.Origin.GetAddress() == bs.cache.debugAddress {
			fmt.Printf("[%s] [bottomSender]\tDiscard write rsp - 3.4: addr %x\n", bs.cache.name, msg.Origin.GetAddress())
		}
		// [RDMA-LAYER DIAG] Silent drop counter.
		bs.lostWriteDoneRspCount++
		bs.lostWriteDoneRspSampleID = msg.GetRspTo()
		port.RetrieveIncoming()
		return true
	}

	// 타겟 트랜잭션 포인터 획득
	var trans *transaction
	if isBypass {
		trans = bs.localInflightBypassRequest[i]
	} else if isLocal {
		trans = bs.localInflightRequest[i]
	} else {
		trans = bs.remoteInflightRequest[i]
	}

	// [핵심 1] 여러 개의 하위 요청 중 완료된 것만 리스트에서 제거
	trans.reqToBottom[j] = nil
	trans.reqToBottom = append(trans.reqToBottom[:j], trans.reqToBottom[j+1:]...)

	// 하위 요청을 모두 응답받았을 때만 Inflight 큐에서 최종 삭제
	if len(trans.reqToBottom) == 0 {
		if isBypass {
			bs.removeInflightBypassRequest(i)
		} else {
			bs.removeInflightRequest(i, isLocal)
		}
	}

	msg.RespondTo = trans.accessReq().Meta().ID
	msg.Src = bs.cache.topPort.AsRemote()
	msg.Dst = trans.accessReq().Meta().Src
	msg.WaitFor = trans.ack
	if mempath.Enabled {
		msg.PathProbe = trans.accessReq().GetPathProbe()
	}

	// [핵심 2] 포트(topPort)에 직접 Send하지 않고 용도에 맞는 전송 큐에 삽입 (블로킹 방지)
	if isBypass || trans.action == BypassingDirectory {
		bs.bypassRspQue = append(bs.bypassRspQue, msg)
	} else if !trans.fromLocal && !strings.Contains(fmt.Sprintf("%s", msg.Meta().Dst), "RDMA") {
		// remote 요청(RDMAPort 수신)의 응답인데 Dst에 "RDMA"가 없는 경우
		// (예: GPU[X].L2Cache.bottomPort 로부터 온 write eviction)
		// topPort로 보내면 도달 불가 → RDMAPort 전용 큐를 통해 전송
		bs.sendToRemoteTopQue = append(bs.sendToRemoteTopQue, msg)
	} else {
		// [ITER17 F4/D7] classify WriteDoneRsp by egress port.
		dst := fmt.Sprintf("%s", msg.Meta().Dst)
		if strings.Contains(dst, "RDMAInv") {
			bs.sendToRDMAInvQue = append(bs.sendToRDMAInvQue, msg)
		} else if strings.Contains(dst, "RDMA") {
			bs.sendToRDMADataRspQue = append(bs.sendToRDMADataRspQue, msg)
		} else {
			bs.sendToTopRspQue = append(bs.sendToTopRspQue, msg)
		}
	}

	port.RetrieveIncoming()

	if bs.cache.debugProcess && trans.write != nil && trans.write.Address == bs.cache.debugAddress {
		fmt.Printf("[%s] [bottomSender]\tSend write rsp - 3.5: addr %x, dst %s\n", bs.cache.name, trans.write.Address, msg.Dst)
	}
	return true
}

func (bs *bottomSender) processInvRspFromBottom(rsp *mem.InvRsp, port sim.Port) bool {
	i := bs.findInvalidationByID(rsp.RespondTo, bs.inflightInvToBottom)
	if i == -1 {
		if bs.cache.debugProcess {
			fmt.Printf("[%s]\tCannot find transaction for InvRsp with RspTo %s\n", bs.cache.Name(), rsp.RespondTo)
		}
		port.RetrieveIncoming()
		return true
	}

	inflightInv := bs.inflightInvToBottom[i]
	inflightInv.ack--
	// superdirectory 고유 통계 데이터 누적 유지
	inflightInv.numInv = inflightInv.numInv + rsp.NumInv
	inflightInv.accessed = inflightInv.accessed + rsp.Accessed

	if inflightInv.ack > 0 {
		// [중요 버그 수정] 처리가 끝난 메시지는 반드시 버퍼에서 꺼내주어야(RetrieveIncoming) 데드락에 빠지지 않습니다.
		port.RetrieveIncoming()
		return true
	}

	req := inflightInv.req
	// [ITER19 INV-RSP ROUTE FIX] Rewrite InvRsp Dst RDMAInvInside ->
	// RDMAInvRspInside. Paired with draining sendToRDMAInvRspQue on the new
	// RDMAInvRspOutPort (ForInvRsp) below, so the InvRsp reaches RDMA's
	// processFromInvRspInside instead of landing on the InvReq connection.
	rspDst := sim.RemotePort(strings.Replace(
		string(req.Meta().Src), ".RDMAInvInside", ".RDMAInvRspInside", 1))
	rspToOutside := mem.InvRspBuilder{}.
		WithSrc(bs.cache.topPort.AsRemote()).
		WithDst(rspDst).
		WithRspTo(req.ReqFrom).
		WithNumInv(inflightInv.numInv).
		WithAccessed(inflightInv.accessed).
		WithSrcRDMA(req.DstRDMA).
		Build()

	// [R3] outbound InvRsp → dedicated InvRsp lane (Dst is peer GPU).
	// Split from the request lane so InvReq backpressure on RDMAInvPort
	// cannot HoL-block InvRsp drain.
	bs.sendToRDMAInvRspQue = append(bs.sendToRDMAInvRspQue, rspToOutside)

	port.RetrieveIncoming()

	// [핵심 추가] 처리가 완료된 트랜잭션을 Inflight 배열에서 안전하게 삭제
	bs.removeInflightInvalidation(i)

	return true
}

func (bs *bottomSender) processInvalidationRsp() bool {
	rsp := bs.cache.invRspBuffer.Pop()
	if rsp == nil {
		return false
	}

	switch rsp := rsp.(type) {
	case *mem.InvRsp:
		return bs.processInvRsp(rsp)
	default:
		panic("unknown msg type")
	}
}

func (bs *bottomSender) processInvRsp(rsp *mem.InvRsp) bool {
	// fmt.Printf("[%s.BS]\tF.0. Process InvRsp: rspTo %s, SrcRDMA %s\n", bs.cache.Name(), rsp.RespondTo, rsp.SrcRDMA)

	// [ORIGIN-SPLIT] search BOTH origin lists for the matching inv-inflight.
	list, i := bs.findInflightInvToOutsideByID(rsp.RespondTo)
	if i == -1 {
		// fmt.Printf("[%s]\tF. Cannot find transaction for InvRsp with RspTo %s\n", bs.cache.Name(), rsp.RespondTo)
		return true
	}
	trans := (*list)[i]

	for j, sh := range trans.pendingEviction {
		// fmt.Printf("[%s]\tF.1.0. Check pending eviction: %s\n", bs.cache.Name(), sh)

		// [수정] directory에서 적용한 안전한 문자열 변환 기반 포트 비교 적용
		if fmt.Sprintf("%s", sh) == fmt.Sprintf("%s", rsp.SrcRDMA) {
			trans.pendingEviction = append(trans.pendingEviction[:j], trans.pendingEviction[j+1:]...)

			// superdirectory 고유 로직 유지 (통계 누적)
			trans.numInv += rsp.NumInv
			trans.accessed += rsp.Accessed

			// fmt.Printf("[%s]\tF.1.1. Remove pending Eviction: %s\n", bs.cache.Name(), rsp.SrcRDMA)
			break
		}
	}

	// [수정] 대기 목록이 비워지면 안전한 헬퍼 함수를 사용하여 Inflight에서 트랜잭션 제거
	if len(trans.pendingEviction) == 0 {
		bs.removeInflightInvToOutsideFrom(list, i)
		// fmt.Printf("[%s]\tF.2. Remove inflight invalidation to outside\n", bs.cache.Name())

		// write에 의한 invalidation 완료 후 처리.
		//
		// EvictAndInsertNewEntry 의 경우 sendInvalidationRequest 에서
		// 이미 sendRequestToBottom 을 호출했으므로 trans 가 inflightRequest
		// 에 존재함. 여기서 pendingWriteAfterInv 로 재큐잉하면 L2 로 이중
		// 송신되어 inflight 카운터 폭발 → vgg16_REC 데드락 (모든 write 가
		// evict-insert 경로). 따라서 InvalidateAndUpdateEntry 경우에만
		// 재큐잉 (이쪽은 sendInvalidationRequest 가 sendRequestToBottom 을
		// 호출하지 않고 defer 함).
		//
		// [ACK-LEAK FIX] RemoteWriteHitPreserveWriter (OP5b) defers its L2
		// write behind the invalidations EXACTLY like InvalidateAndUpdateEntry
		// (sendInvalidationRequestByWrite never calls sendRequestToBottom up
		// front), but this requeue gate predated OP5b and excluded it — the
		// deferred peer write was silently LOST here once the last InvRsp
		// arrived: never forwarded to the home L2, so no WriteDoneRsp was
		// ever produced, permanently pinning one numRemoteInflEvictOwn slot
		// at the sender. Reciprocal leaks across 4 GPUs pinned all own-
		// eviction slots (96 cap) -> stencil2d REC freeze at win79.
		if trans.write != nil && (trans.action == InvalidateAndUpdateEntry ||
			trans.action == RemoteWriteHitPreserveWriter) {
			trans.action = Nothing
			if trans.fromLocal {
				bs.pendingLocalWriteAfterInv = append(bs.pendingLocalWriteAfterInv, trans)
			} else {
				bs.pendingRemoteWriteAfterInv = append(bs.pendingRemoteWriteAfterInv, trans)
			}
		}
	}

	return true
}

// processPendingWriteAfterInv tries to drain BOTH local and remote
// pending queues each Tick (see superdirectory equivalent for full
// rationale: avoids HoL stall when one fromLocal direction's quota
// is saturated under the asymmetric soft cap).
func (bs *bottomSender) processPendingWriteAfterInv() bool {
	madeProgress := false

	if len(bs.pendingLocalWriteAfterInv) > 0 {
		trans := bs.pendingLocalWriteAfterInv[0]
		if bs.sendRequestToBottom(trans, true) {
			bs.pendingLocalWriteAfterInv = bs.pendingLocalWriteAfterInv[1:]
			madeProgress = true
		}
	}

	if len(bs.pendingRemoteWriteAfterInv) > 0 {
		trans := bs.pendingRemoteWriteAfterInv[0]
		if bs.sendRequestToBottom(trans, false) {
			bs.pendingRemoteWriteAfterInv = bs.pendingRemoteWriteAfterInv[1:]
			madeProgress = true
		}
	}

	return madeProgress
}

func (bs *bottomSender) sendBypassRspToTop() bool {
	if len(bs.bypassRspQue) == 0 {
		return false
	}

	if !bs.cache.topPort.CanSend() {
		bs.cache.stallTopPortBusy++
		return false
	}

	msg := bs.bypassRspQue[0]
	err := bs.cache.topPort.Send(msg)

	if err != nil {
		return false
	}

	bs.bypassRspQue[0] = nil
	bs.bypassRspQue = bs.bypassRspQue[1:]

	return true
}

// sendRemoteRspToTop은 Dst에 "RDMA"가 없는 remote 응답을 RDMADataRspPort 를 통해 전송한다.
// GPU[X].L2Cache.bottomPort 로부터 온 write eviction 응답 등이 해당된다.
// [R2 BUGFIX] Use the new typed RDMADataRspPort. The legacy RDMAPort
// is still allocated but its name is NOT registered into RDMAToCohDir
// (only "RDMA" name string is plugged, but routing is by port-name
// string which is "<cache>.RDMAPort"). DirectConnection cannot find
// it, causing "port not found" panic. RDMADataRspPort is paired with
// rdma.RDMADataRspInside on RDMAToCohDirForDataRsp, so messages route.
func (bs *bottomSender) sendRemoteRspToTop() bool {
	if len(bs.sendToRemoteTopQue) == 0 {
		return false
	}

	if !bs.cache.RDMADataRspPort.CanSend() {
		bs.cache.stallTopPortBusy++
		return false
	}

	msg := bs.sendToRemoteTopQue[0]
	msg.Meta().Src = bs.cache.RDMADataRspPort.AsRemote()
	err := bs.cache.RDMADataRspPort.Send(msg)
	if err != nil {
		return false
	}
	bs.evictAckEgressedCount++ // [ITER20 DIAG C]

	bs.sendToRemoteTopQue[0] = nil
	bs.sendToRemoteTopQue = bs.sendToRemoteTopQue[1:]
	return true
}

// [ITER17 F4/D7] Drain each typed sub-queue INDEPENDENTLY.  A stalled
// egress port can no longer HoL-block the other ports.  Replaces the
// single sendToTopQue iteration that conditionally skipped RDMAPort
// but still serialized RDMAInvPort and topPort.
func (bs *bottomSender) sendToTop() bool {
	progress := false
	if bs.drainOneTypedQueue(&bs.sendToTopRspQue, bs.cache.topPort) {
		progress = true
	}
	// [R2 BUGFIX] sendToRDMADataRspQue drains via the typed RSP port
	// (paired with rdma.RDMADataRspInside via RDMAToCohDirForDataRsp).
	if bs.drainOneTypedQueue(&bs.sendToRDMADataRspQue, bs.cache.RDMADataRspPort) {
		progress = true
	}
	// [INV-FIDELITY C4] peer-lane InvReq drain honors maxInvEmitPerCycle.
	if bs.drainRDMAInvQueuePeerBudget() {
		progress = true
	}
	// [ITER19 INV-RSP ROUTE FIX] Drain the InvRsp lane on the dedicated
	// RDMAInvRspOutPort (on ForInvRsp), NOT RDMAInvPort (on ForInv). The
	// InvRsp Dst is now RDMA.RDMAInvRspInside which is only reachable on
	// ForInvRsp. InvReq egress stays on RDMAInvPort -> req/rsp fully split.
	if bs.drainOneTypedQueue(&bs.sendToRDMAInvRspQue, bs.cache.RDMAInvRspOutPort) {
		progress = true
	}
	return progress
}

// drainOneTypedQueue pops the head of `q` and Sends it via `port` (if
// the port has room).  Returns true if a message was sent.  Each queue
// is type-pure so no HoL across queues is possible.
func (bs *bottomSender) drainOneTypedQueue(q *[]sim.Msg, port sim.Port) bool {
	if len(*q) == 0 {
		return false
	}
	if !port.CanSend() {
		bs.cache.stallTopPortBusy++
		return false
	}
	msg := (*q)[0]
	msg.Meta().Src = port.AsRemote()
	if err := port.Send(msg); err != nil {
		return false
	}
	(*q)[0] = nil
	*q = (*q)[1:]
	return true
}

// [수정] 분할된 2개의 Bottom 포트 및 큐 처리
func (bs *bottomSender) sendToBottom() bool {
	madeProgress := false

	// Phase F equivalent: drain dedicated inv egress queue FIRST so
	// data-path backpressure cannot starve invalidation traffic. All
	// entries are *mem.InvReq by construction.
	if len(bs.sendToRemoteBottomInvQue) > 0 {
		head := bs.sendToRemoteBottomInvQue[0]
		if !bs.canEmitInvToRDMA() {
			// budget exhausted; defer to next cycle
		} else if bs.cache.remoteBottomPort.CanSend() {
			err := bs.cache.remoteBottomPort.Send(head)
			if err == nil {
				bs.sendToRemoteBottomInvQue[0] = nil
				bs.sendToRemoteBottomInvQue = bs.sendToRemoteBottomInvQue[1:]
				bs.invEmittedToRDMAThisCycle++
				madeProgress = true
			}
		} else {
			bs.cache.stallBottomPortBusy++
		}
	}

	// [R3] Local-bottom InvReq lane drained FIRST so data-path
	// backpressure on bottomPort cannot starve invalidation traffic
	// into our own L2. Queue is type-pure (only InvReq pushed here).
	if len(bs.sendToBottomInvQue) > 0 {
		head := bs.sendToBottomInvQue[0]
		if !bs.canEmitInvToBottom() {
			// budget exhausted; defer to next cycle
		} else if bs.cache.bottomPort.CanSend() {
			err := bs.cache.bottomPort.Send(head)
			if err == nil {
				bs.sendToBottomInvQue[0] = nil
				bs.sendToBottomInvQue = bs.sendToBottomInvQue[1:]
				bs.invEmittedToBottomThisCycle++
				madeProgress = true
			}
		} else {
			bs.cache.stallBottomPortBusy++
		}
	}

	// 1. Remote Bottom 전송 (우선)
	// [R3] Queue is type-pure (data-only) after split — InvReq routed to
	// sendToRemoteBottomInvQue at push-site. Runtime headIsInv check
	// removed; per-cycle inv-emit budget no longer applies here.
	if len(bs.sendToRemoteBottomQue) > 0 {
		head := bs.sendToRemoteBottomQue[0]
		if bs.cache.remoteBottomPort.CanSend() {
			err := bs.cache.remoteBottomPort.Send(head)
			if err == nil {
				bs.sendToRemoteBottomQue[0] = nil
				bs.sendToRemoteBottomQue = bs.sendToRemoteBottomQue[1:]
				madeProgress = true
			}
		} else {
			bs.cache.stallBottomPortBusy++
		}
	}

	// 2. Local Bottom 전송
	// [R3] Queue is type-pure (data-only) after split — InvReq routed to
	// sendToBottomInvQue at push-site. Runtime headIsInv check removed.
	if len(bs.sendToBottomQue) > 0 {
		head := bs.sendToBottomQue[0]
		if bs.cache.bottomPort.CanSend() {
			err := bs.cache.bottomPort.Send(head)
			if err == nil {
				bs.sendToBottomQue[0] = nil
				bs.sendToBottomQue = bs.sendToBottomQue[1:]
				madeProgress = true
			}
		} else {
			bs.cache.stallBottomPortBusy++
		}
	}

	return madeProgress
}

func (bs *bottomSender) sendToDir() bool {
	if len(bs.sendToDirQue) == 0 {
		return false
	}

	if !bs.cache.dirStageMotionBuffer.CanPush() {
		return false
	}

	msg := bs.sendToDirQue[0]
	bs.cache.dirStageMotionBuffer.Push(msg)

	bs.sendToDirQue = bs.sendToDirQue[1:]
	return true
}

// Asymmetric soft cap: remote can use the full maxInflightRequest
// budget; local is bounded at 3/4. Total inflight is hard-capped at
// maxInflightRequest. The 1/4 reserve protects remote from being
// starved by a local burst, while remote-dominant workloads can
// use the full budget when local is idle. Mirrors the writebackcoh
// L2 writeBufferStage tooManyInflightEvictions scheme.
//
// [OUTGOING-REMOTE CAP FIX, REC layer] Additionally cap the remote
// branch at maxOutgoingRemoteInflight (default = 3/4 of
// maxInflightRequest). With cap=128 and outgoing-remote cap=96, the
// remaining 32 slots stay headroom for local-origin replies/flows
// that must still drain when remote is saturated. Mirrors the L2-
// level pattern (numPendingRemoteEvictions + numRemoteInflightEviction
// <= maxOutgoingRemotePending) that closes the cross-GPU writeBuffer
// cycle; this REC variant closes the parallel cycle observed at the
// bottomSender layer (stencil2d REC sim 19.798 ms stall, where
// remoteInflightRequest=123 of total cap=128 saturated REC's
// inflight bookkeeping, head-blocking incoming requests at
// remoteDirStageBuffer).
func (bs *bottomSender) tooManyInflightRequest(isLocal bool, isWrite bool) bool {
	// [ITER15 PEER-INCOMING BYPASS LANE]
	// iter14 instrumentation (preserved returnFalse2) captured the exact
	// hang reason at stencil2d sim 17.12 ms / 615 windows:
	//   GPU[3].REC.bottomSender.returnFalse2 =
	//   "processNewTransaction: sendRequestToBottom returned false
	//    (action=3, isLocal=false, fromLocal=false)"
	// State at hang: localInflightRequest=0, remoteInflightRequest=128.
	// total = 0+128 = 128 = maxInflightRequest → cap hit. The shared
	// cap blocks ALL peer-incoming (fromLocal=false) sendRequestToBottom
	// admissions even though local sender is idle. Without that lane
	// open, the peer cannot get its req admitted into our REC pipeline
	// → no ACK back to peer → peer's own cap stays held → symmetric
	// cross-GPU cycle. Mirrors the L2 writebackcoh wB peer-incoming
	// bypass introduced in iter13 fix #1, applied here at the REC
	// inflight bookkeeping. Sender-side caps are unchanged so total
	// system load remains bounded.
	if !isLocal {
		// [PEER R/W SPLIT] Reserve a sub-pool of the peer-inflight cap for
		// incoming WRITES (peer evictions → home DRAM, a guaranteed sink:
		// they always complete and ACK, freeing the slot). Without the
		// reserve, incoming READS (→ home L2, which can stall) saturate the
		// whole maxPeerInflightRequest, so the dir cannot forward a single
		// eviction → that eviction never reaches DRAM → no WriteDoneRsp ACK
		// → the sender's outgoing-eviction slot stays held → symmetric
		// cross-GPU tail deadlock (win79: GPU[3] hot home, all 256 peer-
		// inflight were READs, peerWriteAckSent confirms writes flowed until
		// the cap saturated with reads). Partition of the existing cap —
		// NOT an increase. Writes may use the full cap (incl. the reserve);
		// reads are held to maxPeerInflightRequest-reserve.
		if bs.maxPeerInflightRequest <= 0 {
			return false
		}
		if isWrite {
			return bs.numPeerInflightRequest >= bs.maxPeerInflightRequest
		}
		reserveWrite := bs.maxPeerInflightRequest / 4
		return bs.numPeerInflightRead >= bs.maxPeerInflightRequest-reserveWrite
	}
	// [ITER20 FIX] LOCAL inflight cap ONLY — not the shared (local+remote)
	// total. win80 deadlock root: the shared `total >= maxInflightRequest`
	// check let a saturated REMOTE branch HoL-block LOCAL forwarding even
	// when local was nearly idle — captured at the hang as
	// returnFalse2="tooManyInflightRequest localInflight=9/128" while
	// remoteInflight≈119 pinned total≥128. The blocked local path jams
	// L1→dir→L2→writeBuffer, freezing the GPU and starving the cross-GPU
	// eviction-ack flow. Peer-incoming (remote) admissions are bounded
	// separately by maxPeerInflightRequest in the !isLocal lane above, so
	// dropping the shared total adds no unbounded growth and no cap
	// increase. Mirrors the L2 writebackcoh numLocal/numRemoteInflight
	// Eviction split (ITER16).
	localLimit := bs.maxInflightRequest - bs.maxInflightRequest/4
	return len(bs.localInflightRequest) >= localLimit
}

// tooManyInflightInvalidation gates an outbound invalidation admit by ORIGIN
// against the shared maxInflightInvalidation budget, using the SAME asymmetric
// soft-cap pattern as tooManyInflightRequest: own-origin (fromLocal=true) is
// held to max - max/4 so a local eviction/write inv-storm always leaves the
// reserve free for peer-serve (fromLocal=false) invalidations, which may use
// the full cap. The total occupancy (both lists) is still hard-bounded by
// maxInflightInvalidation. [ORIGIN-SPLIT] inv-path analogue of the already-
// fixed tooManyInflightRequest.
func (bs *bottomSender) tooManyInflightInvalidation(fromLocal bool) bool {
	total := bs.inflightInvToOutsideLen()
	if total >= bs.maxInflightInvalidation {
		return true
	}
	if fromLocal {
		localLimit := bs.maxInflightInvalidation - bs.maxInflightInvalidation/4
		return len(bs.inflightInvToOutsideOwn) >= localLimit
	}
	return false
}

func (bs *bottomSender) tooManyInflightInvalidationToBottom() bool {
	return len(bs.inflightInvToBottom) >= bs.maxInflightInvalidation
}

func (bs *bottomSender) Reset() {
	bs.cache.localBypassBuffer.Clear()

	bs.localInflightRequest = nil
	bs.remoteInflightRequest = nil

	bs.inflightInvToBottom = nil
	// [ORIGIN-SPLIT] clear both inv-inflight origin lists.
	bs.inflightInvToOutsideOwn = nil
	bs.inflightInvToOutsideRemote = nil
	bs.pendingLocalWriteAfterInv = nil
	bs.pendingRemoteWriteAfterInv = nil
	// [ITER17 F4/D7] clear typed sub-queues.
	bs.sendToTopRspQue = nil
	bs.sendToRDMADataRspQue = nil
	bs.sendToRDMAInvQue = nil
	// [R3] clear new InvRsp + local-InvReq lanes.
	bs.sendToRDMAInvRspQue = nil
	bs.sendToBottomInvQue = nil
	bs.sendToRemoteTopQue = nil
	bs.sendToBottomQue = nil
	bs.sendToRemoteBottomQue = nil
	bs.sendToRemoteBottomInvQue = nil
	// [R4] clear new split BSB buffers (data + inv class, both sides).
	bs.cache.localBSBData.Clear()
	bs.cache.localBSBInv.Clear()
	bs.cache.remoteBSBData.Clear()
	bs.cache.remoteBSBInv.Clear()
	bs.sendToDirQue = nil
	bs.bypassRspQue = nil
	bs.numPeerInflightRequest = 0 // [ITER17 F5b]
}

// func (bs *bottomSender) findTransactionByReqIDToBottom(ID string, list []*transaction) int {
// 	for i, tr := range list {
// 		if tr.reqIDToBottom == ID {
// 			return i
// 		}
// 	}
// 	return -1
// }

func (bs *bottomSender) findTransactionByID(ID string, list []*transaction) (int, int) {
	for i, tr := range list {
		for j, req := range tr.reqToBottom {
			if req == nil {
				continue
			}

			if (*req).Meta().ID == ID {
				return i, j
			}
		}
	}
	return -1, -1
}

func (bs *bottomSender) findInvTransactionByID(ID string, list []*transaction) int {
	for i, tr := range list {
		if tr.accessReq().Meta().ID == ID {
			return i
		}
	}
	return -1
}

// [ORIGIN-SPLIT] inflightInvToOutsideLen reports the combined depth of the
// two ORIGIN inv-inflight lists.
func (bs *bottomSender) inflightInvToOutsideLen() int {
	return len(bs.inflightInvToOutsideOwn) + len(bs.inflightInvToOutsideRemote)
}

// inflightInvToOutsideContains reports whether either ORIGIN list already
// holds an inv-inflight transaction with the given access-req ID (the
// duplicate guard the append sites used to express via findInvTransactionByID
// == -1).
func (bs *bottomSender) inflightInvToOutsideContains(ID string) bool {
	if bs.findInvTransactionByID(ID, bs.inflightInvToOutsideOwn) != -1 {
		return true
	}
	return bs.findInvTransactionByID(ID, bs.inflightInvToOutsideRemote) != -1
}

// appendInflightInvToOutside registers an inv-inflight transaction into the
// list matching its ORIGIN (trans.fromLocal).
func (bs *bottomSender) appendInflightInvToOutside(trans *transaction) {
	if trans.fromLocal {
		bs.inflightInvToOutsideOwn = append(bs.inflightInvToOutsideOwn, trans)
	} else {
		bs.inflightInvToOutsideRemote = append(bs.inflightInvToOutsideRemote, trans)
	}
}

// findInflightInvToOutsideByID searches BOTH ORIGIN lists for a transaction
// matching the response's RespondTo ID, returning the owning slice pointer
// and the index, or (nil, -1) if not found.
func (bs *bottomSender) findInflightInvToOutsideByID(ID string) (*[]*transaction, int) {
	if i := bs.findInvTransactionByID(ID, bs.inflightInvToOutsideOwn); i != -1 {
		return &bs.inflightInvToOutsideOwn, i
	}
	if i := bs.findInvTransactionByID(ID, bs.inflightInvToOutsideRemote); i != -1 {
		return &bs.inflightInvToOutsideRemote, i
	}
	return nil, -1
}

// removeInflightInvToOutsideFrom removes index i from the given ORIGIN list.
func (bs *bottomSender) removeInflightInvToOutsideFrom(list *[]*transaction, i int) {
	if i < 0 || i >= len(*list) {
		panic(fmt.Sprintf("Trying to remove inflightInvToOutside at out of bounds index %d", i))
	}
	copy((*list)[i:], (*list)[i+1:])
	(*list)[len(*list)-1] = nil
	*list = (*list)[:len(*list)-1]
}

func (bs *bottomSender) findInvalidationByID(ID string, list []*invTrans) int {
	for i, tr := range list {
		if tr.req.Meta().ID == ID {
			return i
		}
	}
	return -1
}

func (bs *bottomSender) removeInflightInvalidation(i int) {
	if len(bs.inflightInvToBottom) <= i {
		panic(fmt.Sprintf("Trying to remove inflight invalidation at index %d...", i))
	}
	copy(bs.inflightInvToBottom[i:], bs.inflightInvToBottom[i+1:])
	bs.inflightInvToBottom[len(bs.inflightInvToBottom)-1] = nil
	bs.inflightInvToBottom = bs.inflightInvToBottom[:len(bs.inflightInvToBottom)-1]
}

// [수정] 배열에서 제거하는 헬퍼 함수
func (bs *bottomSender) removeInflightRequest(i int, isLocal bool) {
	var trans *transaction
	if isLocal {
		trans = bs.localInflightRequest[i]
		copy(bs.localInflightRequest[i:], bs.localInflightRequest[i+1:])
		bs.localInflightRequest[len(bs.localInflightRequest)-1] = nil
		bs.localInflightRequest = bs.localInflightRequest[:len(bs.localInflightRequest)-1]
	} else {
		trans = bs.remoteInflightRequest[i]
		copy(bs.remoteInflightRequest[i:], bs.remoteInflightRequest[i+1:])
		bs.remoteInflightRequest[len(bs.remoteInflightRequest)-1] = nil
		bs.remoteInflightRequest = bs.remoteInflightRequest[:len(bs.remoteInflightRequest)-1]
	}
	// [ITER17 F5b] Decrement peer-inflight count when the removed trans
	// was peer-originated. Use the trans.fromLocal flag to detect.
	if trans != nil && !trans.fromLocal && bs.numPeerInflightRequest > 0 {
		bs.numPeerInflightRequest--
		if trans.read != nil && bs.numPeerInflightRead > 0 { // [PEER R/W SPLIT]
			bs.numPeerInflightRead--
		}
	}
}

// [추가] Bypass Inflight 배열에서 트랜잭션을 안전하게 삭제하는 헬퍼 함수
func (bs *bottomSender) removeInflightBypassRequest(i int) {
	if i < 0 || i >= len(bs.localInflightBypassRequest) {
		panic(fmt.Sprintf("Trying to remove localInflightBypassRequest at out of bounds index %d", i))
	}

	// 뒤의 원소들을 앞으로 당김
	copy(bs.localInflightBypassRequest[i:], bs.localInflightBypassRequest[i+1:])
	// 마지막 원소 포인터 명시적 해제 (메모리 누수 방지)
	bs.localInflightBypassRequest[len(bs.localInflightBypassRequest)-1] = nil
	// 슬라이스 길이 축소
	bs.localInflightBypassRequest = bs.localInflightBypassRequest[:len(bs.localInflightBypassRequest)-1]
}

// [ORIGIN-SPLIT] removeInflightInvToOutside is superseded by the per-origin
// removeInflightInvToOutsideFrom(list, i) helper above; the single-list
// version is removed because the inv-inflight list is now origin-partitioned.
