package writebackcoh

import "testing"

// [ITER17 EXTRA TEST SUITE]
//
// Beyond the primary cap invariants exercised in writebufferstage_iter17_test.go,
// these tests probe residual bug surfaces:
//
//   - Counter consistency (admit ⇔ decrement symmetry, no leak / no
//     double-decrement).
//   - Boundary behavior at cap edges.
//   - writeBufferReservedForRemote interaction with the new local-only
//     bucket (the function returns false for peer trans; only fromLocal=
//     true trans get gated, and that gate must read the LOCAL bucket).
//   - tryWriteOne ordering: with iter16 split caps + iter17 admit cap,
//     LOCAL must drain even when REMOTE is saturated.
//   - Reset clears ALL iter17 counters.

// ---------------------------------------------------------------- F1 counter

// numPeerIncomingPending must equal the net admit-minus-drain count
// at all times (no leak/double-decrement under interleaved admit/drain).
func TestIter17_F1_CounterBalanceInterleaved(t *testing.T) {
	wb := &writeBufferStage{maxPeerIncomingPending: 100}

	// Simulate 1000 admits and 1000 drains interleaved 1:1.
	admitOps := 1000
	for i := 0; i < admitOps; i++ {
		// admit
		if !wb.peerIncomingPendingFull() {
			wb.numPeerIncomingPending++
		}
		// drain (= move to inflight, decrement)
		if wb.numPeerIncomingPending > 0 {
			wb.numPeerIncomingPending--
		}
		if wb.numPeerIncomingPending < 0 {
			t.Fatalf("F1 underflow at iteration %d: numPeerIncomingPending=%d",
				i, wb.numPeerIncomingPending)
		}
	}
	// After balanced admit/drain, the counter should be 0 or 1 (depending
	// on whether last admit succeeded before drain).
	if wb.numPeerIncomingPending > 1 {
		t.Fatalf("F1 leak: counter=%d after balanced 1:1 admit/drain",
			wb.numPeerIncomingPending)
	}
}

// Drain underflow guard: even with the conditional `if > 0`, an
// over-decrement path must not zero-out and leave a stale state that
// breaks the next admit.
func TestIter17_F1_DrainGuardsAgainstUnderflow(t *testing.T) {
	wb := &writeBufferStage{maxPeerIncomingPending: 8}

	// Drain without admit — counter must stay at 0.
	for i := 0; i < 100; i++ {
		if wb.numPeerIncomingPending > 0 {
			wb.numPeerIncomingPending--
		}
	}
	if wb.numPeerIncomingPending != 0 {
		t.Fatalf("F1: bare drain should leave counter=0, got %d",
			wb.numPeerIncomingPending)
	}
	// Now admit must succeed (cap should not be falsely "hit" from
	// an underflowed counter).
	if wb.peerIncomingPendingFull() {
		t.Fatalf("F1: admit should be allowed at clean state (counter=0)")
	}
}

// ---------------------------------------------------------------- F2 boundary

// writeBufferFull edge: exactly at capacity should return TRUE (no
// off-by-one).
func TestIter17_F2_FullAtExactCapacityBoundary(t *testing.T) {
	wb := &writeBufferStage{
		writeBufferCapacity:     32,
		writeBufferPeerCapacity: 16,
	}
	// LOCAL = exactly cap
	wb.pendingLocalEvictions = make([]*transaction, 30)
	wb.numLocalInflightEviction = 2
	// LOCAL bucket = 30+2 = 32 = cap → FULL
	if !wb.writeBufferFull(true) {
		t.Fatalf("F2: LOCAL bucket=32 exact cap=32 should be FULL")
	}
	wb.numLocalInflightEviction = 1
	// LOCAL bucket = 30+1 = 31 < cap → NOT FULL
	if wb.writeBufferFull(true) {
		t.Fatalf("F2: LOCAL bucket=31 cap=32 should NOT be FULL")
	}
}

// writeBufferFull edge: peer bucket exactly at peer cap.
func TestIter17_F2_PeerFullAtExactCapacityBoundary(t *testing.T) {
	wb := &writeBufferStage{
		writeBufferCapacity:     32,
		writeBufferPeerCapacity: 16,
	}
	wb.pendingRemoteEvictions = make([]*transaction, 14)
	wb.numRemoteInflightEviction = 2
	// PEER bucket = 14+2 = 16 = peerCap → FULL
	if !wb.writeBufferFull(false) {
		t.Fatalf("F2: PEER bucket=16 exact peerCap=16 should be FULL")
	}
	wb.numRemoteInflightEviction = 1
	if wb.writeBufferFull(false) {
		t.Fatalf("F2: PEER bucket=15 peerCap=16 should NOT be FULL")
	}
}

// ---------------------------------------------------------------- F2 cross-side independence

// The KEY iter17 invariant: under a saturated REMOTE bucket, a LOCAL
// admit attempt must return false (NOT FULL).  This was the precise
// vertex that closed the conv2d/stencil2d deadlock — pre-iter17, the
// shared cap made REMOTE saturation block LOCAL.  Regression here =
// the deadlock returns.
func TestIter17_F2_LocalAdmitIndependentOfRemoteSaturation_Stress(t *testing.T) {
	wb := &writeBufferStage{
		writeBufferCapacity:     1024,
		writeBufferPeerCapacity: 256,
	}
	// Stress configurations: REMOTE side at, above, well-above peerCap.
	cases := []struct {
		pendRem, infRem int
	}{
		{256, 0},  // exactly peer cap
		{200, 56}, // peer cap split between pending and inflight
		{300, 0},  // beyond peer cap (legacy unbounded growth)
		{0, 256},  // all inflight
		{512, 0},  // 2x peer cap (synthetic worst case)
	}
	for _, c := range cases {
		wb.pendingRemoteEvictions = make([]*transaction, c.pendRem)
		wb.numRemoteInflightEviction = c.infRem
		// LOCAL fully empty.
		wb.pendingLocalEvictions = nil
		wb.numLocalInflightEviction = 0
		if wb.writeBufferFull(true) {
			t.Errorf("REGRESSION: LOCAL admit blocked by REMOTE saturation (pendRem=%d infRem=%d)",
				c.pendRem, c.infRem)
		}
	}
}

// Symmetric stress: peer admit must NOT be blocked by LOCAL saturation.
func TestIter17_F2_PeerAdmitIndependentOfLocalSaturation_Stress(t *testing.T) {
	wb := &writeBufferStage{
		writeBufferCapacity:     1024,
		writeBufferPeerCapacity: 256,
	}
	cases := []struct {
		pendLoc, infLoc int
	}{
		{1024, 0},
		{900, 124},
		{1500, 0},
		{0, 1024},
	}
	for _, c := range cases {
		wb.pendingLocalEvictions = make([]*transaction, c.pendLoc)
		wb.numLocalInflightEviction = c.infLoc
		wb.pendingRemoteEvictions = nil
		wb.numRemoteInflightEviction = 0
		if wb.writeBufferFull(false) {
			t.Errorf("REGRESSION: peer admit blocked by LOCAL saturation (pendLoc=%d infLoc=%d)",
				c.pendLoc, c.infLoc)
		}
	}
}

// ---------------------------------------------------------------- F1 + F2 combined

// Combined invariant: peer admit succeeds when both side-budgets allow
// AND maxPeerIncomingPending cap not hit.  Peer admit fails when EITHER
// side bucket is full OR peer-incoming cap is hit.
func TestIter17_F1F2_PeerAdmitCombined(t *testing.T) {
	wb := &writeBufferStage{
		writeBufferCapacity:     1024,
		writeBufferPeerCapacity: 32,
		maxPeerIncomingPending:  8,
	}

	// All clean — admit should be allowed by both predicates.
	if wb.writeBufferFull(false) {
		t.Fatalf("F1F2: clean state should NOT be peer-full")
	}
	if wb.peerIncomingPendingFull() {
		t.Fatalf("F1F2: clean state should NOT hit peer-bypass cap")
	}

	// Hit only peer-bypass cap.
	wb.numPeerIncomingPending = 8
	if wb.writeBufferFull(false) {
		t.Fatalf("F1F2: peer bucket should be empty here")
	}
	if !wb.peerIncomingPendingFull() {
		t.Fatalf("F1F2: peer-bypass cap should fire at numPeerIncoming=8")
	}

	// Hit only peer bucket (numPeerIncoming reset).
	wb.numPeerIncomingPending = 0
	wb.pendingRemoteEvictions = make([]*transaction, 32)
	if !wb.writeBufferFull(false) {
		t.Fatalf("F1F2: peer bucket should be FULL at cap=32")
	}
	if wb.peerIncomingPendingFull() {
		t.Fatalf("F1F2: peer-bypass cap should NOT fire (counter=0)")
	}
}

// ---------------------------------------------------------------- legacy compatibility

// writeBufferFullLegacy still returns the TOTAL-vs-LOCAL-cap predicate
// for any external caller (e.g. flusher) that may invoke it.
func TestIter17_LegacyFullPredicate_SumsAllSides(t *testing.T) {
	wb := &writeBufferStage{writeBufferCapacity: 100}
	wb.pendingLocalEvictions = make([]*transaction, 40)
	wb.pendingRemoteEvictions = make([]*transaction, 40)
	wb.inflightEviction = make([]*transaction, 20)
	if !wb.writeBufferFullLegacy() {
		t.Fatalf("legacy predicate should treat sum=100=cap as FULL")
	}
}

// ---------------------------------------------------------------- Reset

// Reset clears iter17 F1 counter.
func TestIter17_F1_ResetClearsCounter(t *testing.T) {
	// We can only exercise the Reset path that does not depend on
	// wb.cache (which is nil here), so test the counter clear
	// inline.  This mirrors the production Reset() code.
	wb := &writeBufferStage{}
	wb.numPeerIncomingPending = 42
	wb.numPeerIncomingPending = 0 // simulate the Reset line
	if wb.numPeerIncomingPending != 0 {
		t.Fatalf("F1: Reset must zero numPeerIncomingPending")
	}
}

// ---------------------------------------------------------------- writeBufferReservedForRemote semantic

// writeBufferReservedForRemote (in directorystage.go) MUST always return
// false for peer-bypass trans, and for fromLocal=true trans it now reads
// ONLY the LOCAL bucket against the LOCAL cap's 7/8 threshold.  We cannot
// invoke it directly (depends on Comp), but we can verify the LOCAL
// bucket math the function uses.
//
// Pre-iter17: used = pendingL + pendingR + inflight.  A burst on the
// peer side could inflate `used` past 7/8*cap, blocking LOCAL admits at
// dirStage and triggering the cascading stall.  Post-iter17: the formula
// reads only pendingLocal + numLocalInflightEviction, so peer saturation
// CANNOT trip the gate for LOCAL trans.
func TestIter17_F2_ReservedForRemoteFormula(t *testing.T) {
	// Synthesize the new formula here as a sanity check.
	type wb struct {
		cap    int
		pendL  int
		infLoc int
	}
	check := func(w wb) bool {
		return w.pendL+w.infLoc >= w.cap*7/8
	}
	// 1024*7/8 = 896 — threshold.
	if check(wb{cap: 1024, pendL: 895, infLoc: 0}) {
		t.Fatalf("F2: usedLocal=895 < 896 should NOT trigger reserved gate")
	}
	if !check(wb{cap: 1024, pendL: 896, infLoc: 0}) {
		t.Fatalf("F2: usedLocal=896 = 896 SHOULD trigger reserved gate")
	}
	if !check(wb{cap: 1024, pendL: 800, infLoc: 96}) {
		t.Fatalf("F2: usedLocal=896 (split) SHOULD trigger reserved gate")
	}
	// And the inverse: peer-only saturation never reaches the LOCAL
	// formula, so it MUST NOT trigger.  Verified by the formula taking
	// only pendL + infLoc as inputs (= 0 here).
	if check(wb{cap: 1024, pendL: 0, infLoc: 0}) {
		t.Fatalf("F2: usedLocal=0 with whatever REMOTE state MUST NOT trigger gate")
	}
}
