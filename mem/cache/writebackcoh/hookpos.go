package writebackcoh

import "github.com/sarchlab/akita/v4/sim"

// HookPosL2Access fires in directoryStage.doRead/doWrite when an L2 directory
// lookup resolves a hit or miss (after the transaction successfully commits).
// ctx.Domain is *Comp; ctx.Detail is L2AccessDetail.
var HookPosL2Access = &sim.HookPos{Name: "L2Access"}

// HookPosRegionFetch fires in directoryStage.fetch() when a cache-miss fetch
// to lower memory is committed (MSHR entry created, transaction popped).
// ctx.Domain is *Comp; ctx.Detail is RegionFetchDetail.
var HookPosRegionFetch = &sim.HookPos{Name: "RegionFetch"}

// L2AccessDetail carries the result of an L2 directory lookup.
type L2AccessDetail struct {
	Hit  bool
	Addr uint64 // cacheline-aligned address (cacheLineID)
}

// RegionFetchDetail carries information about a committed fetch to lower memory.
type RegionFetchDetail struct {
	RegionTag       uint64 // cacheline-aligned address (= cacheLineID from fetch())
	RegionSizeBytes uint64 // bytes per cacheline (= 1 << log2BlockSize)
}