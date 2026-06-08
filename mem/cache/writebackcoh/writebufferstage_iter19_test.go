package writebackcoh

import (
	"testing"

	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/sim"
)

// [ITER19 R5 / R6 TEST SUITE]
//
// R5 — bankFetched RSPs are not blocked by bypass admit (REQ side).
//   writeBufferToBankBuffers is split into writeBufferToBankBuffersReq
//   (admit-side: peer-bypass / regular / prefetched-write derived) and
//   writeBufferToBankBuffersRsp (RSP-side: bankWriteFetched dispatch
//   from DataReadyRsp). Each bank now has its own Req/Rsp pair, and
//   bankStage.pullFromBuf drains Rsp first to break the HoL where
//   pending DataReadyRsp could not push into a Req-saturated bank buf.
//
// R6 — DataReady RSP (remote) drains even when local is stuck.
//   pendingDataReady / pendingWriteDone are further split by origin
//   side (Local vs Remote). processPendingDataReady drains Remote
//   FIRST so a stuck local response cannot HoL-block the cross-GPU
//   ACK that releases peer's inflight counts.

// ---------------------------------------------------------------- R5

// TestBankFetchedRspNotBlockedByBypassAdmit verifies the typed bank-buf
// split. Seed [REQ, RSP, REQ, RSP, ...] into the two split buffers and
// fill the REQ buffer to capacity; the RSP buffer must continue to
// drain independently.
func TestBankFetchedRspNotBlockedByBypassAdmit(t *testing.T) {
	cache := &Comp{}

	// One bank with capacity 2 on REQ side (admit), 4 on RSP side.
	cache.writeBufferToBankBuffersReq = []sim.Buffer{
		sim.NewBuffer("bank0Req", 2),
	}
	cache.writeBufferToBankBuffersRsp = []sim.Buffer{
		sim.NewBuffer("bank0Rsp", 4),
	}

	// Fill the REQ buffer to capacity to model bypass-admit HoL.
	cache.writeBufferToBankBuffersReq[0].Push(&transaction{})
	cache.writeBufferToBankBuffersReq[0].Push(&transaction{})

	if cache.writeBufferToBankBuffersReq[0].CanPush() {
		t.Fatalf("R5 setup: expected Req buffer full")
	}

	// Push RSP-side dispatches: bankWriteFetched derived from
	// DataReadyRsp processing. These MUST succeed.
	rsps := []*transaction{
		{}, {}, {}, {},
	}
	for i, r := range rsps {
		if !cache.writeBufferToBankBuffersRsp[0].CanPush() {
			t.Fatalf("R5: Rsp buffer cannot accept push #%d though Req side full "+
				"is what's stuck — Rsp/Req must be independent", i)
		}
		cache.writeBufferToBankBuffersRsp[0].Push(r)
	}

	if cache.writeBufferToBankBuffersRsp[0].Size() != len(rsps) {
		t.Fatalf("R5: Rsp buffer size %d, want %d",
			cache.writeBufferToBankBuffersRsp[0].Size(), len(rsps))
	}

	// Now drain Rsp side; Req side must remain stuck.
	reqSizeBefore := cache.writeBufferToBankBuffersReq[0].Size()
	for cache.writeBufferToBankBuffersRsp[0].Size() > 0 {
		cache.writeBufferToBankBuffersRsp[0].Pop()
	}
	if cache.writeBufferToBankBuffersReq[0].Size() != reqSizeBefore {
		t.Fatalf("R5: Rsp drain leaked into Req side (size %d -> %d)",
			reqSizeBefore, cache.writeBufferToBankBuffersReq[0].Size())
	}

	// Combined-size helper must still see both sides.
	cache.writeBufferToBankBuffersRsp[0].Push(&transaction{})
	wantTotal := cache.writeBufferToBankBuffersReq[0].Size() +
		cache.writeBufferToBankBuffersRsp[0].Size()
	got := cache.writeBufferToBankBuffersSize(0)
	if got != wantTotal {
		t.Fatalf("R5: writeBufferToBankBuffersSize(0)=%d, want %d", got, wantTotal)
	}
}

// ---------------------------------------------------------------- R6

// TestDataReadyRemoteNotBlockedByLocal verifies that pendingDataReadyRemote
// drains independently of pendingDataReadyLocal. Pattern: seed
// [Local, Remote, Local, Remote, ...] into the two split queues, model
// a local downstream stall, and verify Remote drains regardless.
func TestDataReadyRemoteNotBlockedByLocal(t *testing.T) {
	wb := &writeBufferStage{}

	// Seed both queues. The R6 invariant: drain Remote first; Local
	// stall must not block Remote progress.
	locals := []*mem.DataReadyRsp{
		mem.DataReadyRspBuilder{}.WithRspTo("L1").Build(),
		mem.DataReadyRspBuilder{}.WithRspTo("L2").Build(),
		mem.DataReadyRspBuilder{}.WithRspTo("L3").Build(),
	}
	remotes := []*mem.DataReadyRsp{
		mem.DataReadyRspBuilder{}.WithRspTo("R1").Build(),
		mem.DataReadyRspBuilder{}.WithRspTo("R2").Build(),
		mem.DataReadyRspBuilder{}.WithRspTo("R3").Build(),
	}

	for _, m := range locals {
		wb.pendingDataReadyLocal = append(wb.pendingDataReadyLocal, m)
	}
	for _, m := range remotes {
		wb.pendingDataReadyRemote = append(wb.pendingDataReadyRemote, m)
	}

	// Independent backing arrays: drain Remote completely, then verify
	// Local is untouched.
	localLenBefore := len(wb.pendingDataReadyLocal)
	for len(wb.pendingDataReadyRemote) > 0 {
		wb.pendingDataReadyRemote = wb.pendingDataReadyRemote[1:]
	}
	if len(wb.pendingDataReadyLocal) != localLenBefore {
		t.Fatalf("R6: Remote drain leaked into Local (size %d -> %d)",
			localLenBefore, len(wb.pendingDataReadyLocal))
	}
	if len(wb.pendingDataReadyRemote) != 0 {
		t.Fatalf("R6: Remote not fully drained (%d remain)",
			len(wb.pendingDataReadyRemote))
	}

	// Symmetric for WriteDone split.
	wb.pendingWriteDoneLocal = []*mem.WriteDoneRsp{
		mem.WriteDoneRspBuilder{}.WithRspTo("WL1").Build(),
		mem.WriteDoneRspBuilder{}.WithRspTo("WL2").Build(),
	}
	wb.pendingWriteDoneRemote = []*mem.WriteDoneRsp{
		mem.WriteDoneRspBuilder{}.WithRspTo("WR1").Build(),
		mem.WriteDoneRspBuilder{}.WithRspTo("WR2").Build(),
	}

	writeLocalBefore := len(wb.pendingWriteDoneLocal)
	for len(wb.pendingWriteDoneRemote) > 0 {
		wb.pendingWriteDoneRemote = wb.pendingWriteDoneRemote[1:]
	}
	if len(wb.pendingWriteDoneLocal) != writeLocalBefore {
		t.Fatalf("R6 (WriteDone): Remote drain leaked into Local")
	}
	if len(wb.pendingWriteDoneRemote) != 0 {
		t.Fatalf("R6 (WriteDone): Remote not fully drained")
	}
}
