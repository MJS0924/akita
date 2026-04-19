package writeback

import (
	"fmt"
	"log"

	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/akita/v4/tracing"
)

type topParser struct {
	cache *Comp
}

func (p *topParser) Tick() bool {
	if p.cache.state != cacheStateRunning {
		return false
	}

	if !p.cache.dirStageBuffer.CanPush() {
		return false
	}

	if len(p.cache.shadowInFlightTransaction) > 0 {
		trans := p.cache.shadowInFlightTransaction[0]
		p.cache.shadowInFlightTransaction = p.cache.shadowInFlightTransaction[1:]

		p.cache.dirStageBuffer.Push(trans)
		p.cache.inFlightTransactions = append(p.cache.inFlightTransactions, trans)

		return true
	}

	req := p.cache.topPort.PeekIncoming()
	if req == nil {
		return false
	}

	trans := &transaction{
		id: sim.GetIDGenerator().Generate(),
	}

	switch req := req.(type) {
	case *mem.ReadReq:
		trans.read = req
	case *mem.WriteReq:
		trans.write = req
	case *mem.InvReq:
		trans.invalidation = req
	default:
		log.Panic(fmt.Sprintf("[%s]\nErr: Cannot handle req type\n", p.cache.name))
	}

	// fmt.Printf("[%s]\tRequest(ID: %s) %x from %s\n", p.cache.Name(), req.Meta().ID, req.(mem.AccessReq).GetAddress(), req.Meta().Src)

	p.cache.dirStageBuffer.Push(trans)

	p.cache.inFlightTransactions = append(p.cache.inFlightTransactions, trans)

	tracing.TraceReqReceive(req, p.cache)

	p.cache.topPort.RetrieveIncoming()

	return true
}
