package superdirectory

import (
	"reflect"
	"testing"

	"github.com/sarchlab/akita/v4/mem/cache/superdirectory/internal"
	"github.com/sarchlab/akita/v4/sim"
)

// These tests pin down the Sharer-field assignment performed by
// mshrStage.insertDemotionEntry (mshrstage.go:580-607).
//
// Contract being validated:
//
//   For the sub-entry that contains the triggering address `addr`:
//     - fromLocal == false (remote-triggered demotion):
//         Sharer = [owner]   (owner = trans.accessReq().GetSrcRDMA())
//         IsValid = true
//     - fromLocal == true (local-triggered demotion):
//         IsValid = false, Sharer untouched (nil on the fresh CohSubEntry)
//
//   For every OTHER sub-entry of the newly built block:
//     Sharer = copy(trans.invalidationList)
//     IsValid = true
//
// In our test setup (testRegionLen = [14,12,10,8,6], bankID=1 → bankID+1=2,
// testAddr = 0x4000000, log2NumSubEntry = 2):
//   prevRegionLen = 12, currRegionLen = 10
//   currAddr = (addr>>12)<<12 = 0x4000000, endAddr = 0x4001000
//   Loop generates 4 sub-entries of stride 1<<10 = 0x400.
//   i=0 covers [0x4000000, 0x4000400) which contains testAddr → trigger slot.
//   i=1,2,3 are non-trigger slots.

// popDemotedBlock retrieves the block that insertDemotionEntry pushed into
// dirStageMotionBuffer and asserts a single push happened.
func popDemotedBlock(t *testing.T, s *mshrStage) *internal.CohEntry {
	t.Helper()
	buf := s.cache.dirStageMotionBuffer
	if buf.Size() != 1 {
		t.Fatalf("expected 1 entry pushed to dirStageMotionBuffer, got %d", buf.Size())
	}
	popped := buf.Pop()
	nt, ok := popped.(*transaction)
	if !ok {
		t.Fatalf("popped element is not *transaction: %T", popped)
	}
	if nt.action != InsertDemotionEntry {
		t.Errorf("expected action=InsertDemotionEntry, got %d", nt.action)
	}
	if nt.bankID != 2 {
		t.Errorf("expected new bankID=2, got %d", nt.bankID)
	}
	if !nt.fromLocal {
		t.Errorf("expected fromLocal=true on the produced transaction, got false")
	}
	if nt.block == nil {
		t.Fatalf("produced transaction has nil block")
	}
	if got := len(nt.block.SubEntry); got != 4 {
		t.Fatalf("expected 4 sub-entries, got %d", got)
	}
	return nt.block
}

// Case 1: fromLocal=false with invalidationList=[A,B]
//
//	trigger sub-entry → Sharer == [owner], IsValid=true
//	non-trigger sub-entries → Sharer == [A,B], IsValid=true
func TestInsertDemotion_Sharer_RemoteWithList(t *testing.T) {
	s, _ := newTestMshrStage(false)

	owner := sim.RemotePort("OWNER.RDMA")
	a := sim.RemotePort("GPU_A.RDMA")
	b := sim.RemotePort("GPU_B.RDMA")

	trans := buildDemotionTrans(owner)
	trans.fromLocal = false
	trans.invalidationList = []sim.RemotePort{a, b}

	s.demotionQueue = []*transaction{trans}
	if !s.insertDemotionEntry() {
		t.Fatal("insertDemotionEntry returned false")
	}

	blk := popDemotedBlock(t, s)

	// Trigger sub-entry (i=0)
	trig := blk.SubEntry[0]
	if !trig.IsValid {
		t.Errorf("trigger sub-entry: expected IsValid=true, got false")
	}
	wantTrig := []sim.RemotePort{owner}
	if !reflect.DeepEqual(trig.Sharer, wantTrig) {
		t.Errorf("trigger sub-entry: Sharer = %v, want %v", trig.Sharer, wantTrig)
	}

	// Non-trigger sub-entries inherit invalidationList
	wantOther := []sim.RemotePort{a, b}
	for i := 1; i < 4; i++ {
		sub := blk.SubEntry[i]
		if !sub.IsValid {
			t.Errorf("sub-entry[%d]: expected IsValid=true, got false", i)
		}
		if !reflect.DeepEqual(sub.Sharer, wantOther) {
			t.Errorf("sub-entry[%d]: Sharer = %v, want %v", i, sub.Sharer, wantOther)
		}
	}
}

// Case 2: fromLocal=true with invalidationList=[A,B]
//
//	trigger sub-entry → IsValid=true (CBF patch §3.2 symmetrised this; the
//	  old IsValid=false produced 4-of-3 valid asymmetry that drove CBF
//	  Insert/Evict imbalance and ~100% FPR), Sharer nil/empty
//	non-trigger sub-entries → Sharer == [A,B], IsValid=true
func TestInsertDemotion_Sharer_LocalWithList(t *testing.T) {
	s, _ := newTestMshrStage(false)

	owner := sim.RemotePort("OWNER.RDMA")
	a := sim.RemotePort("GPU_A.RDMA")
	b := sim.RemotePort("GPU_B.RDMA")

	trans := buildDemotionTrans(owner)
	trans.fromLocal = true
	trans.invalidationList = []sim.RemotePort{a, b}

	s.demotionQueue = []*transaction{trans}
	if !s.insertDemotionEntry() {
		t.Fatal("insertDemotionEntry returned false")
	}

	blk := popDemotedBlock(t, s)

	trig := blk.SubEntry[0]
	if !trig.IsValid {
		t.Errorf("trigger sub-entry (local): expected IsValid=true, got false")
	}
	if len(trig.Sharer) != 0 {
		t.Errorf("trigger sub-entry (local): expected empty Sharer, got %v", trig.Sharer)
	}

	wantOther := []sim.RemotePort{a, b}
	for i := 1; i < 4; i++ {
		sub := blk.SubEntry[i]
		if !sub.IsValid {
			t.Errorf("sub-entry[%d]: expected IsValid=true, got false", i)
		}
		if !reflect.DeepEqual(sub.Sharer, wantOther) {
			t.Errorf("sub-entry[%d]: Sharer = %v, want %v", i, sub.Sharer, wantOther)
		}
	}
}

// Case 3: fromLocal=false with empty invalidationList
//
//	trigger sub-entry → Sharer == [owner]
//	non-trigger sub-entries → Sharer length 0, IsValid=true
func TestInsertDemotion_Sharer_RemoteEmptyList(t *testing.T) {
	s, _ := newTestMshrStage(false)

	owner := sim.RemotePort("OWNER.RDMA")

	trans := buildDemotionTrans(owner)
	trans.fromLocal = false
	trans.invalidationList = []sim.RemotePort{}

	s.demotionQueue = []*transaction{trans}
	if !s.insertDemotionEntry() {
		t.Fatal("insertDemotionEntry returned false")
	}

	blk := popDemotedBlock(t, s)

	trig := blk.SubEntry[0]
	if !trig.IsValid {
		t.Errorf("trigger sub-entry: expected IsValid=true, got false")
	}
	wantTrig := []sim.RemotePort{owner}
	if !reflect.DeepEqual(trig.Sharer, wantTrig) {
		t.Errorf("trigger sub-entry: Sharer = %v, want %v", trig.Sharer, wantTrig)
	}

	for i := 1; i < 4; i++ {
		sub := blk.SubEntry[i]
		if !sub.IsValid {
			t.Errorf("sub-entry[%d]: expected IsValid=true, got false", i)
		}
		if len(sub.Sharer) != 0 {
			t.Errorf("sub-entry[%d]: expected empty Sharer, got %v", i, sub.Sharer)
		}
	}
}

// Case 4: invalidationList must not alias the produced Sharer slices.
//
//	After insertDemotionEntry returns, mutating trans.invalidationList must
//	leave every produced SubEntry.Sharer untouched. This guards the
//	copiedSh := make+copy pattern at mshrstage.go:596-599.
func TestInsertDemotion_Sharer_NoAlias(t *testing.T) {
	s, _ := newTestMshrStage(false)

	owner := sim.RemotePort("OWNER.RDMA")
	a := sim.RemotePort("GPU_A.RDMA")
	b := sim.RemotePort("GPU_B.RDMA")

	trans := buildDemotionTrans(owner)
	trans.fromLocal = false
	trans.invalidationList = []sim.RemotePort{a, b}

	s.demotionQueue = []*transaction{trans}
	if !s.insertDemotionEntry() {
		t.Fatal("insertDemotionEntry returned false")
	}
	blk := popDemotedBlock(t, s)

	// Snapshot before mutation
	beforeSnap := make([][]sim.RemotePort, 4)
	for i := range blk.SubEntry {
		beforeSnap[i] = append([]sim.RemotePort(nil), blk.SubEntry[i].Sharer...)
	}

	// Mutate the source list: both rewrite and grow.
	trans.invalidationList[0] = sim.RemotePort("MUTATED")
	trans.invalidationList = append(trans.invalidationList, sim.RemotePort("EXTRA"))

	for i := range blk.SubEntry {
		got := blk.SubEntry[i].Sharer
		if !reflect.DeepEqual(got, beforeSnap[i]) {
			t.Errorf("sub-entry[%d].Sharer changed after mutating invalidationList: before=%v, after=%v",
				i, beforeSnap[i], got)
		}
	}
}
