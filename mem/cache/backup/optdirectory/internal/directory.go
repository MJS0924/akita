package internal

import (
	"fmt"

	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
)

type Block struct {
	PID          vm.PID
	Tag          uint64
	WayID        int
	SetID        int
	CacheAddress uint64
	IsValid      bool
	IsDirty      bool
	ReadCount    int
	IsLocked     bool
	DirtyMask    []bool
	Accessed     bool
	Sharer       []sim.RemotePort
	VAddr        uint64
}

type Set struct {
	Blocks   []*Block
	LRUQueue []*Block
}

type CohDirectory interface {
	Lookup(pid vm.PID, address uint64) *Block
	FindVictim(address uint64) *Block
	Visit(block *Block)
	TotalSize() uint64
	WayAssociativity() int
	GetSets() []Set
	Reset()
}

type CohDirectoryImpl struct {
	NumSets       int
	NumWays       int
	BlockSize     int
	log2UnitSize  uint64
	UnitSize      uint64
	AddrConverter mem.AddressConverter
	Ideal         bool // 용량 한계에 의한 eviction이 발생하지 않음

	Sets []Set

	victimFinder VictimFinder
}

// NewDirectory returns a new directory object
func NewCohDirectory(
	set, way, blockSize int, UnitSize uint64,
	victimFinder VictimFinder,
) *CohDirectoryImpl {
	d := new(CohDirectoryImpl)
	if set == 0 {
		set = 1
	}

	d.victimFinder = victimFinder
	d.Sets = make([]Set, set)

	d.NumSets = set
	d.NumWays = way
	d.BlockSize = blockSize
	d.log2UnitSize = UnitSize
	d.UnitSize = 1 << UnitSize

	d.Reset()

	fmt.Printf("[Directory]\tBuild new coherence directory: %d sets, %d ways, %d entries\n", d.NumSets, d.NumWays, d.NumSets*d.NumWays)

	return d
}

// TotalSize returns the maximum number of bytes can be stored in the cache
func (d *CohDirectoryImpl) TotalSize() uint64 {
	return uint64(d.NumSets) * uint64(d.NumWays) * uint64(d.BlockSize)
}

// Get the set that a certain address should store at
func (d *CohDirectoryImpl) getSet(reqAddr uint64) (set *Set, setID int) {
	if d.AddrConverter != nil {
		reqAddr = d.AddrConverter.ConvertExternalToInternal(reqAddr)
	}

	setID = int(reqAddr / uint64(d.BlockSize) / d.UnitSize % uint64(d.NumSets))
	set = &d.Sets[setID]

	return
}

// Lookup finds the block that reqAddr. If the reqAddr is valid
// in the cache, return the block information. Otherwise, return nil
func (d *CohDirectoryImpl) Lookup(PID vm.PID, reqAddr uint64) *Block {
	set, _ := d.getSet(reqAddr)
	for _, block := range set.Blocks {
		if block.IsValid && block.Tag == reqAddr && block.PID == PID {
			return block
		}
	}

	return nil
}

// FindVictim returns a block that can be used to stored data at address addr.
//
// If it is valid, the cache controller need to decide what to do to evict the
// the data in the block
func (d *CohDirectoryImpl) FindVictim(addr uint64) *Block {
	set, _ := d.getSet(addr)
	block := d.victimFinder.FindVictim(set)

	if d.Ideal && block.IsValid {
		block = &Block{}
		block.SetID = set.Blocks[0].SetID
		block.WayID = len(set.Blocks)
		block.CacheAddress = 0xffffffff
		set.Blocks = append(set.Blocks, block)
		set.LRUQueue = append(set.LRUQueue, block)
	}

	return block
}

// Visit moves the block to the end of the LRUQueue
func (d *CohDirectoryImpl) Visit(block *Block) {
	set := d.Sets[block.SetID]

	for i, b := range set.LRUQueue {
		if b == block {
			set.LRUQueue = append(set.LRUQueue[:i], set.LRUQueue[i+1:]...)
			break
		}
	}

	set.LRUQueue = append(set.LRUQueue, block)
}

// GetSets returns all the sets in a directory
func (d *CohDirectoryImpl) GetSets() []Set {
	return d.Sets
}

// Reset will mark all the blocks in the directory invalid
func (d *CohDirectoryImpl) Reset() {
	d.Sets = make([]Set, d.NumSets)
	for i := 0; i < d.NumSets; i++ {
		for j := 0; j < d.NumWays; j++ {
			block := new(Block)
			block.IsValid = false
			block.SetID = i
			block.WayID = j
			block.CacheAddress = uint64(i*d.NumWays+j) * uint64(d.BlockSize)
			d.Sets[i].Blocks = append(d.Sets[i].Blocks, block)
			d.Sets[i].LRUQueue = append(d.Sets[i].LRUQueue, block)
		}
	}
}

// WayAssociativity returns the number of ways per set in the cache.
func (d *CohDirectoryImpl) WayAssociativity() int {
	return d.NumWays
}
