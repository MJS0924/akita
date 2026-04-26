package optdirectory

import (
	"testing"

	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/sim"
)

// TestOptDirectory_OP5a_LocalWrite_SoleRemoteSharer_NoStaleData exposes the
// OP5a deviation in writePermission for local writes. Same scenario, same
// expected outcome as the REC version — the optdirectory flavor backs
// CoherenceDirectory (model id 0), LargeBlockCache (id 1), and HMG (id 4),
// so this fix simultaneously closes OP5a in CD, LBC, and HMG.
//
// optdirectory's writePermission uses isFromLocal which inspects
// req.Meta().Src for the string "RDMA"; setting Src to a non-RDMA value
// reproduces the local-write code path that previously took the
// "len(sharer) <= 1 → return true" shortcut and silently dropped the
// invalidation message to the lone remote sharer.
func TestOptDirectory_OP5a_LocalWrite_SoleRemoteSharer_NoStaleData(t *testing.T) {
	ds := &directoryStage{cache: &Comp{}}

	// Local write: Src has no "RDMA" substring → isFromLocal returns true.
	trans := &transaction{
		fromLocal: true,
		write: mem.WriteReqBuilder{}.
			WithAddress(0x1000).
			WithSrc(sim.RemotePort("L1V.Bottom")).
			WithSrcRDMA(sim.RemotePort("")).
			Build(),
	}

	var remoteSharer sim.RemotePort = "GPU1.RDMA"
	sharer := []sim.RemotePort{remoteSharer}

	if ds.writePermission(trans, sharer) {
		t.Fatalf("OP5a deviation: writePermission returned true for local " +
			"write with sole remote sharer (GPU1.RDMA); expected false to " +
			"force invalidation per HMG/CD baseline coherence protocol")
	}
}

// TestOptDirectory_OP5a_LocalWrite_NoSharers_NoInvalidation guardrail:
// local write with no recorded sharers must still take the no-inv path.
func TestOptDirectory_OP5a_LocalWrite_NoSharers_NoInvalidation(t *testing.T) {
	ds := &directoryStage{cache: &Comp{}}

	trans := &transaction{
		fromLocal: true,
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

// TestOptDirectory_OP5a_LocalWrite_MultipleSharers_Invalidation is the
// existing-correct behaviour: multi-sharer case must continue to force
// invalidation.
func TestOptDirectory_OP5a_LocalWrite_MultipleSharers_Invalidation(t *testing.T) {
	ds := &directoryStage{cache: &Comp{}}

	trans := &transaction{
		fromLocal: true,
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

// TestOptDirectory_OP5a_HMGVariant_SameFix is the variant micro-test required
// by the user's brief: HMG (model id 4) shares optdirectory's writePermission,
// so the fix simultaneously applies. There is no separate HMG configuration
// flag in writePermission itself; the variant micro-test confirms the
// cross-config invariant by exercising the same code path with HMG-typical
// coalescing config (log2NumSubEntry > 0 in the wider system, but this
// test isolates writePermission which is config-agnostic).
func TestOptDirectory_OP5a_HMGVariant_SameFix(t *testing.T) {
	ds := &directoryStage{cache: &Comp{}}

	trans := &transaction{
		fromLocal: true,
		write: mem.WriteReqBuilder{}.
			WithAddress(0x2000).
			WithSrc(sim.RemotePort("HMG.L1V.Bottom")).
			WithSrcRDMA(sim.RemotePort("")).
			Build(),
	}

	if ds.writePermission(trans, []sim.RemotePort{"GPU3.RDMA"}) {
		t.Fatalf("OP5a deviation under HMG-typical Src naming: writePermission " +
			"returned true; expected false")
	}
}
