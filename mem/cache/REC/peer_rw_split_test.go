package REC

import "testing"

// [PEER R/W SPLIT] The dir's peer-inflight cap is partitioned so incoming
// WRITES (peer evictions → home DRAM, a guaranteed sink) always retain a
// forwarding slot, even when incoming READS (→ home L2, which can stall)
// saturate the cap. This is the win79 deadlock fix: at the freeze, all 256
// peer-inflight entries were READs, so the dir could not forward a single
// eviction → the eviction never reached DRAM → no ACK → sender's outgoing
// slot stayed held → symmetric cross-GPU tail deadlock. Writes always
// flowing breaks it.
func TestPeerRWSplit_WriteReservedFromReadSaturation(t *testing.T) {
	bs := &bottomSender{maxPeerInflightRequest: 256, maxInflightRequest: 128}
	// reserveWrite = 256/4 = 64 → reads held to 192, writes use full 256.

	// Reads saturate at their cap (192).
	bs.numPeerInflightRead = 192
	bs.numPeerInflightRequest = 192

	// A peer READ is now blocked (192 >= 256-64).
	if !bs.tooManyInflightRequest(false, false) {
		t.Fatalf("peer read at 192 must be blocked (read cap = 256-64 = 192)")
	}
	// A peer WRITE (eviction) must STILL proceed — 64 slots reserved for it.
	if bs.tooManyInflightRequest(false, true) {
		t.Fatalf("peer WRITE must NOT be blocked while reads saturate — this is the deadlock fix")
	}

	// Writes may fill toward the full cap; reads cannot touch the reserve.
	bs.numPeerInflightRequest = 255
	if bs.tooManyInflightRequest(false, true) {
		t.Fatalf("peer write at total=255 must proceed (<256)")
	}
	bs.numPeerInflightRequest = 256
	if !bs.tooManyInflightRequest(false, true) {
		t.Fatalf("peer write at full total=256 must be blocked")
	}

	// A read below its cap proceeds.
	bs.numPeerInflightRead = 191
	bs.numPeerInflightRequest = 191
	if bs.tooManyInflightRequest(false, false) {
		t.Fatalf("peer read at 191 must proceed (<192)")
	}

	// maxPeerInflightRequest<=0 disables the peer cap entirely (legacy).
	bs2 := &bottomSender{maxPeerInflightRequest: 0}
	bs2.numPeerInflightRead = 9999
	if bs2.tooManyInflightRequest(false, false) || bs2.tooManyInflightRequest(false, true) {
		t.Fatalf("peer cap disabled (<=0) must never block")
	}

	// LOCAL path unchanged by the peer split (kept the iter20 local-cap fix:
	// localLimit = maxInflightRequest - maxInflightRequest/4 = 96).
	bsL := &bottomSender{maxInflightRequest: 128}
	bsL.localInflightRequest = make([]*transaction, 95)
	if bsL.tooManyInflightRequest(true, false) {
		t.Fatalf("local at 95 must proceed (local cap 96)")
	}
	bsL.localInflightRequest = make([]*transaction, 96)
	if !bsL.tooManyInflightRequest(true, false) {
		t.Fatalf("local at 96 must be blocked (local cap 96)")
	}
}
