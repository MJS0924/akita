package writebackcoh

import (
	"fmt"
	"log"

	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/mempath"
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
	// [L2] Local-side typed queues — mirror the remote-side fix so that
	// an InvReq sitting behind a ReadReq/WriteReq at topPort head (L1
	// originated) cannot HoL-block once dirStageBuffer is full.
	topParserPendingLocalInvCap    = 16
	topParserPendingLocalAccessCap = 4
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
	// [L2] Local-side typed queues for topPort. Symmetric with the
	// remote-side queues above.
	pendingLocalInv    []*localInvPending
	pendingLocalAccess []*localAccessPending
}

type localInvPending struct {
	req *mem.InvReq
}

type localAccessPending struct {
	req sim.Msg // *mem.ReadReq or *mem.WriteReq
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

	// 2) Dispatch one typed pending item per class to its downstream
	//    buffer. Priority WITHIN each class stays InvReq > Access (part of
	//    the documented CD_0 anti-HoL design), but a successful dispatch no
	//    longer early-returns: the old `return true` let a burst of remote
	//    invs consume every subtick and starve local L1 admission entirely
	//    — an emergent parser-scheduling artifact, not a modeled resource.
	//    [INV-FIDELITY C8a] Probe-vs-demand contention is now modeled
	//    explicitly at the dir-stage admission token pool and commit
	//    budget, so the parser just moves one item per class per Tick.
	if p.dispatchRemoteInv() {
		madeProgress = true
	}
	if p.dispatchRemoteAccess() {
		madeProgress = true
	}

	// 3) Local L1 path (topPort). Symmetric typed-queue split so that
	//    InvReq (e.g., L1 self-invalidation) cannot HoL-block behind a
	//    stalled ReadReq/WriteReq.
	if p.drainLocalOne() {
		madeProgress = true
	}
	if p.dispatchLocalInv() {
		madeProgress = true
	}
	if p.dispatchLocalAccess() {
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

	p.cache.stampProbe(trans, mempath.EvL2In)

	p.cache.remoteDirStageBuffer.Push(trans)
	p.cache.inFlightTransactions = append(p.cache.inFlightTransactions, trans)
	tracing.TraceReqReceive(pending.req, p.cache)

	p.pendingRemoteAccess = p.pendingRemoteAccess[1:]
	return true
}

// [L2] drainLocalOne dequeues one head from topPort into the appropriate
// typed pending queue (InvReq vs Read/WriteReq). Cap rejection just
// returns false without RetrieveIncoming so the next Tick retries.
func (p *topParser) drainLocalOne() bool {
	msg := p.cache.topPort.PeekIncoming()
	if msg == nil {
		return false
	}

	switch req := msg.(type) {
	case *mem.InvReq:
		if len(p.pendingLocalInv) >= topParserPendingLocalInvCap {
			return false
		}
		p.pendingLocalInv = append(p.pendingLocalInv, &localInvPending{req: req})
	case *mem.ReadReq, *mem.WriteReq:
		if len(p.pendingLocalAccess) >= topParserPendingLocalAccessCap {
			return false
		}
		p.pendingLocalAccess = append(p.pendingLocalAccess, &localAccessPending{req: msg})
	default:
		log.Panic(fmt.Sprintf("[%s]\nErr: Cannot handle req type from topPort\n", p.cache.name))
	}

	p.cache.topPort.RetrieveIncoming()
	return true
}

// [L2] dispatchLocalInv pushes one queued InvReq into invStageBuffer.
func (p *topParser) dispatchLocalInv() bool {
	if len(p.pendingLocalInv) == 0 {
		return false
	}

	pending := p.pendingLocalInv[0]
	if !p.cache.invStageBuffer.CanPush() {
		return false
	}

	trans := &transaction{
		id:           sim.GetIDGenerator().Generate(),
		fromLocal:    true,
		invalidation: pending.req,
	}

	if p.cache.debugProcess && pending.req != nil && pending.req.GetAddress() == p.cache.debugAddress0 {
		fmt.Printf("[%s] [topparser]\tReceived inv req - 0: addr %x\n", p.cache.name, pending.req.GetAddress())
	}

	p.cache.invStageBuffer.Push(trans)
	p.cache.inFlightTransactions = append(p.cache.inFlightTransactions, trans)
	tracing.TraceReqReceive(pending.req, p.cache)

	p.pendingLocalInv = p.pendingLocalInv[1:]
	return true
}

// [L2] dispatchLocalAccess pushes one queued ReadReq/WriteReq into
// dirStageBuffer.
func (p *topParser) dispatchLocalAccess() bool {
	if len(p.pendingLocalAccess) == 0 {
		return false
	}

	pending := p.pendingLocalAccess[0]
	if !p.cache.dirStageBuffer.CanPush() {
		return false
	}

	trans := &transaction{
		id:        sim.GetIDGenerator().Generate(),
		fromLocal: true,
	}

	switch req := pending.req.(type) {
	case *mem.ReadReq:
		trans.toLocal = p.cache.toLocal(req.Address)
		trans.read = req
	case *mem.WriteReq:
		trans.toLocal = p.cache.toLocal(req.Address)
		trans.write = req
	default:
		log.Panic("unexpected type in pendingLocalAccess")
	}

	p.cache.stampProbe(trans, mempath.EvL2In)

	if p.cache.debugProcess && trans.accessReq() != nil && trans.accessReq().GetAddress() == p.cache.debugAddress0 {
		fmt.Printf("[%s] [topparser]\tReceived req - 0: addr %x\n", p.cache.name, trans.accessReq().GetAddress())
	}
	if p.cache.debugProcess && trans.accessReq() != nil && trans.accessReq().GetAddress() == p.cache.debugAddress1 {
		fmt.Printf("[%s] [topparser]\tReceived req - 0: addr %x\n", p.cache.name, trans.accessReq().GetAddress())
	}

	p.cache.dirStageBuffer.Push(trans)
	p.cache.inFlightTransactions = append(p.cache.inFlightTransactions, trans)
	tracing.TraceReqReceive(pending.req, p.cache)

	p.pendingLocalAccess = p.pendingLocalAccess[1:]
	return true
}
