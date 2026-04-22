package REC

import (
	"testing"

	"github.com/sarchlab/akita/v4/mem/cache/REC/internal"
	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
)

// ── helpers ──────────────────────────────────────────────────────────────────

const (
	testLog2Block    = 6  // 64-byte cache line
	testLog2SubEntry = 4  // 16 sub-entries per entry (REC default)
	testAddr         uint64 = 0x4000000
	testPID          vm.PID = 1
)

var testSharer sim.RemotePort = "GPU0.L1"

// buildReadReq creates a minimal ReadReq with SrcRDMA set.
func buildReadReq(addr uint64, sharer sim.RemotePort) *mem.ReadReq {
	return mem.ReadReqBuilder{}.
		WithAddress(addr).
		WithByteSize(64).
		WithSrcRDMA(sharer).
		Build()
}

// buildBankStage returns a bankStage wired to a minimal Comp.
// The Comp has real sim.Buffers so Push/Pop work without a running engine.
func buildBankStage() (*bankStage, sim.Buffer, sim.Buffer) {
	bottomBuf := sim.NewBuffer("Test.Bottom", 32)
	mshrBuf := sim.NewBuffer("Test.MSHR", 32)

	comp := &Comp{
		debugPromotion: false,
		debugProcess:   false,
	}

	bs := &bankStage{cache: comp}
	return bs, bottomBuf, mshrBuf
}

// makeEntry allocates a CohEntry with IsValid=true and SubEntry[idx].IsValid=false.
func makeEntry(idx int) *internal.CohEntry {
	e := &internal.CohEntry{}
	e.IsValid = true
	e.Tag = testAddr
	e.PID = testPID
	// SubEntry[idx].IsValid deliberately left false — this is the bug-trigger state
	return e
}

// ── directory (Bug 1) tests ───────────────────────────────────────────────────

// TestREC_Lookup_IgnoresSubEntryIsValid demonstrates Bug 1:
// Lookup returns the entry even when SubEntry[index].IsValid == false.
// Correct behaviour (matching superdirectory) would be to return nil,-1.
func TestREC_Lookup_IgnoresSubEntryIsValid(t *testing.T) {
	dir := internal.NewRECDirectory(
		16, 4, 1<<testLog2Block, testLog2SubEntry, testLog2Block,
		internal.NewLRUVictimFinder(),
	)

	// Obtain a victim slot and set it up manually:
	// entry.IsValid=true, SubEntry[0].IsValid=false
	entry, _ := dir.FindVictim(testPID, testAddr)
	entry.IsValid = true
	entry.Tag = testAddr
	entry.PID = testPID
	// SubEntry[0].IsValid is already false (zero value)

	got, idx := dir.Lookup(testPID, testAddr)

	// Lookup must return nil,-1 when SubEntry[index].IsValid is false.
	if got != nil {
		t.Errorf("Lookup returned non-nil entry (idx=%d) even though SubEntry[%d].IsValid=false — Bug 1 not fixed",
			idx, idx)
	}
}

// TestREC_Lookup_ValidSubEntry verifies that Lookup succeeds when both
// entry.IsValid and SubEntry[idx].IsValid are true.
func TestREC_Lookup_ValidSubEntry(t *testing.T) {
	dir := internal.NewRECDirectory(
		16, 4, 1<<testLog2Block, testLog2SubEntry, testLog2Block,
		internal.NewLRUVictimFinder(),
	)

	entry, _ := dir.FindVictim(testPID, testAddr)
	entry.IsValid = true
	entry.Tag = testAddr
	entry.PID = testPID

	// Compute which subentry index testAddr maps to, then mark it valid.
	maskLen := testLog2Block + testLog2SubEntry
	idx := (int(testAddr) >> testLog2Block) % (1 << testLog2SubEntry)
	_ = maskLen
	entry.SubEntry[idx].IsValid = true
	entry.SubEntry[idx].Sharer = []sim.RemotePort{testSharer}

	got, gotIdx := dir.Lookup(testPID, testAddr)
	if got == nil {
		t.Fatal("Lookup returned nil for fully-valid entry")
	}
	if gotIdx != idx {
		t.Errorf("Lookup index: want %d, got %d", idx, gotIdx)
	}
}

// ── bankstage (Bug 2) tests ───────────────────────────────────────────────────

// TestREC_InsertNewEntry_DoesNotSetSubEntryIsValid demonstrates Bug 2:
// InsertNewEntry sets Sharer and IsLocked=false but never sets SubEntry[idx].IsValid=true.
// After the call, SubEntry[blockIdx].IsValid remains false.
func TestREC_InsertNewEntry_DoesNotSetSubEntryIsValid(t *testing.T) {
	bs, bottomBuf, mshrBuf := buildBankStage()

	blk := makeEntry(0)
	trans := &transaction{
		action:   InsertNewEntry,
		block:    blk,
		blockIdx: 0,
		read:     buildReadReq(testAddr, testSharer),
	}

	done := bs.InsertNewEntry(trans, bottomBuf, mshrBuf)
	if !done {
		t.Fatal("InsertNewEntry returned false")
	}

	// Sharer must be set — that part works.
	entry := &blk.SubEntry[0]
	if len(entry.Sharer) == 0 {
		t.Error("Sharer not set by InsertNewEntry")
	}

	// BUG: IsValid is never set to true.
	if !entry.IsValid {
		t.Error("SubEntry[0].IsValid must be true after InsertNewEntry — Bug 2 not fixed")
	}
}

// TestREC_UpdateEntry_DoesNotSetSubEntryIsValid demonstrates Bug 2 for UpdateEntry:
// UpdateEntry appends a new sharer but never sets SubEntry[idx].IsValid=true.
func TestREC_UpdateEntry_DoesNotSetSubEntryIsValid(t *testing.T) {
	bs, bottomBuf, mshrBuf := buildBankStage()

	blk := makeEntry(0)
	// Pre-populate with a different sharer so UpdateEntry takes the append path.
	blk.SubEntry[0].Sharer = []sim.RemotePort{"GPU1.L1"}

	var newSharer sim.RemotePort = "GPU2.L1"
	trans := &transaction{
		action:   UpdateEntry,
		block:    blk,
		blockIdx: 0,
		read:     buildReadReq(testAddr, newSharer),
	}

	done := bs.UpdateEntry(trans, bottomBuf, mshrBuf)
	if !done {
		t.Fatal("UpdateEntry returned false")
	}

	entry := &blk.SubEntry[0]
	if len(entry.Sharer) < 2 {
		t.Errorf("Sharer not appended: got %v", entry.Sharer)
	}

	if !entry.IsValid {
		t.Errorf("SubEntry[0].IsValid must be true after UpdateEntry — Bug 2 not fixed (sharers=%v)", entry.Sharer)
	}
}

// ── bankstage (Bug 3) tests ───────────────────────────────────────────────────

// TestREC_InvalidateAndUpdateEntry_ZombieEntry demonstrates Bug 3:
// After the last subentry is invalidated, blk.IsValid is incorrectly set to true
// instead of being left false. This creates a "zombie" entry that Lookup can
// match via entry.IsValid==true even though IsValidEntry()==false.
func TestREC_InvalidateAndUpdateEntry_ZombieEntry(t *testing.T) {
	bs, bottomBuf, mshrBuf := buildBankStage()

	blk := &internal.CohEntry{}
	blk.IsValid = true
	blk.Tag = testAddr
	blk.PID = testPID

	// Single live subentry at index 0.
	blk.SubEntry[0].IsValid = true
	blk.SubEntry[0].Sharer = []sim.RemotePort{testSharer}

	trans := &transaction{
		action:   InvalidateAndUpdateEntry,
		block:    blk,
		blockIdx: 0,
		read:     buildReadReq(testAddr, testSharer),
	}

	done := bs.InvalidateAndUpdateEntry(trans, bottomBuf, mshrBuf)
	if !done {
		t.Fatal("InvalidateAndUpdateEntry returned false")
	}

	// SubEntry[0] must be invalid after invalidation — both implementations agree.
	if blk.SubEntry[0].IsValid {
		t.Error("SubEntry[0].IsValid must be false after InvalidateAndUpdateEntry")
	}

	// IsValidEntry() must return false — no live subentries remain.
	if blk.IsValidEntry() {
		t.Error("IsValidEntry() must return false when no SubEntry is valid")
	}

	if blk.IsValid {
		t.Error("blk.IsValid must be false after last-subentry invalidation — Bug 3 not fixed (zombie entry)")
	}
}

// TestREC_InvalidateAndUpdateEntry_PartialInvalidation verifies that
// InvalidateAndUpdateEntry on one of multiple live subentries does NOT
// corrupt blk.IsValid (the zombie-entry bug only triggers on the last subentry).
func TestREC_InvalidateAndUpdateEntry_PartialInvalidation(t *testing.T) {
	bs, bottomBuf, mshrBuf := buildBankStage()

	blk := &internal.CohEntry{}
	blk.IsValid = true
	blk.Tag = testAddr
	blk.PID = testPID

	// Two live subentries.
	blk.SubEntry[0].IsValid = true
	blk.SubEntry[0].Sharer = []sim.RemotePort{testSharer}
	blk.SubEntry[1].IsValid = true
	blk.SubEntry[1].Sharer = []sim.RemotePort{"GPU1.L1"}

	trans := &transaction{
		action:   InvalidateAndUpdateEntry,
		block:    blk,
		blockIdx: 0,
		read:     buildReadReq(testAddr, testSharer),
	}

	bs.InvalidateAndUpdateEntry(trans, bottomBuf, mshrBuf)

	// SubEntry[1] must still be valid.
	if !blk.SubEntry[1].IsValid {
		t.Error("SubEntry[1].IsValid must not be touched by invalidation of SubEntry[0]")
	}

	// Entry as a whole must still be considered valid.
	if !blk.IsValidEntry() {
		t.Error("IsValidEntry() must return true when SubEntry[1] is still live")
	}
}

// ── multi-sharer coalescence test ────────────────────────────────────────────

// TestREC_MultiSharer_NoDuplication verifies that calling InsertNewEntry
// followed by UpdateEntry from the same sharer does not duplicate sharer entries.
func TestREC_MultiSharer_NoDuplication(t *testing.T) {
	bs, bottomBuf, mshrBuf := buildBankStage()

	blk := makeEntry(0)

	// First access: InsertNewEntry
	trans1 := &transaction{
		action:   InsertNewEntry,
		block:    blk,
		blockIdx: 0,
		read:     buildReadReq(testAddr, testSharer),
	}
	bs.InsertNewEntry(trans1, bottomBuf, mshrBuf)

	// Second access from the same sharer: UpdateEntry should not duplicate.
	trans2 := &transaction{
		action:   UpdateEntry,
		block:    blk,
		blockIdx: 0,
		read:     buildReadReq(testAddr, testSharer),
	}
	bs.UpdateEntry(trans2, bottomBuf, mshrBuf)

	entry := &blk.SubEntry[0]
	if len(entry.Sharer) != 1 {
		t.Errorf("duplicate sharer: want 1 entry, got %d: %v", len(entry.Sharer), entry.Sharer)
	}
}

// TestREC_MultiSharer_TwoDistinctSharers verifies that two distinct sharers
// are both recorded after InsertNewEntry + UpdateEntry.
func TestREC_MultiSharer_TwoDistinctSharers(t *testing.T) {
	bs, bottomBuf, mshrBuf := buildBankStage()

	blk := makeEntry(0)
	var sharer2 sim.RemotePort = "GPU1.L1"

	trans1 := &transaction{
		action: InsertNewEntry, block: blk, blockIdx: 0,
		read: buildReadReq(testAddr, testSharer),
	}
	bs.InsertNewEntry(trans1, bottomBuf, mshrBuf)

	trans2 := &transaction{
		action: UpdateEntry, block: blk, blockIdx: 0,
		read: buildReadReq(testAddr, sharer2),
	}
	bs.UpdateEntry(trans2, bottomBuf, mshrBuf)

	entry := &blk.SubEntry[0]
	if len(entry.Sharer) != 2 {
		t.Errorf("want 2 sharers, got %d: %v", len(entry.Sharer), entry.Sharer)
	}
}

// ── eviction correctness ──────────────────────────────────────────────────────

// TestREC_InvalidateEntry_ClearsAllSubEntries verifies InvalidateEntry
// resets the entire block (all subentries + entry-level IsValid).
func TestREC_InvalidateEntry_ClearsAllSubEntries(t *testing.T) {
	bs, bottomBuf, _ := buildBankStage()
	// InvalidateEntry only uses bottomSenderBuffer.
	_ = bs

	blk := &internal.CohEntry{}
	blk.IsValid = true
	for i := range blk.SubEntry {
		blk.SubEntry[i].IsValid = true
		blk.SubEntry[i].Sharer = []sim.RemotePort{testSharer}
	}

	trans := &transaction{
		action: InvalidateEntry,
		block:  blk,
		read:   buildReadReq(testAddr, testSharer),
	}
	bs.InvalidateEntry(trans, bottomBuf)

	if blk.IsValid {
		t.Error("blk.IsValid must be false after InvalidateEntry")
	}
	for i := range blk.SubEntry {
		if blk.SubEntry[i].IsValid {
			t.Errorf("SubEntry[%d].IsValid must be false after InvalidateEntry", i)
		}
		if len(blk.SubEntry[i].Sharer) != 0 {
			t.Errorf("SubEntry[%d].Sharer must be nil after InvalidateEntry", i)
		}
	}
}
