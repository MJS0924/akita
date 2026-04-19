package coherence

import (
	"github.com/sarchlab/akita/v4/mem/cache"
	"github.com/sarchlab/akita/v4/mem/cache/cohdirectory/internal"
	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
)

type action int

const (
	InsertNewEntry action = iota
	EvictAndInsertNewEntry
	UpdateEntry
	InvalidateAndUpdateEntry
	Nothing
	InvalidateEntry
)

type transaction struct {
	action

	id               string
	read             *mem.ReadReq
	write            *mem.WriteReq
	flush            *cache.FlushReq
	block            *internal.Block
	victim           *internal.Block
	invalidationList []sim.RemotePort
	fromLocal        bool

	evictingPID     vm.PID
	evictingAddr    uint64
	pendingEviction []sim.RemotePort

	reqIDToBottom string
	// fetchPID     vm.PID
	// fetchAddress uint64
	// fetchedData  []byte
	// fetchReadReq *mem.ReadReq

	// evictingData      []byte
	// evictingDirtyMask []bool
	// evictionWriteReq  *mem.WriteReq

	mshrEntry *internal.MSHREntry
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

func (t transaction) isReadTrans() bool {
	if t.read != nil {
		return true
	}

	return false
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
