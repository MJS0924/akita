package writebackcoh

// Cross-GPU writeBuffer/MSHR symmetric deadlock reproduction.
//
// Reproduces the historical stencil2d REC hang (memory: project_sd_fe_deadlock,
// "cross-GPU writeBuffer-MSHR cycle"): two GPUs' L2 caches each evict dirty
// lines homed on the PEER (remote eviction) while also fetching peer-homed
// lines (filling MSHR). With small caps the two saturate symmetrically:
//   GPU_A writeBuffer full of remote evictions to B, waiting for B's WriteDoneRsp
//   <-> B can't serve A's eviction because B's dirStage is blocked (MSHR full /
//   bankBuf full) <-> symmetric. Engine reaches "No More Event" = deadlock.
//
// No RDMA/dir is wired: the L2's toLocal() classifies an address as REMOTE iff
// its bottom-mapper port name contains "RDMA", so each L2 is named "...RDMA..."
// and the peer's RemoteTop is the remote target. bottomPort <-> peer RemoteTop
// is a directconnection; responses route back by Dst (= req.Src).

import (
	"testing"

	"github.com/sarchlab/akita/v4/mem/idealmemcontroller"
	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/akita/v4/sim/directconnection"
)

// agent owns a port to drive an L2's Top; Tick is a no-op (driven from the test).
type agent struct {
	*sim.TickingComponent
	port sim.Port
}

func (a *agent) Tick() bool { return false }

func newAgent(name string, engine sim.Engine, numReq int) *agent {
	a := &agent{}
	a.TickingComponent = sim.NewTickingComponent(name, engine, 1*sim.GHz, a)
	a.port = sim.NewPort(a, numReq*4, numReq*4, name+".Port")
	a.AddPort("Port", a.port)
	return a
}

func buildL2(name string, engine sim.Engine, mapper mem.AddressToPortMapper) *Comp {
	// Small cache + small caps so the symmetric cycle saturates fast.
	// 4-way, blockSize 64, 8 sets => 32 lines total; way=4 => heavy eviction.
	return MakeBuilder().
		WithEngine(engine).
		WithWayAssociativity(4).
		WithLog2BlockSize(6).
		WithByteSize(8 * 4 * 64). // 8 sets * 4 ways * 64B
		WithNumMSHREntry(2).
		WithMaxInflightEviction(2).
		WithMaxInflightFetch(4).
		WithNumReqPerCycle(1).
		WithAddressToPortMapper(mapper).
		Build(name)
}

func TestCrossGPUWriteBufferDeadlock(t *testing.T) {
	// WIP: the 2-L2 short-circuit drives the real writebackcoh L2 across a
	// shared directconnection, but does not yet faithfully reach the
	// writeBuffer-eviction capacity cycle (the L2 quiesces after the first
	// op with writeBuffer/MSHR empty — an akita port-flow/driving artifact,
	// not the target deadlock). It also cannot cover the dir/RDMA
	// completion lane. Faithful validation is the stencil2d REC sim. Kept
	// for future hardening of the L2-layer reservation fix.
	t.Skip("WIP harness: does not yet reproduce the writeBuffer capacity cycle")
	engine := sim.NewSerialEngine()

	const half = uint64(0x40000) // A owns [0,half), B owns [half, 2*half)

	dramA := idealmemcontroller.MakeBuilder().WithEngine(engine).
		WithNewStorage(uint64(2) * half).WithFreq(1 * sim.GHz).WithLatency(100).
		Build("DramA")
	dramB := idealmemcontroller.MakeBuilder().WithEngine(engine).
		WithNewStorage(uint64(2) * half).WithFreq(1 * sim.GHz).WithLatency(100).
		Build("DramB")

	// Bottom mapper: local addr -> own DRAM; remote addr -> peer RemoteTop.
	// remoteTopPort's real name is Name()+".ToRemoteTop" (see createPorts);
	// it contains "RDMA" so toLocal() classifies peer addresses as remote.
	mapA := &mem.L2BottomMapper{
		LocalBank: dramA.GetPortByName("Top").AsRemote(),
		RDMAPort:  sim.RemotePort("RDMAL2B.ToRemoteTop"),
		LocalLow:  0, LocalHigh: half,
	}
	mapB := &mem.L2BottomMapper{
		LocalBank: dramB.GetPortByName("Top").AsRemote(),
		RDMAPort:  sim.RemotePort("RDMAL2A.ToRemoteTop"),
		LocalLow:  half, LocalHigh: 2 * half,
	}

	l2A := buildL2("RDMAL2A", engine, mapA)
	l2B := buildL2("RDMAL2B", engine, mapB)

	agentA := newAgent("AgentA", engine, 64)
	agentB := newAgent("AgentB", engine, 64)

	// Single mem-side medium: directconnection routes by Dst name, so all
	// cross-GPU ports (both L2 Bottom + RemoteTop + both DRAM) share one
	// connection. This stands in for the dir/RDMA fabric that would route
	// remoteTopPort-originated evictions in the real topology.
	connMem := directconnection.MakeBuilder().WithEngine(engine).WithFreq(1 * sim.GHz).Build("ConnMem")
	connMem.PlugIn(l2A.GetPortByName("Bottom"))
	connMem.PlugIn(l2A.GetPortByName("RemoteTop"))
	connMem.PlugIn(l2B.GetPortByName("Bottom"))
	connMem.PlugIn(l2B.GetPortByName("RemoteTop"))
	connMem.PlugIn(dramA.GetPortByName("Top"))
	connMem.PlugIn(dramB.GetPortByName("Top"))

	connTopA := directconnection.MakeBuilder().WithEngine(engine).WithFreq(1 * sim.GHz).Build("ConnTopA")
	connTopA.PlugIn(l2A.GetPortByName("Top"))
	connTopA.PlugIn(agentA.port)

	connTopB := directconnection.MakeBuilder().WithEngine(engine).WithFreq(1 * sim.GHz).Build("ConnTopB")
	connTopB.PlugIn(l2B.GetPortByName("Top"))
	connTopB.PlugIn(agentB.port)

	// Build the symmetric request streams: each agent writes (dirty) + reads
	// PEER-homed addresses across a few sets -> forces remote evictions + remote
	// fetches. blockSize=64; addresses step by 64; cycle across 8 sets so writes
	// keep evicting dirty remote lines.
	mkReqs := func(remoteBase uint64) []mem.AccessReq {
		var reqs []mem.AccessReq
		for i := 0; i < 200; i++ {
			// span 32 distinct lines across 8 sets -> repeated eviction
			addr := remoteBase + uint64(i%32)*64 + uint64(i/32)*64*8
			if i%2 == 0 {
				reqs = append(reqs, mem.WriteReqBuilder{}.
					WithAddress(addr).WithData(make([]byte, 64)).Build())
			} else {
				reqs = append(reqs, mem.ReadReqBuilder{}.
					WithAddress(addr).WithByteSize(64).Build())
			}
		}
		return reqs
	}
	reqsA := mkReqs(half)    // A targets B's range (remote)
	reqsB := mkReqs(0)       // B targets A's range (remote)

	idxA, idxB, recvA, recvB := 0, 0, 0, 0

	topA := l2A.GetPortByName("Top")
	topB := l2B.GetPortByName("Top")
	deadlock := false
	for round := 0; round < 20000; round++ {
		// Deliver straight into each L2's Top incoming buffer (drained to
		// quiescence each round). 1 per round per L2 keeps within the
		// small Top capacity while letting the engine process between.
		if idxA < len(reqsA) {
			r := reqsA[idxA]
			r.Meta().Src = agentA.port.AsRemote()
			r.Meta().Dst = topA.AsRemote()
			topA.Deliver(r)
			idxA++
		}
		if idxB < len(reqsB) {
			r := reqsB[idxB]
			r.Meta().Src = agentB.port.AsRemote()
			r.Meta().Dst = topB.AsRemote()
			topB.Deliver(r)
			idxB++
		}
		// Direct Deliver bypasses the connection's TickNow, so wake the
		// L2s explicitly (cross-GPU traffic is woken by connMem).
		l2A.TickLater()
		l2B.TickLater()

		engine.Run()

		// drain responses
		progressed := false
		for agentA.port.RetrieveIncoming() != nil {
			recvA++
			progressed = true
		}
		for agentB.port.RetrieveIncoming() != nil {
			recvB++
			progressed = true
		}

		allSent := idxA == len(reqsA) && idxB == len(reqsB)
		allRecv := recvA >= len(reqsA) && recvB >= len(reqsB)
		if allSent && allRecv {
			break // completed without deadlock
		}
		if allSent && !progressed {
			// nothing left to inject and engine made no progress -> deadlock
			deadlock = true
			break
		}
	}

	dump := func(tag string, c *Comp) {
		wb := c.writeBuffer
		t.Logf("%s wB: remoteInflight=%d (own=%d/%d peer=%d/%d) pendingRemote=%d peerIncoming=%d mshrFull=%v lastAdmitFail=%q",
			tag, wb.numRemoteInflightEviction,
			wb.numRemoteInflEvictOwn, wb.maxRemoteInflEvictOwn,
			wb.numRemoteInflEvictPeer, wb.maxRemoteInflEvictPeer,
			wb.pendingRemoteEvictionsLen(), wb.numPeerIncomingPending,
			c.mshr.IsFull(), wb.lastAdmitFailReason)
	}
	t.Logf("sentA=%d/%d recvA=%d  sentB=%d/%d recvB=%d", idxA, len(reqsA), recvA, idxB, len(reqsB), recvB)
	dump("A", l2A)
	dump("B", l2B)

	if !deadlock {
		t.Fatalf("expected cross-GPU writeBuffer deadlock, but sims drained/completed")
	}
	t.Logf("DEADLOCK reproduced: engine reached No-More-Event with requests outstanding")
}
