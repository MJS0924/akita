package superdirectory

import "testing"

// Verify asymmetric soft cap: remote can use full maxInflightRequest
// budget; local hard-capped at 3/4; total bounded by maxInflightRequest.

func TestSDTooManyInflightRequest_RemoteUnboundedWhenLocalIdle(t *testing.T) {
	bs := &bottomSender{maxInflightRequest: 128}
	for i := 0; i < 127; i++ {
		bs.remoteInflightRequest = append(bs.remoteInflightRequest, nil)
	}
	if bs.tooManyInflightRequest(false) {
		t.Fatalf("remote=127 local=0 should not be too many")
	}
	bs.remoteInflightRequest = append(bs.remoteInflightRequest, nil)
	if !bs.tooManyInflightRequest(false) {
		t.Fatalf("remote=128 local=0 should hit total cap")
	}
}

func TestSDTooManyInflightRequest_LocalCappedAtThreeQuarters(t *testing.T) {
	bs := &bottomSender{maxInflightRequest: 128}
	for i := 0; i < 95; i++ {
		bs.localInflightRequest = append(bs.localInflightRequest, nil)
	}
	if bs.tooManyInflightRequest(true) {
		t.Fatalf("local=95 should not be too many")
	}
	bs.localInflightRequest = append(bs.localInflightRequest, nil)
	if !bs.tooManyInflightRequest(true) {
		t.Fatalf("local=96 should hit 3/4 hard cap")
	}
}

func TestSDTooManyInflightRequest_TotalCapEnforced(t *testing.T) {
	bs := &bottomSender{maxInflightRequest: 128}
	for i := 0; i < 50; i++ {
		bs.localInflightRequest = append(bs.localInflightRequest, nil)
	}
	for i := 0; i < 78; i++ {
		bs.remoteInflightRequest = append(bs.remoteInflightRequest, nil)
	}
	if !bs.tooManyInflightRequest(true) {
		t.Fatalf("total=128: local request blocked")
	}
	if !bs.tooManyInflightRequest(false) {
		t.Fatalf("total=128: remote request blocked")
	}
}

func TestSDTooManyInflightRequest_RemoteReserveProtected(t *testing.T) {
	bs := &bottomSender{maxInflightRequest: 128}
	// local at its 96 cap → remote always retains 32 reserve.
	for i := 0; i < 96; i++ {
		bs.localInflightRequest = append(bs.localInflightRequest, nil)
	}
	for i := 0; i < 31; i++ {
		bs.remoteInflightRequest = append(bs.remoteInflightRequest, nil)
	}
	if bs.tooManyInflightRequest(false) {
		t.Fatalf("remote=31 within 32 reserve should be allowed")
	}
	bs.remoteInflightRequest = append(bs.remoteInflightRequest, nil)
	if !bs.tooManyInflightRequest(false) {
		t.Fatalf("remote=32 + local=96 = 128: total cap")
	}
}
