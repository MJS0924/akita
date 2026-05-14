package internal

import (
	"testing"
	"time"

	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
)

// ── RegionSizeBuffer ──────────────────────────────────────────────────────────

// TestRegionSizeBuffer_Search_DefaultZeroCollision shows that Search returns a
// "match" against the zero-initialized backing array when an unrelated address
// happens to map to addr>>log2PageSize == 0. The caller cannot distinguish a
// real hit (RegionID=0) from a default-init zero entry.
//
// This is a correctness bug if any caller relies on Search to detect *absence*
// of an addr inserted via Push. Fortunately the REC pipeline does not invoke
// this method on its hot path, but any future caller would inherit the bug.
func TestRegionSizeBuffer_Search_DefaultZeroCollision(t *testing.T) {
	const log2PageSize = 12
	b := NewRegionSizeBuffer(8, log2PageSize)

	got := b.Search(0x000) // addr 0 → addr>>12 == 0; should be "not found"
	if got.RegionID != -1 {
		t.Errorf("Search returned RegionID=%d for never-pushed addr 0; expected -1 "+
			"(default-zero entries in the backing slice falsely match)",
			got.RegionID)
	}
}

// TestRegionSizeBuffer_Delete_InfiniteLoopWhenHeadDoesNotMatch demonstrates the
// `i--` bug in Delete:
//
//	for i := 0; i < len(b.entries); i++ {
//	    if b.entries[i] == e { ...remove... }
//	    i--   // <-- always runs, even when no removal happened
//	}
//
// Net effect: when entries[0] != e, i++ from the for-clause and i-- in the body
// cancel out. The loop never advances and never terminates. We bound the test
// with a goroutine + time.After so a regression doesn't hang `go test`.
func TestRegionSizeBuffer_Delete_InfiniteLoopWhenHeadDoesNotMatch(t *testing.T) {
	const log2PageSize = 12
	b := NewRegionSizeBuffer(4, log2PageSize)

	// Push a real entry so entries[0] is non-zero.
	b.Push(0x4000, 7) // stored as Addr=0x4, RegionID=7

	// Construct a bogus target the buffer cannot contain (RegionID=-1 with
	// Addr=0). Note: the default-zero entries in the backing slice have
	// RegionID=0, not -1, so none match — Delete will spin forever.
	bogus := RegionSizeBufferEntry{Addr: 0, RegionID: -1}

	done := make(chan struct{})
	go func() {
		defer close(done)
		b.Delete(bogus)
	}()

	select {
	case <-done:
		// Bug fixed (or never present in this build).
	case <-time.After(500 * time.Millisecond):
		t.Fatal("RegionSizeBuffer.Delete did not return within 500ms — " +
			"infinite loop bug: `i--` is unconditional, cancels the for-loop's `i++` " +
			"when entries[0] != e (regionSizeBuffer.go:50-58)")
	}
}

// TestRegionSizeBuffer_Update_HangsOnUnknownAddr is the user-facing form of
// the Delete bug: Update calls Search → Delete. If addr was never pushed, the
// Search returns the {RegionID:-1} sentinel which Delete cannot find, hanging
// the buffer.
func TestRegionSizeBuffer_Update_HangsOnUnknownAddr(t *testing.T) {
	const log2PageSize = 12
	b := NewRegionSizeBuffer(4, log2PageSize)

	b.Push(0x4000, 7)

	done := make(chan struct{})
	go func() {
		defer close(done)
		b.Update(0x8000, 9) // addr never pushed → Search returns -1
	}()

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("RegionSizeBuffer.Update hung on unknown addr — same root " +
			"cause as Delete infinite-loop")
	}
}

// ── CountingBloomFilter ──────────────────────────────────────────────────────

func TestCountingBloomFilter_InsertSearchEvict(t *testing.T) {
	cbf := NewCountingBloomFilter(0, 256)

	addr := uint64(0xDEADBEEF) << 6 // align so masking matters
	const numMaskBit = 6

	if cbf.Search(addr, numMaskBit) {
		t.Error("fresh CBF: Search returned true for never-inserted addr")
	}

	cbf.Insert(addr, numMaskBit)
	if !cbf.Search(addr, numMaskBit) {
		t.Error("after Insert: Search returned false (false negative on definitely-present addr)")
	}

	cbf.Evict(addr, numMaskBit)
	if cbf.Search(addr, numMaskBit) {
		t.Error("after balanced Insert/Evict: Search returned true (counter not decremented)")
	}
}

// TestCountingBloomFilter_StickyAt511 verifies the stale-comment area: code
// uses 9-bit saturation (511) with a sticky-at-saturation rule. Confirm Evict
// does NOT decrement when the counter is at 511.
func TestCountingBloomFilter_StickyAt511(t *testing.T) {
	cbf := NewCountingBloomFilter(0, 64)

	// Drive a single set of indices to saturation.
	addr := uint64(0xCAFE)
	const numMaskBit = 0
	for i := 0; i < 600; i++ { // > 511
		cbf.Insert(addr, numMaskBit)
	}

	// One Evict must NOT bring the counter below 511 (sticky behavior).
	cbf.Evict(addr, numMaskBit)
	if !cbf.Search(addr, numMaskBit) {
		t.Error("sticky-at-511 violated: Search false after one Evict on saturated counter " +
			"(comment claims 4-bit/15 max but code uses 9-bit/511 — comment is stale, code is the contract)")
	}
}

// ── RECDirectory FindVictim alloc semantics ──────────────────────────────────

// TestRECDirectory_FindVictim_HitMatchingEntry checks the documented contract:
// when a valid entry exists with the same tag/PID, FindVictim returns that
// entry with alloc=false (caller should UpdateEntry, not allocate a new slot).
func TestRECDirectory_FindVictim_HitMatchingEntry(t *testing.T) {
	const (
		numSets    = 4
		numWays    = 4
		blockSize  = 64
		log2Sub    = 4
		log2Block  = 6
	)
	dir := NewRECDirectory(numSets, numWays, blockSize, log2Sub, log2Block, NewLRUVictimFinder())

	pid := vm.PID(1)
	addr := uint64(0x10000)
	maskLen := log2Block + log2Sub
	tag := addr >> maskLen << maskLen

	// First call: must return alloc=true (empty set).
	entry1, alloc1 := dir.FindVictim(pid, addr)
	if !alloc1 {
		t.Fatal("FindVictim on empty set returned alloc=false")
	}
	entry1.IsValid = true
	entry1.PID = pid
	entry1.Tag = tag
	entry1.SubEntry[(addr>>log2Block)%(1<<log2Sub)].IsValid = true

	// Second call same addr: must return alloc=false (existing valid match).
	entry2, alloc2 := dir.FindVictim(pid, addr)
	if alloc2 {
		t.Errorf("FindVictim on same addr returned alloc=true; expected false (entry exists with matching tag/PID)")
	}
	if entry2 != entry1 {
		t.Error("FindVictim returned a different entry pointer for the same tag")
	}
}

// TestRECDirectory_FindVictim_DifferentPID_NoFalseHit guards against the case
// where two PIDs have entries that share a Tag value (post-translation) — the
// directory must distinguish by PID.
func TestRECDirectory_FindVictim_DifferentPID_NoFalseHit(t *testing.T) {
	dir := NewRECDirectory(4, 4, 64, 4, 6, NewLRUVictimFinder())

	addr := uint64(0x10000)
	maskLen := 6 + 4
	tag := addr >> maskLen << maskLen

	// Pre-populate PID=1 entry.
	e1, _ := dir.FindVictim(vm.PID(1), addr)
	e1.IsValid = true
	e1.PID = vm.PID(1)
	e1.Tag = tag
	e1.SubEntry[(addr>>6)%16].IsValid = true

	// Lookup with PID=2 must miss.
	got, idx := dir.Lookup(vm.PID(2), addr)
	if got != nil || idx != -1 {
		t.Errorf("Lookup with PID=2 matched PID=1's entry: got=%v idx=%d", got, idx)
	}

	// FindVictim with PID=2 must allocate a fresh slot (alloc=true).
	_, alloc := dir.FindVictim(vm.PID(2), addr)
	if !alloc {
		t.Error("FindVictim with different PID returned alloc=false; expected true (no PID=2 entry exists yet)")
	}
}

// TestRECDirectory_Visit_LRUOrdering verifies Visit moves the entry to the
// MRU end of LRUQueue. A bug here would skew victim selection toward fresh
// entries and inflate eviction churn.
func TestRECDirectory_Visit_LRUOrdering(t *testing.T) {
	dir := NewRECDirectory(1, 4, 64, 4, 6, NewLRUVictimFinder())

	sets := dir.GetSets()
	set := &sets[0]
	if len(set.LRUQueue) != 4 {
		t.Fatalf("expected 4 LRU entries, got %d", len(set.LRUQueue))
	}

	// Visit the first entry; it should move to the tail.
	first := set.LRUQueue[0]
	dir.Visit(first)

	sets = dir.GetSets()
	set = &sets[0]
	if set.LRUQueue[len(set.LRUQueue)-1] != first {
		t.Error("Visit did not move the entry to the tail (MRU position)")
	}
	if set.LRUQueue[0] == first {
		t.Error("Visit did not remove the entry from the head")
	}
}

// ── RECDirectory Lookup against Reset-but-not-zeroed Tag ─────────────────────

// TestRECDirectory_Lookup_AfterEntryReset asserts that an entry whose
// IsValid was set false (e.g. via CohEntry.Reset, which the code does on the
// last-subentry invalidation path and inside writeToBank's allocation flow)
// is NOT returned by Lookup, even though Tag is preserved.
//
// Pre-fix REC's Lookup only checks entry.IsValid — so this scenario is fine.
// The test pins that contract so future "optimizations" can't bring back a
// stale-tag hit.
func TestRECDirectory_Lookup_AfterEntryReset(t *testing.T) {
	dir := NewRECDirectory(4, 4, 64, 4, 6, NewLRUVictimFinder())
	pid := vm.PID(1)
	addr := uint64(0x10000)

	e, _ := dir.FindVictim(pid, addr)
	e.IsValid = true
	e.PID = pid
	e.Tag = addr >> 10 << 10
	e.SubEntry[(addr>>6)%16].IsValid = true
	e.SubEntry[(addr>>6)%16].Sharer = []sim.RemotePort{"GPU0.RDMA"}

	// Reset the entry (mimics writeToBank's victim.Reset()).
	e.Reset()

	got, idx := dir.Lookup(pid, addr)
	if got != nil {
		t.Errorf("Lookup matched a Reset entry (Tag preserved but IsValid=false): got idx=%d", idx)
	}
}
