package REC

import (
	"testing"

	"github.com/sarchlab/akita/v4/mem/cache/REC/internal"
	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/sim"
)

// TestREC_OP5a_LocalWrite_SoleRemoteSharer_NoStaleData exposes the OP5a
// deviation in writePermission for local writes.
//
// Scenario: a remote GPU previously read a line, so the directory's sub-entry
// holds exactly one sharer = that remote GPU. Now the home GPU issues a local
// write to the same line. Per REC paper §4.2 ("Local writes … if found,
// invalidations are propagated to the recorded sharers in the background"),
// writePermission must return false so doWriteHit emits an
// InvalidateAndUpdateEntry, ensuring the remote sharer's L2 copy is
// invalidated.
//
// The pre-fix REC implementation took a "len(sharer) <= 1 → return true"
// shortcut that skipped the invalidation, leaving stale data on the remote
// sharer. This test must FAIL pre-fix and PASS post-fix.
func TestREC_OP5a_LocalWrite_SoleRemoteSharer_NoStaleData(t *testing.T) {
	ds := &directoryStage{cache: &Comp{}}

	// Local write: home GPU is the writer; SrcRDMA empty (no RDMA hop).
	trans := &transaction{
		fromLocal: true,
		write: mem.WriteReqBuilder{}.
			WithAddress(0x1000).
			WithSrcRDMA(sim.RemotePort("")).
			Build(),
	}

	// One pre-existing remote sharer.
	var remoteSharer sim.RemotePort = "GPU1.RDMA"
	sharer := []sim.RemotePort{remoteSharer}

	if ds.writePermission(trans, sharer) {
		t.Fatalf("OP5a deviation: writePermission returned true for local "+
			"write with sole remote sharer (%s); expected false to force "+
			"invalidation per REC paper §4.2", remoteSharer)
	}
}

// TestREC_OP5a_LocalWrite_NoSharers_NoInvalidation is a guardrail for the
// fix: local writes with zero sharers must still take the no-invalidation
// path (no remote GPU holds the line, so no message needed).
func TestREC_OP5a_LocalWrite_NoSharers_NoInvalidation(t *testing.T) {
	ds := &directoryStage{cache: &Comp{}}

	trans := &transaction{
		fromLocal: true,
		write: mem.WriteReqBuilder{}.
			WithAddress(0x1000).
			WithSrcRDMA(sim.RemotePort("")).
			Build(),
	}

	if !ds.writePermission(trans, nil) {
		t.Errorf("guardrail: writePermission returned false for local write "+
			"with zero sharers; expected true (nothing to invalidate)")
	}
}

// TestREC_OP5a_LocalWrite_MultipleSharers_Invalidation is the existing-correct
// behaviour preserved: multiple sharers must always force invalidation
// (this branch was already correct pre-fix).
func TestREC_OP5a_LocalWrite_MultipleSharers_Invalidation(t *testing.T) {
	ds := &directoryStage{cache: &Comp{}}

	trans := &transaction{
		fromLocal: true,
		write: mem.WriteReqBuilder{}.
			WithAddress(0x1000).
			WithSrcRDMA(sim.RemotePort("")).
			Build(),
	}

	sharer := []sim.RemotePort{"GPU1.RDMA", "GPU2.RDMA"}

	if ds.writePermission(trans, sharer) {
		t.Errorf("regression: writePermission returned true for local write "+
			"with multiple sharers; expected false")
	}
}

// internalImportGuard keeps the internal import in scope for forthcoming
// fix-companion tests that walk into bankstage. Removing this requires also
// removing the import.
var internalImportGuard *internal.CohEntry
