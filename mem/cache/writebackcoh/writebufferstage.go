package writebackcoh

import (
	"fmt"
	"os"
	"strings"

	"github.com/sarchlab/akita/v4/mem/cache/writebackcoh/internal"
	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/akita/v4/tracing"
)

type writeBufferStage struct {
	cache *Comp

	// [ITER17 F2] writeBufferCapacity split into local/peer sub-budgets.
	// Old single cap shared between LOCAL and REMOTE pending+inflight
	// meant peer-bypass (iter13 #1) admits that pushed to
	// pendingLocalEvictions grew the LOCAL bucket past cap → blocked
	// fromLocal=true at writeBufferReservedForRemote → cascading stall
	// observed in conv2d sim 7.71 ms and stencil2d sim 21.38 ms.
	// Now each side has its own ceiling; one side's saturation does not
	// block admit / drain of the other. Defaults: local=1024 (= legacy),
	// peer=256 (~conservative since peer is rate-limited by sender caps).
	writeBufferCapacity     int // = writeBufferLocalCapacity (kept name for back-compat)
	writeBufferPeerCapacity int
	maxInflightFetch        int
	maxInflightEviction     int

	// [ORIGIN-SPLIT] Per-ORIGIN sub-budgets of the REMOTE-destination
	// inflight-eviction ceiling. own + peer sum to the existing remote
	// ceiling (maxInflightEviction); no net capacity increase. Set in
	// builder.go (own≈3/4, peer≈1/4, mirroring the maxInflightRequest
	// soft-cap convention). tooManyInflightEvictions gates a remote-bound
	// eviction against the sub-budget matching its origin so peer-serve
	// always retains its reserved slots. The LOCAL-destination side keeps
	// the single maxInflightEviction ceiling (it drains via DRAM ACK,
	// independent of the cross-GPU cycle).
	maxRemoteInflEvictOwn  int
	maxRemoteInflEvictPeer int

	// [ITER17 F1] Cap on peer-bypass admit lane (the iter13 #1 path
	// that admits fromLocal=false unconditionally except for bankBuf.
	// Without this cap, observed: pendingLocalEvictions = 1432 (1.4×
	// writeBufferCapacity) and writeBufferReservedForRemote stuck TRUE
	// permanently. Incremented in peer-bypass admit, decremented when
	// the trans transitions into inflight (tryWriteOne).
	maxPeerIncomingPending int
	numPeerIncomingPending int

	// [ITER10 STRUCTURAL FIX] pendingEvictions split into local/remote
	// queues to prevent LOCAL-blocks-REMOTE head-of-line in the FIFO.
	// Previously a LOCAL-destined eviction at head (waiting for DRAM)
	// would block all REMOTE evictions behind it, and vice versa.
	// Each queue is drained independently in write(); per-tick only
	// one is sent (bottomPort.CanSend is shared), so total throughput
	// is unchanged but a stalled head no longer starves the other
	// category.
	//
	// [ORIGIN-SPLIT] The pendingRemoteEvictions queue (remote DESTINATION)
	// is further partitioned by ORIGIN (trans.fromLocal): own-L1-originated
	// remote evictions vs peer-serve-originated displacement flushes. A pile
	// of OWN remote evictions must not head-of-line-block the PEER-serve
	// flush whose drain emits the ACK that frees the peer's remote-inflict
	// cap (the closing edge of the cross-GPU serve deadlock). tryWriteOne
	// drains the Peer queue FIRST. pendingLocalEvictions (local DESTINATION)
	// stays single — the deadlock cycle is entirely on the remote-bound side.
	pendingLocalEvictions      []*transaction
	pendingRemoteEvictionsOwn  []*transaction
	pendingRemoteEvictionsPeer []*transaction
	inflightFetch              []*transaction
	inflightEviction           []*transaction

	// [CD8-DEADLOCK FIX] Dedicated bounded admit reserve for INVALIDATION-DRIVEN
	// dirty victim writebacks (enqueueInvDirtyFlush). These drain to LOCAL DRAM
	// ONLY (acyclic: invBuf-head ← here ← bottomPort ← local DRAM, no cross-GPU /
	// CohDir dependency), so a fixed reserve here can NEVER participate in a
	// credit cycle. It exists solely so a saturated own-origin remote
	// write-through pile (numRemoteInflEvictOwn at cap) clogging the shared
	// writeBufferBuffer/deferredFlushOwn admit lanes can no longer block the
	// InvRsp emission gated at directorystage.go:791. tryWriteOne(isLocal=true)
	// drains this FIRST, ahead of pendingLocalEvictions. Bounded => no unbounded
	// memory. Preserves write-completion ordering (the deferral is untouched).
	invDirtyFlushReserve    []*transaction
	maxInvDirtyFlushReserve int

	// [SD-ACK-RESERVE] Dedicated bounded placement reserve for the
	// displacement victim of an INCOMING PEER WRITE (fromLocal=false,
	// writeToHomeNode). finalizeWriteHit/finalizeBankWriteFetched emit the
	// peer WriteDoneRsp the instant that victim is PLACED (RESPONSE-DECOUPLE),
	// not drained — but the ack is gated on placing it into
	// writeBufferBufferRemote (cap=numReqPerCycle) OR deferredFlushPeer
	// (cap=numReqPerCycle), both tiny and shared with bulk peer-bypass work.
	// Under a fine-granularity (e.g. SD 9-bank) invalidation flood both fill,
	// so the ack is never emitted, so the remote sender's numRemoteInflEvictOwn
	// credit stays pinned -> 4-GPU symmetric cross-GPU eviction-credit deadlock.
	// This reserve is a THIRD placement lane consumed ONLY by that peer-ack
	// victim (own/bulk work can never occupy it), so the ack can ALWAYS leave,
	// freeing the remote credit and unjamming the cycle. The parked victim
	// drains via tryWriteOne (routed by its own destination). Bounded => no
	// unbounded memory. Disabled (cap 0) unless -sd-ack-reserve is set.
	ackDisplaceReserve    []*transaction
	maxAckDisplaceReserve int

	// [RESPONSE-DECOUPLE] Bounded reordering buffer for displacement-flush
	// transactions whose response (WriteDoneRsp/DataReadyRsp) has already been
	// emitted but whose victim flush could not yet enter writeBufferBuffer
	// (its admit lane was momentarily full). The victim's data/identity is
	// already snapshotted on the *transaction (evictingData/evictingAddr/...),
	// so this holds only the pointer — NO data copy, NO eviction-capacity
	// increase. Drained every Tick (drainDeferredFlush) the instant a
	// writeBufferBuffer slot frees; the victim then traverses the UNCHANGED
	// tooManyOutgoingRemote / numRemoteInflightEviction caps. This lets the
	// response leave ahead of the displacement so a saturated own-eviction cap
	// can no longer block the response that drains a peer's cap (the closing
	// edge of the cross-GPU serve deadlock).
	// [ORIGIN-SPLIT] deferredFlush partitioned by ORIGIN (trans.fromLocal)
	// each with cap = maxDeferredFlush/2 (no net increase). A shared, full
	// deferredFlush re-closes the very cross-GPU writeback cycle the
	// response-decouple buffer exists to break: own-L1 displacement flushes
	// (fromLocal=true) could exhaust the deferral list and strand the
	// peer-serve WriteDoneRsp/DataReadyRsp ack. drainDeferredFlush drains
	// the Peer list FIRST.
	deferredFlushOwn  []*transaction
	deferredFlushPeer []*transaction
	maxDeferredFlush  int

	// Local/remote split mirrors the fetch quota in superdirectory's
	// bottomSender (75% local / 25% remote). Without this split,
	// remote-routed evictions (cross-GPU writebacks via SD/REC) and
	// local DRAM evictions share one cap; a backpressure burst on
	// the cross-GPU path can fill the cap and HoL-block local evictions
	// (and vice versa), producing cross-GPU circular wait deadlocks
	// observed under stencil2d SD.
	//
	// numRemoteInflightEviction (remote DESTINATION) is kept as the exact
	// sum of the two ORIGIN sub-counters below so writeBufferFull's PEER
	// bucket accounting is unchanged. The ORIGIN split is what the inflight
	// admit gate (tooManyInflightEvictions) now keys on.
	numLocalInflightEviction  int
	numRemoteInflightEviction int

	// [ORIGIN-SPLIT] Remote-DESTINATION inflight evictions partitioned by
	// ORIGIN (trans.fromLocal). The single 128-slot remote cap used to be
	// consumed by BOTH own cross-GPU evictions (fromLocal=true) AND
	// peer-serve displacement flushes (fromLocal=false); when own work
	// saturated it the peer-serve flush could never drain → no
	// WriteDoneRsp/ack → mutual cross-GPU deadlock (the confirmed seed).
	// Each origin now gates against its own sub-budget
	// (maxRemoteInflEvictOwn / maxRemoteInflEvictPeer) carved from the
	// existing remote ceiling, so the peer-serve flush always has reserved
	// slots and own work can never starve it.
	numRemoteInflEvictOwn  int
	numRemoteInflEvictPeer int

	// [OUTGOING-REMOTE CAP FIX] Cap on the SUM of pending + inflight
	// remote-bound evictions. Closes the cross-GPU symmetric
	// wB-saturation cycle observed under stencil2d REC (sim 19.95ms):
	//
	//   Sender L2.wB inflightEviction=128 (cap) → can't push more
	//   to inflight → pendingEvictions grows → pending+inflight hits
	//   writeBufferCapacity=1024 → wB declares full → receiver's
	//   incoming WriteReq triggers eviction that can't enter wB →
	//   receiver L2 stalls → WriteDoneRsp never returns to sender →
	//   sender's inflight stays at 128 → mutual.
	//
	// Cap (numPendingRemoteEvictions + numRemoteInflightEviction) at
	// maxOutgoingRemotePending so the per-cache wB total (used for
	// writeBufferFull) cannot reach 1024 from sender-side remote
	// evictions alone — guaranteeing headroom for incoming-write-
	// triggered evictions at the receiver side. Backpressure stays on
	// sender's upstream (dirStage → topparser → L1) instead of the
	// receiver's wB cap. maxOutgoingRemotePending <= 0 disables the
	// cap (legacy behavior).
	//
	// [ORIGIN-SPLIT] numPendingRemoteEvictions partitioned by ORIGIN. The
	// peer-bypass admit lane never consulted tooManyOutgoingRemote, so
	// peer-serve work used to FILL this counter while only OWN remote
	// evictions were gated by it — own work both filled and was starved by
	// a sum peer-serve also filled. The outgoing-remote admit gate now
	// applies ONLY to OWN-origin pending; peer-serve stays ungated there.
	numPendingRemoteEvictionsOwn  int
	numPendingRemoteEvictionsPeer int
	maxOutgoingRemotePending      int

	// Typed sub-queues for bottomPort response handling. Without these,
	// processDataReadyRsp may fail (writeBufferToBankBuffers cap full →
	// returns false without RetrieveIncoming), leaving a DataReadyRsp
	// stuck at bottomPort head. WriteDoneRsp behind it in the FIFO is
	// then never observed, so inflightEviction never frees and the
	// writeBuffer cap → write-through stall → cross-GPU cyclic deadlock
	// (observed under CD coherence-unit-size=0 page-migration burst).
	// Caps match the actual port buffer (4 each) so total cache buffer
	// stays nearly unchanged — invalidation/write-through cost models
	// are not affected.
	// [R6] Pending bottomPort responses split into Local / Remote halves
	// keyed by trans.fromLocal (for DataReadyRsp via inflightFetch) and
	// e.evictionToLocal — actually trans.fromLocal — (for WriteDoneRsp
	// via inflightEviction). Without the split, a Local DataReadyRsp at
	// head that needs a full Local-side bankBuf stalls behind it the
	// Remote DataReadyRsp that could otherwise progress (and vice versa),
	// re-introducing the cross-GPU symmetric-stall the bottomPort typed
	// queues were meant to break.
	pendingDataReadyLocal  []*mem.DataReadyRsp
	pendingDataReadyRemote []*mem.DataReadyRsp
	pendingWriteDoneLocal  []*mem.WriteDoneRsp
	pendingWriteDoneRemote []*mem.WriteDoneRsp

	// [ITER12 INSTRUMENTATION] Track the last reason that
	// processWriteBufferEvictAndWrite (admit-side) returned false. This
	// pinpoints whether the symmetric cross-GPU "remote-blocks-remote"
	// cycle is being closed by writeBufferFull, tooManyOutgoingRemote,
	// or bankBuf cap. Set on every return-false site; never cleared on
	// success.
	lastAdmitFailReason string

	// [ITER13 fix #2] Set by processNewTransaction after peeking the
	// split writeBufferBuffer{,Remote} pair. All downstream Pop sites
	// (processWriteBufferEvictAndWrite, processWriteBufferFlush,
	// processWriteBufferFetchAndEvict, processWriteBufferEvictAndPrefetch,
	// processWriteBufferFetch's sendFetchedDataToBank/fetchFromBottom)
	// pop from this exact buffer so the right slot is freed regardless
	// of whether the head came from the local or remote queue.
	currentEvictionSrcBuf sim.Buffer

	// [ITER20 DIAG] WriteDoneRsp acks whose RespondTo matched no
	// inflightEviction.evictionWriteReq.ID — a silent numRemoteInflight
	// leak (ack consumed at bottomPort, but no decrement). Non-zero ==
	// the win80 quiesced-with-128-inflight deadlock root.
	writeDoneNoMatchCount  uint64
	writeDoneNoMatchSample string
	// [ITER20 DIAG E] every WriteDoneRsp that arrives at bottomPort
	// (matched or not). If 0 at the win80 deadlock while
	// numRemoteInflightEviction is pinned, the ack never reached the
	// sender L2 — the leak is on the return path (peer/RDMA/network).
	writeDoneReceivedCount uint64
}

func (wb *writeBufferStage) Tick() bool {
	madeProgress := false

	// [ITER6 RESPONSE PRIORITY] Process incoming responses FIRST so the
	// inflict counts get decremented before write() tries to send more
	// outgoing. Without this ordering, write() runs first with stale
	// (capped) inflict counts and bottomPort.CanSend may fail; even
	// though responses arrive in the same tick, they don't help that
	// tick. Reordering ensures each tick uses the freshest inflict
	// state, breaking the closed wait cycle in REC where sender-side
	// outgoing piles up while receiver-side responses linger.
	//
	// Drain bottomPort head into type-classified sub-queues, then process
	// each type from its own queue. Separates DataReadyRsp from
	// WriteDoneRsp so a stuck DataReadyRsp (bankBuf full, processPrefetch
	// retry, etc.) cannot HoL-block WriteDoneRsp behind it in the port
	// FIFO. WriteDoneRsp processing must always make progress to free
	// inflightEviction slots; otherwise the writeBuffer caps and the
	// cross-GPU write-through cycle deadlocks.
	madeProgress = wb.drainBottomTyped() || madeProgress
	madeProgress = wb.processPendingDataReady() || madeProgress
	madeProgress = wb.processPendingWriteDone() || madeProgress

	madeProgress = wb.write() || madeProgress

	// [RESPONSE-DECOUPLE] Re-inject already-acked displacement flushes back
	// into the normal writeBufferBuffer admit path as soon as a slot frees.
	// Runs before processNewTransaction so deferred victims (whose responses
	// already left) get the freed slot ahead of brand-new work.
	madeProgress = wb.drainDeferredFlush() || madeProgress

	madeProgress = wb.processNewTransaction() || madeProgress
	// [FIX: head-of-line] writeBufferFetchBuffer(fetch 전용)를 writeBufferBuffer(eviction 전용)와
	// 독립적으로 처리. fetch 블로킹이 eviction 진행을 막지 않도록 분리.
	madeProgress = wb.processNewFetch() || madeProgress

	return madeProgress
}

// [RESPONSE-DECOUPLE] deferFlushCanPush reports whether the bounded
// reordering buffer for the given ORIGIN can accept one more already-acked
// displacement flush. [ORIGIN-SPLIT] Each origin has its own half-cap so an
// own-L1 displacement-flush flood cannot exhaust the deferral list and strand
// the peer-serve ack (and vice versa).
func (wb *writeBufferStage) deferFlushCanPush(fromLocal bool) bool {
	half := wb.maxDeferredFlush / 2
	if fromLocal {
		return len(wb.deferredFlushOwn) < half
	}
	return len(wb.deferredFlushPeer) < half
}

// deferFlushPush parks a displacement-flush transaction whose response has
// already been emitted but whose victim could not yet enter writeBufferBuffer.
// Holds the pointer only — the victim data already lives on the transaction.
// [ORIGIN-SPLIT] Routes by trans.fromLocal to the matching half-list.
func (wb *writeBufferStage) deferFlushPush(trans *transaction) {
	if trans.fromLocal {
		wb.deferredFlushOwn = append(wb.deferredFlushOwn, trans)
	} else {
		wb.deferredFlushPeer = append(wb.deferredFlushPeer, trans)
	}
}

// [CD8-DEADLOCK FIX] invDirtyFlushReserve admit helpers. The reserve holds
// invalidation-driven dirty victim writebacks (LOCAL-DRAM destination, acyclic);
// tryWriteOne(isLocal=true) drains it FIRST so a saturated remote write-through
// pile can never block InvRsp emission. Bounded by maxInvDirtyFlushReserve.
func (wb *writeBufferStage) invDirtyFlushReserveCanPush() bool {
	return len(wb.invDirtyFlushReserve) < wb.maxInvDirtyFlushReserve
}

func (wb *writeBufferStage) invDirtyFlushReservePush(trans *transaction) {
	wb.invDirtyFlushReserve = append(wb.invDirtyFlushReserve, trans)
}

// [SD-ACK-RESERVE] ackDisplaceReserve helpers. The reserve holds the
// displacement victim of an incoming peer write so its WriteDoneRsp can always
// be emitted. CanPush is gated by maxAckDisplaceReserve (0 => disabled). Push
// is only ever called for a peer-incoming (fromLocal=false) victim that could
// not enter writeBufferBufferRemote/deferredFlushPeer.
func (wb *writeBufferStage) ackDisplaceReserveCanPush() bool {
	return wb.maxAckDisplaceReserve > 0 &&
		len(wb.ackDisplaceReserve) < wb.maxAckDisplaceReserve
}

func (wb *writeBufferStage) ackDisplaceReservePush(trans *transaction) {
	wb.ackDisplaceReserve = append(wb.ackDisplaceReserve, trans)
}

// drainDeferredFlush re-injects the head deferred flush into the normal
// writeBufferBuffer admit path the instant a slot frees. The victim then
// traverses the UNCHANGED tooManyOutgoingRemote / numRemoteInflictEviction
// caps via processWriteBufferFlush → tryWriteOne — no cap is bypassed; only
// the response left ahead of the displacement. [ORIGIN-SPLIT] Drains the PEER
// half FIRST so a pile of own deferred flushes cannot strand the peer-serve
// ack-bearing displacement.
func (wb *writeBufferStage) drainDeferredFlush() bool {
	if wb.drainOneDeferredFlush(&wb.deferredFlushPeer) {
		return true
	}
	return wb.drainOneDeferredFlush(&wb.deferredFlushOwn)
}

func (wb *writeBufferStage) drainOneDeferredFlush(list *[]*transaction) bool {
	if len(*list) == 0 {
		return false
	}
	head := (*list)[0]
	if !wb.cache.writeBufferBufferCanPush(head.fromLocal) {
		return false
	}
	wb.cache.writeBufferBufferPush(head, head.fromLocal)
	*list = (*list)[1:]
	return true
}

func (wb *writeBufferStage) drainBottomTyped() bool {
	// [L2 LOCAL/REMOTE SPLIT] Drain remoteBottomPort FIRST (cross-GPU critical
	// path = the ACK return for own remote evictions / remote fetches), then
	// bottomPort — each independently, mirroring the dir's remote-first
	// processReturnRsp. Each port has its own head so a stalled local response
	// can never HoL-block a remote response and vice versa.
	r := wb.drainOneBottomPort(wb.cache.remoteBottomPort)
	l := wb.drainOneBottomPort(wb.cache.bottomPort)
	return r || l
}

// drainOneBottomPort drains one head from the given bottom-side port into the
// typed sub-queues (classified by fromLocal). Caps are intentionally NOT
// applied here (see original rationale: capping would re-introduce HoL).
func (wb *writeBufferStage) drainOneBottomPort(port sim.Port) bool {
	msg := port.PeekIncoming()
	if msg == nil {
		return false
	}

	switch m := msg.(type) {
	case *mem.DataReadyRsp:
		trans := wb.findInflightFetchByFetchReadReqID(m.RespondTo)
		if trans != nil && !trans.fromLocal {
			wb.pendingDataReadyRemote = append(wb.pendingDataReadyRemote, m)
		} else {
			wb.pendingDataReadyLocal = append(wb.pendingDataReadyLocal, m)
		}
	case *mem.WriteDoneRsp:
		wb.writeDoneReceivedCount++
		var fromLocal = true
		for _, e := range wb.inflightEviction {
			if e.evictionWriteReq != nil && e.evictionWriteReq.ID == m.RespondTo {
				fromLocal = e.fromLocal
				break
			}
		}
		if fromLocal {
			wb.pendingWriteDoneLocal = append(wb.pendingWriteDoneLocal, m)
		} else {
			wb.pendingWriteDoneRemote = append(wb.pendingWriteDoneRemote, m)
		}
	default:
		panic("unknown msg type on bottom port")
	}

	port.RetrieveIncoming()
	return true
}

func (wb *writeBufferStage) processPendingDataReady() bool {
	// [R6] Try Remote first; if blocked, fall through to try Local
	// independently — do NOT block Local when Remote fails (HoL-in-reverse).
	if len(wb.pendingDataReadyRemote) > 0 {
		head := wb.pendingDataReadyRemote[0]
		if wb.tryProcessDataReadyRsp(head) {
			wb.pendingDataReadyRemote = wb.pendingDataReadyRemote[1:]
			return true
		}
		// Remote blocked — fall through and try Local below
	}
	if len(wb.pendingDataReadyLocal) == 0 {
		return false
	}

	head := wb.pendingDataReadyLocal[0]
	if !wb.tryProcessDataReadyRsp(head) {
		return false
	}

	wb.pendingDataReadyLocal = wb.pendingDataReadyLocal[1:]
	return true
}

func (wb *writeBufferStage) processPendingWriteDone() bool {
	// [R6] Drain Remote half first.
	if len(wb.pendingWriteDoneRemote) > 0 {
		head := wb.pendingWriteDoneRemote[0]
		wb.applyWriteDoneRsp(head)
		wb.pendingWriteDoneRemote = wb.pendingWriteDoneRemote[1:]
		return true
	}
	if len(wb.pendingWriteDoneLocal) == 0 {
		return false
	}

	head := wb.pendingWriteDoneLocal[0]
	wb.applyWriteDoneRsp(head)
	wb.pendingWriteDoneLocal = wb.pendingWriteDoneLocal[1:]
	return true
}

func (wb *writeBufferStage) processNewTransaction() bool {
	// [ITER13 fix #2] Peek Remote first (peer-incoming priority); fall
	// back to Local. Record which buffer the head came from so all
	// downstream action handlers Pop from the correct buffer.
	item := wb.cache.writeBufferBufferRemote.Peek()
	wb.currentEvictionSrcBuf = wb.cache.writeBufferBufferRemote
	if item == nil {
		item = wb.cache.writeBufferBuffer.Peek()
		wb.currentEvictionSrcBuf = wb.cache.writeBufferBuffer
	}
	if item == nil {
		return false
	}

	trans := item.(*transaction)
	if wb.cache.debugProcess && trans.accessReq() != nil && trans.accessReq().GetAddress() == wb.cache.debugAddress0 {
		fmt.Printf("[%s] [writebufferstage]\tReceived read req - 3: addr %x, action %d\n", wb.cache.name, trans.accessReq().GetAddress(), trans.action)
	}
	if wb.cache.debugProcess && trans.accessReq() != nil && trans.accessReq().GetAddress() == wb.cache.debugAddress1 {
		fmt.Printf("[%s] [writebufferstage]\tReceived read req - 3: addr %x, action %d\n", wb.cache.name, trans.accessReq().GetAddress(), trans.action)
	}
	if wb.cache.debugProcess && trans.responsing {
		fmt.Printf("[%s]\tTransaction %x is responsing, discard.\n", wb.cache.name, trans.accessReq().GetAddress())
		wb.currentEvictionSrcBuf.Pop()
		return true
	}
	switch trans.action {
	case writeBufferFetch:
		return wb.processWriteBufferFetch(trans)
	case writeBufferEvictAndWrite:
		return wb.processWriteBufferEvictAndWrite(trans)
	case writeBufferEvictAndFetch:
		return wb.processWriteBufferFetchAndEvict(trans)
	case writeBufferEvictAndPrefetch:
		return wb.processWriteBufferEvictAndPrefetch(trans)
	case writeBufferFlush:
		return wb.processWriteBufferFlush(trans, true)
	default:
		fmt.Printf("[%s]\t[WARNING]\tUnknown transaction action %d, trans: %p\n", wb.cache.name, trans.action, trans)
		for true {
		}
		panic("unknown transaction action")
	}
}

func (wb *writeBufferStage) processWriteBufferFetch(
	trans *transaction,
) bool {
	if wb.findDataLocally(trans) {
		if wb.cache.debugProcess && trans.accessReq() != nil && trans.accessReq().GetAddress() == wb.cache.debugAddress0 {
			fmt.Printf("[%s] [writebufferstage]\tReceived req - 3.0: addr %x, action %d\n", wb.cache.name, trans.accessReq().GetAddress(), trans.action)
		}
		if wb.cache.debugProcess && trans.accessReq() != nil && trans.accessReq().GetAddress() == wb.cache.debugAddress1 {
			fmt.Printf("[%s] [writebufferstage]\tReceived req - 3.0: addr %x, action %d\n", wb.cache.name, trans.accessReq().GetAddress(), trans.action)
		}
		// EvictAndFetch 변환 케이스: pass the buffer the trans came from
		return wb.sendFetchedDataToBank(trans, wb.currentEvictionSrcBuf)
	}

	if wb.cache.debugProcess && trans.accessReq() != nil && trans.accessReq().GetAddress() == wb.cache.debugAddress0 {
		fmt.Printf("[%s] [writebufferstage]\tReceived req - 3.1: addr %x, action %d\n", wb.cache.name, trans.accessReq().GetAddress(), trans.action)
	}
	if wb.cache.debugProcess && trans.accessReq() != nil && trans.accessReq().GetAddress() == wb.cache.debugAddress1 {
		fmt.Printf("[%s] [writebufferstage]\tReceived req - 3.1: addr %x, action %d\n", wb.cache.name, trans.accessReq().GetAddress(), trans.action)
	}
	// EvictAndFetch 변환 케이스: pass the buffer the trans came from
	return wb.fetchFromBottom(trans, wb.currentEvictionSrcBuf)
}

// [FIX: head-of-line] dirStage.fetch()가 직접 push한 순수 fetch 항목을 처리.
// writeBufferBuffer(eviction 전용)와 독립적으로 동작하므로 fetch 블로킹이
// finalizeBankEviction을 막지 않는다.
func (wb *writeBufferStage) processNewFetch() bool {
	item := wb.cache.writeBufferFetchBuffer.Peek()
	if item == nil {
		return false
	}
	trans := item.(*transaction)

	if wb.findDataLocally(trans) {
		return wb.sendFetchedDataToBank(trans, wb.cache.writeBufferFetchBuffer)
	}
	return wb.fetchFromBottom(trans, wb.cache.writeBufferFetchBuffer)
}

func (wb *writeBufferStage) findDataLocally(trans *transaction) bool {
	for _, e := range wb.inflightEviction {
		if e.evictingAddr == trans.fetchAddress {
			trans.fetchedData = e.evictingData
			return true
		}
	}

	// [ITER10] check both local and remote pending eviction queues.
	// [ORIGIN-SPLIT] remote pending is now two origin half-queues; scan both.
	for _, e := range wb.pendingLocalEvictions {
		if e.evictingAddr == trans.fetchAddress {
			trans.fetchedData = e.evictingData
			return true
		}
	}
	for _, e := range wb.pendingRemoteEvictionsOwn {
		if e.evictingAddr == trans.fetchAddress {
			trans.fetchedData = e.evictingData
			return true
		}
	}
	for _, e := range wb.pendingRemoteEvictionsPeer {
		if e.evictingAddr == trans.fetchAddress {
			trans.fetchedData = e.evictingData
			return true
		}
	}

	return false
}

func (wb *writeBufferStage) sendFetchedDataToBank(
	trans *transaction,
	buf sim.Buffer,
) bool {
	bankNum := bankID(trans.block,
		wb.cache.directory.WayAssociativity(),
		len(wb.cache.dirToBankBuffers))
	bankBuf := wb.cache.writeBufferToBankBufferRsp(bankNum, trans.fromLocal) // [R5 + BANK SPLIT] bankWriteFetched

	if !bankBuf.CanPush() {
		trans.fetchedData = nil
		return false
	}

	trans.mshrEntry.Data = trans.fetchedData
	trans.action = bankWriteFetched
	wb.combineData(trans.mshrEntry)

	if trans.fromLocal {
		wb.cache.mshrLocalRemoved++
	} else {
		wb.cache.mshrRemoteRemoved++
	}
	wb.cache.mshr.Remove(trans.mshrEntry.PID, trans.mshrEntry.Address)

	bankBuf.Push(trans)

	buf.Pop()

	// log.Printf("%.10f, %s, wb data fetched locally，" +
	// " %s, %04X, %04X, (%d, %d), %v\n",
	// 	now, wb.cache.Name(),
	// 	trans.accessReq().Meta().ID,
	// 	trans.accessReq().GetAddress(), trans.block.Tag,
	// 	trans.block.SetID, trans.block.WayID,
	// 	trans.fetchedData,
	// )

	return true
}

func (wb *writeBufferStage) fetchFromBottom(
	trans *transaction,
	buf sim.Buffer,
) bool {
	if wb.tooManyInflightFetches() {
		return false
	}

	// [L2 LOCAL/REMOTE SPLIT] remote-homed fetches egress on remoteBottomPort.
	egress := wb.cache.bottomPort
	if !wb.cache.toLocal(trans.fetchAddress) {
		egress = wb.cache.remoteBottomPort
	}
	if !egress.CanSend() {
		return false
	}

	lowModulePort := wb.cache.addressToPortMapper.Find(trans.fetchAddress)
	// if strings.Contains(fmt.Sprintf("%s", lowModulePort), "RDMA") {
	// 	fmt.Printf("[%s]\tFetch %x from %s\n", wb.cache.Name(), trans.fetchAddress, lowModulePort)
	// }
	read := mem.ReadReqBuilder{}.
		WithSrc(egress.AsRemote()).
		WithDst(lowModulePort).
		WithPID(trans.fetchPID).
		WithAddress(trans.fetchAddress).
		WithByteSize(1 << wb.cache.log2BlockSize).
		WithFetchForWriteMiss(trans.write != nil).
		Build()
	if trans.accessReq() != nil {
		read.VAddr = trans.accessReq().GetVAddr()
	}

	egress.Send(read)

	trans.fetchReadReq = read
	wb.inflightFetch = append(wb.inflightFetch, trans)
	buf.Pop()

	tracing.TraceReqInitiate(read, wb.cache,
		tracing.MsgIDAtReceiver(trans.req(), wb.cache))

	what := ""
	if strings.Contains(fmt.Sprintf("%s", read.Meta().Dst), "DRAM") {
		what = "ToLocal"
	} else {
		what = "ToRemote"
	}
	wb.cache.incEvent(what)

	if wb.cache.debugProcess && trans.accessReq() != nil && trans.accessReq().GetAddress() == wb.cache.debugAddress0 {
		fmt.Printf("[%s] [writebufferstage]\tReceived req - 3.1.0: addr %x, action %d\n", wb.cache.name, trans.accessReq().GetAddress(), trans.action)
	}
	if wb.cache.debugProcess && trans.accessReq() != nil && trans.accessReq().GetAddress() == wb.cache.debugAddress1 {
		fmt.Printf("[%s] [writebufferstage]\tReceived req - 3.1.0: addr %x, action %d\n", wb.cache.name, trans.accessReq().GetAddress(), trans.action)
	}
	return true
}

func (wb *writeBufferStage) processWriteBufferEvictAndWrite(
	trans *transaction,
) bool {
	// [ITER13 PEER-INCOMING PRIORITY LANE]
	// Workflow audit (iter12) confirmed the cross-GPU symmetric hang
	// (stencil2d sim 17.50 ms, GPU[1].L2 wB total=1759 > cap=1024)
	// closes here: peer-incoming WriteReq (trans.fromLocal=false) is
	// what generates the ACK that unblocks the sender's
	// numRemoteInflightEviction. When the receiver's wB is full of its
	// OWN remote-bound evictions, the receiver-triggered eviction is
	// rejected, no WriteDoneRsp is emitted, sender stays at cap, and
	// the symmetric peer is in the equivalent state — full deadlock.
	//
	// Resolution: peer-incoming trans gets an unconditional admit lane
	// that bypasses writeBufferFull and tooManyOutgoingRemote. Only the
	// bank buffer back-pressure remains (necessary for correctness).
	// fromLocal=true (sender-side OWN eviction) still respects caps so
	// total system load remains bounded by the upstream caps (L1V
	// transactions, REC bypass cap, etc.). This is exactly the iter2
	// design that was reverted during the structural-cycle hunt, but
	// now backed by the explicit workflow audit finding that the L2 wB
	// admit gate is the principal cycle vertex.
	if !trans.fromLocal {
		// [ITER17 F1] Peer-bypass admit lane MUST honor a sane upper
		// cap. iter13 #1 omitted any pending-queue check, which let
		// pendingLocalEvictions grow to 1432 (1.4× writeBufferCapacity)
		// in conv2d sim 7.71 ms hang. The cycle then closed via
		// writeBufferReservedForRemote=TRUE permanently. Restore the
		// design invariant: peer admits are bounded by
		// maxPeerIncomingPending. When hit, RDMA backpressure
		// propagates to peer's REC.bottomSender — peer slows its own
		// sender-side, which lets receiver drain.
		if wb.maxPeerIncomingPending > 0 &&
			wb.numPeerIncomingPending >= wb.maxPeerIncomingPending {
			wb.lastAdmitFailReason = fmt.Sprintf("peerIncomingPendingCap: numPeerIncomingPending=%d >= max=%d",
				wb.numPeerIncomingPending, wb.maxPeerIncomingPending)
			return false
		}
		bankNum := bankID(
			trans.block,
			wb.cache.directory.WayAssociativity(),
			len(wb.cache.dirToBankBuffers),
		)
		bankBuf := wb.cache.writeBufferToBankBufferReq(bankNum, trans.fromLocal) // [R5 + BANK SPLIT] bankWriteHit (peer-incoming bypass, fromLocal=false)
		if !bankBuf.CanPush() {
			wb.lastAdmitFailReason = fmt.Sprintf("bankBuf.CanPush()=false bankNum=%d fromLocal=false (peer-incoming bypass lane, still bank-backpressured)",
				bankNum)
			return false
		}
		trans.action = bankWriteHit
		bankBuf.Push(trans)
		isLocal := wb.cache.toLocal(trans.evictingAddr)
		if isLocal {
			wb.pendingLocalEvictions = append(wb.pendingLocalEvictions, trans)
		} else {
			// [ORIGIN-SPLIT] peer-bypass admit = fromLocal=false = PEER origin.
			wb.pendingRemoteEvictionsPeer = append(wb.pendingRemoteEvictionsPeer, trans)
			wb.numPendingRemoteEvictionsPeer++
		}
		wb.numPeerIncomingPending++
		wb.currentEvictionSrcBuf.Pop()
		return true
	}

	if wb.writeBufferFull(trans.fromLocal) {
		// [ITER12] precise reason
		wb.lastAdmitFailReason = fmt.Sprintf("writeBufferFull(side=%v): pendingLoc=%d pendingRem=%d numLocalInfl=%d numRemoteInfl=%d locCap=%d peerCap=%d",
			trans.fromLocal, len(wb.pendingLocalEvictions), wb.pendingRemoteEvictionsLen(),
			wb.numLocalInflightEviction, wb.numRemoteInflightEviction, wb.writeBufferCapacity, wb.writeBufferPeerCapacity)
		return false
	}

	// [OUTGOING-REMOTE CAP FIX] Refuse new remote-bound evictions when
	// the per-cache outgoing-remote quota is saturated. Keeps receiver
	// L2's wB headroom available for incoming-triggered evictions.
	// [ORIGIN-SPLIT] This own-eviction path is fromLocal=true (peer-bypass
	// returned above), so the outgoing-remote gate applies to OWN pending.
	isLocal := wb.cache.toLocal(trans.evictingAddr)
	if wb.tooManyOutgoingRemote(isLocal) {
		wb.lastAdmitFailReason = fmt.Sprintf("tooManyOutgoingRemote: numPendingRemoteOwn=%d numRemoteInflight=%d maxOutgoingRemotePending=%d isLocal=%v fromLocal=%v",
			wb.numPendingRemoteEvictionsOwn, wb.numRemoteInflightEviction,
			wb.maxOutgoingRemotePending, isLocal, trans.fromLocal)
		return false
	}

	bankNum := bankID(
		trans.block,
		wb.cache.directory.WayAssociativity(),
		len(wb.cache.dirToBankBuffers),
	)
	bankBuf := wb.cache.writeBufferToBankBufferReq(bankNum, trans.fromLocal) // [R5 + BANK SPLIT] bankWriteHit

	if !bankBuf.CanPush() {
		wb.lastAdmitFailReason = fmt.Sprintf("bankBuf.CanPush()=false bankNum=%d fromLocal=%v",
			bankNum, trans.fromLocal)
		return false
	}

	trans.action = bankWriteHit
	bankBuf.Push(trans)

	if isLocal {
		wb.pendingLocalEvictions = append(wb.pendingLocalEvictions, trans)
	} else {
		// [ORIGIN-SPLIT] own (fromLocal=true) remote eviction.
		wb.pendingRemoteEvictionsOwn = append(wb.pendingRemoteEvictionsOwn, trans)
		wb.numPendingRemoteEvictionsOwn++
	}
	wb.currentEvictionSrcBuf.Pop()

	// log.Printf("%.10f, %s, wb evict and write，" +
	// " %s, %04X, %04X, (%d, %d), %v\n",
	// 	now, wb.cache.Name(),
	// 	trans.accessReq().Meta().ID,
	// 	trans.accessReq().GetAddress(), trans.block.Tag,
	// 	trans.block.SetID, trans.block.WayID,
	// 	trans.evictingData,
	// )

	return true
}

func (wb *writeBufferStage) processWriteBufferEvictAndPrefetch(
	trans *transaction,
) bool {
	if true {
		panic("invalid function called")
	}

	if wb.writeBufferFull(trans.fromLocal) {
		return false
	}

	bankNum := bankID(
		trans.block,
		wb.cache.directory.WayAssociativity(),
		len(wb.cache.dirToBankBuffers),
	)
	bankBuf := wb.cache.writeBufferToBankBufferReq(bankNum, trans.fromLocal) // [R5 + BANK SPLIT] bankWritePrefetched

	if !bankBuf.CanPush() {
		return false
	}

	trans.action = bankWritePrefetched
	bankBuf.Push(trans)

	// [ITER10] route to local/remote pending queue based on dst.
	// [ORIGIN-SPLIT] remote pending further keyed by origin (dead code path).
	if wb.cache.toLocal(trans.evictingAddr) {
		wb.pendingLocalEvictions = append(wb.pendingLocalEvictions, trans)
	} else if trans.fromLocal {
		wb.pendingRemoteEvictionsOwn = append(wb.pendingRemoteEvictionsOwn, trans)
		wb.numPendingRemoteEvictionsOwn++
	} else {
		wb.pendingRemoteEvictionsPeer = append(wb.pendingRemoteEvictionsPeer, trans)
		wb.numPendingRemoteEvictionsPeer++
	}

	wb.currentEvictionSrcBuf.Pop()

	// log.Printf("%.10f, %s, wb evict and write，" +
	// " %s, %04X, %04X, (%d, %d), %v\n",
	// 	now, wb.cache.Name(),
	// 	trans.accessReq().Meta().ID,
	// 	trans.accessReq().GetAddress(), trans.block.Tag,
	// 	trans.block.SetID, trans.block.WayID,
	// 	trans.evictingData,
	// )

	return true
}

func (wb *writeBufferStage) processWriteBufferFetchAndEvict(
	trans *transaction,
) bool {
	ok := wb.processWriteBufferFlush(trans, false)
	if ok {
		trans.action = writeBufferFetch
		return true
	}

	// log.Printf("%.10f, %s, wb fetch and evict， " +
	// "%s, %04X, %04X, (%d, %d), %v\n",
	// 	now, wb.cache.Name(),
	// 	trans.write.ID,
	// 	trans.write.Address, trans.block.Tag,
	// 	trans.block.SetID, trans.block.WayID,
	// 	trans.evictingData,
	// )

	return false
}

func (wb *writeBufferStage) processWriteBufferFlush(
	trans *transaction,
	popAfterDone bool,
) bool {
	// [ITER13 PEER-INCOMING PRIORITY LANE] Same rationale as
	// processWriteBufferEvictAndWrite: peer-incoming flush request
	// MUST be admitted so the ACK path stays open.
	if trans.fromLocal {
		if wb.writeBufferFull(true) {
			wb.lastAdmitFailReason = fmt.Sprintf("flush:writeBufferFull(true) pendingLoc=%d numLocalInfl=%d cap=%d evictAddr=%x",
				len(wb.pendingLocalEvictions), wb.numLocalInflightEviction, wb.writeBufferCapacity, trans.evictingAddr)
			return false
		}
		// [OUTGOING-REMOTE CAP FIX] Same admit-side guard as
		// processWriteBufferEvictAndWrite — guards against sender L2
		// over-filling its pending+inflight remote eviction count.
		// [ORIGIN-SPLIT] fromLocal=true here, so the gate reads OWN pending.
		isLocalDst := wb.cache.toLocal(trans.evictingAddr)
		if wb.tooManyOutgoingRemote(isLocalDst) {
			wb.lastAdmitFailReason = fmt.Sprintf("flush:tooManyOutgoingRemote numPendingRemoteOwn=%d numRemoteInfl=%d max=%d evictAddr=%x",
				wb.numPendingRemoteEvictionsOwn, wb.numRemoteInflightEviction, wb.maxOutgoingRemotePending, trans.evictingAddr)
			return false
		}
	} else {
		// [ITER17 F1] Cap peer-bypass admit path here too (mirror
		// processWriteBufferEvictAndWrite). Without this, peer-incoming
		// flush requests grow pendingEvictions unboundedly.
		if wb.maxPeerIncomingPending > 0 &&
			wb.numPeerIncomingPending >= wb.maxPeerIncomingPending {
			wb.lastAdmitFailReason = fmt.Sprintf("peerIncomingPendingCap(flush): numPeerIncomingPending=%d >= max=%d",
				wb.numPeerIncomingPending, wb.maxPeerIncomingPending)
			return false
		}
	}

	// [ITER10] route to local/remote pending queue (works for both
	// peer-bypass and sender paths).
	// [ORIGIN-SPLIT] remote-destination pending further keyed by origin so
	// own flushes and peer-serve flushes occupy independent queues; the
	// peer-bypass path falls into the Peer queue here too.
	isLocal := wb.cache.toLocal(trans.evictingAddr)
	if isLocal {
		wb.pendingLocalEvictions = append(wb.pendingLocalEvictions, trans)
	} else if trans.fromLocal {
		wb.pendingRemoteEvictionsOwn = append(wb.pendingRemoteEvictionsOwn, trans)
		wb.numPendingRemoteEvictionsOwn++
	} else {
		wb.pendingRemoteEvictionsPeer = append(wb.pendingRemoteEvictionsPeer, trans)
		wb.numPendingRemoteEvictionsPeer++
	}
	// [ITER17 F1] Track peer-bypass admits so the cap above can fire.
	if !trans.fromLocal {
		wb.numPeerIncomingPending++
	}

	if popAfterDone {
		wb.currentEvictionSrcBuf.Pop()
	}

	if wb.cache.debugProcess && trans.evictingAddr == wb.cache.debugAddress0 {
		fmt.Printf("[%s] [writebufferstage]\tReceived req - 3.3: addr %x, action %d\n", wb.cache.name, trans.evictingAddr, trans.action)
	}
	if wb.cache.debugProcess && trans.evictingAddr == wb.cache.debugAddress1 {
		fmt.Printf("[%s] [writebufferstage]\tReceived req - 3.3: addr %x, action %d\n", wb.cache.name, trans.evictingAddr, trans.action)
	}

	return true
}

// [ITER10] tryWriteOne attempts to send the head of a single category
// queue (local or remote). Returns true if the head was sent and popped.
// Caller is responsible for choosing which queue to drain.
//
// [ORIGIN-SPLIT] The local-destination side is a single queue. The
// remote-destination side is split into Peer/Own origin queues drained
// Peer-FIRST in write() so a pile of own remote evictions cannot
// head-of-line-block the peer-serve flush whose drain emits the freeing ACK.
func (wb *writeBufferStage) tryWriteOne(isLocal bool) bool {
	// [SD-ACK-RESERVE] Drain the peer-ack displacement-victim reserve FIRST,
	// in whichever destination lane (local DRAM vs remote RDMA) the head belongs
	// to. The ack for each already left when the victim was placed; draining
	// here frees the reserve slot and writes the victim back.
	if len(wb.ackDisplaceReserve) > 0 &&
		wb.cache.toLocal(wb.ackDisplaceReserve[0].evictingAddr) == isLocal {
		if wb.tryWriteOneFrom(&wb.ackDisplaceReserve, isLocal) {
			return true
		}
	}
	if isLocal {
		// [CD8-DEADLOCK FIX] Drain the invalidation-driven dirty-flush reserve
		// (LOCAL DRAM destination, acyclic) FIRST, ahead of pendingLocalEvictions,
		// so an InvRsp-enabling writeback always makes forward progress even when
		// the shared admit lanes are clogged by cross-GPU write-throughs.
		if wb.tryWriteOneFrom(&wb.invDirtyFlushReserve, true) {
			return true
		}
		return wb.tryWriteOneFrom(&wb.pendingLocalEvictions, true)
	}
	// Remote-destination: drain Peer origin first, then Own.
	if wb.tryWriteOneFrom(&wb.pendingRemoteEvictionsPeer, false) {
		return true
	}
	return wb.tryWriteOneFrom(&wb.pendingRemoteEvictionsOwn, false)
}

// tryWriteOneFrom drains the head of one specific pending-eviction queue.
// isLocalDest selects the egress port (DRAM vs RDMA) AND the destination
// inflict/outgoing accounting. The ORIGIN accounting (own vs peer remote
// inflict counters, peer-bypass admit release) keys on trans.fromLocal.
func (wb *writeBufferStage) tryWriteOneFrom(
	queue *[]*transaction,
	isLocal bool,
) bool {
	if len(*queue) == 0 {
		return false
	}

	trans := (*queue)[0]

	// [ORIGIN-SPLIT] tooManyInflightEvictions gates a remote-bound eviction
	// against the sub-budget matching its ORIGIN (trans.fromLocal); local
	// keeps the single ceiling.
	if wb.tooManyInflightEvictions(isLocal, trans.fromLocal) {
		return false
	}

	// [L2 LOCAL/REMOTE SPLIT] remote (RDMA-bound) evictions egress on
	// remoteBottomPort, local (DRAM-bound) on bottomPort — each its own
	// CanSend budget so a saturated local-DRAM egress never starves the
	// remote-RDMA egress (the serve/ACK critical path) and vice versa.
	egress := wb.cache.bottomPort
	if !isLocal {
		egress = wb.cache.remoteBottomPort
	}
	if !egress.CanSend() {
		return false
	}

	lowModulePort := wb.cache.addressToPortMapper.Find(trans.evictingAddr)
	write := mem.WriteReqBuilder{}.
		WithSrc(egress.AsRemote()).
		WithDst(lowModulePort).
		WithPID(trans.evictingPID).
		WithAddress(trans.evictingAddr).
		WithData(trans.evictingData).
		WithDirtyMask(trans.evictingDirtyMask).
		Build()

	if trans.accessReq() != nil {
		write.VAddr = trans.accessReq().GetVAddr()
	}

	egress.Send(write)

	trans.evictionWriteReq = write
	trans.evictionToLocal = isLocal
	*queue = (*queue)[1:]
	wb.inflightEviction = append(wb.inflightEviction, trans)
	// [ITER17 F1] trans left the pending queue — release peer-bypass
	// admit slot so the next peer-incoming can take it. Note: we
	// release at PENDING→INFLIGHT transition, not at ACK, because the
	// cap is on PENDING growth (writeBufferReservedForRemote keys off
	// pending+inflight, but the unbounded growth was in PENDING).
	if !trans.fromLocal && wb.numPeerIncomingPending > 0 {
		wb.numPeerIncomingPending--
	}
	if isLocal {
		wb.numLocalInflightEviction++
	} else {
		wb.numRemoteInflightEviction++
		// [ORIGIN-SPLIT] bump the per-origin remote-inflict sub-counter and
		// decrement the matching per-origin pending counter.
		if trans.fromLocal {
			wb.numRemoteInflEvictOwn++
			if wb.numPendingRemoteEvictionsOwn > 0 {
				wb.numPendingRemoteEvictionsOwn--
			}
		} else {
			wb.numRemoteInflEvictPeer++
			if wb.numPendingRemoteEvictionsPeer > 0 {
				wb.numPendingRemoteEvictionsPeer--
			}
		}
	}

	tracing.TraceReqInitiate(write, wb.cache,
		tracing.MsgIDAtReceiver(write, wb.cache))

	what := ""
	if strings.Contains(fmt.Sprintf("%s", write.Meta().Dst), "DRAM") {
		what = "ToLocal"
	} else {
		what = "ToRemote"
	}
	wb.cache.incEvent(what)

	// if trans.writeToHomeNode {
	// 	fmt.Printf("[%s]\tWrite(%s -> %s) %x to %s\n",
	// 		wb.cache.Name(), trans.req().Meta().ID, write.Meta().ID, trans.evictingAddr, lowModulePort)
	// }
	// log.Printf("%.10f, %s, wb write to bottom， "+
	// " %s, %04X, %04X, (%d, %d), %v\n",
	// 	now, wb.cache.Name(),
	// 	trans.accessReq().Meta().ID,
	// 	trans.evictingAddr, trans.evictingAddr,
	// 	trans.block.SetID, trans.block.WayID,
	// 	trans.evictingData,findInflightFetchByFetchReadReqID
	// )

	return true
}

// [ITER10] write drains BOTH local and remote pending queues per tick.
// Critical structural fix: previously a single FIFO meant a LOCAL eviction
// blocked on DRAM at head would head-of-line-block a REMOTE eviction
// behind it (and vice versa). With separate queues each category can
// drain independently. bottomPort.CanSend is shared so the effective
// throughput is bounded by the port's capacity, but neither category
// starves the other when both have work.
func (wb *writeBufferStage) write() bool {
	madeProgress := false
	// Try remote first (cross-GPU is the critical path that backpressures
	// the entire kernel via REC's bypass cap).
	if wb.tryWriteOne(false) {
		madeProgress = true
	}
	if wb.tryWriteOne(true) {
		madeProgress = true
	}
	return madeProgress
}

// tryProcessDataReadyRsp processes a DataReadyRsp already drained from
// bottomPort. Returns true on success (dispatched to bank), false if it
// must be retried later (downstream buffer full). The caller decides
// whether to pop from pendingDataReady based on the return value.
func (wb *writeBufferStage) tryProcessDataReadyRsp(
	dataReady *mem.DataReadyRsp,
) bool {
	return wb.processDataReadyRsp(dataReady)
}

// applyWriteDoneRsp processes a WriteDoneRsp already drained from
// bottomPort. Always succeeds (only mutates internal state).
func (wb *writeBufferStage) applyWriteDoneRsp(writeDone *mem.WriteDoneRsp) {
	wb.processWriteDoneRsp(writeDone)
}

func (wb *writeBufferStage) processDataReadyRsp(
	dataReady *mem.DataReadyRsp,
) bool {
	trans := wb.findInflightFetchByFetchReadReqID(dataReady.RespondTo)

	if trans != nil && trans.responsing { // 이미 응답이 도착하여 처리 중인 trans -> discard
		wb.removeInflightFetch(trans)
		return true
	}

	if trans == nil { // prefetch 처리
		return wb.processPrefetch(dataReady)
	}

	bankIndex := bankID(
		trans.block,
		wb.cache.directory.WayAssociativity(),
		len(wb.cache.dirToBankBuffers),
	)
	bankBuf := wb.cache.writeBufferToBankBufferRsp(bankIndex, trans.fromLocal) // [R5 + BANK SPLIT] bankWriteFetched

	if !bankBuf.CanPush() {
		return false
	}

	trans.fetchedData = dataReady.Data
	trans.action = bankWriteFetched
	trans.mshrEntry.Data = dataReady.Data
	// trans.responsing = true
	wb.combineData(trans.mshrEntry)

	if trans.fromLocal {
		wb.cache.mshrLocalRemoved++
	} else {
		wb.cache.mshrRemoteRemoved++
	}
	wb.cache.mshr.Remove(trans.mshrEntry.PID, trans.mshrEntry.Address)

	bankBuf.Push(trans)

	wb.removeInflightFetch(trans)

	tracing.TraceReqFinalize(trans.fetchReadReq, wb.cache)

	if wb.cache.debugProcess && trans.accessReq() != nil && trans.accessReq().GetAddress() == wb.cache.debugAddress0 {
		fmt.Printf("[%s] [writebufferstage]\tReceived read rsp - 3.2.0: addr %x, action %d\n", wb.cache.name, trans.accessReq().GetAddress(), trans.action)
	}
	if wb.cache.debugProcess && trans.accessReq() != nil && trans.accessReq().GetAddress() == wb.cache.debugAddress1 {
		fmt.Printf("[%s] [writebufferstage]\tReceived read rsp - 3.2.0: addr %x, action %d\n", wb.cache.name, trans.accessReq().GetAddress(), trans.action)
	}
	// log.Printf("%.10f, %s, wb data fetched from bottom, "+
	//" %s, %04X, %04X, (%d, %d), %v\n",
	// 	now, wb.cache.Name(),
	// 	trans.accessReq().Meta().ID,
	// 	trans.accessReq().GetAddress(), trans.block.Tag,
	// 	trans.block.SetID, trans.block.WayID,
	// 	trans.fetchedData,
	// )

	return true
}

func (wb *writeBufferStage) combineData(mshrEntry *internal.MSHREntry) {
	mshrEntry.Block.DirtyMask = make([]bool, 1<<wb.cache.log2BlockSize)
	for _, t := range mshrEntry.Requests {
		trans := t.(*transaction)
		if trans.read != nil {
			continue
		}

		mshrEntry.Block.IsDirty = true
		write := trans.write
		_, offset := getCacheLineID(write.Address, wb.cache.log2BlockSize)

		for i := 0; i < len(write.Data); i++ {
			if write.DirtyMask == nil || write.DirtyMask[i] {
				index := offset + uint64(i)
				mshrEntry.Data[index] = write.Data[i]
				mshrEntry.Block.DirtyMask[index] = true
			}
		}
	}
}

func (wb *writeBufferStage) findInflightFetchByFetchReadReqID(
	id string,
) *transaction {
	for _, t := range wb.inflightFetch {
		if t.fetchReadReq.ID == id {
			return t
		}
	}

	return nil
	// panic("inflight read not found")
}

// findInflightFetchByAddress fallback used when RspTo-based match fails.
// If the response carries the expected fetch address, we can still
// identify the right transaction even if the ID got mangled in the
// RDMA→CD chain.
func (wb *writeBufferStage) findInflightFetchByAddress(addr uint64) *transaction {
	for _, t := range wb.inflightFetch {
		if t.fetchAddress == addr {
			return t
		}
	}
	return nil
}

func (wb *writeBufferStage) removeInflightFetch(f *transaction) {
	for i, trans := range wb.inflightFetch {
		if trans == f {
			wb.inflightFetch = append(
				wb.inflightFetch[:i],
				wb.inflightFetch[i+1:]...,
			)

			return
		}
	}

	panic("not found")
}

func (wb *writeBufferStage) processPrefetch(
	rsp *mem.DataReadyRsp,
) bool {
	// 1. MSHR 확인하기
	entry := wb.cache.mshr.Query(rsp.Origin.GetPID(), rsp.Origin.GetAddress())

	// 2. MSHR hit
	// 해당하는 trans에 대해 fetch가 온 것처럼 처리
	// 기존 trans는 따로 responsing 기록 (기존 trans에 대한 요청이 들어오면 prefetch처럼 처리하지 않고 discard하기 위해)
	if entry != nil {
		tr := entry.Requests[0].(*transaction)
		trans := *tr

		bankIndex := bankID(
			trans.block,
			wb.cache.directory.WayAssociativity(),
			len(wb.cache.dirToBankBuffers),
		)
		bankBuf := wb.cache.writeBufferToBankBufferRsp(bankIndex, trans.fromLocal) // [R5 + BANK SPLIT] bankWriteFetched derived

		if !bankBuf.CanPush() {
			return false
		}

		tr.responsing = true
		trans.fetchedData = rsp.Data
		trans.action = bankWriteFetched
		trans.mshrEntry.Data = rsp.Data
		trans.fetchedData = rsp.Data
		wb.combineData(trans.mshrEntry)
		if trans.fromLocal {
			wb.cache.mshrLocalRemoved++
		} else {
			wb.cache.mshrRemoteRemoved++
		}
		wb.cache.mshr.Remove(trans.mshrEntry.PID, trans.mshrEntry.Address)

		bankBuf.Push(&trans)

		if trans.fetchReadReq != nil {
			tracing.TraceReqFinalize(trans.fetchReadReq, wb.cache)
		}

		if wb.cache.debugProcess && trans.accessReq() != nil && trans.accessReq().GetAddress() == wb.cache.debugAddress0 {
			fmt.Printf("[%s] [writebufferstage]\tReceived read rsp - 3.2.1: addr %x, action %d\n", wb.cache.name, trans.accessReq().GetAddress(), trans.action)
		}
		if wb.cache.debugProcess && trans.accessReq() != nil && trans.accessReq().GetAddress() == wb.cache.debugAddress1 {
			fmt.Printf("[%s] [writebufferstage]\tReceived read rsp - 3.2.1: addr %x, action %d\n", wb.cache.name, trans.accessReq().GetAddress(), trans.action)
		}

		return true
	}

	// 3. MSHR miss
	// directory, bank에서 prefetch 처리 수행
	if true {
		fmt.Fprintf(os.Stdout, "[%s][processPrefetch]\tStart prefetching: %x\n", wb.cache.name, rsp.Origin.GetAddress())
	}
	trans := &transaction{
		id:           sim.GetIDGenerator().Generate(),
		read:         nil,
		write:        nil, // 2개 모두 nil인 경우, directory에서 prefetch로 처리
		fetchPID:     rsp.Origin.GetPID(),
		fetchAddress: rsp.Origin.GetAddress(),
		fetchedData:  rsp.Data,
		action:       bankWritePrefetched,
		prefetch:     rsp,
	}

	tracing.TraceReqReceive(rsp, wb.cache)
	wb.cache.incEvent("PrefetchStart")

	if !wb.cache.dirStageBuffer.CanPush() {
		// 1. 버리기

		wb.cache.incEvent("PrefetchDiscard - Busy")
		tracing.TraceReqFinalize(trans.prefetch, wb.cache)

		return true

		// 2. Buffer의 tail에 삽입하기
		// wb.cache.bottomPort.RetrieveIncoming()
		// wb.cache.bottomPort.GetIncomingBuf().Push(rsp)
		// return false

		// 3. 별도의 queue에 저장, background로 insert 수행
	}

	wb.cache.dirStageBuffer.Push(trans)

	if wb.cache.debugProcess && trans.accessReq() != nil && trans.accessReq().GetAddress() == wb.cache.debugAddress0 {
		fmt.Printf("[%s] [writebufferstage]\tReceived read prefetch - 3.3: addr %x, action %d\n", wb.cache.name, trans.accessReq().GetAddress(), trans.action)
	}
	if wb.cache.debugProcess && trans.accessReq() != nil && trans.accessReq().GetAddress() == wb.cache.debugAddress1 {
		fmt.Printf("[%s] [writebufferstage]\tReceived read prefetch - 3.3: addr %x, action %d\n", wb.cache.name, trans.accessReq().GetAddress(), trans.action)
	}

	return true
}

func (wb *writeBufferStage) processWriteDoneRsp(
	writeDone *mem.WriteDoneRsp,
) bool {
	for i := len(wb.inflightEviction) - 1; i >= 0; i-- {
		e := wb.inflightEviction[i]
		if e.evictionWriteReq.ID == writeDone.RespondTo {
			if e.evictionToLocal {
				wb.numLocalInflightEviction--
			} else {
				wb.numRemoteInflightEviction--
				// [ORIGIN-SPLIT] decrement the per-origin remote-inflict
				// sub-counter matching this eviction's ORIGIN (e.fromLocal).
				if e.fromLocal {
					wb.numRemoteInflEvictOwn--
				} else {
					wb.numRemoteInflEvictPeer--
				}
			}
			wb.inflightEviction = append(
				wb.inflightEviction[:i],
				wb.inflightEviction[i+1:]...,
			)
			tracing.TraceReqFinalize(e.evictionWriteReq, wb.cache)

			// log.Printf("%.10f, %s, wb write to bottom，
			//  %s, %04X, %04X, (%d, %d), %v\n",
			//  now, wb.cache.Name(),
			//  e.accessReq().Meta().ID,
			//  e.evictingAddr, e.evictingAddr,
			//  e.block.SetID, e.block.WayID,
			//  e.evictingData,
			// )

			return true
		}
	}

	// [ITER20 DIAG] No inflightEviction matched this ack's RespondTo:
	// the eviction stays counted forever (numRemoteInflightEviction leak).
	wb.writeDoneNoMatchCount++
	if wb.writeDoneNoMatchSample == "" {
		wb.writeDoneNoMatchSample = writeDone.RespondTo
	}
	return true
}

// pendingRemoteEvictionsLen reports the combined depth of the two ORIGIN
// remote-pending half-queues (own + peer). Used by writeBufferFull's PEER
// bucket and diagnostics so the destination-side accounting is unchanged by
// the origin split.
func (wb *writeBufferStage) pendingRemoteEvictionsLen() int {
	return len(wb.pendingRemoteEvictionsOwn) + len(wb.pendingRemoteEvictionsPeer)
}

// [ITER17 F2] writeBufferFull now checks the appropriate side bucket
// instead of the shared total. A local-side caller (fromLocal=true) is
// admitted as long as the LOCAL bucket has headroom even when REMOTE
// is saturated; vice versa for peer-side. Used by both
// processWriteBufferEvictAndWrite and processWriteBufferFlush.
func (wb *writeBufferStage) writeBufferFull(fromLocal bool) bool {
	if fromLocal {
		// LOCAL bucket = pendingLocalEvictions + numLocalInflightEviction
		used := len(wb.pendingLocalEvictions) + wb.numLocalInflightEviction
		return used >= wb.writeBufferCapacity
	}
	// PEER bucket = pendingRemoteEvictions(own+peer) + numRemoteInflightEviction
	used := wb.pendingRemoteEvictionsLen() + wb.numRemoteInflightEviction
	cap := wb.writeBufferPeerCapacity
	if cap <= 0 {
		cap = wb.writeBufferCapacity // fall back to legacy if peer cap unset
	}
	return used >= cap
}

// Legacy accessor preserved for compatibility with any external caller
// (e.g. flusher) that does not know the side. Reports the total entry
// count vs the LOCAL cap (mirrors pre-iter17 semantics for those callers).
func (wb *writeBufferStage) writeBufferFullLegacy() bool {
	numEntry := len(wb.pendingLocalEvictions) + wb.pendingRemoteEvictionsLen() + len(wb.inflightEviction)
	return numEntry >= wb.writeBufferCapacity
}

// tooManyOutgoingRemote returns true when this L2 already holds the
// configured number of OWN remote-bound evictions across the pending+inflight
// stages. New OWN remote-bound evictions are refused at admit until the
// inflight portion drains via processWriteDoneRsp. Always false when
// the eviction targets local DRAM (isLocal=true) or the cap is disabled.
//
// [ORIGIN-SPLIT] This cap is applied ONLY to OWN-origin pending+inflight
// (numPendingRemoteEvictionsOwn + numRemoteInflEvictOwn). The peer-bypass
// admit lane never consulted this gate, so peer-serve work must not be
// counted here — otherwise peer-serve fills a counter that only gates own.
func (wb *writeBufferStage) tooManyOutgoingRemote(isLocal bool) bool {
	if isLocal || wb.maxOutgoingRemotePending <= 0 {
		return false
	}
	return wb.numPendingRemoteEvictionsOwn+wb.numRemoteInflEvictOwn >=
		wb.maxOutgoingRemotePending
}

func (wb *writeBufferStage) tooManyInflightFetches() bool {
	return len(wb.inflightFetch) >= wb.maxInflightFetch
}

// tooManyInflightEvictions gates an eviction admit-to-inflight by DESTINATION
// (isLocal) and, on the remote side, by ORIGIN (fromLocal).
//
// [ITER16 SPLIT INFLIGHT CAP] each DESTINATION side caps INDEPENDENTLY: LOCAL
// drains via DRAM ACK, REMOTE via peer L2 ACK; a remote-inflict pile must not
// block local admits whose drain path is independent.
//
// [ORIGIN-SPLIT] On the REMOTE-destination side the single 128-slot ceiling
// was the confirmed deadlock seed: own cross-GPU evictions (fromLocal=true)
// and peer-serve displacement flushes (fromLocal=false) shared it, so own work
// could saturate it and the peer-serve flush could never drain → no
// WriteDoneRsp/ack → mutual cross-GPU deadlock. The remote side now gates each
// origin against its own sub-budget (maxRemoteInflEvictOwn /
// maxRemoteInflEvictPeer), which sum to the existing remote ceiling — peer-
// serve always retains its reserved slots and own can never starve it.
func (wb *writeBufferStage) tooManyInflightEvictions(isLocal, fromLocal bool) bool {
	if isLocal {
		return wb.numLocalInflightEviction >= wb.maxInflightEviction
	}
	if fromLocal {
		return wb.numRemoteInflEvictOwn >= wb.maxRemoteInflEvictOwn
	}
	return wb.numRemoteInflEvictPeer >= wb.maxRemoteInflEvictPeer
}

// _unused_tooManyInflightEvictions_old is the pre-iter16 shared-cap
// version kept here as a reference. Do not call; left for review.
func (wb *writeBufferStage) _unused_tooManyInflightEvictions_old(isLocal bool) bool {
	total := wb.numLocalInflightEviction + wb.numRemoteInflightEviction
	if total >= wb.maxInflightEviction {
		return true
	}
	if isLocal {
		localLimit := wb.maxInflightEviction - wb.maxInflightEviction/4
		return wb.numLocalInflightEviction >= localLimit
	}
	return false
}

func (wb *writeBufferStage) Reset() {
	wb.cache.writeBufferBuffer.Clear()
	wb.cache.writeBufferBufferRemote.Clear()
	wb.cache.writeBufferFetchBuffer.Clear()
	wb.currentEvictionSrcBuf = nil
	// [ITER10] reset both split pending queues.
	// [ORIGIN-SPLIT] reset both remote-pending origin halves.
	wb.pendingLocalEvictions = nil
	wb.pendingRemoteEvictionsOwn = nil
	wb.pendingRemoteEvictionsPeer = nil
	wb.invDirtyFlushReserve = nil // [CD8-DEADLOCK FIX]
	wb.ackDisplaceReserve = nil   // [SD-ACK-RESERVE]
	wb.inflightFetch = nil
	wb.inflightEviction = nil
	// [RESPONSE-DECOUPLE] [ORIGIN-SPLIT] reset both deferred-flush origin halves.
	wb.deferredFlushOwn = nil
	wb.deferredFlushPeer = nil
	wb.numLocalInflightEviction = 0
	wb.numRemoteInflightEviction = 0
	// [ORIGIN-SPLIT] reset the per-origin remote-inflict sub-counters.
	wb.numRemoteInflEvictOwn = 0
	wb.numRemoteInflEvictPeer = 0
	// [R6] reset all 4 halves.
	wb.pendingDataReadyLocal = nil
	wb.pendingDataReadyRemote = nil
	wb.pendingWriteDoneLocal = nil
	wb.pendingWriteDoneRemote = nil
	// [ORIGIN-SPLIT] reset both pending-remote origin counters.
	wb.numPendingRemoteEvictionsOwn = 0
	wb.numPendingRemoteEvictionsPeer = 0
	wb.numPeerIncomingPending = 0 // [ITER17 F1]
}
