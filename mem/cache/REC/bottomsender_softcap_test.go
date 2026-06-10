package REC

import "testing"

// Same asymmetric soft cap semantics as superdirectory.

func TestRECTooManyInflightRequest_RemoteUnboundedWhenLocalIdle(t *testing.T) {
	bs := &bottomSender{maxInflightRequest: 128}
	for i := 0; i < 127; i++ {
		bs.remoteInflightRequest = append(bs.remoteInflightRequest, nil)
	}
	if bs.tooManyInflightRequest(false, false) {
		t.Fatalf("remote=127 local=0 should not be too many")
	}
	bs.remoteInflightRequest = append(bs.remoteInflightRequest, nil)
	if !bs.tooManyInflightRequest(false, false) {
		t.Fatalf("remote=128 should hit total cap")
	}
}

func TestRECTooManyInflightRequest_LocalCappedAtThreeQuarters(t *testing.T) {
	bs := &bottomSender{maxInflightRequest: 128}
	for i := 0; i < 95; i++ {
		bs.localInflightRequest = append(bs.localInflightRequest, nil)
	}
	if bs.tooManyInflightRequest(true, false) {
		t.Fatalf("local=95 should not be too many")
	}
	bs.localInflightRequest = append(bs.localInflightRequest, nil)
	if !bs.tooManyInflightRequest(true, false) {
		t.Fatalf("local=96 should hit 3/4 hard cap")
	}
}

func TestRECTooManyInflightRequest_RemoteReserveProtected(t *testing.T) {
	bs := &bottomSender{maxInflightRequest: 128}
	for i := 0; i < 96; i++ {
		bs.localInflightRequest = append(bs.localInflightRequest, nil)
	}
	for i := 0; i < 31; i++ {
		bs.remoteInflightRequest = append(bs.remoteInflightRequest, nil)
	}
	if bs.tooManyInflightRequest(false, false) {
		t.Fatalf("remote=31 within 32 reserve should be allowed")
	}
	bs.remoteInflightRequest = append(bs.remoteInflightRequest, nil)
	if !bs.tooManyInflightRequest(false, false) {
		t.Fatalf("remote=32 + local=96 total cap")
	}
}
