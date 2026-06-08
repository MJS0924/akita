package REC

import (
	"testing"

	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/sim"
)

// [ITER19 R2 TEST]
//
// R2 verifies that an INV-RSP returning from a peer GPU lands in a
// separate ingress lane (invRspBuffer) from incoming INV REQs
// (invReqBuffer). When invReqBuffer is saturated, the topparser must
// still admit INV RSPs from the dedicated RDMAInvRspPort.
//
// Pattern: seed [InvReq, InvRsp, InvReq, InvRsp, ...] alternation into
// the topparser's two ingress sources, fill invReqBuffer to capacity
// (blocking REQ admit), and verify that all RSPs drain into
// invRspBuffer regardless.
//
// Self-contained: we exercise the processReq dispatch path directly by
// constructing a *Comp with minimal fields (invReqBuffer, invRspBuffer)
// and calling processReq. No engine / port wiring required.

func TestPeerInvRspNotBlockedByPeerInvReq(t *testing.T) {
	// Capacities chosen so REQ buffer fills quickly and RSP buffer
	// stays loose — mirrors the deadlock pattern where invReqBuffer
	// = 16/16 but invRspBuffer was sized similarly without isolation.
	cache := &Comp{
		state:         cacheStateRunning,
		invReqBuffer:  sim.NewBuffer("invReqBuf", 2),
		invRspBuffer:  sim.NewBuffer("invRspBuf", 8),
	}
	p := &topParser{cache: cache}

	// Pre-fill invReqBuffer to capacity so all subsequent InvReq
	// admits will be refused.
	cache.invReqBuffer.Push(mem.InvReqBuilder{}.WithAddress(0x100).Build())
	cache.invReqBuffer.Push(mem.InvReqBuilder{}.WithAddress(0x200).Build())

	// Seed the [REQ, RSP, REQ, RSP, ...] interleave directly through
	// processReq (which is what the per-port Tick loops call).
	rsps := []*mem.InvRsp{
		mem.InvRspBuilder{}.WithRspTo("q1").Build(),
		mem.InvRspBuilder{}.WithRspTo("q2").Build(),
		mem.InvRspBuilder{}.WithRspTo("q3").Build(),
		mem.InvRspBuilder{}.WithRspTo("q4").Build(),
	}
	reqs := []*mem.InvReq{
		mem.InvReqBuilder{}.WithAddress(0x300).Build(),
		mem.InvReqBuilder{}.WithAddress(0x400).Build(),
		mem.InvReqBuilder{}.WithAddress(0x500).Build(),
	}

	interleave := []sim.Msg{}
	for i := 0; i < len(rsps); i++ {
		if i < len(reqs) {
			interleave = append(interleave, reqs[i])
		}
		interleave = append(interleave, rsps[i])
	}

	// Run N tick-equivalent admit attempts. Each RSP must be admitted
	// (RSP buffer has slack); each REQ must be REFUSED (REQ buffer
	// pre-filled).
	rspsAdmitted := 0
	reqsRefused := 0
	for _, msg := range interleave {
		ok := p.processReq(msg, false)
		switch msg.(type) {
		case *mem.InvRsp:
			if !ok {
				t.Fatalf("R2: InvRsp admit rejected though RSP buffer has slack "+
					"(rspBuf size=%d cap=%d)",
					cache.invRspBuffer.Size(), cache.invRspBuffer.Capacity())
			}
			rspsAdmitted++
		case *mem.InvReq:
			if ok {
				t.Fatalf("R2: InvReq admit accepted though REQ buffer is full "+
					"(reqBuf size=%d cap=%d)",
					cache.invReqBuffer.Size(), cache.invReqBuffer.Capacity())
			}
			reqsRefused++
		}
	}

	if rspsAdmitted != len(rsps) {
		t.Fatalf("R2: expected %d RSPs admitted (REQ HoL must not block), got %d",
			len(rsps), rspsAdmitted)
	}
	if reqsRefused != len(reqs) {
		t.Fatalf("R2: expected %d REQs refused (REQ buffer full), got %d",
			len(reqs), reqsRefused)
	}
	if cache.invRspBuffer.Size() != len(rsps) {
		t.Fatalf("R2: RSP buffer size = %d, want %d",
			cache.invRspBuffer.Size(), len(rsps))
	}
}
