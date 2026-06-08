package optdirectory

// Invariants that explain why stencil2D's CD variant exhibits a 7.3×
// growth in stall_inflight_fetch from CD_0 to CD_6 even though the
// per-transaction fetch granularity is always exactly one cacheline.
//
// Each test isolates one of the three mechanisms identified by the
// CD unit-size analysis:
//
//   (a) processInvalidationReq unrolls one incoming InvReq into
//       2^log2UnitSize per-cacheline InvReqs onto invRemoteSendToBottomQue.
//   (c) directoryStage.doWrite keys MSHR coalescing by region-aligned
//       cachelineID (log2BlockSize + log2UnitSize), so distinct sub-lines
//       within the same region share one MSHR entry.
//   (d) internal.CohDirectory.setID coarsens with log2UnitSize, mapping
//       2^log2UnitSize neighboring cachelines into the same directory
//       entry — the structural prerequisite for the false-sharing burst
//       observed in stencil2D.
//
// Plus one diagnostic for the doWrite write-HoL guard (hypothesis (b),
// refuted by stencil2D's stall_block_locked == 0 across CD_0..CD_8 but
// the code path itself is still verified to exist), and one diagnostic
// for the inv-egress drain priority in sendToBottom (the downstream
// HoL between inv and remote-fetch on the shared remoteBottomPort).

import (
	"testing"

	"github.com/sarchlab/akita/v4/mem/cache/optdirectory/internal"
	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
)

// --- Test (a): per-line InvReq unroll factor in processInvalidationReq.

func TestInvUnrollFactor_EqualsTwoPowLog2UnitSize(t *testing.T) {
	cases := []struct {
		log2UnitSize uint64
		wantUnroll   int
	}{
		{0, 1},
		{2, 4},
		{6, 64},
		{8, 256},
	}

	for _, tc := range cases {
		bs, _ := newProcessInvalidationReqFixture(t, 6, tc.log2UnitSize)

		ok := bs.processInvalidationReq()
		if !ok {
			t.Fatalf("log2UnitSize=%d: processInvalidationReq returned false",
				tc.log2UnitSize)
		}

		if got := len(bs.invRemoteSendToBottomQue); got != tc.wantUnroll {
			t.Fatalf("log2UnitSize=%d: invRemoteSendToBottomQue len=%d, "+
				"want %d (= 2^log2UnitSize)",
				tc.log2UnitSize, got, tc.wantUnroll)
		}

		// Address step must be one block (per-cacheline expansion), not
		// one region — otherwise the lower module would dedupe by
		// identical address and the inflightInv.ack mismatch deadlock
		// (documented at bottomSender.go:800-807) would not exist.
		const log2BlockSize uint64 = 6
		blockSize := uint64(1) << log2BlockSize
		for i, m := range bs.invRemoteSendToBottomQue {
			inv := m.(*mem.InvReq)
			wantAddr := uint64(i) * blockSize
			if inv.Address != wantAddr {
				t.Fatalf("log2UnitSize=%d sub-inv[%d] addr=%#x, want %#x",
					tc.log2UnitSize, i, inv.Address, wantAddr)
			}
		}
	}
}

// newProcessInvalidationReqFixture builds the minimum Comp + bottomSender
// state required to drive processInvalidationReq once with a single
// region-aligned incoming InvReq.
func newProcessInvalidationReqFixture(
	t *testing.T, log2BlockSize, log2UnitSize uint64,
) (*bottomSender, *mem.InvReq) {
	t.Helper()

	rb := &fakePort{name: "RBP"}
	invBuf := sim.NewBuffer("InvReqBuffer", 4)

	c := &Comp{
		log2BlockSize:                   log2BlockSize,
		log2UnitSize:                    log2UnitSize,
		remoteBottomPort:                rb,
		invReqBuffer:                    invBuf,
		addressToPortMapperForRemoteReq: &mem.SinglePortMapper{Port: sim.RemotePort("L2")},
	}

	bs := &bottomSender{
		cache:                   c,
		maxInflightInvalidation: 1024,
	}

	req := mem.InvReqBuilder{}.
		WithSrc(sim.RemotePort("RDMA_SRC")).
		WithDst(sim.RemotePort("CohDir")).
		WithAddress(0).
		WithPID(vm.PID(1)).
		WithReqFrom("upstream-id").
		Build()
	invBuf.Push(req)

	return bs, req
}

// --- Test (c): region-MSHR coalescing key alignment.
//
// directoryStage.doWrite (line 147) computes the MSHR key as
//   cachelineID = getCacheLineID(addr, log2BlockSize + log2UnitSize)
// so distinct sub-line addresses within the same 2^log2UnitSize region
// collapse to one MSHR entry. We test the key computation directly,
// then assert mshr.Query returns the same entry, mirroring the exact
// query path doWrite takes — without invoking doWrite itself (which
// would require a real *sim.TickingComponent for tracing).

func TestMSHRRegionCoalescing_DiffSubLinesShareEntry(t *testing.T) {
	cases := []uint64{0, 2, 6}

	for _, log2UnitSize := range cases {
		const log2BlockSize uint64 = 6
		regionBytes := uint64(1) << (log2BlockSize + log2UnitSize)
		blockSize := uint64(1) << log2BlockSize

		regionBase := uint64(0)
		probeAddr := regionBase + blockSize
		if log2UnitSize == 0 {
			probeAddr = regionBase // region == block; no distinct sub-line
		}
		if probeAddr >= regionBase+regionBytes {
			t.Fatalf("log2UnitSize=%d: probe addr %#x escapes region "+
				"[%#x, %#x)",
				log2UnitSize, probeAddr, regionBase, regionBase+regionBytes)
		}

		// Both base and probe must produce the same MSHR key.
		baseKey, _ := getCacheLineID(regionBase, log2BlockSize+log2UnitSize)
		probeKey, _ := getCacheLineID(probeAddr, log2BlockSize+log2UnitSize)
		if baseKey != probeKey {
			t.Fatalf("log2UnitSize=%d: cachelineID(base=%#x)=%#x != "+
				"cachelineID(probe=%#x)=%#x — sub-lines in the same "+
				"region must share the MSHR key",
				log2UnitSize, regionBase, baseKey, probeAddr, probeKey)
		}

		// And Query must return the SAME entry instance for either key.
		mshr := internal.NewMSHR(64)
		entry := mshr.Add(vm.PID(1), baseKey)
		if got := mshr.Query(vm.PID(1), probeKey); got != entry {
			t.Fatalf("log2UnitSize=%d: mshr.Query(probeKey=%#x) returned "+
				"a different entry than the one inserted at baseKey=%#x",
				log2UnitSize, probeKey, baseKey)
		}
	}

	// Negative control: addresses in DIFFERENT regions must NOT collapse.
	const log2BlockSize uint64 = 6
	const log2UnitSize uint64 = 2 // region = 4 blocks (256 B)
	regionBytes := uint64(1) << (log2BlockSize + log2UnitSize)
	addrA := uint64(0)
	addrB := regionBytes // first address of the next region
	keyA, _ := getCacheLineID(addrA, log2BlockSize+log2UnitSize)
	keyB, _ := getCacheLineID(addrB, log2BlockSize+log2UnitSize)
	if keyA == keyB {
		t.Fatalf("addresses %#x and %#x straddle region boundary but "+
			"yielded identical cachelineID %#x", addrA, addrB, keyA)
	}
}

// --- Test (b) code-path pinning: the doWrite mshr-hit guard returns
// false for writes (line 151-152) and true for reads (via
// doWriteMSHRHit, line 207-211). The data refutes (b) as a dominant
// mechanism (stall_block_locked == 0 across CD_0..CD_8), but the path
// itself must remain so future refactors don't silently flip the
// behavior. We exercise doWriteMSHRHit directly to avoid the
// tracing.AddTaskStep call in doWrite's outer wrapper.

func TestDoWriteMSHRHit_ReadAppendsToEntryRequests(t *testing.T) {
	mshr := internal.NewMSHR(64)
	entry := mshr.Add(vm.PID(1), 0)
	c := &Comp{
		log2BlockSize: 6,
		log2UnitSize:  2,
		mshr:          mshr,
	}
	ds := &directoryStage{cache: c}
	trans := &transaction{
		read: newReadReq(64, vm.PID(1)),
	}

	if !ds.doWriteMSHRHit(trans, entry) {
		t.Fatalf("doWriteMSHRHit should return true for a read trans")
	}
	if len(entry.Requests) != 1 || entry.Requests[0] != trans {
		t.Fatalf("trans was not appended to mshrEntry.Requests; len=%d",
			len(entry.Requests))
	}
	if trans.mshrEntry != entry {
		t.Fatalf("trans.mshrEntry was not set to the MSHR entry")
	}
}

// --- Test (d): directory setID coarsens with log2UnitSize.

func TestRegionDirectorySetID_CoarsensWithLog2UnitSize(t *testing.T) {
	const blockSize = 64

	tests := []struct {
		log2UnitSize uint64
		addrs        []uint64
		wantSameSet  bool
		desc         string
	}{
		{
			log2UnitSize: 0,
			addrs:        []uint64{0, blockSize, 2 * blockSize},
			wantSameSet:  false,
			desc:         "unit=0: neighboring cachelines map to different sets",
		},
		{
			log2UnitSize: 2,
			addrs:        []uint64{0, blockSize, 3 * blockSize}, // 4-line region
			wantSameSet:  true,
			desc:         "unit=2: 4 neighboring cachelines collapse to one entry",
		},
		{
			log2UnitSize: 6,
			addrs:        []uint64{0, 7 * blockSize, 63 * blockSize}, // 64-line region
			wantSameSet:  true,
			desc:         "unit=6: 64 neighboring cachelines collapse to one entry",
		},
	}

	for _, tc := range tests {
		dir := internal.NewCohDirectory(
			8 /*sets*/, 4 /*ways*/, blockSize, tc.log2UnitSize,
			internal.NewLRUVictimFinder(),
		)
		seen := map[int]struct{}{}
		for _, a := range tc.addrs {
			v := dir.FindVictim(a)
			seen[v.SetID] = struct{}{}
		}
		if tc.wantSameSet && len(seen) != 1 {
			t.Fatalf("%s: addrs %v map to %d distinct sets, want 1",
				tc.desc, tc.addrs, len(seen))
		}
		if !tc.wantSameSet && len(seen) < 2 {
			t.Fatalf("%s: addrs %v map to %d distinct sets, want ≥2",
				tc.desc, tc.addrs, len(seen))
		}
	}
}

// --- Test (a) downstream: inv-egress queue is drained before
// remote-fetch on the shared remoteBottomPort.

func TestSendToBottom_InvEgressDrainsBeforeRemoteFetch(t *testing.T) {
	const log2BlockSize uint64 = 6

	// remoteBottomPort can send exactly 1 message per cycle (sendBudget=1).
	// We seed both invRemoteSendToBottomQue AND remoteSendToBottomQue, then
	// invoke sendToBottom once. The inv head must win the one available
	// port slot; the fetch head must remain queued.
	rb := &fakePort{name: "RBP", sendBudget: 1}
	lb := &fakePort{name: "LBP", sendBudget: 0}

	c := &Comp{
		log2BlockSize:    log2BlockSize,
		log2UnitSize:     6,
		remoteBottomPort: rb,
		bottomPort:       lb,
	}
	bs := &bottomSender{cache: c}

	// One InvReq in the dedicated inv egress queue.
	inv := mem.InvReqBuilder{}.
		WithSrc(rb.AsRemote()).
		WithDst(sim.RemotePort("L2_REMOTE")).
		WithAddress(0).
		WithPID(vm.PID(1)).
		Build()
	bs.invRemoteSendToBottomQue = append(bs.invRemoteSendToBottomQue, inv)

	// One regular remote-fetch in the data queue. Use a ReadReq so the
	// headIsInv branch in sendToBottom does NOT trigger the per-cycle inv
	// budget guard for this entry.
	rd := newReadReq(0x100, vm.PID(1))
	rd.Meta().Src = rb.AsRemote()
	rd.Meta().Dst = sim.RemotePort("L2_REMOTE")
	bs.remoteSendToBottomQue = append(bs.remoteSendToBottomQue, rd)

	bs.sendToBottom()

	if len(bs.invRemoteSendToBottomQue) != 0 {
		t.Fatalf("invRemoteSendToBottomQue should be drained first; "+
			"residual=%d", len(bs.invRemoteSendToBottomQue))
	}
	if len(bs.remoteSendToBottomQue) != 1 {
		t.Fatalf("remoteSendToBottomQue should retain its fetch; "+
			"len=%d", len(bs.remoteSendToBottomQue))
	}
	if len(rb.sent) != 1 {
		t.Fatalf("remoteBottomPort.Send should fire exactly once; "+
			"got %d", len(rb.sent))
	}
	if _, ok := rb.sent[0].(*mem.InvReq); !ok {
		t.Fatalf("the message sent to remoteBottomPort should be the "+
			"InvReq, got %T", rb.sent[0])
	}

	// The fetch lost the CanSend race and must register a port-busy stall.
	if c.stallBottomPortBusy == 0 {
		t.Fatalf("stallBottomPortBusy should have been incremented "+
			"for the blocked remote-fetch")
	}
}

// --- Local helpers.

func newReadReq(addr uint64, pid vm.PID) *mem.ReadReq {
	return mem.ReadReqBuilder{}.
		WithSrc(sim.RemotePort("L1")).
		WithDst(sim.RemotePort("CohDir")).
		WithAddress(addr).
		WithPID(pid).
		WithByteSize(4).
		Build()
}

func newWriteReq(addr uint64, pid vm.PID) *mem.WriteReq {
	return mem.WriteReqBuilder{}.
		WithSrc(sim.RemotePort("L1")).
		WithDst(sim.RemotePort("CohDir")).
		WithAddress(addr).
		WithPID(pid).
		WithData([]byte{0, 0, 0, 0}).
		Build()
}

// fakePort is the minimal sim.Port implementation needed by these tests.
// Only CanSend, Send, AsRemote and Name are exercised; every other method
// either no-ops or returns nil because these unit tests never drive the
// engine, connection, or hook machinery.
type fakePort struct {
	sim.HookableBase
	name       string
	sendBudget int     // CanSend returns true while > 0; Send decrements it
	sent       []sim.Msg
}

func (p *fakePort) Name() string             { return p.name }
func (p *fakePort) AsRemote() sim.RemotePort { return sim.RemotePort(p.name) }

func (p *fakePort) CanSend() bool { return p.sendBudget > 0 }

func (p *fakePort) Send(msg sim.Msg) *sim.SendError {
	if p.sendBudget <= 0 {
		return sim.NewSendError()
	}
	p.sent = append(p.sent, msg)
	p.sendBudget--
	return nil
}

// Stubs — never reached by the tests in this file.
func (p *fakePort) SetConnection(sim.Connection)         {}
func (p *fakePort) Component() sim.Component             { return nil }
func (p *fakePort) Deliver(sim.Msg) *sim.SendError       { return nil }
func (p *fakePort) NotifyAvailable()                     {}
func (p *fakePort) RetrieveOutgoing() sim.Msg            { return nil }
func (p *fakePort) PeekOutgoing() sim.Msg                { return nil }
func (p *fakePort) RetrieveIncoming() sim.Msg            { return nil }
func (p *fakePort) PeekIncoming() sim.Msg                { return nil }
func (p *fakePort) GetIncomingBuf() sim.Buffer           { return nil }
func (p *fakePort) GetOutgoingBuf() sim.Buffer           { return nil }
