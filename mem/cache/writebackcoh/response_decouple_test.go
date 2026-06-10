package writebackcoh

import (
	"testing"

	"github.com/sarchlab/akita/v4/sim"
)

// [RESPONSE-DECOUPLE TEST]
//
// The cross-GPU serve deadlock closes when a response (WriteDoneRsp /
// DataReadyRsp) cannot be emitted because the displacement victim it would
// trigger cannot enter writeBufferBuffer — and writeBufferBuffer is full of
// the L2's own remote evictions whose caps (numRemoteInflightEviction=128,
// tooManyOutgoingRemote=384) are saturated. The fix emits the response FIRST
// and parks the victim in a bounded deferredFlush list when writeBufferBuffer
// is momentarily full, then re-injects it (drainDeferredFlush) the instant a
// slot frees — the victim still traverses the UNCHANGED eviction caps, so no
// cap is bypassed and no eviction capacity is added.
//
// These tests guard the new mechanism at the writeBufferStage level. The
// end-to-end proof (finalizeWriteHit/finalizeBankWriteFetched emit their
// response while the caps are saturated, and the sim advances past win80) is
// the integration run with the akita RTM + engine-stop dump.

// [ORIGIN-SPLIT] deferredFlush is now partitioned by ORIGIN, each half capped
// at maxDeferredFlush/2. These tests exercise the PEER half (fromLocal=false),
// which is the serve-ack-bearing path. maxDeferredFlush=4 → peer half cap = 2,
// preserving the original "2 peer slots" assertion strength.
func newWBForDeferTest(remoteCap int) *Comp {
	cache := &Comp{}
	cache.writeBufferBuffer = sim.NewBuffer("WBB", 4)
	cache.writeBufferBufferRemote = sim.NewBuffer("WBBRemote", remoteCap)
	cache.writeBuffer = &writeBufferStage{
		cache:            cache,
		maxDeferredFlush: 4, // peer half = 2 (and own half = 2)
	}
	return cache
}

// A peer (fromLocal=false) displacement flush must have SOMEWHERE to go even
// when writeBufferBufferRemote is full — the bounded deferral list — so the
// response that already left is never stranded and no dirty data is lost.
func TestDeferredFlushAbsorbsWhenWriteBufferFull(t *testing.T) {
	cache := newWBForDeferTest(1)
	wb := cache.writeBuffer

	cache.writeBufferBufferRemote.Push(&transaction{fromLocal: false})
	if cache.writeBufferBufferCanPush(false) {
		t.Fatal("precondition: writeBufferBufferRemote should be full")
	}

	if !wb.deferFlushCanPush(false) {
		t.Fatal("deferFlush must accept the victim when writeBufferBuffer is full " +
			"(else the already-emitted response would strand the victim)")
	}
	wb.deferFlushPush(&transaction{fromLocal: false, action: writeBufferFlush})
	if len(wb.deferredFlushPeer) != 1 {
		t.Fatalf("deferredFlushPeer len = %d, want 1", len(wb.deferredFlushPeer))
	}
}

// deferFlushCanPush honors the bounded per-origin cap (the reordering buffer
// cannot grow without bound — it is a transient shock absorber, not added
// capacity). The PEER half cap = maxDeferredFlush/2 = 2.
func TestDeferredFlushBounded(t *testing.T) {
	cache := newWBForDeferTest(1)
	wb := cache.writeBuffer
	wb.deferFlushPush(&transaction{fromLocal: false})
	wb.deferFlushPush(&transaction{fromLocal: false})
	if wb.deferFlushCanPush(false) {
		t.Fatal("deferFlush must reject past the peer half-cap (maxDeferredFlush/2)")
	}
}

// [ORIGIN-SPLIT] An own-origin (fromLocal=true) deferred-flush flood must NOT
// consume the peer half's slots — this is exactly the cross-GPU re-close the
// split prevents. Fill the OWN half to its cap; the PEER half must stay open.
func TestDeferredFlushOwnFloodDoesNotStarvePeer(t *testing.T) {
	cache := newWBForDeferTest(1)
	wb := cache.writeBuffer
	// Own half cap = 2 — fill it.
	wb.deferFlushPush(&transaction{fromLocal: true})
	wb.deferFlushPush(&transaction{fromLocal: true})
	if wb.deferFlushCanPush(true) {
		t.Fatal("own half should be full at its cap")
	}
	if !wb.deferFlushCanPush(false) {
		t.Fatal("REGRESSION: own deferred-flush flood starved the peer-serve half")
	}
}

// drainDeferredFlush re-injects the parked victim into the normal
// writeBufferBuffer admit path the instant a slot frees, and must NEVER drop
// a victim when no slot is free (dirty-data-safety). It drains the PEER half
// FIRST so the serve ack is never stranded behind own work.
func TestDrainDeferredFlushReinjects(t *testing.T) {
	cache := newWBForDeferTest(1)
	wb := cache.writeBuffer

	// writeBufferBufferRemote starts empty (cap=1, CanPush true). Park a victim.
	wb.deferFlushPush(&transaction{fromLocal: false, action: writeBufferFlush})

	if !wb.drainDeferredFlush() {
		t.Fatal("drainDeferredFlush should re-inject when a slot is free")
	}
	if len(wb.deferredFlushPeer) != 0 {
		t.Fatalf("deferredFlushPeer len = %d, want 0 after drain", len(wb.deferredFlushPeer))
	}
	if cache.writeBufferBufferCanPush(false) {
		t.Fatal("victim should now occupy writeBufferBufferRemote")
	}

	// writeBufferBufferRemote now full: drain must HOLD the victim, not drop it.
	wb.deferFlushPush(&transaction{fromLocal: false, action: writeBufferFlush})
	if wb.drainDeferredFlush() {
		t.Fatal("drainDeferredFlush should not move when no slot is free")
	}
	if len(wb.deferredFlushPeer) != 1 {
		t.Fatalf("deferredFlushPeer len = %d, want 1 (victim held, not dropped)",
			len(wb.deferredFlushPeer))
	}
}

// drainDeferredFlush drains the PEER half before the OWN half: when both have a
// parked victim and exactly one writeBufferBufferRemote slot is free, the PEER
// victim must be the one re-injected (it carries the serve ack).
func TestDrainDeferredFlushPeerFirst(t *testing.T) {
	cache := newWBForDeferTest(1) // one remote slot free
	wb := cache.writeBuffer

	wb.deferFlushPush(&transaction{fromLocal: true, action: writeBufferFlush})
	wb.deferFlushPush(&transaction{fromLocal: false, action: writeBufferFlush})

	if !wb.drainDeferredFlush() {
		t.Fatal("drainDeferredFlush should re-inject one victim")
	}
	if len(wb.deferredFlushPeer) != 0 {
		t.Fatalf("peer half should have drained FIRST, len=%d want 0", len(wb.deferredFlushPeer))
	}
	if len(wb.deferredFlushOwn) != 1 {
		t.Fatalf("own half should still hold its victim, len=%d want 1", len(wb.deferredFlushOwn))
	}
}
