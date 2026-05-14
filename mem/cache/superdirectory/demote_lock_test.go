package superdirectory

import (
	"testing"

	"github.com/sarchlab/akita/v4/mem/cache/superdirectory/internal"
	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
)

// ─────────────────────────────────────────────────────────────────────────────
// H3e demote-lock fix: 5 unit tests covering the spec areas
//   1. Lock正常 set on entry created by demote        → TestDemoteLock_DoDemotion_SetsLockOnTarget
//   2. Promote 후 lock reset                          → TestDemoteLock_DoPromotion_ClearsLockOnTarget
//   3. Eviction (== CohEntry.Reset()) clears lock     → TestDemoteLock_Reset_ClearsLock
//   4. Sharer correctness — invalidationList preserved → TestDemoteLock_ProcessOneReq_PreservesInvalidationList
//   5. Multi-bank cascade prevention via lock-skip    → TestDemoteLock_ProcessOneReq_SkipsDemoteWhenLocked
// ─────────────────────────────────────────────────────────────────────────────

// ─── Test scaffolding for doDemotion/doPromotion ─────────────────────────────

// dirStub is a minimal SuperDirectory mock that supports Lookup (Miss) and
// FindVictim (returns a fresh empty entry) so doDemotion / doPromotion can
// run without panicking. Other directory methods are unused on these paths.
type dirStub struct {
	mockSuperDirectory
	victim *internal.CohEntry
}

func (d *dirStub) Lookup(_ int, _ vm.PID, _ uint64) (*internal.CohEntry, int) {
	return nil, -1
}
func (d *dirStub) FindVictim(_ int, _ vm.PID, _ uint64) (*internal.CohEntry, bool) {
	return d.victim, true
}

// newDirStageForMotion builds a directoryStage wired up with bank buffers
// so doDemotion/doPromotion can push their target trans through.
func newDirStageForMotion(victim *internal.CohEntry) (*directoryStage, *Comp) {
	dir := &dirStub{victim: victim}
	localBufs := make([]sim.Buffer, testNumBanks)
	for i := range localBufs {
		localBufs[i] = sim.NewBuffer("DirToBank", 32)
	}
	comp := &Comp{
		directory:             dir,
		numBanks:              testNumBanks,
		regionLen:             testRegionLen,
		log2NumSubEntry:       2,
		mshr:                  &mockMSHR{},
		localDirToBankBuffers: localBufs,
	}
	ds := &directoryStage{cache: comp}
	ds.returnFalse = &ds.returnFalse0
	return ds, comp
}

// ─── softMockMSHR — mockMSHR with non-panicking Remove for processOneReq ─────

type softMockMSHR struct {
	mockMSHR
	removeCalls int
}

func (m *softMockMSHR) Remove(_ vm.PID, _ uint64) *internal.MSHREntry {
	m.removeCalls++
	return nil
}

// newMshrStageForProcess builds a mshrStage with the surrounding Comp wired
// up to let processOneReq run end-to-end with empty MSHREntry.Requests.
func newMshrStageForProcess() (*mshrStage, *Comp) {
	logger := newEventLogger(&mockClock{time: 1.0}, 4)
	localBuf := sim.NewBuffer("LocalMSHR", 16)
	remoteBuf := sim.NewBuffer("RemoteMSHR", 16)
	dirStageMotion := sim.NewBuffer("DirMotion", 16)
	comp := &Comp{
		numBanks:                testNumBanks,
		regionLen:               testRegionLen,
		log2NumSubEntry:         2,
		mshr:                    &softMockMSHR{},
		directory:               &mockSuperDirectory{},
		dirStageMotionBuffer:    dirStageMotion,
		eventLogger:             logger,
		localMshrStageBuffer:    localBuf,
		remoteMshrStageBuffer:   remoteBuf,
	}
	return &mshrStage{cache: comp}, comp
}

// ─── Test 1: doDemotion sets DemoteLocked=true on target block ───────────────

// Spec test 1 (정상 동작): when a demote completes, the freshly created entry
// at the target (lower) bank must have DemoteLocked=true.
func TestDemoteLock_DoDemotion_SetsLockOnTarget(t *testing.T) {
	// victim is the slot at bank+1 that doDemotion will populate. Initially
	// not a demote product (lock=false). After doDemotion it should be true.
	victim := &internal.CohEntry{
		SubEntry: make([]internal.CohSubEntry, 4),
	}
	ds, _ := newDirStageForMotion(victim)

	// Build the source block (the entry being demoted) and the staged newBlk
	// that mshrStage.insertDemotionEntry would have produced.
	srcBlock := &internal.CohEntry{
		IsValid: true,
		PID:     vm.PID(1),
		Tag:     testAddr,
		// trans.block.SubEntry holds the staged sub-entries to copy into target
		SubEntry: make([]internal.CohSubEntry, 4),
	}
	// Mark all staged sub-entries IsValid so the copy loop populates target.
	for i := range srcBlock.SubEntry {
		srcBlock.SubEntry[i].IsValid = true
	}

	read := mem.ReadReqBuilder{}.WithAddress(testAddr).WithByteSize(64).Build()
	read.SetSrcRDMA(sim.RemotePort("GPU0"))
	trans := &transaction{
		action:       InsertDemotionEntry,
		bankID:       2, // demote target = bank 2
		block:        srcBlock,
		evictingAddr: testAddr,
		read:         read,
	}

	if !ds.doDemotion(trans, true) {
		t.Fatal("doDemotion returned false; cannot evaluate lock state")
	}

	// trans.block must now point at the target block (the one populated).
	if !trans.block.DemoteLocked {
		t.Errorf("doDemotion did not set DemoteLocked=true on target block")
	}
}

// ─── Test 2: doPromotion clears DemoteLocked on target ───────────────────────

// Spec test 2 (Promote 후 lock reset): a promotion target should not carry an
// inherited demote lock. Cover the Lookup-hit path where Reset() is skipped.
//
// We test with a victim that is *not* eligible for eviction (no valid sharers)
// so the alloc path in doPromotion does not call Reset(). This isolates the
// explicit clear added in directorystage.go.
func TestDemoteLock_DoPromotion_ClearsLockOnTarget(t *testing.T) {
	// Pre-populate the victim to look like an existing locked entry.
	victim := &internal.CohEntry{
		IsValid:      false, // !IsValidEntry → needEviction false → no Reset()
		DemoteLocked: true,
		SubEntry:     make([]internal.CohSubEntry, 4),
	}
	ds, _ := newDirStageForMotion(victim)

	read := mem.ReadReqBuilder{}.WithAddress(testAddr).WithByteSize(64).Build()
	read.SetSrcRDMA(sim.RemotePort("GPU0"))
	trans := &transaction{
		action:       InsertPromotionEntry,
		bankID:       1, // target bank 1 (promoted from bank 2)
		fetchingAddr: testAddr,
		fetchingPID:  vm.PID(1),
		sharers:      []sim.RemotePort{sim.RemotePort("GPU0")},
		read:         read,
	}

	if !ds.doPromotion(trans, true) {
		t.Fatal("doPromotion returned false; cannot evaluate lock state")
	}

	if trans.block.DemoteLocked {
		t.Errorf("doPromotion did not clear DemoteLocked on target block (Lookup-hit path)")
	}
}

// ─── Test 3: CohEntry.Reset() clears DemoteLocked (eviction path) ───────────

// Spec test 3 (Eviction 후 lock state): a victim that is evicted (Reset) must
// have DemoteLocked cleared so the slot can be reused for a fresh entry that
// is not a demote product.
func TestDemoteLock_Reset_ClearsLock(t *testing.T) {
	e := &internal.CohEntry{
		IsValid:      true,
		DemoteLocked: true,
		SubEntry:     make([]internal.CohSubEntry, 4),
	}
	for i := range e.SubEntry {
		e.SubEntry[i].IsValid = true
		e.SubEntry[i].Sharer = []sim.RemotePort{sim.RemotePort("GPU0")}
	}

	e.Reset()

	if e.DemoteLocked {
		t.Errorf("Reset() did not clear DemoteLocked (cache eviction would leave lock stale)")
	}
	// sanity: Reset must also clear IsValid, sharers — verify side effects unchanged
	if e.IsValid {
		t.Errorf("Reset() did not clear IsValid")
	}
	if len(e.SubEntry[0].Sharer) != 0 {
		t.Errorf("Reset() did not clear sub-entry sharer")
	}
}

// ─── Test 4: invalidate-only path preserves invalidationList ─────────────────

// Spec test 4 (Sharer correctness): when a demote is skipped due to lock,
// the trans.invalidationList (consumed by directoryStage / bottomSender to
// emit InvReq messages to remote sharers) must remain intact, otherwise
// remote caches keep stale shared copies after the writer commits.
func TestDemoteLock_ProcessOneReq_PreservesInvalidationList(t *testing.T) {
	s, _ := newMshrStageForProcess()

	// trans.block is the source entry (already created by an earlier demote)
	// → DemoteLocked = true.
	srcBlock := &internal.CohEntry{
		IsValid:      true,
		DemoteLocked: true,
		PID:          vm.PID(1),
		Tag:          testAddr,
		SubEntry:     make([]internal.CohSubEntry, 4),
	}
	srcBlock.SubEntry[0].IsValid = false // bankstage cleared it before mshrstage
	srcBlock.SubEntry[0].Sharer = nil

	mshrEntry := internal.NewMSHREntry()
	mshrEntry.PID = vm.PID(1)
	mshrEntry.Address = testAddr

	read := mem.ReadReqBuilder{}.WithAddress(testAddr).WithByteSize(64).Build()
	read.SetSrcRDMA(sim.RemotePort("GPU0"))
	originalSharers := []sim.RemotePort{sim.RemotePort("GPU1"), sim.RemotePort("GPU2")}
	invList := make([]sim.RemotePort, len(originalSharers))
	copy(invList, originalSharers)
	trans := &transaction{
		action:           InvalidateAndUpdateEntry,
		needToDemotion:   true,
		bankID:           2, // mid-bank, not finest; demote-eligible if not locked
		blockIdx:         0,
		block:            srcBlock,
		mshrEntry:        mshrEntry,
		invalidationList: invList,
		read:             read,
	}
	s.cache.localMshrStageBuffer.Push(trans)
	s.processingTrans = trans
	s.isProcessingLocal = true

	s.processOneReq()

	// invalidationList survives untouched → InvReq emission can proceed.
	if got := len(trans.invalidationList); got != len(originalSharers) {
		t.Fatalf("invalidationList length: want %d, got %d (lost during demote-skip)",
			len(originalSharers), got)
	}
	for i, sh := range trans.invalidationList {
		if sh != originalSharers[i] {
			t.Errorf("invalidationList[%d]: want %s, got %s", i, originalSharers[i], sh)
		}
	}
}

// ─── Test 5: lock-skip path prevents cascade demote ─────────────────────────

// Spec test 5 (Multi-bank correctness): a demote trigger on a locked entry
// must NOT enqueue another demote (would create a deeper bank entry =
// cascade). It must reset the lock and increment the diagnostic counter.
func TestDemoteLock_ProcessOneReq_SkipsDemoteWhenLocked(t *testing.T) {
	s, comp := newMshrStageForProcess()

	srcBlock := &internal.CohEntry{
		IsValid:      true,
		DemoteLocked: true, // entry is itself a demote product
		PID:          vm.PID(1),
		Tag:          testAddr,
		SubEntry:     make([]internal.CohSubEntry, 4),
	}
	srcBlock.SubEntry[0].IsValid = false

	mshrEntry := internal.NewMSHREntry()
	mshrEntry.PID = vm.PID(1)
	mshrEntry.Address = testAddr

	read := mem.ReadReqBuilder{}.WithAddress(testAddr).WithByteSize(64).Build()
	read.SetSrcRDMA(sim.RemotePort("GPU0"))
	trans := &transaction{
		action:           InvalidateAndUpdateEntry,
		needToDemotion:   true,
		bankID:           2, // not finest → would normally demote to bank 3
		blockIdx:         0,
		block:            srcBlock,
		mshrEntry:        mshrEntry,
		invalidationList: []sim.RemotePort{sim.RemotePort("GPU1")},
		read:             read,
	}
	s.cache.localMshrStageBuffer.Push(trans)
	s.processingTrans = trans
	s.isProcessingLocal = true

	s.processOneReq()

	// Cascade prevented:
	if len(s.demotionQueue) != 0 {
		t.Errorf("demotionQueue should be empty (demote skipped), got len=%d", len(s.demotionQueue))
	}
	if comp.demoteLockHits != 1 {
		t.Errorf("demoteLockHits: want 1, got %d", comp.demoteLockHits)
	}
	// Lock released so a future legitimate demote (after re-promote) can proceed.
	if srcBlock.DemoteLocked {
		t.Errorf("DemoteLocked must be reset after skip; remained true")
	}
}

// ─── Negative control: ensure unlocked entries still demote normally ────────

// Confirms the fix does not break the normal demote path. An unlocked entry
// that triggers needToDemotion must still enqueue a demote.
func TestDemoteLock_ProcessOneReq_UnlockedDemoteProceeds(t *testing.T) {
	s, comp := newMshrStageForProcess()

	srcBlock := &internal.CohEntry{
		IsValid:      true,
		DemoteLocked: false, // never been demoted
		PID:          vm.PID(1),
		Tag:          testAddr,
		SubEntry:     make([]internal.CohSubEntry, 4),
	}
	srcBlock.SubEntry[0].IsValid = false

	mshrEntry := internal.NewMSHREntry()
	mshrEntry.PID = vm.PID(1)
	mshrEntry.Address = testAddr

	read := mem.ReadReqBuilder{}.WithAddress(testAddr).WithByteSize(64).Build()
	read.SetSrcRDMA(sim.RemotePort("GPU0"))
	trans := &transaction{
		action:           InvalidateAndUpdateEntry,
		needToDemotion:   true,
		bankID:           2,
		blockIdx:         0,
		block:            srcBlock,
		mshrEntry:        mshrEntry,
		invalidationList: []sim.RemotePort{sim.RemotePort("GPU1")},
		read:             read,
	}
	s.cache.localMshrStageBuffer.Push(trans)
	s.processingTrans = trans
	s.isProcessingLocal = true

	s.processOneReq()

	if len(s.demotionQueue) != 1 {
		t.Errorf("normal demote path: demotionQueue len: want 1, got %d", len(s.demotionQueue))
	}
	if comp.demoteLockHits != 0 {
		t.Errorf("normal demote path: demoteLockHits should remain 0, got %d", comp.demoteLockHits)
	}
}
