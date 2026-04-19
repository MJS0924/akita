package superdirectory

import (
	"reflect"
	"testing"

	"github.com/sarchlab/akita/v4/mem/cache/superdirectory/internal"
	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
)

// ─── mock SuperDirectory ───────────────────────────────────────────────────────

type mockSuperDirectory struct {
	getBankResult  []int
	insertedRegion []int
	insertedAddr   []uint64
}

func (m *mockSuperDirectory) GetBank(_ uint64) []int { return m.getBankResult }
func (m *mockSuperDirectory) InsertBloomfilter(regionID int, addr uint64) {
	m.insertedRegion = append(m.insertedRegion, regionID)
	m.insertedAddr = append(m.insertedAddr, addr)
}
func (m *mockSuperDirectory) Visit(_ int, _ *internal.CohEntry)            {}
func (m *mockSuperDirectory) EvictBloomfilter(_ int, _ uint64)             {}
func (m *mockSuperDirectory) Reset()                                       {}
func (m *mockSuperDirectory) Lookup(_ int, _ vm.PID, _ uint64) (*internal.CohEntry, int) {
	panic("not implemented")
}
func (m *mockSuperDirectory) GetSet(_ int, _ uint64) (*internal.CohSet, int) {
	panic("not implemented")
}
func (m *mockSuperDirectory) GetBanks() [][]internal.CohSet { panic("not implemented") }
func (m *mockSuperDirectory) FindVictim(_ int, _ vm.PID, _ uint64) (*internal.CohEntry, bool) {
	panic("not implemented")
}
func (m *mockSuperDirectory) TotalSize() uint64  { panic("not implemented") }
func (m *mockSuperDirectory) WayAssociativity() int { panic("not implemented") }
func (m *mockSuperDirectory) GetRegionLen(_ int) int { panic("not implemented") }

// ─── mock MSHR ────────────────────────────────────────────────────────────────

type mockMSHR struct{}

func (m *mockMSHR) IsFull() bool { return false }
func (m *mockMSHR) Add(_ vm.PID, _ uint64, _ uint64, _ int) *internal.MSHREntry {
	return internal.NewMSHREntry()
}
func (m *mockMSHR) Query(_ vm.PID, _ uint64) *internal.MSHREntry          { return nil }
func (m *mockMSHR) QueryWithMask(_ vm.PID, _ uint64, _ uint64) []*internal.MSHREntry {
	return nil
}
func (m *mockMSHR) Remove(_ vm.PID, _ uint64) *internal.MSHREntry { panic("not implemented") }
func (m *mockMSHR) AllEntries() []*internal.MSHREntry             { return nil }
func (m *mockMSHR) Reset()                                        {}

// ─── constants and helpers ────────────────────────────────────────────────────

var testRegionLen = []int{14, 12, 10, 8, 6}

const testNumBanks = 5
const testLog2PageSize = 12

// testAddr is 64 MB — large enough that (addr>>12)>>14 != 0 so zero-initialised
// RSB slots (Addr=0, RegionID=0, mask=14) do not falsely match.
const testAddr uint64 = 0x4000000

func newMockDir(getBankResult []int) *mockSuperDirectory {
	return &mockSuperDirectory{getBankResult: getBankResult}
}

// newTestDirStage builds the minimal directoryStage for selectBank tests.
// rsbRegionID >= 0 pushes an RSB entry; rsbRegionID < 0 leaves RSB empty.
func newTestDirStage(dir *mockSuperDirectory, rsbRegionID int) *directoryStage {
	// numEntries=1: Push evicts the zero-initialised slot, leaving only the
	// actual entry and preventing spurious RSB hits on zero-valued entries.
	rsb := internal.NewRegionSizeBuffer(1, testLog2PageSize, testRegionLen)
	if rsbRegionID >= 0 {
		rsb.Push(testAddr, rsbRegionID)
	}
	comp := &Comp{
		directory:        dir,
		regionSizeBuffer: *rsb,
		numBanks:         testNumBanks,
	}
	return &directoryStage{cache: comp}
}

// newTestDirStageForWriteToBank builds a Comp with enough fields to let
// writeToBank reach (and pass) the BF-insert decision point.
func newTestDirStageForWriteToBank(dir *mockSuperDirectory) *directoryStage {
	buffers := make([]sim.Buffer, testNumBanks)
	for i := range buffers {
		buffers[i] = sim.NewBuffer("TestBuf"+string(rune('0'+i)), 16)
	}
	comp := &Comp{
		directory:             dir,
		numBanks:              testNumBanks,
		regionLen:             testRegionLen,
		log2NumSubEntry:       2,
		mshr:                  &mockMSHR{},
		localDirToBankBuffers: buffers,
	}
	ds := &directoryStage{cache: comp}
	// writeToBank dereferences returnFalse; point it at the pre-allocated field.
	ds.returnFalse = &ds.returnFalse0
	return ds
}

func buildDummyRead(addr uint64) *mem.ReadReq {
	return mem.ReadReqBuilder{}.WithAddress(addr).WithByteSize(64).Build()
}

// ─── PHASE 1: selectBank routing (5 cases) ────────────────────────────────────

// (a) RSB hit, BF empty → normal sub-case: bankID=RSB, bankList=nil, bfEager=true
func TestSelectBank_RSBHit_BFEmpty(t *testing.T) {
	ds := newTestDirStage(newMockDir(nil), 2)
	sel := ds.selectBank(testAddr)
	if sel.bankID != 2 {
		t.Errorf("bankID: want 2, got %d", sel.bankID)
	}
	if sel.bankList != nil {
		t.Errorf("bankList: want nil, got %v", sel.bankList)
	}
	if !sel.bfEager {
		t.Error("bfEager: want true for normal RSB hit")
	}
	if sel.onCommit == nil {
		t.Error("onCommit: want non-nil for normal RSB hit")
	}
}

// (b) RSB=1, BF=[3,2] (bfList[last]=2 > 1) → non-stale → bankID=1, bankList=nil
func TestSelectBank_RSBHit_BFNonStale(t *testing.T) {
	ds := newTestDirStage(newMockDir([]int{3, 2}), 1)
	sel := ds.selectBank(testAddr)
	if sel.bankID != 1 {
		t.Errorf("bankID: want 1, got %d", sel.bankID)
	}
	if sel.bankList != nil {
		t.Errorf("bankList: want nil, got %v", sel.bankList)
	}
	if !sel.bfEager {
		t.Error("bfEager: want true for normal RSB hit")
	}
}

// (c) RSB=2, BF=[4,0] (bfList[last]=0 < 2) → stale → bankID=4, bankList=[0], bfEager=false
func TestSelectBank_RSBHit_BFStale(t *testing.T) {
	ds := newTestDirStage(newMockDir([]int{4, 0}), 2)
	sel := ds.selectBank(testAddr)
	if sel.bankID != 4 {
		t.Errorf("bankID: want 4, got %d", sel.bankID)
	}
	if !reflect.DeepEqual(sel.bankList, []int{0}) {
		t.Errorf("bankList: want [0], got %v", sel.bankList)
	}
	if sel.bfEager {
		t.Error("bfEager: want false for stale RSB")
	}
}

// (d) RSB miss, BF=[3,1] → bankID=3, bankList=[1]
func TestSelectBank_RSBMiss_BFNonEmpty(t *testing.T) {
	ds := newTestDirStage(newMockDir([]int{3, 1}), -1)
	sel := ds.selectBank(testAddr)
	if sel.bankID != 3 {
		t.Errorf("bankID: want 3, got %d", sel.bankID)
	}
	if !reflect.DeepEqual(sel.bankList, []int{1}) {
		t.Errorf("bankList: want [1], got %v", sel.bankList)
	}
	if sel.bfEager {
		t.Error("bfEager: want false for RSB miss")
	}
}

// (e) RSB miss, BF empty → bankID=numBanks-1=4, bankList=nil
func TestSelectBank_RSBMiss_BFEmpty(t *testing.T) {
	ds := newTestDirStage(newMockDir(nil), -1)
	sel := ds.selectBank(testAddr)
	if sel.bankID != testNumBanks-1 {
		t.Errorf("bankID: want %d, got %d", testNumBanks-1, sel.bankID)
	}
	if sel.bankList != nil {
		t.Errorf("bankList: want nil, got %v", sel.bankList)
	}
}

// ─── PHASE 2: commit-safe [S1] guarantees ─────────────────────────────────────

// S1-before: before onCommit, RSB entry must still exist and BF must be clean.
func TestSelectBank_CommitSafe_S1_BeforeOnCommit(t *testing.T) {
	dir := newMockDir(nil)
	ds := newTestDirStage(dir, 2)

	_ = ds.selectBank(testAddr) // state must NOT change here

	if got := ds.cache.regionSizeBuffer.Search(testAddr); got.RegionID != 2 {
		t.Errorf("RSB must not be deleted before onCommit: got RegionID=%d", got.RegionID)
	}
	if len(dir.insertedRegion) != 0 {
		t.Errorf("InsertBloomfilter must not be called before onCommit: %d calls", len(dir.insertedRegion))
	}
}

// S1-after: after onCommit, RSB entry is deleted and BF is inserted exactly once.
func TestSelectBank_CommitSafe_S1_AfterOnCommit(t *testing.T) {
	dir := newMockDir(nil)
	ds := newTestDirStage(dir, 2)

	sel := ds.selectBank(testAddr)
	if sel.onCommit == nil {
		t.Fatal("onCommit must be non-nil for normal RSB hit")
	}
	sel.onCommit()

	if got := ds.cache.regionSizeBuffer.Search(testAddr); got.RegionID != -1 {
		t.Errorf("RSB must be deleted after onCommit: RegionID=%d", got.RegionID)
	}
	if len(dir.insertedRegion) != 1 || dir.insertedRegion[0] != 2 {
		t.Errorf("InsertBloomfilter: want 1 call for bank 2, got %v", dir.insertedRegion)
	}
}

// Race: after onCommit, a second request for the same region uses BF guidance
// (not the default finest bank).
func TestSelectBank_Race_BFInsertPreventsDefaultRouting(t *testing.T) {
	// After eager insert, the BF reports bank 2 as a hit for testAddr.
	// Simulate this by pre-loading GetBank to return [2].
	dir := newMockDir([]int{2})

	// Commit X1's onCommit (RSB deleted, BF[2] inserted conceptually).
	ds1 := newTestDirStage(dir, 2)
	sel1 := ds1.selectBank(testAddr)
	sel1.onCommit() // RSB deleted; real BF updated (mock records the call)

	// X2 arrives on same Comp (RSB now empty, BF returns [2]).
	ds2 := &directoryStage{cache: ds1.cache}
	sel2 := ds2.selectBank(testAddr)
	if sel2.bankID != 2 {
		t.Errorf("X2 bankID: want 2 (BF-guided), got %d", sel2.bankID)
	}
}

// Stale path must not set bfEager=true (would suppress the allocation-path BF insert).
func TestSelectBank_Stale_BFEagerFalse(t *testing.T) {
	ds := newTestDirStage(newMockDir([]int{4, 0}), 2)
	sel := ds.selectBank(testAddr)
	if sel.bfEager {
		t.Error("stale sub-case must not set bfEager=true")
	}
	if len(sel.bankList) == 0 {
		t.Error("stale sub-case must carry bankList")
	}
}

// S2: bfEagerInserted=true suppresses InsertBloomfilter inside writeToBank.
func TestWriteToBank_BFEagerSuppression(t *testing.T) {
	dir := newMockDir(nil)
	ds := newTestDirStageForWriteToBank(dir)

	block := &internal.CohEntry{}
	// SubEntry[0].IsValid = false → would normally trigger InsertBloomfilter

	trans := &transaction{
		bankID:          2,
		bfEagerInserted: true,
		read:            buildDummyRead(testAddr),
	}

	ds.writeToBank(trans, block, 0, true)

	if len(dir.insertedRegion) != 0 {
		t.Errorf("InsertBloomfilter must be suppressed when bfEagerInserted=true, got %d calls", len(dir.insertedRegion))
	}
}

// S2-control: with bfEagerInserted=false and IsValid=false, InsertBloomfilter IS called.
func TestWriteToBank_BFInsert_WhenNotEager(t *testing.T) {
	dir := newMockDir(nil)
	ds := newTestDirStageForWriteToBank(dir)

	block := &internal.CohEntry{}
	// SubEntry[0].IsValid = false, bfEagerInserted = false → insert expected

	trans := &transaction{
		bankID:          2,
		bfEagerInserted: false,
		read:            buildDummyRead(testAddr),
	}

	ds.writeToBank(trans, block, 0, true)

	if len(dir.insertedRegion) != 1 {
		t.Errorf("InsertBloomfilter must be called when bfEagerInserted=false and IsValid=false, got %d calls", len(dir.insertedRegion))
	}
}