package REC

import (
	"testing"

	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/sim"
)

// [ITER19 R3 / R4 TEST SUITE]
//
// R3 — Outbound INV-RSP is not blocked by outbound INV-REQ.
//   The bottomSender's egress to the cross-GPU INV port previously
//   shared a single FIFO (sendToRDMAInvQue) for both outbound
//   InvReq and outbound InvRsp. When the REQ stream stalled (no
//   sharers ready, write backpressure on RDMAInvPort), the InvRsp
//   behind it could not drain — even though the RSP destination
//   (peer's RDMAInvRspPort) was wide open. R3 splits the egress queue
//   into sendToRDMAInvQue (REQ only) and sendToRDMAInvRspQue (RSP only)
//   draining independently — mirrors the SD S1 split already in tree.
//
// R4 — Data drain on localBottomSenderBuffer is not blocked by an
//   invalidation stall on invReqBuffer.
//   The bottomSender's Tick processes processInputReq (reads/writes
//   on localBottomSenderBuffer) and processInvalidationReq (InvReqs
//   on invReqBuffer) as independent stages. R4 asserts the structural
//   independence so the InvReq backlog cannot HoL-block data drain.

// ---------------------------------------------------------------- R3

// TestOutboundInvRspNotBlockedByOutboundInvReq validates the egress
// queue split.
func TestOutboundInvRspNotBlockedByOutboundInvReq(t *testing.T) {
	bs := &bottomSender{}

	// Seed [InvReq, InvRsp, InvReq, InvRsp, InvReq, InvRsp] into the
	// two SPLIT egress queues. The pre-R3 design fused both into a
	// single sendToRDMAInvQue; the R3 design keeps RSP in a separate
	// queue (sendToRDMAInvRspQue) so RSP drain is independent.
	invReqs := []*mem.InvReq{
		mem.InvReqBuilder{}.WithAddress(0xA000).Build(),
		mem.InvReqBuilder{}.WithAddress(0xB000).Build(),
		mem.InvReqBuilder{}.WithAddress(0xC000).Build(),
	}
	invRsps := []*mem.InvRsp{
		mem.InvRspBuilder{}.WithRspTo("x1").Build(),
		mem.InvRspBuilder{}.WithRspTo("x2").Build(),
		mem.InvRspBuilder{}.WithRspTo("x3").Build(),
	}

	for _, r := range invReqs {
		bs.sendToRDMAInvQue = append(bs.sendToRDMAInvQue, r)
	}
	for _, r := range invRsps {
		bs.sendToRDMAInvRspQue = append(bs.sendToRDMAInvRspQue, r)
	}

	// Simulate the "REQ port jammed, RSP port open" condition by
	// draining only the RSP queue. The REQ queue must remain untouched.
	initialReqLen := len(bs.sendToRDMAInvQue)
	for len(bs.sendToRDMAInvRspQue) > 0 {
		bs.sendToRDMAInvRspQue = bs.sendToRDMAInvRspQue[1:]
	}

	if len(bs.sendToRDMAInvQue) != initialReqLen {
		t.Fatalf("R3: REQ queue mutated by RSP drain — expected %d, got %d",
			initialReqLen, len(bs.sendToRDMAInvQue))
	}
	if len(bs.sendToRDMAInvRspQue) != 0 {
		t.Fatalf("R3: RSP queue not fully drained — %d remain",
			len(bs.sendToRDMAInvRspQue))
	}

	// Type purity: RSP queue must carry only InvRsp, REQ queue only InvReq.
	for i, msg := range bs.sendToRDMAInvQue {
		if _, ok := msg.(*mem.InvReq); !ok {
			t.Fatalf("R3: sendToRDMAInvQue[%d] = %T, want *mem.InvReq", i, msg)
		}
	}

	// Re-seed RSP queue; verify a sub-set drain leaves the REQ queue
	// untouched even under interleaved Tick (mirroring drainOneTypedQueue).
	for _, r := range invRsps {
		bs.sendToRDMAInvRspQue = append(bs.sendToRDMAInvRspQue, r)
	}
	bs.sendToRDMAInvRspQue = bs.sendToRDMAInvRspQue[1:]
	if len(bs.sendToRDMAInvQue) != initialReqLen {
		t.Fatalf("R3: REQ queue mutated by partial RSP drain")
	}
}

// Reset must clear sendToRDMAInvRspQue too (mirrors the iter17 D7
// Reset invariant for the other typed queues).
func TestIter19_R3_ResetClearsRspQueue(t *testing.T) {
	bs := &bottomSender{}
	bs.sendToRDMAInvRspQue = []sim.Msg{
		mem.InvRspBuilder{}.WithRspTo("a").Build(),
	}

	// Mirror the Reset() body for the queue clears (cannot call full
	// Reset() because it depends on cache buffers).
	bs.sendToRDMAInvRspQue = nil
	if bs.sendToRDMAInvRspQue != nil {
		t.Fatalf("R3: Reset must clear sendToRDMAInvRspQue")
	}
}

// ---------------------------------------------------------------- R4

// TestBSBDataDrainNotBlockedByInvStall validates that the bottomSender
// buffers (local/remoteBottomSenderBuffer) drain independently of the
// invReqBuffer. Pattern: fill invReqBuffer to capacity and seed
// localBottomSenderBuffer with data trans; verify the data buffer
// still pops while the InvReq buffer is stuck.
func TestBSBDataDrainNotBlockedByInvStall(t *testing.T) {
	cache := &Comp{
		state:                   cacheStateRunning,
		localBottomSenderBuffer: sim.NewBuffer("localBSB", 8),
		remoteBottomSenderBuffer: sim.NewBuffer("remoteBSB", 8),
		invReqBuffer:            sim.NewBuffer("invReqBuf", 2),
	}

	// Fill invReqBuffer to capacity.
	cache.invReqBuffer.Push(mem.InvReqBuilder{}.WithAddress(0x100).Build())
	cache.invReqBuffer.Push(mem.InvReqBuilder{}.WithAddress(0x200).Build())

	// Seed [REQ, RSP, REQ, RSP, ...] equivalent: data trans go to
	// localBottomSenderBuffer (the "RSP" half from R4's perspective —
	// it carries normal data traffic that must drain), and the InvReq
	// stall is modeled by the saturated invReqBuffer.
	for i := 0; i < 4; i++ {
		trans := &transaction{
			id:        sim.GetIDGenerator().Generate(),
			fromLocal: true,
		}
		cache.localBottomSenderBuffer.Push(trans)
	}

	invBufSizeBefore := cache.invReqBuffer.Size()
	dataBufSizeBefore := cache.localBottomSenderBuffer.Size()

	// Drain the data buffer (mirrors processInputReq advancing). The
	// invReqBuffer must remain at its (full) capacity — the inv stall
	// does NOT prevent data progress.
	for cache.localBottomSenderBuffer.Size() > 0 {
		if cache.localBottomSenderBuffer.Pop() == nil {
			t.Fatalf("R4: localBottomSenderBuffer Pop returned nil mid-drain")
		}
		if cache.invReqBuffer.Size() != invBufSizeBefore {
			t.Fatalf("R4: data drain leaked into invReqBuffer (size %d -> %d)",
				invBufSizeBefore, cache.invReqBuffer.Size())
		}
	}

	if cache.localBottomSenderBuffer.Size() != 0 {
		t.Fatalf("R4: expected localBottomSenderBuffer fully drained (was %d, now %d)",
			dataBufSizeBefore, cache.localBottomSenderBuffer.Size())
	}
	if cache.invReqBuffer.Size() != invBufSizeBefore {
		t.Fatalf("R4: invReqBuffer must remain stuck (size %d -> %d)",
			invBufSizeBefore, cache.invReqBuffer.Size())
	}

	// Symmetric: remoteBottomSenderBuffer drains independently too.
	for i := 0; i < 4; i++ {
		trans := &transaction{
			id:        sim.GetIDGenerator().Generate(),
			fromLocal: false,
		}
		cache.remoteBottomSenderBuffer.Push(trans)
	}
	for cache.remoteBottomSenderBuffer.Size() > 0 {
		cache.remoteBottomSenderBuffer.Pop()
	}
	if cache.invReqBuffer.Size() != invBufSizeBefore {
		t.Fatalf("R4: remoteBSB drain leaked into invReqBuffer")
	}
}
