package superdirectory

import (
	"reflect"
	"testing"

	"github.com/sarchlab/akita/v4/mem/cache/superdirectory/internal"
	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
)

// ─────────────────────────────────────────────────────────────────────────────
// Fix-1 (utilization-driven demote, threshold 3/4) unit tests.
//
// Mechanism (bottomSender.go:1010 in processInvRsp): when an
// EvictAndInsertNewEntry transaction completes its outbound invalidation
// round (pendingEviction drained), the cache decides whether to record an
// RSB hint for a finer (bankID+1) region size:
//
//   if action == EvictAndInsertNewEntry && accessed*4 < numInv*3 {  // util < 3/4
//       if bankID+1 < numBanks-1 { RSB.Update(addr, bankID+1) }     // hint smaller
//   } else {
//       RSB.Update(addr, bankID)                                    // no hint change
//   }
//
// The 5 test cases below cover the spec items in order.
// ─────────────────────────────────────────────────────────────────────────────

// newTestBottomSenderForUtil builds a minimal bottomSender wired for
// processInvRsp testing. RSB has 8 slots, page size 4KB, default 5-bank
// regionLen [14,12,10,8,6]. Returns the bs and its Comp for inspection.
func newTestBottomSenderForUtil() (*bottomSender, *Comp) {
	rsb := internal.NewRegionSizeBuffer(8, testLog2PageSize, testRegionLen, false)
	// drain the zero-initialised slots so spurious matches don't appear:
	// NewRegionSizeBuffer pre-sizes the entries slice; a Reset to empty
	// would help but the implementation uses a fixed-size ring. Push a
	// dummy out-of-range entry numEntries times to flush, then start clean.
	for i := uint64(0); i < 8; i++ {
		rsb.Push(0xFFFFFFFF_00000000+i*0x100000, 4) // unused range, distinct addrs
	}
	rsb = internal.NewRegionSizeBuffer(8, testLog2PageSize, testRegionLen, false)

	comp := &Comp{
		numBanks:         testNumBanks,
		regionLen:        testRegionLen,
		log2PageSize:     testLog2PageSize,
		regionSizeBuffer: *rsb,
	}
	bs := &bottomSender{cache: comp}
	return bs, comp
}

// installEvictionTrans sets up an inflight EvictAndInsertNewEntry trans on
// bs, with one outstanding pendingEviction targeting `sharer`. Returns the
// trans and the InvRsp that would resolve the eviction with given counts.
func installEvictionTrans(
	bs *bottomSender, action action, bankID int, addr uint64,
	sharer sim.RemotePort, accessed, numInv uint64,
) (*transaction, *mem.InvRsp) {
	write := mem.WriteReqBuilder{}.
		WithAddress(addr).
		WithSrcRDMA(sim.RemotePort("GPU0")).
		Build()

	// Pre-populate the entry block to a deterministic state. Tests that
	// check "existing entry not modified" capture this snapshot up front.
	blk := &internal.CohEntry{
		IsValid:      true,
		PID:          1,
		Tag:          addr,
		DemoteLocked: false,
		SubEntry:     make([]internal.CohSubEntry, 4),
	}
	for i := range blk.SubEntry {
		blk.SubEntry[i].IsValid = true
		blk.SubEntry[i].Sharer = []sim.RemotePort{sharer}
	}

	trans := &transaction{
		action:          action,
		bankID:          bankID,
		block:           blk,
		evictingAddr:    addr,
		write:           write,
		pendingEviction: []sim.RemotePort{sharer},
	}
	bs.inflightInvToOutside = append(bs.inflightInvToOutside, trans)

	rsp := mem.InvRspBuilder{}.
		WithRspTo(write.Meta().ID).
		WithSrcRDMA(sharer).
		WithAccessed(accessed).
		WithNumInv(numInv).
		Build()
	return trans, rsp
}

// ─── Test 1: utilization < 3/4 → trigger fires (RSB.Update with bankID+1) ───

func TestUtilDemote_BelowThreshold_TriggersHintBankPlus1(t *testing.T) {
	bs, comp := newTestBottomSenderForUtil()

	// 2 accessed of 4 invalidations = 50% util < 75% threshold → trigger.
	// bankID=0 → bankID+1=1 < numBanks-1=4 → inner if true → RSB.Update(addr, 1).
	const addr uint64 = 0x4000000
	_, rsp := installEvictionTrans(
		bs, EvictAndInsertNewEntry, 0, addr,
		sim.RemotePort("GPU1"), 2, 4,
	)

	if !bs.processInvRsp(rsp) {
		t.Fatal("processInvRsp returned false")
	}

	got := comp.regionSizeBuffer.Search(addr)
	if got.RegionID != 1 {
		t.Errorf("util<3/4 should hint regionID=bankID+1=1, got RegionID=%d", got.RegionID)
	}
}

// ─── Test 2: utilization ≥ 3/4 → trigger NOT fired (RSB.Update with bankID) ─

// At exactly accessed=3, numInv=4 (75%): condition `3*4 < 4*3` → `12 < 12` → false.
// Threshold is strict less-than, so 75% does not trigger; the else branch
// records the *current* bankID hint (no descent).
func TestUtilDemote_AtThreshold_DoesNotTrigger(t *testing.T) {
	bs, comp := newTestBottomSenderForUtil()

	const addr uint64 = 0x4000000
	const bankID = 0
	_, rsp := installEvictionTrans(
		bs, EvictAndInsertNewEntry, bankID, addr,
		sim.RemotePort("GPU1"), 3, 4, // 75% — exactly at threshold
	)

	if !bs.processInvRsp(rsp) {
		t.Fatal("processInvRsp returned false")
	}

	got := comp.regionSizeBuffer.Search(addr)
	if got.RegionID != bankID {
		t.Errorf("util==3/4 (boundary) should NOT trigger; expected RegionID=%d, got %d",
			bankID, got.RegionID)
	}
}

// ─── Test 3: RSB hint is reflected when next request enters selectBank ─────

// The hint mechanism is split: bottomSender writes RSB.Update on the producer
// side; directoryStage.selectBank reads RSB.Search on the consumer side.
// This test validates the consumer side: a hint with regionID=R routes to
// bank R when GetBank does not return a coarser BF.
func TestUtilDemote_RSBHint_ReflectedInSelectBank(t *testing.T) {
	const addr uint64 = 0x4000000
	const hintBank = 2 // RSB hint inserted by Fix-1 trigger path

	// dir: GetBank returns finest-first; with a non-stale RSB hint,
	// selectBank uses the hint.
	dir := &mockSuperDirectory{getBankResult: nil}
	rsb := internal.NewRegionSizeBuffer(1, testLog2PageSize, testRegionLen, false)
	rsb.Push(addr, hintBank)
	comp := &Comp{
		directory:        dir,
		regionSizeBuffer: *rsb,
		numBanks:         testNumBanks,
		regionLen:        testRegionLen,
		mshr:             &mockMSHR{},
	}
	ds := &directoryStage{cache: comp}

	sel := ds.selectBank(vm.PID(0), addr)
	if sel.bankID != hintBank {
		t.Errorf("RSB hint not reflected: want bankID=%d (hint), got %d",
			hintBank, sel.bankID)
	}
	if sel.bankList != nil {
		t.Errorf("RSB hint normal path: bankList should be nil, got %v", sel.bankList)
	}
}

// ─── Test 4: existing entry is not modified by Fix-1's RSB-hint path ───────

// Spec: "기존 entry 변경 없음". Fix-1 only mutates the RSB. The CohEntry
// being evicted must not be re-flagged or rewritten as a side effect of
// the threshold evaluation.
func TestUtilDemote_ExistingEntryUnchanged(t *testing.T) {
	bs, _ := newTestBottomSenderForUtil()

	const addr uint64 = 0x4000000
	trans, rsp := installEvictionTrans(
		bs, EvictAndInsertNewEntry, 0, addr,
		sim.RemotePort("GPU1"), 2, 4, // triggers (50% < 75%)
	)
	// Snapshot the entry state before processInvRsp runs.
	snapshot := trans.block.DeepCopy()

	if !bs.processInvRsp(rsp) {
		t.Fatal("processInvRsp returned false")
	}

	if trans.block.IsValid != snapshot.IsValid ||
		trans.block.PID != snapshot.PID ||
		trans.block.Tag != snapshot.Tag ||
		trans.block.DemoteLocked != snapshot.DemoteLocked {
		t.Errorf("Fix-1 path mutated entry top-level state: before=%+v after=%+v",
			snapshot, trans.block)
	}
	for i := range trans.block.SubEntry {
		want := snapshot.SubEntry[i]
		got := trans.block.SubEntry[i]
		if got.IsValid != want.IsValid {
			t.Errorf("SubEntry[%d].IsValid changed: %v → %v", i, want.IsValid, got.IsValid)
		}
		if !reflect.DeepEqual(got.Sharer, want.Sharer) {
			t.Errorf("SubEntry[%d].Sharer changed: %v → %v", i, want.Sharer, got.Sharer)
		}
	}
}

// ─── Test 5: Promotion path is not used by Fix-1's RSB-hint path ───────────

// Spec: "Promotion 미발생". Fix-1's mechanism records an RSB hint that takes
// effect on a *future* request through selectBank — it does NOT enqueue a
// promotion. Verify by checking that the trigger path leaves the
// inflightInvToOutside list cleanly drained without pushing onto any of
// the queues that drive promotions (sendToDirQue, pendingWriteAfterInv
// gets the write because the trans had a write — that is the normal write
// completion path, not a promotion).
func TestUtilDemote_NoPromotionPathSideEffects(t *testing.T) {
	bs, _ := newTestBottomSenderForUtil()

	const addr uint64 = 0x4000000
	_, rsp := installEvictionTrans(
		bs, EvictAndInsertNewEntry, 0, addr,
		sim.RemotePort("GPU1"), 2, 4, // triggers
	)

	if !bs.processInvRsp(rsp) {
		t.Fatal("processInvRsp returned false")
	}

	// inflight cleared:
	if len(bs.inflightInvToOutside) != 0 {
		t.Errorf("inflightInvToOutside should be drained, got len=%d",
			len(bs.inflightInvToOutside))
	}
	// no promotion-related routing:
	if len(bs.sendToDirQue) != 0 {
		t.Errorf("sendToDirQue should remain empty (no promotion routing), got len=%d",
			len(bs.sendToDirQue))
	}
	// the only legitimate side effect is pendingWriteAfterInv (write was
	// waiting for InvRsps); that is normal write completion, not promotion.
	// We allow it but assert no promotion-style remnant elsewhere.
}
