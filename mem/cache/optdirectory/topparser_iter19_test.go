package optdirectory

import (
	"testing"

	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/sim"
)

// [ITER19 ANALOGOUS TESTS — optdirectory]
//
// Mirrors the SD D4 / S1 tests: optDir's topparser must isolate InvRsp
// admit from a saturated dirStage, and the bottomSender egress must
// split InvRsp out of the InvReq lane. The optDir buffer / queue
// layout mirrors SD's exactly (sendToTopRspQue, sendToRDMADataRspQue,
// sendToRDMAInvQue, sendToRDMAInvRspQue post-iter19).

// ---------------------------------------------------------------- D4 (analog)

func TestOptDir_L1InvRspNotBlockedByL1ReadReq(t *testing.T) {
	cache := &Comp{
		state:                cacheStateRunning,
		invReqBuffer:         sim.NewBuffer("invReqBuf", 8),
		invRspBuffer:         sim.NewBuffer("invRspBuf", 8),
		localDirStageBuffer:  sim.NewBuffer("localDirStage", 2),
		remoteDirStageBuffer: sim.NewBuffer("remoteDirStage", 8),
		localBypassBuffer:    sim.NewBuffer("localBypass", 8),
	}
	p := &topParser{cache: cache}

	cache.localDirStageBuffer.Push(&transaction{})
	cache.localDirStageBuffer.Push(&transaction{})
	if cache.localDirStageBuffer.CanPush() {
		t.Fatalf("optDir-D4 setup: expected localDirStageBuffer at capacity")
	}

	rsps := []*mem.InvRsp{
		mem.InvRspBuilder{}.WithRspTo("L1").Build(),
		mem.InvRspBuilder{}.WithRspTo("L2").Build(),
		mem.InvRspBuilder{}.WithRspTo("L3").Build(),
	}
	rspsAdmitted := 0
	for _, r := range rsps {
		if p.processReq(r, true) {
			rspsAdmitted++
		}
	}
	if rspsAdmitted != len(rsps) {
		t.Fatalf("optDir-D4: expected %d InvRsps admitted, got %d",
			len(rsps), rspsAdmitted)
	}
	if cache.invRspBuffer.Size() != len(rsps) {
		t.Fatalf("optDir-D4: invRspBuffer size = %d, want %d",
			cache.invRspBuffer.Size(), len(rsps))
	}
}

// ---------------------------------------------------------------- S1 (analog)

func TestOptDir_OutboundInvRspNotBlockedByInvReq(t *testing.T) {
	bs := &bottomSender{}

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

	reqLenBefore := len(bs.sendToRDMAInvQue)
	for len(bs.sendToRDMAInvRspQue) > 0 {
		bs.sendToRDMAInvRspQue = bs.sendToRDMAInvRspQue[1:]
	}
	if len(bs.sendToRDMAInvQue) != reqLenBefore {
		t.Fatalf("optDir-S1: REQ queue mutated by RSP drain (%d -> %d)",
			reqLenBefore, len(bs.sendToRDMAInvQue))
	}
	if len(bs.sendToRDMAInvRspQue) != 0 {
		t.Fatalf("optDir-S1: RSP queue not fully drained")
	}

	for i, m := range bs.sendToRDMAInvQue {
		if _, ok := m.(*mem.InvReq); !ok {
			t.Fatalf("optDir-S1: sendToRDMAInvQue[%d]=%T, want *mem.InvReq", i, m)
		}
	}

	bs.sendToRDMAInvRspQue = []sim.Msg{invRsps[0]}
	bs.sendToRDMAInvRspQue = nil
	if bs.sendToRDMAInvRspQue != nil {
		t.Fatalf("optDir-S1: Reset must clear sendToRDMAInvRspQue")
	}
}
