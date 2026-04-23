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
	CardinalityDistJSON   string // per-block cardinality distribution (legacy/debug)

	// Per-region cardinality ratios (Exp-C).
	// "Region cardinality" = popcount of the union of all sharerSets within
	// the R-byte region. For strict regions this equals the single sharer set.
	RegCard1Ratio           float64 // regions with card==1 / total
	RegCard2Ratio           float64 // regions with card==2 / total
	RegCard3Ratio           float64 // regions with card==3 / total
	RegCard4PlusRatio       float64 // regions with card>=4 / total
	CoalescablePrivateRatio float64 // strict=1 AND card==1 / total
	CoalescableSharedRatio  float64 // strict=1 AND card>=2 / total
	// SharedOnlyStrictRatio: among card>=2 regions only, what fraction are strict.
	// -1.0 if no card>=2 regions exist.
	SharedOnlyStrictRatio float64
}

var coalescabilityViews = []string{"read_only", "union", "snapshot"}

// per-GPU CSV file handles. Single-threaded sim, but guard anyway.
var (
	csvMu     sync.Mutex
	csvFiles  = make(map[string]*os.File)
	csvHeader = "sim_time,gpu_id,kernel_id,view,region_size_bytes," +
		"num_regions_with_entries,strict_coalescable_ratio,avg_relaxed_ratio," +
		"full_redundant_ratio,sharer_set_cardinality_dist_json," +
		"reg_card1_ratio,reg_card2_ratio,reg_card3_ratio,reg_card4plus_ratio," +
		"coalescable_private_ratio,coalescable_shared_ratio,shared_only_strict_ratio\n"
	cumulHeader = "gpu_id,view,region_size_bytes,num_kernels," +
		"avg_strict_ratio,avg_relaxed_ratio,avg_full_redundant_ratio," +
		"min_strict_ratio,max_strict_ratio,phase0_pass," +
		"avg_reg_card1_ratio,avg_reg_card2_ratio,avg_reg_card3_ratio,avg_reg_card4plus_ratio," +
		"avg_coalescable_private_ratio,avg_coalescable_shared_ratio," +
		"avg_shared_only_strict_ratio\n"
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
	cardDist := make(map[int]int) // per-block cardinality (legacy)

	// per-region cardinality counters
	regCardCount := [5]int{} // index 0 unused; [1]=card1, [2]=card2, [3]=card3, [4]=card4+
	coalescablePrivateCount := 0
	coalescableSharedCount := 0
	sharedRegionCount := 0   // card>=2
	strictSharedCount := 0   // strict=1 AND card>=2

	for _, rd := range regions {
		// per-block cardinality histogram (legacy JSON column)
		for _, s := range rd.sharers {
			cardDist[popcount64(s)]++
		}

		// strict: all sharer sets in region are identical
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

		// per-region cardinality = popcount of union of all sharerSets in region
		unionSharers := uint64(0)
		for _, s := range rd.sharers {
			unionSharers |= s
		}
		regionCard := popcount64(unionSharers)

		if regionCard <= 0 {
			regionCard = 0
		}
		if regionCard >= 4 {
			regCardCount[4]++
		} else if regionCard >= 1 {
			regCardCount[regionCard]++
		}

		if regionCard >= 2 {
			sharedRegionCount++
			if allSame {
				strictSharedCount++
				coalescableSharedCount++
			}
		} else if regionCard == 1 {
			if allSame {
				coalescablePrivateCount++
			}
		}
	}

	cardJSON, _ := json.Marshal(cardDist)

	fTotal := float64(numRegions)
	sharedOnlyStrict := -1.0
	if sharedRegionCount > 0 {
		sharedOnlyStrict = float64(strictSharedCount) / float64(sharedRegionCount)
	}

	return &coalescabilitySnapshot{
		SimTime:               simTime,
		GPUID:                 c.deviceID,
		KernelID:              kernelID,
		View:                  view,
		RegionSizeBytes:       R,
		NumRegionsWithEntries: numRegions,
		StrictRatio:           float64(strictCount) / fTotal,
		AvgRelaxedRatio:       totalRelaxed / fTotal,
		FullRedundantRatio:    float64(fullRedundantCount) / fTotal,
		CardinalityDistJSON:   string(cardJSON),

		RegCard1Ratio:           float64(regCardCount[1]) / fTotal,
		RegCard2Ratio:           float64(regCardCount[2]) / fTotal,
		RegCard3Ratio:           float64(regCardCount[3]) / fTotal,
		RegCard4PlusRatio:       float64(regCardCount[4]) / fTotal,
		CoalescablePrivateRatio: float64(coalescablePrivateCount) / fTotal,
		CoalescableSharedRatio:  float64(coalescableSharedCount) / fTotal,
		SharedOnlyStrictRatio:   sharedOnlyStrict,
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
	fmt.Fprintf(f,
		"%.9f,%d,%d,%s,%d,%d,%.6f,%.6f,%.6f,%s,%.6f,%.6f,%.6f,%.6f,%.6f,%.6f,%.6f\n",
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
		s.RegCard1Ratio,
		s.RegCard2Ratio,
		s.RegCard3Ratio,
		s.RegCard4PlusRatio,
		s.CoalescablePrivateRatio,
		s.CoalescableSharedRatio,
		s.SharedOnlyStrictRatio,
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

		// Exp-C cardinality aggregates
		sumCard1, sumCard2, sumCard3, sumCard4Plus float64
		sumPrivate, sumShared                      float64
		sumSharedOnlyStrict                        float64
		sharedOnlyStrictCount                      int // rows where sharedOnlyStrict != -1
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
		a.sumCard1 += snap.RegCard1Ratio
		a.sumCard2 += snap.RegCard2Ratio
		a.sumCard3 += snap.RegCard3Ratio
		a.sumCard4Plus += snap.RegCard4PlusRatio
		a.sumPrivate += snap.CoalescablePrivateRatio
		a.sumShared += snap.CoalescableSharedRatio
		if snap.SharedOnlyStrictRatio >= 0 {
			a.sumSharedOnlyStrict += snap.SharedOnlyStrictRatio
			a.sharedOnlyStrictCount++
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
	fmt.Printf("%-10s %-8s %-10s %-10s %-10s %-8s %-8s %-6s %-8s %-8s %-8s\n",
		"view", "R(B)", "avg_strict", "priv_coal", "shar_coal", "shar_str",
		"card1%", "card2%", "card3%", "card4+%", "kernels")

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
		n := float64(a.count)
		avgStrict := a.sumStrict / n
		avgRelaxed := a.sumRelaxed / n
		avgRedundant := a.sumRedundant / n
		avgCard1 := a.sumCard1 / n
		avgCard2 := a.sumCard2 / n
		avgCard3 := a.sumCard3 / n
		avgCard4Plus := a.sumCard4Plus / n
		avgPrivate := a.sumPrivate / n
		avgShared := a.sumShared / n

		avgSharedOnlyStrict := -1.0
		if a.sharedOnlyStrictCount > 0 {
			avgSharedOnlyStrict = a.sumSharedOnlyStrict / float64(a.sharedOnlyStrictCount)
		}

		pass := 0
		if avgStrict >= 0.30 {
			passedAny = true
			pass = 1
		}

		fmt.Printf("%-10s %-8d %-10.4f %-10.4f %-10.4f %-8.4f %-8.4f %-8.4f %-8.4f %-8.4f %-6d\n",
			k.view, k.R, avgStrict, avgPrivate, avgShared,
			avgSharedOnlyStrict, avgCard1, avgCard2, avgCard3, avgCard4Plus,
			a.count)

		fmt.Fprintf(cumFile,
			"%d,%s,%d,%d,%.6f,%.6f,%.6f,%.6f,%.6f,%d,%.6f,%.6f,%.6f,%.6f,%.6f,%.6f,%.6f\n",
			c.deviceID, k.view, k.R, a.count,
			avgStrict, avgRelaxed, avgRedundant,
			a.minStrict, a.maxStrict, pass,
			avgCard1, avgCard2, avgCard3, avgCard4Plus,
			avgPrivate, avgShared, avgSharedOnlyStrict,
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