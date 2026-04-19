package optdirectory

import (
	"github.com/sarchlab/akita/v4/mem/cache/optdirectory/internal"
	"github.com/sarchlab/akita/v4/sim"
)

func getCacheLineID(
	addr uint64,
	blockSizeAsPowerOf2 uint64,
) (cacheLineID, offset uint64) {
	mask := uint64(0xffffffffffffffff << blockSizeAsPowerOf2)
	cacheLineID = addr & mask
	offset = addr & ^mask

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
