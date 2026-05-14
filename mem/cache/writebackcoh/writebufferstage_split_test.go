package writebackcoh

import "testing"

// Asymmetric soft cap semantics:
//   - total = numLocal + numRemote, hard-capped at maxInflightEviction
//   - local additionally hard-capped at 3/4 of maxInflightEviction
//   - remote has no per-side cap beyond the total
//   - effect: remote-dominated workloads (like stencil2d cross-GPU
//     evictions) can use all maxInflightEviction slots when local is
//     idle, while local cannot starve remote (always >= 1/4 reserve).

func TestTooManyInflightEvictions_RemoteUnboundedWhenLocalIdle(t *testing.T) {
	wb := &writeBufferStage{maxInflightEviction: 128}

	// Remote can grow up to total cap when local is idle.
	wb.numLocalInflightEviction = 0
	wb.numRemoteInflightEviction = 127
	if wb.tooManyInflightEvictions(false) {
		t.Fatalf("remote=127 local=0 should not be too many (total<128)")
	}
	wb.numRemoteInflightEviction = 128
	if !wb.tooManyInflightEvictions(false) {
		t.Fatalf("remote=128 local=0 should be too many (total>=128)")
	}
}

func TestTooManyInflightEvictions_LocalCappedAtThreeQuarters(t *testing.T) {
	wb := &writeBufferStage{maxInflightEviction: 128}

	// Local hard cap at 96 = 128 - 128/4.
	wb.numLocalInflightEviction = 95
	wb.numRemoteInflightEviction = 0
	if wb.tooManyInflightEvictions(true) {
		t.Fatalf("local=95 should not be too many")
	}
	wb.numLocalInflightEviction = 96
	if !wb.tooManyInflightEvictions(true) {
		t.Fatalf("local=96 should be too many (hits 3/4 hard cap)")
	}
}

func TestTooManyInflightEvictions_TotalCapEnforced(t *testing.T) {
	wb := &writeBufferStage{maxInflightEviction: 128}

	// total cap blocks both sides regardless of which one asks.
	wb.numLocalInflightEviction = 50
	wb.numRemoteInflightEviction = 78 // total = 128
	if !wb.tooManyInflightEvictions(true) {
		t.Fatalf("total=128: local request should be blocked by total cap")
	}
	if !wb.tooManyInflightEvictions(false) {
		t.Fatalf("total=128: remote request should be blocked by total cap")
	}

	// Just below total: both can proceed (subject to local's 3/4 cap).
	wb.numLocalInflightEviction = 50
	wb.numRemoteInflightEviction = 77 // total = 127
	if wb.tooManyInflightEvictions(true) {
		t.Fatalf("total=127, local=50<96: should not be too many")
	}
	if wb.tooManyInflightEvictions(false) {
		t.Fatalf("total=127: remote should not be too many")
	}
}

func TestTooManyInflightEvictions_RemoteReserveProtected(t *testing.T) {
	wb := &writeBufferStage{maxInflightEviction: 128}

	// When local fills to its 96 cap, remote always retains the
	// remaining 32 slots — it never gets crowded out below 1/4.
	wb.numLocalInflightEviction = 96
	wb.numRemoteInflightEviction = 0
	if wb.tooManyInflightEvictions(false) {
		t.Fatalf("remote=0 with local=96 should not be too many (32 reserved)")
	}
	wb.numRemoteInflightEviction = 31
	if wb.tooManyInflightEvictions(false) {
		t.Fatalf("remote=31 (within 32 reserve) should not be too many")
	}
	wb.numRemoteInflightEviction = 32
	if !wb.tooManyInflightEvictions(false) {
		t.Fatalf("remote=32 + local=96 = 128: should hit total cap")
	}
}

func TestTooManyInflightEvictions_QuotaArithmetic(t *testing.T) {
	cases := []struct {
		cap, wantLocalLimit int
	}{
		{128, 96},  // 128 - 32
		{256, 192}, // 256 - 64
		{512, 384}, // 512 - 128
		{100, 75},  // 100 - 25
		{4, 3},     // 4 - 1
	}
	for _, c := range cases {
		wb := &writeBufferStage{maxInflightEviction: c.cap}
		// Local just below limit OK.
		wb.numLocalInflightEviction = c.wantLocalLimit - 1
		wb.numRemoteInflightEviction = 0
		if wb.tooManyInflightEvictions(true) {
			t.Errorf("cap=%d local=%d should be fine", c.cap, c.wantLocalLimit-1)
		}
		// Local at limit blocked.
		wb.numLocalInflightEviction = c.wantLocalLimit
		if !wb.tooManyInflightEvictions(true) {
			t.Errorf("cap=%d local=%d should hit local cap", c.cap, c.wantLocalLimit)
		}
		// Remote can use full cap when local idle.
		wb.numLocalInflightEviction = 0
		wb.numRemoteInflightEviction = c.cap - 1
		if wb.tooManyInflightEvictions(false) {
			t.Errorf("cap=%d remote=%d local=0 should be fine", c.cap, c.cap-1)
		}
		wb.numRemoteInflightEviction = c.cap
		if !wb.tooManyInflightEvictions(false) {
			t.Errorf("cap=%d remote=%d should hit total cap", c.cap, c.cap)
		}
	}
}
