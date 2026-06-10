package writebackcoh

import "testing"

// [ORIGIN-SPLIT] tooManyInflightEvictions now takes (isLocal, fromLocal).
//
//   - LOCAL destination (isLocal=true): single ceiling maxInflightEviction,
//     keyed off numLocalInflightEviction (origin-agnostic; LOCAL drains via
//     DRAM ACK, independent of the cross-GPU cycle).
//   - REMOTE destination (isLocal=false): partitioned by ORIGIN into
//     maxRemoteInflEvictOwn (own=3/4) and maxRemoteInflEvictPeer (peer=1/4),
//     summing to the legacy remote ceiling. Own saturation can NEVER block a
//     peer-serve eviction (the confirmed deadlock seed) and vice versa.

// helper: a wb with the production 3/4 own / 1/4 peer partition of `cap`.
func wbWithRemoteSplit(cap int) *writeBufferStage {
	return &writeBufferStage{
		maxInflightEviction:    cap,
		maxRemoteInflEvictOwn:  cap - cap/4,
		maxRemoteInflEvictPeer: cap / 4,
	}
}

func TestTooManyInflightEvictions_LocalDestUsesFullCeiling(t *testing.T) {
	wb := wbWithRemoteSplit(128)

	wb.numLocalInflightEviction = 127
	if wb.tooManyInflightEvictions(true, true) {
		t.Fatalf("local dest=127 < cap 128 should not be too many")
	}
	wb.numLocalInflightEviction = 128
	if !wb.tooManyInflightEvictions(true, true) {
		t.Fatalf("local dest=128 == cap should be too many")
	}
}

// The KEY invariant: a saturated OWN remote sub-budget must NOT block a
// PEER-serve remote eviction (this is the confirmed cross-GPU seed).
func TestTooManyInflightEvictions_OwnSaturationDoesNotBlockPeer(t *testing.T) {
	wb := wbWithRemoteSplit(128) // own=96, peer=32

	// Own remote inflight saturated to its full sub-budget.
	wb.numRemoteInflEvictOwn = 96
	wb.numRemoteInflEvictPeer = 0

	if !wb.tooManyInflightEvictions(false, true) {
		t.Fatalf("own remote=96 == own sub-budget should be too many for own")
	}
	// Peer-serve remote eviction must still be admissible — it has its own
	// reserved 32 slots untouched by own saturation.
	if wb.tooManyInflightEvictions(false, false) {
		t.Fatalf("REGRESSION: peer-serve remote eviction blocked by OWN saturation")
	}
}

// Symmetric: a saturated PEER sub-budget must not block OWN.
func TestTooManyInflightEvictions_PeerSaturationDoesNotBlockOwn(t *testing.T) {
	wb := wbWithRemoteSplit(128) // own=96, peer=32

	wb.numRemoteInflEvictPeer = 32
	wb.numRemoteInflEvictOwn = 0

	if !wb.tooManyInflightEvictions(false, false) {
		t.Fatalf("peer remote=32 == peer sub-budget should be too many for peer")
	}
	if wb.tooManyInflightEvictions(false, true) {
		t.Fatalf("REGRESSION: own remote eviction blocked by PEER saturation")
	}
}

// Each origin sub-budget enforces its own ceiling exactly.
func TestTooManyInflightEvictions_OriginSubBudgetEdges(t *testing.T) {
	cases := []struct {
		cap, wantOwn, wantPeer int
	}{
		{128, 96, 32},
		{256, 192, 64},
		{512, 384, 128},
		{4, 3, 1},
	}
	for _, c := range cases {
		wb := wbWithRemoteSplit(c.cap)
		if wb.maxRemoteInflEvictOwn != c.wantOwn || wb.maxRemoteInflEvictPeer != c.wantPeer {
			t.Fatalf("cap=%d partition own=%d peer=%d, want own=%d peer=%d (sum must == cap)",
				c.cap, wb.maxRemoteInflEvictOwn, wb.maxRemoteInflEvictPeer, c.wantOwn, c.wantPeer)
		}
		// own edge
		wb.numRemoteInflEvictOwn = c.wantOwn - 1
		if wb.tooManyInflightEvictions(false, true) {
			t.Errorf("cap=%d own=%d should be fine", c.cap, c.wantOwn-1)
		}
		wb.numRemoteInflEvictOwn = c.wantOwn
		if !wb.tooManyInflightEvictions(false, true) {
			t.Errorf("cap=%d own=%d should hit own sub-budget", c.cap, c.wantOwn)
		}
		// peer edge
		wb.numRemoteInflEvictOwn = 0
		wb.numRemoteInflEvictPeer = c.wantPeer - 1
		if wb.tooManyInflightEvictions(false, false) {
			t.Errorf("cap=%d peer=%d should be fine", c.cap, c.wantPeer-1)
		}
		wb.numRemoteInflEvictPeer = c.wantPeer
		if !wb.tooManyInflightEvictions(false, false) {
			t.Errorf("cap=%d peer=%d should hit peer sub-budget", c.cap, c.wantPeer)
		}
	}
}

// No net capacity increase: the two REMOTE sub-budgets sum to the legacy
// single remote ceiling.
func TestTooManyInflightEvictions_RemoteSubBudgetsSumToCeiling(t *testing.T) {
	for _, cap := range []int{4, 100, 128, 256, 512} {
		wb := wbWithRemoteSplit(cap)
		if got := wb.maxRemoteInflEvictOwn + wb.maxRemoteInflEvictPeer; got != cap {
			t.Errorf("cap=%d: own+peer=%d, want %d (NO net capacity increase)", cap, got, cap)
		}
	}
}
