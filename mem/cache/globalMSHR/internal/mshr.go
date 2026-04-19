package internal

import (
	"fmt"
	"log"

	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
)

type transaction struct {
	Req   mem.AccessReq
	Index uint64
}

// MSHREntry is an entry in MSHR
type MSHREntry struct {
	PID             vm.PID
	Address         uint64
	RequestFromTop  []transaction
	RequestToBottom []mem.AccessReq
	Fetching        uint64
}

// NewMSHREntry returns a new MSHR entry object
func NewMSHREntry(pid vm.PID, addr uint64) *MSHREntry {
	e := new(MSHREntry)
	e.PID = pid
	e.Address = addr
	e.RequestFromTop = make([]transaction, 0)
	e.RequestToBottom = make([]mem.AccessReq, 0)
	e.Fetching = 0

	return e
}

// MSHR is an interface that controls MSHR entries
type MSHR interface {
	Query(pid vm.PID, addr uint64) *MSHREntry
	Add(req mem.AccessReq) *MSHREntry
	Remove(pid vm.PID, addr uint64) *MSHREntry
	RemoveEntryByIndex(i int)
	AllEntries() []*MSHREntry
	IsFull() bool
	Reset()
}

// NewMSHR returns a new MSHR object
func NewMSHR(capacity int, log2UnitSize uint64, log2BlkSize uint64) MSHR {
	m := new(mshrImpl)
	m.capacity = capacity
	m.log2UnitSize = log2UnitSize
	m.log2BlkSize = log2BlkSize

	return m
}

type mshrImpl struct {
	*sim.ComponentBase

	capacity     int
	entries      []*MSHREntry
	log2UnitSize uint64
	log2BlkSize  uint64
}

func (m *mshrImpl) Add(req mem.AccessReq) *MSHREntry {
	pid := req.GetPID()
	addr := req.GetAddress()
	trans := transaction{
		Req:   req,
		Index: addr % (1 << m.log2UnitSize) / (1 << m.log2BlkSize),
	}

	for _, e := range m.entries {
		if e.PID == pid && e.Address>>m.log2UnitSize == addr>>m.log2UnitSize {
			e.RequestFromTop = append(e.RequestFromTop, trans)
		}
	}

	if len(m.entries) >= m.capacity {
		log.Panic("MSHR is full")
	}

	entry := NewMSHREntry(pid, addr)
	entry.RequestFromTop = append(entry.RequestFromTop, trans)
	m.entries = append(m.entries, entry)

	return entry
}

func (m *mshrImpl) Query(pid vm.PID, addr uint64) *MSHREntry {
	for _, e := range m.entries {
		if e.PID == pid && e.Address>>m.log2UnitSize == addr>>m.log2UnitSize {
			return e
		}
	}

	return nil
}

func (m *mshrImpl) Remove(pid vm.PID, addr uint64) *MSHREntry {
	for i, e := range m.entries {
		if e.PID == pid && e.Address>>m.log2UnitSize == addr>>m.log2UnitSize {
			m.entries = append(m.entries[:i], m.entries[i+1:]...)
			return e
		}
	}

	panic("trying to remove an non-exist entry")
}

func (m *mshrImpl) RemoveEntryByIndex(i int) {
	if len(m.entries) <= i {
		fmt.Printf("[globalMSHR][MSHR] WARNING: Trying to remove an non-exist entry\n")
		return
	}

	m.entries = append(m.entries[:i], m.entries[i+1:]...)
}

// AllEntries returns all the MSHREntries that are currently in the MSHR
func (m *mshrImpl) AllEntries() []*MSHREntry {
	return m.entries
}

// IsFull returns true if no more MSHR entries can be added
func (m *mshrImpl) IsFull() bool {
	return len(m.entries) >= m.capacity
}

func (m *mshrImpl) Reset() {
	m.entries = nil
}
