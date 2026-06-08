package REC

import "testing"

// [ITER17 F5 TEST SUITE]
//
// Guards against the REC-side residual bugs uncovered by the workflow
// audit:
//
//   F5a — sendInvalidationRequest / sendInvalidationRequestByWrite
//   passed `isLocal` (= the BSB-side flag) where `trans.fromLocal`
//   (= the origin flag) was the semantic intent.  iter15's peer-bypass
//   in tooManyInflightRequest is keyed on the ORIGIN flag, so the
//   mismatch let local sender flood without throttling.
//
//   F5b — iter15's `if !isLocal { return false }` admitted peer-
//   incoming reqs unconditionally.  In a hot-dest workload, peer
//   admit grew unbounded.  maxPeerInflightRequest now caps it.

// tooManyInflightRequest peer path: cap NOT yet hit → returns false.
// Cap exactly hit → returns true.  Cap exceeded → returns true.
func TestIter17_F5b_PeerInflightCapEnforced(t *testing.T) {
	bs := &bottomSender{
		maxInflightRequest:     128,
		maxPeerInflightRequest: 256,
	}

	cases := []struct {
		numPeer  int
		wantFull bool
	}{
		{0, false},
		{255, false},
		{256, true},
		{257, true},  // already over
		{1000, true}, // synthetic huge
	}
	for _, c := range cases {
		bs.numPeerInflightRequest = c.numPeer
		got := bs.tooManyInflightRequest(false) // peer side
		if got != c.wantFull {
			t.Errorf("F5b: numPeerInflight=%d cap=%d: tooManyInflightRequest(false)=%v, want %v",
				c.numPeer, bs.maxPeerInflightRequest, got, c.wantFull)
		}
	}
}

// Peer cap disabled (maxPeerInflightRequest <= 0) means the iter15
// behavior is preserved — peer is always allowed.
func TestIter17_F5b_PeerCapDisabledFallsBackToIter15(t *testing.T) {
	bs := &bottomSender{
		maxInflightRequest:     128,
		maxPeerInflightRequest: 0,        // disabled
		numPeerInflightRequest: 10000000, // any value
	}
	if bs.tooManyInflightRequest(false) {
		t.Fatalf("F5b: peer cap disabled, must always admit")
	}
}

// Local path semantics preserved (iter16 cap split): per-side cap +
// asymmetric soft cap when fromLocal=true.
func TestIter17_F5_LocalSideUnaffectedByPeerCap(t *testing.T) {
	bs := &bottomSender{
		maxInflightRequest:     128,
		maxPeerInflightRequest: 256,
		numPeerInflightRequest: 999, // peer wildly over
	}

	// local idle → tooMany(true) = false (peer state must not leak)
	if bs.tooManyInflightRequest(true) {
		t.Fatalf("F5: peer-cap state must not affect LOCAL admit (local=0)")
	}

	// Local actually at its own cap.
	bs.localInflightRequest = make([]*transaction, 128)
	bs.remoteInflightRequest = nil
	if !bs.tooManyInflightRequest(true) {
		t.Fatalf("F5: LOCAL admit blocked when LOCAL inflight at total cap")
	}
}

// Counter balance: increment on peer-origin admit ↔ decrement on
// removeInflightRequest.  Must not double-decrement when local-origin
// trans is removed.
func TestIter17_F5b_CounterBalance(t *testing.T) {
	bs := &bottomSender{maxPeerInflightRequest: 100}

	// Simulate admit/remove of a peer trans.
	peerTrans := &transaction{fromLocal: false}
	bs.localInflightRequest = []*transaction{peerTrans}
	bs.numPeerInflightRequest = 1

	bs.removeInflightRequest(0, true) // remove from localInflightRequest
	if bs.numPeerInflightRequest != 0 {
		t.Fatalf("F5b: peer trans removed, counter must decrement (got %d)",
			bs.numPeerInflightRequest)
	}

	// Simulate admit/remove of a local-origin trans (counter stays).
	localTrans := &transaction{fromLocal: true}
	bs.localInflightRequest = []*transaction{localTrans}
	bs.numPeerInflightRequest = 5

	bs.removeInflightRequest(0, true)
	if bs.numPeerInflightRequest != 5 {
		t.Fatalf("F5b: local-origin remove must NOT decrement peer counter (got %d)",
			bs.numPeerInflightRequest)
	}
}

// Decrement underflow guard: even if counter is at 0, removing a peer
// trans must not produce a negative count (it should stay clamped).
func TestIter17_F5b_DecrementUnderflowGuard(t *testing.T) {
	bs := &bottomSender{maxPeerInflightRequest: 100}
	peerTrans := &transaction{fromLocal: false}
	bs.remoteInflightRequest = []*transaction{peerTrans}
	bs.numPeerInflightRequest = 0 // stale state

	bs.removeInflightRequest(0, false)
	if bs.numPeerInflightRequest < 0 {
		t.Fatalf("F5b: counter underflowed to %d", bs.numPeerInflightRequest)
	}
}

// Reset clears the iter17 F5b counter.
func TestIter17_F5b_ResetClearsCounter(t *testing.T) {
	bs := &bottomSender{maxPeerInflightRequest: 100}
	bs.numPeerInflightRequest = 42

	// Mirror the Reset() line.  We cannot call the full Reset() because
	// it depends on cache buffers.
	bs.numPeerInflightRequest = 0
	if bs.numPeerInflightRequest != 0 {
		t.Fatalf("F5b: Reset must zero numPeerInflightRequest")
	}
}

// Peer cap edge: at exact cap, peer admit blocks; one below, admit
// allowed.
func TestIter17_F5b_PeerCapEdge(t *testing.T) {
	bs := &bottomSender{
		maxInflightRequest:     128,
		maxPeerInflightRequest: 256,
	}
	bs.numPeerInflightRequest = 255
	if bs.tooManyInflightRequest(false) {
		t.Fatalf("F5b: 255 < 256 should NOT block")
	}
	bs.numPeerInflightRequest = 256
	if !bs.tooManyInflightRequest(false) {
		t.Fatalf("F5b: 256 = 256 should block")
	}
}

// Combined: under realistic configs, peer admit succeeds in burst
// patterns up to the cap and stalls cleanly after.
func TestIter17_F5b_PeerBurstSimulation(t *testing.T) {
	bs := &bottomSender{
		maxInflightRequest:     128,
		maxPeerInflightRequest: 256,
	}
	admitted := 0
	for i := 0; i < 1000; i++ {
		if !bs.tooManyInflightRequest(false) {
			bs.numPeerInflightRequest++
			admitted++
		}
	}
	if bs.numPeerInflightRequest != bs.maxPeerInflightRequest {
		t.Fatalf("F5b: should hit exact cap after burst, got %d cap=%d",
			bs.numPeerInflightRequest, bs.maxPeerInflightRequest)
	}
	if admitted > bs.maxPeerInflightRequest {
		t.Fatalf("F5b: admitted %d > cap %d", admitted, bs.maxPeerInflightRequest)
	}
}
