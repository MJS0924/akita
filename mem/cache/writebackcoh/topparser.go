package writebackcoh

import (
	"fmt"
	"log"

	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/akita/v4/tracing"
)

// Bounded typed-queue caps. These mirror the downstream ingress buffer
// capacities so total cache buffer growth is minimal — the queues exist
// only to break head-of-line blocking between InvReq and WriteReq/ReadReq
// at remoteTopPort, not to add inflight parallelism. Cycle/invalidation
// costs remain unaffected: each dispatched message still goes through
// invStageBuffer / remoteDirStageBuffer with the original pipeline.
const (
	topParserPendingRemoteInvCap    = 16 // matches invStageBuffer cap (numReqPerCycle*4)
	topParserPendingRemoteAccessCap = 4  // matches remoteDirStageBuffer cap (numReqPerCycle)
)

type topParser struct {
	cache *Comp

	// Type-classified pending queues drained from remoteTopPort each
	// Tick. Without these, an InvReq behind a WriteReq at port head
	// gets stuck whenever remoteDirStageBuffer is full (which happens
	// during write-through bursts that back up the bank pipeline).
	// Stuck InvReq → CD's inflightInv cap → cross-GPU cyclic deadlock
	// observed under CD coherence-unit-size=0 (cache-line granularity
	// invalidation generates N× more InvReq than coarser variants).
	pendingRemoteInv    []*remoteInvPending
	pendingRemoteAccess []*remoteAccessPending
}

type remoteInvPending struct {
	req *mem.InvReq
}

type remoteAccessPending struct {
	req sim.Msg // *mem.ReadReq or *mem.WriteReq
}

func (p *topParser) Tick() bool {
	if p.cache.state != cacheStateRunning {
		return false
	}

	madeProgress := false

	// 1) Drain remoteTopPort head into typed pending queues (1 per Tick).
	//    Cycle cost: queueing latency is unchanged (the message would
	//    have sat at port head anyway); we just sort it by type so
	//    downstream priority works without HoL.
	if p.drainRemoteOne() {
		madeProgress = true
	}

	// 2) Dispatch one typed pending item to its downstream buffer.
	//    Priority: InvReq > Access. Inv dispatch goes to invStageBuffer
	//    (already prioritized by dirStage). Access goes to
	//    remoteDirStageBuffer.
	if p.dispatchRemoteInv() {
		return true
	}
	if p.dispatchRemoteAccess() {
		return true
	}

	// 3) Local L1 path (topPort). L1 only sends ReadReq/WriteReq, no
	//    InvReq mixed in, so no HoL fix needed here.
	if p.processLocalOne() {
		madeProgress = true
	}

	return madeProgress
}

func (p *topParser) drainRemoteOne() bool {
	msg := p.cache.remoteTopPort.PeekIncoming()
	if msg == nil {
		return false
	}

	switch req := msg.(type) {
	case *mem.InvReq:
		if len(p.pendingRemoteInv) >= topParserPendingRemoteInvCap {
			return false
		}
		p.pendingRemoteInv = append(p.pendingRemoteInv, &remoteInvPending{req: req})
	case *mem.ReadReq, *mem.WriteReq:
		if len(p.pendingRemoteAccess) >= topParserPendingRemoteAccessCap {
			return false
		}
		p.pendingRemoteAccess = append(p.pendingRemoteAccess, &remoteAccessPending{req: msg})
	default:
		log.Panic(fmt.Sprintf("[%s]\nErr: Cannot handle req type from remoteTopPort\n", p.cache.name))
	}

	p.cache.remoteTopPort.RetrieveIncoming()
	return true
}

func (p *topParser) dispatchRemoteInv() bool {
	if len(p.pendingRemoteInv) == 0 {
		return false
	}

	pending := p.pendingRemoteInv[0]
	if !p.cache.invStageBuffer.CanPush() {
		return false
	}

	trans := &transaction{
		id:           sim.GetIDGenerator().Generate(),
		fromLocal:    false,
		invalidation: pending.req,
	}

	p.cache.invStageBuffer.Push(trans)
	p.cache.inFlightTransactions = append(p.cache.inFlightTransactions, trans)
	tracing.TraceReqReceive(pending.req, p.cache)

	p.pendingRemoteInv = p.pendingRemoteInv[1:]
	return true
}

func (p *topParser) dispatchRemoteAccess() bool {
	if len(p.pendingRemoteAccess) == 0 {
		return false
	}

	pending := p.pendingRemoteAccess[0]
	if !p.cache.remoteDirStageBuffer.CanPush() {
		return false
	}

	trans := &transaction{
		id:        sim.GetIDGenerator().Generate(),
		fromLocal: false,
	}

	switch req := pending.req.(type) {
	case *mem.ReadReq:
		trans.toLocal = p.cache.toLocal(req.Address)
		trans.read = req
	case *mem.WriteReq:
		trans.toLocal = p.cache.toLocal(req.Address)
		trans.write = req
	default:
		log.Panic("unexpected type in pendingRemoteAccess")
	}

	p.cache.remoteDirStageBuffer.Push(trans)
	p.cache.inFlightTransactions = append(p.cache.inFlightTransactions, trans)
	tracing.TraceReqReceive(pending.req, p.cache)

	p.pendingRemoteAccess = p.pendingRemoteAccess[1:]
	return true
}

func (p *topParser) processLocalOne() bool {
	msg := p.cache.topPort.PeekIncoming()
	if msg == nil {
		return false
	}

	trans := &transaction{
		id:        sim.GetIDGenerator().Generate(),
		fromLocal: true,
	}

	switch req := msg.(type) {
	case *mem.ReadReq:
		trans.toLocal = p.cache.toLocal(req.Address)
		trans.read = req
	case *mem.WriteReq:
		trans.toLocal = p.cache.toLocal(req.Address)
		trans.write = req
	case *mem.InvReq:
		trans.invalidation = req
	default:
		log.Panic(fmt.Sprintf("[%s]\nErr: Cannot handle req type\n", p.cache.name))
	}

	if p.cache.debugProcess && trans.accessReq() != nil && trans.accessReq().GetAddress() == p.cache.debugAddress0 {
		fmt.Printf("[%s] [topparser]\tReceived req - 0: addr %x\n", p.cache.name, trans.accessReq().GetAddress())
	}
	if p.cache.debugProcess && trans.accessReq() != nil && trans.accessReq().GetAddress() == p.cache.debugAddress1 {
		fmt.Printf("[%s] [topparser]\tReceived req - 0: addr %x\n", p.cache.name, trans.accessReq().GetAddress())
	}

	if trans.invalidation != nil {
		if !p.cache.invStageBuffer.CanPush() {
			return false
		}
		p.cache.invStageBuffer.Push(trans)
	} else {
		if !p.cache.dirStageBuffer.CanPush() {
			return false
		}
		p.cache.dirStageBuffer.Push(trans)
	}

	p.cache.inFlightTransactions = append(p.cache.inFlightTransactions, trans)
	tracing.TraceReqReceive(msg, p.cache)

	p.cache.topPort.RetrieveIncoming()
	return true
}