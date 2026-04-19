package largeblkcache

import (
	"github.com/sarchlab/akita/v4/mem/cache/largeblkcache/internal"
	"github.com/sarchlab/akita/v4/sim"
)

func getCacheLineID(
	addr uint64,
	blockSizeAsPowerOf2 uint64,
) (cacheLineID, offset uint64) {
	// mask := uint64(0xffffffffffffffff << blockSizeAsPowerOf2)
	// cacheLineID = addr & mask
	// offset = addr & ^mask
	cacheLineID = addr >> blockSizeAsPowerOf2 << blockSizeAsPowerOf2
	offset = addr - cacheLineID

	return
}

func bankID(block *internal.Block, wayAssocitivity, numBanks int) int {
	return (block.SetID*wayAssocitivity + block.WayID) % numBanks
}

func clearPort(p sim.Port) {
	for {
		item := p.RetrieveIncoming()
		if item == nil {
			return
		}
	}
}
