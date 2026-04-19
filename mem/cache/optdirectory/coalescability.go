package optdirectory

// R-coalescability metrics. See design_document.md for locked definitions.
// All metrics are emitted simultaneously for every (R x view) combination;
// downstream tooling decides which to plot. This package never selects a
// "winning" granularity here.

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"sync"

	"github.com/sarchlab/akita/v4/sim"
)

// coalesceMode controls which strict/relaxed metrics to compute.
// Default is coalesceModeBoth; other values are reserved for future
// configurations and currently behave identically (both metrics are emitted
// in every CSV row to keep downstream analysis honest).
type coalesceMode int

const (
	coalesceModeBoth coalesceMode = iota
	coalesceModeStrict
	coalesceModeRelaxed
)

// coalescabilitySnapshot is one (kernel, view, R) measurement.
type coalescabilitySnapshot struct {
	SimTime               sim.VTimeInSec
	GPUID                 int
	KernelID              int
	View                  string
	RegionSizeBytes       uint64
	NumRegionsWithEntries int
	StrictRatio           float64
	AvgRelaxedRatio       float64
	FullRedundantRatio    float64
	CardinalityDistJSON   string
}

var coalescabilityViews = []string{"read_only", "union", "snapshot"}

// per-GPU CSV file handles. Single-threaded sim, but guard anyway.
var (
	csvMu       sync.Mutex
	csvFiles    = make(map[string]*os.File)
	csvHeader   = "sim_time,gpu_id,kernel_id,view,region_size_bytes,num_regions_with_entries,strict_coalescable_ratio,avg_relaxed_ratio,full_redundant_ratio,sharer_set_cardinality_dist_json\n"
	cumulHeader = "gpu_id,view,region_size_bytes,num_kernels,avg_strict_ratio,avg_relaxed_ratio,avg_full_redundant_ratio,min_strict_ratio,max_strict_ratio,phase0_pass\n"
)

func openCSV(filename, header string) *os.File {
	csvMu.Lock()
	defer csvMu.Unlock()

	if f, ok := csvFiles[filename]; ok {
		return f
	}

	f, err := os.Create(filename)
	if err != nil {
		panic(fmt.Sprintf("optdirectory: cannot create CSV %s: %v", filename, err))
	}
	if _, err := f.WriteString(header); err != nil {
		panic(fmt.Sprintf("optdirectory: cannot write CSV header to %s: %v", filename, err))
	}
	csvFiles[filename] = f
	return f
}

// emitCoalescabilityMetrics is invoked at every kernel boundary.
// It writes one CSV row per (view, R) pair, appends to cumulativeSnapshots,
// then resets kernel-local state. sharerSet / cohState are NOT reset
// (ideal directory invariant).
func (c *Comp) emitCoalescabilityMetrics(simTime sim.VTimeInSec, kernelID int) {
	c.currentKernelID = kernelID

	if len(c.regionSizesBytes) == 0 {
		c.resetKernelLocalState()
		return
	}

	blockSizeBytes := uint64(1) << c.log2BlockSize

	for _, R := range c.regionSizesBytes {
		rBlocks := R / blockSizeBytes
		if rBlocks == 0 {
			rBlocks = 1
		}

		for _, view := range coalescabilityViews {
			snap := c.computeViewSnapshot(simTime, kernelID, view, R, rBlocks)
			if snap == nil {
				continue
			}
			c.cumulativeSnapshots = append(c.cumulativeSnapshots, *snap)
			c.writeCoalescabilityRow(*snap)
		}
	}

	c.resetKernelLocalState()
}

func (c *Comp) resetKernelLocalState() {
	c.writeMaskKernel = make(map[uint64]bool)
	c.accessMaskKernel = make(map[uint64]bool)
}

// computeViewSnapshot produces the metrics for a single (view, R) pair.
// Returns nil if the view contains no tracked entries.
func (c *Comp) computeViewSnapshot(
	simTime sim.VTimeInSec, kernelID int, view string, R, rBlocks uint64,
) *coalescabilitySnapshot {
	entries := c.getViewEntries(view)
	if len(entries) == 0 {
		return nil
	}

	type regionData struct {
		sharers []uint64
		states  []uint8
	}
	regions := make(map[uint64]*regionData)

	for blockID, sharers := range entries {
		regionID := blockID / rBlocks
		rd, ok := regions[regionID]
		if !ok {
			rd = &regionData{}
			regions[regionID] = rd
		}
		rd.sharers = append(rd.sharers, sharers)
		rd.states = append(rd.states, c.cohState[blockID])
	}

	numRegions := len(regions)
	if numRegions == 0 {
		return nil
	}

	strictCount := 0
	fullRedundantCount := 0
	totalRelaxed := 0.0
	cardDist := make(map[int]int)

	for _, rd := range regions {
		// cardinality histogram across all entries
		for _, s := range rd.sharers {
			cardDist[popcount64(s)]++
		}

		// strict: all sharer sets identical
		ref := rd.sharers[0]
		allSame := true
		for _, s := range rd.sharers[1:] {
			if s != ref {
				allSame = false
				break
			}
		}
		if allSame {
			strictCount++
			refState := rd.states[0]
			allStatesSame := true
			for _, st := range rd.states[1:] {
				if st != refState {
					allStatesSame = false
					break
				}
			}
			if allStatesSame {
				fullRedundantCount++
			}
		}

		// relaxed: max-count of identical sharer sets / total
		counts := make(map[uint64]int, len(rd.sharers))
		for _, s := range rd.sharers {
			counts[s]++
		}
		maxCount := 0
		for _, n := range counts {
			if n > maxCount {
				maxCount = n
			}
		}
		totalRelaxed += float64(maxCount) / float64(len(rd.sharers))
	}

	cardJSON, _ := json.Marshal(cardDist)

	return &coalescabilitySnapshot{
		SimTime:               simTime,
		GPUID:                 c.deviceID,
		KernelID:              kernelID,
		View:                  view,
		RegionSizeBytes:       R,
		NumRegionsWithEntries: numRegions,
		StrictRatio:           float64(strictCount) / float64(numRegions),
		AvgRelaxedRatio:       totalRelaxed / float64(numRegions),
		FullRedundantRatio:    float64(fullRedundantCount) / float64(numRegions),
		CardinalityDistJSON:   string(cardJSON),
	}
}

// getViewEntries assembles {blockID -> sharerSet} for the requested view.
//   - snapshot: every currently-valid directory entry
//   - union:    every block accessed in the current kernel
//   - read_only: union minus writeMaskKernel
func (c *Comp) getViewEntries(view string) map[uint64]uint64 {
	result := make(map[uint64]uint64)
	switch view {
	case "snapshot":
		for _, set := range c.directory.GetSets() {
			for _, blk := range set.Blocks {
				if !blk.IsValid {
					continue
				}
				blockID := blk.Tag >> c.log2BlockSize
				result[blockID] = c.sharerSet[blockID]
			}
		}
	case "union":
		for blockID := range c.accessMaskKernel {
			result[blockID] = c.sharerSet[blockID]
		}
	case "read_only":
		for blockID := range c.accessMaskKernel {
			if c.writeMaskKernel[blockID] {
				continue
			}
			result[blockID] = c.sharerSet[blockID]
		}
	}
	return result
}

func (c *Comp) writeCoalescabilityRow(s coalescabilitySnapshot) {
	filename := fmt.Sprintf("motivation_coalescability_GPU%d.csv", c.deviceID)
	f := openCSV(filename, csvHeader)

	csvMu.Lock()
	defer csvMu.Unlock()
	fmt.Fprintf(f, "%.9f,%d,%d,%s,%d,%d,%.6f,%.6f,%.6f,%s\n",
		float64(s.SimTime),
		s.GPUID,
		s.KernelID,
		s.View,
		s.RegionSizeBytes,
		s.NumRegionsWithEntries,
		s.StrictRatio,
		s.AvgRelaxedRatio,
		s.FullRedundantRatio,
		s.CardinalityDistJSON,
	)
}

// EmitCumulativeReport is called once at simulation end. It aggregates
// per-(view, R) metrics across all kernel snapshots, writes a summary CSV,
// prints a compact table, and evaluates the PHASE 0 exit criterion.
func (c *Comp) EmitCumulativeReport() {
	type key struct {
		view string
		R    uint64
	}
	type agg struct {
		sumStrict, sumRelaxed, sumRedundant float64
		count                               int
		minStrict, maxStrict                float64
	}

	aggregated := make(map[key]*agg)
	for _, snap := range c.cumulativeSnapshots {
		k := key{view: snap.View, R: snap.RegionSizeBytes}
		a, ok := aggregated[k]
		if !ok {
			a = &agg{minStrict: 1.0}
			aggregated[k] = a
		}
		a.sumStrict += snap.StrictRatio
		a.sumRelaxed += snap.AvgRelaxedRatio
		a.sumRedundant += snap.FullRedundantRatio
		a.count++
		if snap.StrictRatio > a.maxStrict {
			a.maxStrict = snap.StrictRatio
		}
		if snap.StrictRatio < a.minStrict {
			a.minStrict = snap.StrictRatio
		}
	}

	// Print a deterministic table (sort by view, then R).
	keys := make([]key, 0, len(aggregated))
	for k := range aggregated {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].view != keys[j].view {
			return keys[i].view < keys[j].view
		}
		return keys[i].R < keys[j].R
	})

	fmt.Printf("\n=== [%s] Cumulative Coalescability Report ===\n", c.name)
	fmt.Printf("%-10s %-10s %-12s %-12s %-12s %-10s %-10s %-8s\n",
		"view", "R(bytes)", "avg_strict", "avg_relaxed", "avg_fullred",
		"min_str", "max_str", "kernels")

	passedAny := false
	cumFile, err := os.Create(fmt.Sprintf("motivation_cumulative_GPU%d.csv", c.deviceID))
	if err != nil {
		fmt.Printf("[ERROR][%s] cannot create cumulative CSV: %v\n", c.name, err)
		return
	}
	defer cumFile.Close()
	if _, err := cumFile.WriteString(cumulHeader); err != nil {
		fmt.Printf("[ERROR][%s] cannot write cumulative header: %v\n", c.name, err)
		return
	}

	for _, k := range keys {
		a := aggregated[k]
		avgStrict := a.sumStrict / float64(a.count)
		avgRelaxed := a.sumRelaxed / float64(a.count)
		avgRedundant := a.sumRedundant / float64(a.count)
		pass := 0
		if avgStrict >= 0.30 {
			passedAny = true
			pass = 1
		}

		fmt.Printf("%-10s %-10d %-12.4f %-12.4f %-12.4f %-10.4f %-10.4f %-8d\n",
			k.view, k.R, avgStrict, avgRelaxed, avgRedundant,
			a.minStrict, a.maxStrict, a.count)

		fmt.Fprintf(cumFile, "%d,%s,%d,%d,%.6f,%.6f,%.6f,%.6f,%.6f,%d\n",
			c.deviceID, k.view, k.R, a.count,
			avgStrict, avgRelaxed, avgRedundant,
			a.minStrict, a.maxStrict, pass,
		)
	}

	// Exit criterion: PHASE 0 PASS iff at least one (R x view) reaches >= 30%.
	// An empty run (no snapshots) intentionally falls through to the R6 path
	// so missing data is loud.
	if passedAny {
		fmt.Printf("[PHASE 0 PASS][%s] strict ratio >= 30%% in at least one (R x view).\n",
			c.name)
	} else {
		fmt.Printf("[WARNING][R6][%s] strict ratio < 30%% in ALL (R x view). Pivot to Plan B/C required.\n",
			c.name)
	}
	fmt.Printf("=============================================\n\n")
}

// popcount64 counts set bits in x. Local helper to avoid importing math/bits
// in case other tooling targets older Go versions.
func popcount64(x uint64) int {
	n := 0
	for x != 0 {
		n += int(x & 1)
		x >>= 1
	}
	return n
}