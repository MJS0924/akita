package coherence

import (
	"testing"

	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/sim"
)

// [ITER19 D1 / D2 / D3 TEST]
//
// CD coherence directory iter19 fixes:
//   D1 — Dedicated RDMAInvRspPort ingress (separate from RDMAInvPort).
//   D2 — Split bottomSenderBuffer into bottomSenderTransBuffer (data
//        trans) and bottomSenderInvBuffer (InvReqs only).
//   D3 — invRspBuffer remains the dedicated InvRsp landing pad.
//
// Combined test pattern: seed [InvReq, InvRsp, InvReq, InvRsp, ...]
// interleaved through the CD topparser dispatch path. Fill the InvReq
// destination buffer (bottomSenderInvBuffer) to capacity; verify all
// InvRsps still drain into invRspBuffer regardless. Also verifies that
// data trans (read/write) routed via bottomSenderTransBuffer is
// untouched by the InvReq stall.

func TestCD_RDMAInvRspNotBlockedByRDMAInvReq(t *testing.T) {
	// CD post-iter19 has split buffers per the D2 design. Sized so
	// the InvReq buffer fills quickly to model the head-of-line stall.
	cache := &Comp{
		state:                    cacheStateRunning,
		bottomSenderInvBuffer:    sim.NewBuffer("bsInv", 2),
		bottomSenderTransBuffer:  sim.NewBuffer("bsTrans", 8),
		invRspBuffer:             sim.NewBuffer("invRsp", 8),
		dirStageBuffer:           sim.NewBuffer("dirStage", 8),
	}
	p := &topParser{cache: cache}

	// Pre-fill InvReq buffer to capacity. New InvReqs will be refused.
	cache.bottomSenderInvBuffer.Push(
		mem.InvReqBuilder{}.WithAddress(0x10).Build())
	cache.bottomSenderInvBuffer.Push(
		mem.InvReqBuilder{}.WithAddress(0x20).Build())

	rsps := []*mem.InvRsp{
		mem.InvRspBuilder{}.WithRspTo("a").Build(),
		mem.InvRspBuilder{}.WithRspTo("b").Build(),
		mem.InvRspBuilder{}.WithRspTo("c").Build(),
		mem.InvRspBuilder{}.WithRspTo("d").Build(),
	}
	reqs := []*mem.InvReq{
		mem.InvReqBuilder{}.WithAddress(0x30).Build(),
		mem.InvReqBuilder{}.WithAddress(0x40).Build(),
		mem.InvReqBuilder{}.WithAddress(0x50).Build(),
	}

	// Interleave [REQ, RSP, REQ, RSP, ...].
	interleave := []sim.Msg{}
	for i := 0; i < len(rsps); i++ {
		if i < len(reqs) {
			interleave = append(interleave, reqs[i])
		}
		interleave = append(interleave, rsps[i])
	}

	rspsAdmitted, reqsRefused := 0, 0
	for _, msg := range interleave {
		ok := p.processReq(msg, false)
		switch msg.(type) {
		case *mem.InvRsp:
			if !ok {
				t.Fatalf("D1: InvRsp refused though invRspBuffer has slack (size=%d cap=%d)",
					cache.invRspBuffer.Size(), cache.invRspBuffer.Capacity())
			}
			rspsAdmitted++
		case *mem.InvReq:
			if ok {
				t.Fatalf("D2: InvReq admitted though bottomSenderInvBuffer is full "+
					"(size=%d cap=%d) — REQ HoL guard broken",
					cache.bottomSenderInvBuffer.Size(),
					cache.bottomSenderInvBuffer.Capacity())
			}
			reqsRefused++
		}
	}

	if rspsAdmitted != len(rsps) {
		t.Fatalf("D1: expected all %d RSPs admitted under REQ stall, got %d",
			len(rsps), rspsAdmitted)
	}
	if reqsRefused != len(reqs) {
		t.Fatalf("D2: expected all %d REQs refused under buffer-full, got %d",
			len(reqs), reqsRefused)
	}

	// D3: invRspBuffer holds exactly the RSPs we admitted.
	if cache.invRspBuffer.Size() != len(rsps) {
		t.Fatalf("D3: invRspBuffer size = %d, want %d",
			cache.invRspBuffer.Size(), len(rsps))
	}

	// D2 cross-check: bottomSenderTransBuffer must not have been
	// polluted by InvReq/InvRsp messages.
	if cache.bottomSenderTransBuffer.Size() != 0 {
		t.Fatalf("D2: bottomSenderTransBuffer received unexpected non-trans messages (size=%d)",
			cache.bottomSenderTransBuffer.Size())
	}
}
