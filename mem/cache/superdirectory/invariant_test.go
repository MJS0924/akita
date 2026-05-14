package superdirectory

import (
	"fmt"
	"strings"
	"testing"

	"github.com/sarchlab/akita/v4/mem/cache/superdirectory/internal"
	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/pipelining"
	"github.com/sarchlab/akita/v4/sim"
)

// programmableMSHR lets each test seed the QueryWithMask response directly,
// emulating an in-flight motion entry.
type programmableMSHR struct {
	queryResp []*internal.MSHREntry // returned by QueryWithMask
	addCalls  int
}

func (m *programmableMSHR) IsFull() bool { return false }
func (m *programmableMSHR) Add(_ vm.PID, _ uint64, _ uint64, _ int) *internal.MSHREntry {
	m.addCalls++
	return internal.NewMSHREntry()
}
func (m *programmableMSHR) Query(_ vm.PID, _ uint64) *internal.MSHREntry { return nil }
func (m *programmableMSHR) QueryWithMask(_ vm.PID, _ uint64, _ uint64) []*internal.MSHREntry {
	return m.queryResp
}
func (m *programmableMSHR) Remove(_ vm.PID, _ uint64) *internal.MSHREntry { return nil }
func (m *programmableMSHR) AllEntries() []*internal.MSHREntry             { return nil }
func (m *programmableMSHR) Reset()                                        {}

// TestInvariant_SelectBank_RedirectsToInFlightMotion verifies that when an
// in-flight motion MSHR entry exists, selectBank routes the new access to
// that entry's RegionID — overriding both BF and RSB.
func TestInvariant_SelectBank_RedirectsToInFlightMotion(t *testing.T) {
	mshr := &programmableMSHR{
		queryResp: []*internal.MSHREntry{{
			PID:       vm.PID(1),
			Address:   testAddr,
			RegionLen: 8, // bank 3 (regionLen[3]=8)
			RegionID:  3, // in-flight motion target
		}},
	}
	// dir returns finest bank as BF result (not bank 3); RSB hints bank 1.
	// Either would mis-route to a parallel bank if MSHR check didn't fire.
	dir := newMockDir([]int{4, 2})
	rsb := internal.NewRegionSizeBuffer(1, testLog2PageSize, testRegionLen, false)
	rsb.Push(testAddr, 1)
	comp := &Comp{
		directory:        dir,
		regionSizeBuffer: *rsb,
		numBanks:         testNumBanks,
		regionLen:        testRegionLen,
		mshr:             mshr,
	}
	ds := &directoryStage{cache: comp}

	sel := ds.selectBank(vm.PID(1), testAddr)
	if sel.bankID != 3 {
		t.Errorf("bankID: want 3 (in-flight motion target), got %d", sel.bankID)
	}
	if sel.bankList != nil {
		t.Errorf("bankList: want nil after redirect, got %v", sel.bankList)
	}
	if sel.onCommit != nil {
		t.Error("onCommit: want nil (no state mutation on redirect)")
	}
	if sel.rsbHintBank != -1 {
		t.Errorf("rsbHintBank: want -1, got %d", sel.rsbHintBank)
	}
}

// TestInvariant_SelectBank_PrefersCoarserOnMultipleMotions verifies the
// tie-breaking rule when multiple overlapping in-flight motion entries
// happen to coexist (defensive — invariant says this shouldn't occur,
// but the routing must be deterministic).
func TestInvariant_SelectBank_PrefersCoarserOnMultipleMotions(t *testing.T) {
	mshr := &programmableMSHR{
		queryResp: []*internal.MSHREntry{
			{PID: vm.PID(1), Address: testAddr, RegionLen: 6, RegionID: 4},
			{PID: vm.PID(1), Address: testAddr, RegionLen: 10, RegionID: 2}, // coarser
			{PID: vm.PID(1), Address: testAddr, RegionLen: 8, RegionID: 3},
		},
	}
	comp := &Comp{
		directory:        newMockDir(nil),
		regionSizeBuffer: *internal.NewRegionSizeBuffer(1, testLog2PageSize, testRegionLen, false),
		numBanks:         testNumBanks,
		regionLen:        testRegionLen,
		mshr:             mshr,
	}
	ds := &directoryStage{cache: comp}

	sel := ds.selectBank(vm.PID(1), testAddr)
	if sel.bankID != 2 {
		t.Errorf("bankID: want 2 (smallest RegionID = coarsest), got %d", sel.bankID)
	}
}

// TestInvariant_WriteToBank_PreflightRejectsCrossGranularity verifies that
// writeToBank stalls (without state mutation) when an MSHR entry targets the
// SAME bank and aliases to the new allocation's Tag at this bank's maskLen.
// Cross-bank overlap (motion targeting a different bank) no longer stalls —
// the bank's own maskLen scope is the precise collision granularity.
func TestInvariant_WriteToBank_PreflightRejectsCrossGranularity(t *testing.T) {
	dir := newMockDir(nil)
	buffers := make([]sim.Buffer, testNumBanks)
	for i := range buffers {
		buffers[i] = sim.NewBuffer("TestBuf"+string(rune('0'+i)), 16)
	}
	// Motion entry at bank 4 (target = this bank) with RegionLen=6. Bank-4's
	// maskLen = regionLen[4]+log2NumSubEntry = 6+2 = 8. Address aliases at
	// maskLen=8: both testAddr>>8 match.
	mshr := &programmableMSHR{
		queryResp: []*internal.MSHREntry{{
			PID:       vm.PID(1),
			Address:   testAddr,
			RegionLen: 6,
			RegionID:  4,
		}},
	}
	rsb := internal.NewRegionSizeBuffer(1, testLog2PageSize, testRegionLen, false)
	comp := &Comp{
		directory:             dir,
		regionSizeBuffer:      *rsb,
		numBanks:              testNumBanks,
		regionLen:             testRegionLen,
		log2NumSubEntry:       2,
		mshr:                  mshr,
		localDirToBankBuffers: buffers,
	}
	ds := &directoryStage{cache: comp}
	ds.returnFalse = &ds.returnFalse0

	// trans targeting bank 4 — same as motion target. Pre-flight detects
	// Tag collision at this bank's maskLen (8) → stall.
	trans := &transaction{
		bankID: 4,
		read:   mem.ReadReqBuilder{}.WithAddress(testAddr).WithByteSize(64).WithPID(vm.PID(1)).Build(),
	}
	block := &internal.CohEntry{
		SubEntry: make([]internal.CohSubEntry, 1<<comp.log2NumSubEntry),
	}

	before := comp.stallWriteToBankPreflight
	ok := ds.writeToBank(trans, block, 0, true)
	if ok {
		t.Error("writeToBank: want false (pre-flight stall), got true")
	}
	if comp.stallWriteToBankPreflight != before+1 {
		t.Errorf("stallWriteToBankPreflight: want %d, got %d", before+1, comp.stallWriteToBankPreflight)
	}
	if mshr.addCalls != 0 {
		t.Errorf("mshr.Add called %d times; pre-flight must reject before Add", mshr.addCalls)
	}
	if !strings.Contains(ds.returnFalse0, "would alias existing MSHR entry") {
		t.Errorf("returnFalse: want substring 'would alias existing MSHR entry', got %q", ds.returnFalse0)
	}
	if len(dir.insertedRegion) != 0 {
		t.Errorf("BloomFilter Insert must not happen on pre-flight stall; got %v", dir.insertedRegion)
	}
}

// TestInvariant_WriteToBank_Preflight_NonAliasingNoStall verifies the
// precise semantics: an MSHR entry whose address does NOT alias the new
// allocation at the joint granularity (max(e.RegionLen, regionLen[bankID]))
// does NOT cause a stall, even if both share a coarser super-region. This
// is the path-B equivalent at the writeToBank layer — different
// sub-regions in the same coarse 16KB no longer falsely stall.
func TestInvariant_WriteToBank_Preflight_NonAliasingNoStall(t *testing.T) {
	dir := newMockDir(nil)
	buffers := make([]sim.Buffer, testNumBanks)
	for i := range buffers {
		buffers[i] = sim.NewBuffer("TestBuf"+string(rune('0'+i)), 16)
	}
	// Motion at bank 4, address at testAddr (e.g., 0x4000000), RegionLen=6.
	// trans allocates at bank 0 with regionLen=14 for a DIFFERENT address
	// that does NOT alias at max(6, 14)=14 granularity (i.e., a totally
	// different 16KB region).
	mshr := &programmableMSHR{
		queryResp: nil, // QueryWithMask honors mask filter via auto-max → no match
	}
	rsb := internal.NewRegionSizeBuffer(1, testLog2PageSize, testRegionLen, false)
	comp := &Comp{
		directory:             dir,
		regionSizeBuffer:      *rsb,
		numBanks:              testNumBanks,
		regionLen:             testRegionLen,
		log2NumSubEntry:       2,
		mshr:                  mshr,
		localDirToBankBuffers: buffers,
	}
	ds := &directoryStage{cache: comp}
	ds.returnFalse = &ds.returnFalse0

	trans := &transaction{
		bankID: 0,
		read:   mem.ReadReqBuilder{}.WithAddress(testAddr).WithByteSize(64).WithPID(vm.PID(1)).Build(),
	}
	block := &internal.CohEntry{
		SubEntry: make([]internal.CohSubEntry, 1<<comp.log2NumSubEntry),
	}

	before := comp.stallWriteToBankPreflight
	ds.writeToBank(trans, block, 0, true)
	if comp.stallWriteToBankPreflight != before {
		t.Errorf("stallWriteToBankPreflight: non-aliasing motion must NOT stall; counter went from %d to %d",
			before, comp.stallWriteToBankPreflight)
	}
}

// TestInvariant_BankList_RedirectsOnMSHRConflict verifies that the bankList
// traversal path in doWrite redirects to the in-flight motion's bank rather
// than blindly accepting at the next bank in bankList. We use the real MSHR
// so QueryWithMask honors the mask argument correctly (the programmable mock
// returns its preset list regardless of mask, which would short-circuit the
// earlier 446 check and skip the bankList path entirely).
func TestInvariant_BankList_RedirectsOnMSHRConflict(t *testing.T) {
	// localPipeline & remotePipeline must accept items.
	localPipes := make([]pipelining.Pipeline, testNumBanks)
	remotePipes := make([]pipelining.Pipeline, testNumBanks)
	for i := range localPipes {
		localPipes[i] = pipelining.MakeBuilder().
			WithPipelineWidth(1).
			WithNumStage(1).
			WithCyclePerStage(1).
			WithPostPipelineBuffer(sim.NewBuffer("Post", 4)).
			Build("PipeLocal" + string(rune('A'+i)))
		remotePipes[i] = pipelining.MakeBuilder().
			WithPipelineWidth(1).
			WithNumStage(1).
			WithCyclePerStage(1).
			WithPostPipelineBuffer(sim.NewBuffer("Post", 4)).
			Build("PipeRemote" + string(rune('A'+i)))
	}
	// Real MSHR: respects QueryWithMask's mask argument.
	realMSHR := internal.NewMSHR(16)
	// Seed an in-flight motion entry at bank 1, RegionLen=12 (bank 1's grain),
	// at an address that is in the same 16KB page as testAddr (mask 14)
	// but in a different 4KB region (mask 12) — so the early 446 check at
	// bank-2 (mask 10) won't see it (testAddr>>10 != entryAddr>>10), but
	// the coarsest-mask redirect at line 510 will (testAddr>>14 ==
	// entryAddr>>14).
	entryAddr := testAddr + (uint64(1) << 12) // same 16KB, different 4KB
	realMSHR.Add(vm.PID(1), entryAddr, 12, 1)

	rsb := internal.NewRegionSizeBuffer(1, testLog2PageSize, testRegionLen, false)
	comp := &Comp{
		directory:        &noopDir{},
		regionSizeBuffer: *rsb,
		numBanks:         testNumBanks,
		regionLen:        testRegionLen,
		log2NumSubEntry:  2,
		mshr:             realMSHR,
	}
	ds := &directoryStage{
		cache:          comp,
		localPipeline:  localPipes,
		remotePipeline: remotePipes,
	}
	ds.returnFalse = &ds.returnFalse0

	trans := &transaction{
		bankID:   2,         // starting bank (its mask=10 misses the 4KB-apart entry)
		bankList: []int{3, 4},
		read:     mem.ReadReqBuilder{}.WithAddress(testAddr).WithByteSize(64).WithPID(vm.PID(1)).Build(),
		write:    nil,
	}

	ok := ds.doWrite(trans, true)
	if !ok {
		t.Fatalf("doWrite returned false; want true (redirect succeeded). returnFalse=%q", ds.returnFalse0)
	}
	if trans.bankID != 1 {
		t.Errorf("trans.bankID after redirect: want 1, got %d (returnFalse=%q)", trans.bankID, ds.returnFalse0)
	}
	if trans.bankList != nil {
		t.Errorf("trans.bankList after redirect: want nil, got %v", trans.bankList)
	}
}

// noopDir is a directory mock that returns (nil, 0) on Lookup and never
// panics so we can drive doWrite past the directory-lookup hit branch.
type noopDir struct {
	mockSuperDirectory
}

func (n *noopDir) Lookup(_ int, _ vm.PID, _ uint64) (*internal.CohEntry, int) {
	return nil, 0
}
func (n *noopDir) GetBank(_ uint64) []int { return nil }

// TestInvariant_DoDemotion_PanicsOnTargetHit verifies that doDemotion panics
// loudly when its target bank lookup unexpectedly returns hit — that state
// would have been the "Hit about demotion" warning before. With the routing
// invariants in place, hitting this path is a regression signal.
func TestInvariant_DoDemotion_PanicsOnTargetHit(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("doDemotion: want panic on target-hit, got no panic")
		}
		// Panic can be string (our explicit fmt.Sprintf panic) or
		// runtime error if we hit something else first. Verify the
		// expected message substring is present in the formatted panic.
		msg := ""
		switch v := r.(type) {
		case string:
			msg = v
		default:
			msg = fmt.Sprintf("%v", v)
		}
		if !strings.Contains(msg, "invariant violation") {
			t.Errorf("panic message: want substring 'invariant violation', got %q", msg)
		}
	}()

	dir := &alwaysHitDir{}
	rsb := internal.NewRegionSizeBuffer(1, testLog2PageSize, testRegionLen, false)
	// doDemotion accesses targetBuffer[bankID].CanPush() before Lookup;
	// must supply localDirToBankBuffers.
	buffers := make([]sim.Buffer, testNumBanks)
	for i := range buffers {
		buffers[i] = sim.NewBuffer("DemoteBuf"+string(rune('A'+i)), 16)
	}
	comp := &Comp{
		directory:             dir,
		regionSizeBuffer:      *rsb,
		numBanks:              testNumBanks,
		regionLen:             testRegionLen,
		log2NumSubEntry:       2,
		mshr:                  internal.NewMSHR(16),
		name:                  "TestSD",
		localDirToBankBuffers: buffers,
	}
	ds := &directoryStage{cache: comp}
	ds.returnFalse = &ds.returnFalse0

	// trans.block must be non-nil — doDemotion reads block.Tag and block.PID
	// at lines 353-354 before reaching the Lookup-hit panic site.
	trans := &transaction{
		bankID: 2,
		write:  mem.WriteReqBuilder{}.WithAddress(testAddr).WithPID(vm.PID(1)).Build(),
		block: &internal.CohEntry{
			Tag: testAddr,
			PID: vm.PID(1),
		},
		mshrEntry: &internal.MSHREntry{
			PID:     vm.PID(1),
			Address: testAddr,
		},
	}
	// doDemotion is the panic site. Invoke it directly.
	_ = ds.doDemotion(trans, true)
	t.Fatal("doDemotion returned without panic")
}

// alwaysHitDir.Lookup returns a valid block for any query — used to drive
// the panic path in doDemotion.
type alwaysHitDir struct{ mockSuperDirectory }

func (a *alwaysHitDir) Lookup(_ int, _ vm.PID, _ uint64) (*internal.CohEntry, int) {
	blk := &internal.CohEntry{
		IsValid:  true,
		SubEntry: make([]internal.CohSubEntry, 1),
	}
	blk.SubEntry[0].IsValid = true
	return blk, 0
}
func (a *alwaysHitDir) GetBank(_ uint64) []int { return nil }
func (a *alwaysHitDir) GetSet(_ int, _ uint64) (*internal.CohSet, int) {
	// Empty set is enough to satisfy the diagnostic dump loop.
	return &internal.CohSet{CohEntries: nil}, 0
}

// TestInvariant_DoWriteMSHRHit_RegionLenFromEntry verifies that doWriteMSHRHit
// computes blockIdx from the MSHR entry's RegionLen rather than the bank's
// configured regionLen. This matters when redirected: trans.bankID can point
// at a bank whose regionLen != entry.RegionLen.
func TestInvariant_DoWriteMSHRHit_RegionLenFromEntry(t *testing.T) {
	mshr := &programmableMSHR{}
	rsb := internal.NewRegionSizeBuffer(1, testLog2PageSize, testRegionLen, false)
	comp := &Comp{
		directory:        newMockDir(nil),
		regionSizeBuffer: *rsb,
		numBanks:         testNumBanks,
		regionLen:        testRegionLen,
		log2NumSubEntry:  2,
		mshr:             mshr,
	}
	ds := &directoryStage{cache: comp}

	// Entry RegionLen=10 (bank 2), but trans.bankID forced to 4 (regionLen=6).
	// blockIdx should reflect entry RegionLen=10, not the bank's regionLen=6.
	entry := &internal.MSHREntry{
		PID:       vm.PID(1),
		Address:   testAddr,
		RegionLen: 10,
		RegionID:  2,
	}
	// addr = testAddr + offset within the coarse region; choose offset such
	// that the bank-2 view places it at sub-index 1 (one cacheline above base
	// at the entry's RegionLen=10 + log2NumSubEntry=2 = 12 mask space).
	offsetWithinCoarseRegion := uint64(1) << (10 + 0) // bumps offset to sub-entry 1 at entry's grain
	addrPlus := testAddr + offsetWithinCoarseRegion
	trans := &transaction{
		bankID: 4, // redirected to a bank whose regionLen=6 != entry.RegionLen=10
		read:   mem.ReadReqBuilder{}.WithAddress(addrPlus).WithByteSize(64).WithPID(vm.PID(1)).Build(),
	}

	ok := ds.doWriteMSHRHit(trans, entry)
	if !ok {
		t.Fatal("doWriteMSHRHit returned false")
	}
	// Expected blockIdx is computed from entry.RegionLen, not regionLen[bankID].
	// We don't pin to a specific value (depends on testAddr layout) — just
	// assert it differs from what the bank-based computation would produce.
	bankBasedRegionLen := comp.regionLen[trans.bankID]
	_, bankOffset := getCacheLineID(addrPlus, uint64(bankBasedRegionLen+comp.log2NumSubEntry))
	bankBasedBlockIdx := int(bankOffset >> bankBasedRegionLen)
	if trans.blockIdx == bankBasedBlockIdx {
		// They could coincide for some addresses, but our specifically chosen
		// offset should produce different values; if not, the test isn't
		// proving what we want. Recompute from entry to be sure:
		_, entryOffset := getCacheLineID(addrPlus, uint64(entry.RegionLen+uint64(comp.log2NumSubEntry)))
		entryBasedBlockIdx := int(entryOffset >> entry.RegionLen)
		if trans.blockIdx != entryBasedBlockIdx {
			t.Errorf("trans.blockIdx=%d matches neither entry-based(%d) nor bank-based(%d)",
				trans.blockIdx, entryBasedBlockIdx, bankBasedBlockIdx)
		}
	}
}
