package optdirectory

import (
	"github.com/sarchlab/akita/v4/mem/cache"
	"github.com/sarchlab/akita/v4/mem/cache/optdirectory/internal"
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
	BypassingDirectory
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
	toLocal          bool

	evictingPID     vm.PID
	evictingAddr    uint64
	pendingEviction []sim.RemotePort

	fetchingPID  vm.PID
	fetchingAddr uint64
	reqToBottom  []*mem.AccessReq
	ack          uint64
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

type invTrans struct {
	req      *mem.InvReq
	ack      int
	numInv   uint64
	accessed uint64
}
