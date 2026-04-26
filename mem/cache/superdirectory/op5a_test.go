package superdirectory

import (
	"testing"

	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/sim"
)

// TestSuperdirectory_OP5a_LocalWrite_SoleRemoteSharer_NoStaleData exposes the
// OP5a deviation in writePermission for local writes. Same scenario as the
// REC and optdirectory variants: a remote GPU previously read the line, the
// home GPU now issues a local write, and the directory must invalidate the
// remote sharer's stale L2 copy.
//
// superdirectory's writePermission is bank-agnostic (no bankID parameter),
// so the same code path applies to fine-bank (numBanks-1) and coarse-bank
// (any other bank) hits. Both bank-typical scenarios are exercised below
// per the user's PHASE C-2 brief; they share the same writePermission call
// and therefore produce the same outcome — verifying that the fix lands
// uniformly across the bank hierarchy.
func TestSuperdirectory_OP5a_LocalWrite_SoleRemoteSharer_NoStaleData(t *testing.T) {
	cases := []struct {
		name   string
		bankID int // 0 = finest in this codebase per directory.GetBanks() convention
	}{
		{name: "FineBank_bankID0", bankID: 0},
		{name: "CoarseBank_bankID2", bankID: 2},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ds := &directoryStage{cache: &Comp{}}

			trans := &transaction{
				fromLocal: true,
				bankID:    tc.bankID,
				write: mem.WriteReqBuilder{}.
					WithAddress(0x1000).
					WithSrc(sim.RemotePort("L1V.Bottom")).
					WithSrcRDMA(sim.RemotePort("")).
					Build(),
			}

			var remoteSharer sim.RemotePort = "GPU1.RDMA"
			sharer := []sim.RemotePort{remoteSharer}

			if ds.writePermission(trans, sharer) {
				t.Fatalf("OP5a deviation (bankID=%d): writePermission "+
					"returned true for local write with sole remote "+
					"sharer (%s); expected false to force invalidation",
					tc.bankID, remoteSharer)
			}
		})
	}
}

// TestSuperdirectory_OP5a_LocalWrite_NoSharers_NoInvalidation guardrail.
func TestSuperdirectory_OP5a_LocalWrite_NoSharers_NoInvalidation(t *testing.T) {
	ds := &directoryStage{cache: &Comp{}}

	trans := &transaction{
		fromLocal: true,
		bankID:    0,
		write: mem.WriteReqBuilder{}.
			WithAddress(0x1000).
			WithSrc(sim.RemotePort("L1V.Bottom")).
			WithSrcRDMA(sim.RemotePort("")).
			Build(),
	}

	if !ds.writePermission(trans, nil) {
		t.Errorf("guardrail: writePermission returned false for local write " +
			"with zero sharers; expected true (nothing to invalidate)")
	}
}

// TestSuperdirectory_OP5a_LocalWrite_MultipleSharers_Invalidation regression
// guard for the existing-correct multi-sharer branch.
func TestSuperdirectory_OP5a_LocalWrite_MultipleSharers_Invalidation(t *testing.T) {
	ds := &directoryStage{cache: &Comp{}}

	trans := &transaction{
		fromLocal: true,
		bankID:    0,
		write: mem.WriteReqBuilder{}.
			WithAddress(0x1000).
			WithSrc(sim.RemotePort("L1V.Bottom")).
			WithSrcRDMA(sim.RemotePort("")).
			Build(),
	}

	sharer := []sim.RemotePort{"GPU1.RDMA", "GPU2.RDMA"}

	if ds.writePermission(trans, sharer) {
		t.Errorf("regression: writePermission returned true for local " +
			"write with multiple sharers; expected false")
	}
}
