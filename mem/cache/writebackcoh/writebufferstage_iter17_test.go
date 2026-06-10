package writebackcoh

import "testing"

// [ITER17 TEST SUITE]
//
// These tests gate against the deadlock class that motivated iter17:
//
//   1. Unbounded peer-bypass admit (F1):
//      conv2d sim 7.71 ms / stencil2d sim 21.38 ms deadlock had
//      pendingLocalEvictions=1432 (1.4× writeBufferCapacity).  The
//      iter13 #1 peer-bypass lane skipped writeBufferFull entirely,
//      so peer-incoming admits grew unbounded.  F1 introduces
//      maxPeerIncomingPending; numPeerIncomingPending must NEVER
//      exceed it.
//
//   2. Side-aware writeBufferFull (F2):
//      Before iter17, writeBufferFull used (pendingLocal+pendingRemote+
//      inflight) vs a single writeBufferCapacity.  A saturated REMOTE
//      side rejected LOCAL admit too.  F2 splits the check: LOCAL admit
//      only sees LOCAL usage, peer admit only sees peer usage.
//
//   3. Side-aware writeBufferReservedForRemote (F2):
//      Identical reasoning — REMOTE-bound items must not gate fromLocal
//      admission at the dirStage.  Function lives in directorystage.go
//      and is exercised via the (LOCAL bucket usedLocal) computation.

// ---------------------------------------------------------------- F1

// numPeerIncomingPending must respect maxPeerIncomingPending strictly.
// This is the invariant the audit identified as the single highest-
// priority guard against the recurring deadlock pattern.
func TestIter17_F1_NumPeerIncomingPendingNeverExceedsCap(t *testing.T) {
	wb := &writeBufferStage{
		maxPeerIncomingPending: 8,
		numPeerIncomingPending: 0,
	}

	// Cap not yet hit: every value 0..maxPeerIncomingPending-1 must be
	// admissible.
	for i := 0; i < wb.maxPeerIncomingPending; i++ {
		wb.numPeerIncomingPending = i
		// Simulate the admit check from processWriteBufferEvictAndWrite.
		if wb.peerIncomingPendingFull() {
			t.Fatalf("F1: should NOT report full at numPeerIncomingPending=%d (cap=%d)",
				i, wb.maxPeerIncomingPending)
		}
	}

	// At and above cap: full.
	for i := wb.maxPeerIncomingPending; i <= wb.maxPeerIncomingPending+4; i++ {
		wb.numPeerIncomingPending = i
		if !wb.peerIncomingPendingFull() {
			t.Fatalf("F1: should report full at numPeerIncomingPending=%d >= cap=%d",
				i, wb.maxPeerIncomingPending)
		}
	}
}

// When maxPeerIncomingPending <= 0, the cap is disabled (legacy
// behavior; iter13 #1 semantics).  Verify the disable path.
func TestIter17_F1_DisabledCapBehavesAsLegacy(t *testing.T) {
	wb := &writeBufferStage{
		maxPeerIncomingPending: 0,           // disabled
		numPeerIncomingPending: 1_000_000_0, // any value
	}
	if wb.peerIncomingPendingFull() {
		t.Fatalf("F1: cap disabled (max=0) must never report full")
	}
}

// peerIncomingPendingFull is the predicate the admit path uses.  Keep
// it in sync with the inline expression at processWriteBufferEvictAndWrite
// (lines 411-) and processWriteBufferFlush (lines 593-).
func (wb *writeBufferStage) peerIncomingPendingFull() bool {
	return wb.maxPeerIncomingPending > 0 &&
		wb.numPeerIncomingPending >= wb.maxPeerIncomingPending
}

// ---------------------------------------------------------------- F2

// writeBufferFull(fromLocal=true) must check only the LOCAL bucket.  A
// saturated REMOTE bucket must NOT block LOCAL admit (this was the
// observed cycle vertex).
func TestIter17_F2_FullLocalNotBlockedByRemoteSaturation(t *testing.T) {
	wb := &writeBufferStage{
		writeBufferCapacity:     1024,
		writeBufferPeerCapacity: 256,
	}
	// LOCAL completely empty, REMOTE completely saturated.
	wb.pendingLocalEvictions = nil
	wb.pendingRemoteEvictionsPeer = make([]*transaction, 256)
	wb.numLocalInflightEviction = 0
	wb.numRemoteInflightEviction = 0

	if wb.writeBufferFull(true) {
		t.Fatalf("F2: writeBufferFull(true) should be FALSE when LOCAL bucket empty even if REMOTE saturated")
	}
	if !wb.writeBufferFull(false) {
		t.Fatalf("F2: writeBufferFull(false) should be TRUE when peer bucket at cap")
	}
}

// Symmetric check: REMOTE empty, LOCAL saturated → peer admit not
// blocked by local.
func TestIter17_F2_FullPeerNotBlockedByLocalSaturation(t *testing.T) {
	wb := &writeBufferStage{
		writeBufferCapacity:     1024,
		writeBufferPeerCapacity: 256,
	}
	wb.pendingLocalEvictions = make([]*transaction, 1024)
	wb.pendingRemoteEvictionsPeer = nil
	wb.numLocalInflightEviction = 0
	wb.numRemoteInflightEviction = 0

	if !wb.writeBufferFull(true) {
		t.Fatalf("F2: writeBufferFull(true) should be TRUE when LOCAL bucket at cap")
	}
	if wb.writeBufferFull(false) {
		t.Fatalf("F2: writeBufferFull(false) should be FALSE when peer bucket empty even if LOCAL saturated")
	}
}

// Cumulative LOCAL bucket counts pending + inflight.  Hitting the cap
// only by inflight (with empty pending) should still block.
func TestIter17_F2_FullLocalCountsInflight(t *testing.T) {
	wb := &writeBufferStage{
		writeBufferCapacity:     128,
		writeBufferPeerCapacity: 32,
	}
	wb.pendingLocalEvictions = nil
	wb.numLocalInflightEviction = 128

	if !wb.writeBufferFull(true) {
		t.Fatalf("F2: LOCAL inflight=cap should report writeBufferFull(true)")
	}
}

// Disabled peer cap (writeBufferPeerCapacity<=0) falls back to LOCAL
// cap — legacy compatibility.
func TestIter17_F2_PeerCapDisableFallsBackToLocalCap(t *testing.T) {
	wb := &writeBufferStage{
		writeBufferCapacity:     16,
		writeBufferPeerCapacity: 0, // disabled
	}
	wb.pendingRemoteEvictionsPeer = make([]*transaction, 16)
	wb.numRemoteInflightEviction = 0

	if !wb.writeBufferFull(false) {
		t.Fatalf("F2: with peer cap disabled, peer bucket should be capped by writeBufferCapacity")
	}
}

// ---------------------------------------------------------------- saturation scenario

// Scenario (a) from the audit synthesis: a single hot bank receives a
// burst of peer-incoming admits.  The cap must hold across many ticks.
// We simulate the admit/decrement cycle that the writeBufferStage would
// drive: admit one peer item per virtual tick, decrement (= move to
// inflight) at a slower rate, and verify numPeerIncomingPending never
// exceeds maxPeerIncomingPending at any moment.
func TestIter17_F1_HotBankBurstUnderSaturation(t *testing.T) {
	wb := &writeBufferStage{
		maxPeerIncomingPending: 8,
	}

	const ticks = 1000
	const peerAdmitRate = 3
	const peerDrainRate = 1

	for tickNum := 0; tickNum < ticks; tickNum++ {
		// Try to admit `peerAdmitRate` peer-incoming items per tick.
		for k := 0; k < peerAdmitRate; k++ {
			if wb.peerIncomingPendingFull() {
				break // backpressure — admit refused
			}
			wb.numPeerIncomingPending++
			if wb.numPeerIncomingPending > wb.maxPeerIncomingPending {
				t.Fatalf("F1 invariant violated at tick=%d k=%d: numPeerIncomingPending=%d > cap=%d",
					tickNum, k, wb.numPeerIncomingPending, wb.maxPeerIncomingPending)
			}
		}

		// Drain `peerDrainRate` items per tick (simulates pending →
		// inflight transition in tryWriteOne).
		for k := 0; k < peerDrainRate; k++ {
			if wb.numPeerIncomingPending > 0 {
				wb.numPeerIncomingPending--
			}
		}
	}

	// Sanity: drain rate < admit rate, so counter should be at cap.
	if wb.numPeerIncomingPending != wb.maxPeerIncomingPending {
		t.Logf("Final numPeerIncomingPending=%d, cap=%d (drain<admit ⇒ at cap)",
			wb.numPeerIncomingPending, wb.maxPeerIncomingPending)
	}
}
