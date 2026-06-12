package superdirectory

import (
	"testing"

	"github.com/sarchlab/akita/v4/mem/cache/superdirectory/internal"
	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/sim"
)

// TestSuperdirectory_OP5b_FinestBankWrite_WriterSurvives verifies that
// superdirectory's coarse-bank-hit demotion is INTENTIONAL design,
// NOT an OP5b deviation. See design_document.md §3.3.4
// "Write-Invalidation 으로 인한 비-typical demotion" (line 236) and §3.3.3
// "Demotion (강등)" (line 203). Future changes that align this with REC's
// OP5b semantics MUST update this test and the design document together.
//
// At the finest bank (bankID == numBanks-1), the InvalidateAndUpdateEntry
// handler's Path A executes:
//   - sets entry.Sharer = [writer] (writer becomes sole sharer)
//   - keeps entry.IsValid = true
//   - keeps blk.IsValid = true
//   - sets needToDemotion = false
//
// This matches REC paper §4.2 OP5b semantics for the writer-survives case.
//
// At a coarser bank (bankID < numBanks-1) with non-empty sharers,
// Path B executes:
//   - clears entry.Sharer entirely
//   - sets entry.IsValid = false
//   - sets needToDemotion = true
//
// This intentionally clobbers the writer's bit at the coarse-bank entry
// because superdirectory will subsequently demote the entry to the
// finer bank where the writer is recorded with full fidelity. The
// design contract is documented in design_document.md §3.3.4 line 238:
// "Remote write 발생 시 해당 sub-entry 의 sharer 를 업데이트하되, 다른
// sub-entry 는 유지." The current code's Path B over-clears compared to
// that wording, but the subsequent EvictAndDemotionEntry handler
// (bankstage.go FinalizeDemotionEntry, line 407) reallocates the entry
// at the finer bank with the writer's record intact — so the
// observable end-to-end behavior matches the design contract.
//
// This test does NOT change any production code. It is an intent test
// that locks in the documented behavior so a future cross-comparison of
// "OP5b deviation counts" across the three models can interpret a
// non-zero superdirectory writer-cleared count as protocol-intentional
// rather than a correctness regression.
func TestSuperdirectory_OP5b_FinestBankWrite_WriterSurvives(t *testing.T) {
	const numBanks = 5
	bs := &bankStage{
		cache: &Comp{
			numBanks:      numBanks,
			log2BlockSize: 6,
		},
	}

	bottomBuf := sim.NewBuffer("Test.Bottom", 32)
	mshrBuf := sim.NewBuffer("Test.MSHR", 32)

	var writer sim.RemotePort = "GPU2.RDMA"
	var otherSharer sim.RemotePort = "GPU1.RDMA"

	// SubEntry is a slice in superdirectory (vs REC's fixed array); allocate it.
	blk := &internal.CohEntry{
		IsValid:  true,
		Tag:      0x1000,
		PID:      1,
		SubEntry: make([]internal.CohSubEntry, 16),
	}
	blk.SubEntry[0].IsValid = true
	blk.SubEntry[0].Sharer = []sim.RemotePort{otherSharer, writer}

	trans := &transaction{
		bankID:   numBanks - 1, // finest bank → Path A
		block:    blk,
		blockIdx: 0,
		write: mem.WriteReqBuilder{}.
			WithAddress(0x1000).
			WithSrcRDMA(writer).
			Build(),
	}

	if !bs.InvalidateAndUpdateEntry(trans, bottomBuf, mshrBuf) {
		t.Fatal("InvalidateAndUpdateEntry returned false")
	}

	if !blk.SubEntry[0].IsValid {
		t.Error("finest bank Path A: SubEntry[0].IsValid cleared; expected preserved")
	}
	if !blk.IsValid {
		t.Error("finest bank Path A: blk.IsValid cleared; expected preserved")
	}
	if got := blk.SubEntry[0].Sharer; len(got) != 1 || got[0] != writer {
		t.Errorf("finest bank Path A: SubEntry[0].Sharer = %v; expected [%s] (writer becomes sole sharer)",
			got, writer)
	}
	if trans.needToDemotion {
		t.Error("finest bank Path A: needToDemotion = true; expected false")
	}
}

// TestSuperdirectory_OP5b_CoarseBankWrite_DemoteByDesign verifies the
// Path B (coarser bank) intentional demotion behavior. See same design
// doc references as above (design_document.md §3.3.3 line 203 demotion
// criteria; §3.3.4 line 236 write-triggered demotion).
//
// At a coarser bank with non-empty sharers, Path B clears the
// sub-entry and flags needToDemotion = true so the directory pipeline
// can re-allocate the entry at a finer bank. The entry's writer record
// is recovered via the demotion-completion path (FinalizeDemotionEntry).
func TestSuperdirectory_OP5b_CoarseBankWrite_DemoteByDesign(t *testing.T) {
	const numBanks = 5
	bs := &bankStage{
		cache: &Comp{
			numBanks:      numBanks,
			log2BlockSize: 6,
		},
	}

	bottomBuf := sim.NewBuffer("Test.Bottom", 32)
	mshrBuf := sim.NewBuffer("Test.MSHR", 32)

	var writer sim.RemotePort = "GPU2.RDMA"
	var otherSharer sim.RemotePort = "GPU1.RDMA"

	// SubEntry is a slice in superdirectory (vs REC's fixed array); allocate it.
	blk := &internal.CohEntry{
		IsValid:  true,
		Tag:      0x1000,
		PID:      1,
		SubEntry: make([]internal.CohSubEntry, 16),
	}
	blk.SubEntry[0].IsValid = true
	blk.SubEntry[0].Sharer = []sim.RemotePort{otherSharer, writer}

	trans := &transaction{
		bankID:   0, // coarsest bank → Path B
		block:    blk,
		blockIdx: 0,
		write: mem.WriteReqBuilder{}.
			WithAddress(0x1000).
			WithSrcRDMA(writer).
			Build(),
	}

	if !bs.InvalidateAndUpdateEntry(trans, bottomBuf, mshrBuf) {
		t.Fatal("InvalidateAndUpdateEntry returned false")
	}

	if blk.SubEntry[0].IsValid {
		t.Error("coarse bank Path B: SubEntry[0].IsValid set; expected cleared (entry will be demoted)")
	}
	if len(blk.SubEntry[0].Sharer) != 0 {
		t.Errorf("coarse bank Path B: SubEntry[0].Sharer = %v; expected empty (entry will be demoted)",
			blk.SubEntry[0].Sharer)
	}
	if !trans.needToDemotion {
		t.Error("coarse bank Path B: needToDemotion = false; expected true (subsequent demotion is the intentional protocol path per design_document.md §3.3.4)")
	}
}
