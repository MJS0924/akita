package largeblkcache

import (
	"github.com/sarchlab/akita/v4/mem/cache"
	"github.com/sarchlab/akita/v4/mem/cache/largeblkcache/internal"
	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
)

type action int

const (
	actionInvalid action = iota
	bankReadHit
	bankWriteHit
	bankWritePrefetched
	bankEvict
	bankEvictAndWrite
	bankEvictAndFetch
	bankWriteFetched
	writeBufferFetch
	writeBufferEvictAndFetch
	writeBufferEvictAndWrite
	writeBufferEvictAndPrefetch
	bankEvictAndPrefetch
	writeBufferFlush

	invalidation
)

type transaction struct {
	action

	id                string
	read              *mem.ReadReq
	write             *mem.WriteReq
	invalidation      *mem.InvReq
	flush             *cache.FlushReq
	block             *internal.Block
	victim            *internal.Block
	fetchPID          vm.PID
	fetchAddress      uint64
	fetchedData       []byte
	fetchReadReq      *mem.ReadReq
	evictingPID       vm.PID
	evictingAddr      uint64
	evictingData      []byte
	evictingDirtyMask []bool
	evictionWriteReq  *mem.WriteReq
	mshrEntry         *internal.MSHREntry
	writeToHomeNode   bool
	responsing        bool

	fromLocal bool
	toLocal   bool
}

func (t transaction) accessReq() mem.AccessReq {
	if t.read != nil {
		return t.read
	}

	if t.write != nil {
		return t.write
	}

	return nil
}

func (t transaction) req() sim.Msg {
	if t.accessReq() != nil {
		return t.accessReq()
	}

	if t.flush != nil {
		return t.flush
	}

	return nil
}
