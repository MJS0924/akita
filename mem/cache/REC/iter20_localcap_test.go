package REC

import "testing"

// [ITER20] tooManyInflightRequest(isLocal=true) must depend ONLY on the
// LOCAL inflight count — never on REMOTE saturation. This is the win80
// stencil2d deadlock fix: previously a shared (local+remote) total cap let
// a saturated remote branch HoL-block local forwarding (returnFalse2
// "tooManyInflightRequest localInflight=9/128" while remoteInflight≈119),
// jamming L1→dir→L2 and freezing the GPU.
func TestIter20_LocalForwardNotBlockedByRemoteSaturation(t *testing.T) {
	mk := func(nLocal, nRemote int) *bottomSender {
		bs := &bottomSender{maxInflightRequest: 128}
		for i := 0; i < nLocal; i++ {
			bs.localInflightRequest = append(bs.localInflightRequest, &transaction{})
		}
		for i := 0; i < nRemote; i++ {
			bs.remoteInflightRequest = append(bs.remoteInflightRequest, &transaction{})
		}
		return bs
	}

	// Remote saturated (119), local nearly idle (9): old shared total
	// (9+119=128) blocked this; the fix must NOT block local.
	if mk(9, 119).tooManyInflightRequest(true, false) {
		t.Fatalf("local(9) must not be blocked by remote saturation(119)")
	}
	// Remote at full 128, local 0: local still must not block.
	if mk(0, 128).tooManyInflightRequest(true, false) {
		t.Fatalf("local(0) must not be blocked even when remote=128")
	}
	// Local sub-cap = 128 - 128/4 = 96. local=95 ok, local=96 blocked.
	if mk(95, 0).tooManyInflightRequest(true, false) {
		t.Fatalf("local(95) < 96 must be allowed")
	}
	if !mk(96, 0).tooManyInflightRequest(true, false) {
		t.Fatalf("local(96) hits the 3/4 local cap, must be blocked")
	}
	// Local cap is independent of remote: local=96 blocked regardless.
	if !mk(96, 100).tooManyInflightRequest(true, false) {
		t.Fatalf("local(96) must be blocked by its own cap, remote irrelevant")
	}
}
