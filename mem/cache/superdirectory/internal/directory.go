package internal

import (
	"fmt"

	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
)

// A CohEntry of a cache is the information that is associated with a cache line
type CohSubEntry struct {
	IsValid   bool
	IsDirty   bool
	ReadCount int
	IsLocked  bool
	Sharer    []sim.RemotePort
	VAddr     uint64
	DirtyMask []bool
	Accessed  bool
}

type CohEntry struct {
	PID          vm.PID
	Tag          uint64
	WayID        int
	SetID        int
	CacheAddress uint64
	SubEntry     []CohSubEntry // length = 1 << log2NumSubEntry
	IsValid      bool
	// DemoteLocked is set true when this entry was created by a demote.
	// While set, a subsequent demote trigger on this entry must skip the
	// demote (invalidate-only) and reset the flag.
	// Reset paths: Reset() (covers eviction, promote-source), explicit
	// reset in directoryStage.doPromotion target, mshrStage demote-skip.
	DemoteLocked bool
	// lock을 subentry가 아니라 entry 단위로 걸어야 할 수도 있음
}

func (c CohEntry) IsValidEntry() bool {
	if !c.IsValid {
		return false
	}

	for _, e := range c.SubEntry {
		if e.IsValid {
			return true
		}
	}

	return false
}

func (c CohEntry) IsLockedEntry() bool {
	for _, e := range c.SubEntry {
		if e.IsLocked {
			return true
		}
	}

	return false
}

func (c CohEntry) GetReadCount() int {
	n := 0
	for _, e := range c.SubEntry {
		n += e.ReadCount
	}

	return n
}

// 포인터 리시버(*CohEntry)를 사용하여 불필요한 메모리 복사를 방지합니다.
// threshold에 따라 다르게 판단하는 로직이 필요할 수도 있음..
func (c *CohEntry) AbleToPromotion() bool {
	sh0 := c.SubEntry[0].Sharer
	len0 := len(sh0)

	if len0 == 0 {
		return false
	}

	if !c.IsValid {
		return false
	}
	for i := range c.SubEntry {
		if !c.SubEntry[i].IsValid {
			return false
		}
	}

	for i := range c.SubEntry {
		shI := c.SubEntry[i].Sharer

		if len(shI) != len0 {
			return false
		}

		for _, targetSharer := range shI {
			found := false
			for _, baseSharer := range sh0 {
				if targetSharer == baseSharer {
					found = true
					break
				}
			}

			if !found {
				return false
			}
		}
	}

	return true
}

// AbleToPromotionRelaxed permits promotion when ALL sub-entries are valid
// AND the intersection of their sharer sets is non-empty (at least one
// common GPU appears in every sub-entry's sharer list). The promoted
// (coarsened) entry uses SharerUnion() so over-broad invalidation keeps
// coherence safe; the intersection check is only the eligibility predicate.
//
// This sits between strict (identical sharer sets) and the prior any-valid
// relaxation. All-valid avoids the orphan-finer-entry / Hit-about-demotion
// regressions caused by partial-valid promotion.
func (c *CohEntry) AbleToPromotionRelaxed() bool {
	if !c.IsValid {
		return false
	}
	for i := range c.SubEntry {
		if !c.SubEntry[i].IsValid {
			return false
		}
	}

	sh0 := c.SubEntry[0].Sharer
	if len(sh0) == 0 {
		return false
	}
	for _, candidate := range sh0 {
		survives := true
		for i := 1; i < len(c.SubEntry); i++ {
			if !Present(c.SubEntry[i].Sharer, candidate) {
				survives = false
				break
			}
		}
		if survives {
			return true
		}
	}
	return false
}

// MaxSharerSubentryCount returns the maximum number of valid sub-entries
// that share any single sharer GPU. It is the "concentration score" used
// by promote-at-evict ranking: a higher value means a single GPU touches
// more of the region, so coarsening it preserves more of the existing
// sharer relationship under SharerUnion.
//
// Returns 0 for invalid entries or entries with no sharers.
func (c *CohEntry) MaxSharerSubentryCount() int {
	if !c.IsValid {
		return 0
	}
	best := 0
	// Iterate each valid sub-entry's sharer list as the candidate GPU
	// pool, then count how many valid sub-entries contain that GPU.
	// Inner Present() makes this O(numSub^2 * sharersPerSub) which is
	// tiny in practice (numSub = 1 << log2NumSubEntry = 4).
	for i := range c.SubEntry {
		if !c.SubEntry[i].IsValid {
			continue
		}
		for _, candidate := range c.SubEntry[i].Sharer {
			appearances := 0
			for j := range c.SubEntry {
				if !c.SubEntry[j].IsValid {
					continue
				}
				if Present(c.SubEntry[j].Sharer, candidate) {
					appearances++
				}
			}
			if appearances > best {
				best = appearances
			}
		}
	}
	return best
}

// PromoteOnEvictEligible is the predicate used by promote-at-evict
// v1/v2 (finest-bank only). An entry qualifies when at least one GPU
// appears as sharer in >= 3 valid sub-entries.
//
// The >=3 threshold (raised from the original >=2) restricts
// coarsening to entries where a single GPU already touches the
// majority of sub-regions, so promoting them up keeps the sharer
// concentration aligned with the coarser granularity. SharerUnion is
// still the safe choice for the promoted entry's sharer list.
func (c *CohEntry) PromoteOnEvictEligible() bool {
	return c.MaxSharerSubentryCount() >= 3
}

// SharerUnion returns the deduplicated union of sharers across all valid
// sub-entries. Used by relaxed promotion so the coarsened entry tracks
// every GPU that may hold a copy in any sub-region.
func (c *CohEntry) SharerUnion() []sim.RemotePort {
	var union []sim.RemotePort
	for i := range c.SubEntry {
		if !c.SubEntry[i].IsValid {
			continue
		}
		for _, sh := range c.SubEntry[i].Sharer {
			if !Present(union, sh) {
				union = append(union, sh)
			}
		}
	}
	return union
}

func Present(list []sim.RemotePort, sh sim.RemotePort) bool {
	for _, entry := range list {
		if entry == sh {
			return true
		}
	}

	return false
}

// A Set is a list of CohEntrys where a certain piece memory can be stored at
type CohSet struct {
	CohEntries []*CohEntry
	LRUQueue   []*CohEntry
}

type SuperDirectory interface {
	Lookup(bankID int, PID vm.PID, reqAddr uint64) (*CohEntry, int)
	GetBank(reqAddr uint64) (bankID []int)
	GetSet(bankID int, reqAddr uint64) (set *CohSet, setID int)
	GetBanks() [][]CohSet

	FindVictim(bankID int, pid vm.PID, addr uint64) (*CohEntry, bool)
	// FindVictimPreferPromotable / FindVictimPreferStrictlyPromotable bias
	// the LRU victim search toward entries eligible for promote-at-evict
	// (§4 v2/v3). They fall back to the regular victim selection when no
	// eligible entry is found. The boolean has the same meaning as
	// FindVictim: true means the returned entry is a freshly chosen victim
	// (needs eviction handling); false means an existing matching entry
	// was found and should be reused.
	FindVictimPreferPromotable(bankID int, pid vm.PID, addr uint64) (*CohEntry, bool)
	FindVictimPreferStrictlyPromotable(bankID int, pid vm.PID, addr uint64) (*CohEntry, bool)
	Visit(bankID int, entry *CohEntry)

	TotalSize() uint64
	WayAssociativity() int
	GetRegionLen(regionID int) int

	InsertBloomfilter(regionID int, addr uint64)
	EvictBloomfilter(regionID int, addr uint64)

	// FPR tracking for CBF-enabled coarse banks (banks 0, 1, 2).
	// SampleCBFForAllBanks probes every CBF-enabled bank for the given
	// address and records the (CBF outcome, directory ground truth) pair
	// — giving an unbiased FPR measurement for *every* incoming request,
	// not only the bank that selectBank ended up choosing. Without this,
	// FPR appears artificially high because the only banks we measure
	// are the ones whose CBF already said "positive" (by construction).
	SampleCBFForAllBanks(pid vm.PID, reqAddr uint64)
	CBFStats() []CBFBankStats
	HasCBF(bankID int) bool

	Reset()
}

// CBFBankStats records per-bank false-positive accounting for the CBF.
// Only coarse banks with an instantiated filter populate these counters.
type CBFBankStats struct {
	Queries         uint64
	Positives       uint64
	TruePositives   uint64
	FalsePositives  uint64
	TrueNegatives   uint64
	FalseNegatives  uint64 // expected zero for a correctly-maintained CBF
	NumEntries      uint64 // m for this bank's filter (0 when no CBF)
	// Diagnostic: cumulative Insert/Evict call counts per bank. If
	// Inserts > Evicts grows unboundedly the CBF saturates regardless of
	// hashing — useful to localize unbalanced call sites.
	Inserts uint64
	Evicts  uint64
	// NumOverflowed: how many CBF slots are currently saturated at
	// cbfCounterMax (and therefore sticky against further Evicts). High
	// values indicate the m/n ratio is too tight for the workload's
	// transient peaks. Captured at report time, so this is a final
	// snapshot rather than a running counter.
	NumOverflowed uint64
}

type SuperDirectoryImpl struct {
	NumBanks        int
	NumSets         []int
	NumWays         int
	log2NumSubEntry int
	BlockSize       int
	AddrConverter   mem.AddressConverter

	// Per-bank CBF: nil for banks without a filter (finer banks 3, 4).
	bloomFilter []*CountingBloomFilter
	disableCBF  bool

	// numCBFBanks: how many leading banks (0..numCBFBanks-1) have a CBF.
	// Per Mehta L-CBF config: coarse banks (0,1,2) only.
	numCBFBanks int

	cbfStats []CBFBankStats

	Sets      [][]CohSet // 1st index: bank, 2nd index: set
	RegionLen []int

	victimFinder     VictimFinder
	isIdealDirectory bool
}

// Default per-bank CBF sizes (m = number of 4-bit counters) for the
// Mehta L-CBF (m/n=4, k=3) configuration. Sized for coarse banks only,
// assuming the BEC_FE geometry (8/16/32 sets × 8 ways = 64/128/256
// coarse entries → 256/512/1024 sub-entries at log2NumSubEntry=2):
//   Bank 0 (regionLen=14) -> 1024 counters (4-bit each = 512 B)
//   Bank 1 (regionLen=12) -> 2048 counters (1 KB)
//   Bank 2 (regionLen=10) -> 4096 counters (2 KB)
// Banks 3 and 4 do not host a filter (always probed regardless).
var defaultCBFSizes = []uint64{1024, 2048, 4096}

// NewSuperDirectory builds a SuperDirectory.
// numSetsPerBank: optional per-bank set counts (len == bank). When nil/empty,
// falls back to the legacy uniform-doubling formula (set >> bank << i),
// which is the geometric progression used before the BEC_FE spec.
func NewSuperDirectory(
	bank, set, way, blockSize, log2NumSubEntry, BFSize int,
	victimFinder VictimFinder, regionLen []int, disableCBF bool,
	numSetsPerBank []int,
) *SuperDirectoryImpl {
	d := new(SuperDirectoryImpl)
	d.victimFinder = victimFinder

	d.NumBanks = bank
	d.NumSets = make([]int, bank)
	d.NumWays = way
	d.BlockSize = blockSize
	d.log2NumSubEntry = log2NumSubEntry
	d.disableCBF = disableCBF
	d.isIdealDirectory = false

	if len(numSetsPerBank) > 0 {
		if len(numSetsPerBank) != bank {
			panic(fmt.Sprintf(
				"ERROR: numSetsPerBank len=%d does not match numBanks=%d",
				len(numSetsPerBank), bank,
			))
		}
		for i := 0; i < bank; i++ {
			if numSetsPerBank[i] <= 0 {
				panic(fmt.Sprintf(
					"ERROR: numSetsPerBank[%d]=%d must be > 0",
					i, numSetsPerBank[i],
				))
			}
			d.NumSets[i] = numSetsPerBank[i]
		}
	} else {
		for i := 0; i < bank; i++ {
			if set>>bank<<i == 0 {
				panic("ERROR: Too small set number")
			}
			d.NumSets[i] = set >> bank << i
		}
	}

	// CBF only for coarse banks (0..min(numBanks, len(defaultCBFSizes))-1).
	d.numCBFBanks = len(defaultCBFSizes)
	if d.numCBFBanks > bank {
		d.numCBFBanks = bank
	}
	d.bloomFilter = make([]*CountingBloomFilter, bank)
	d.cbfStats = make([]CBFBankStats, bank)
	for i := 0; i < d.numCBFBanks; i++ {
		d.bloomFilter[i] = NewCountingBloomFilter(i, defaultCBFSizes[i])
		d.cbfStats[i].NumEntries = defaultCBFSizes[i]
	}

	for i := 0; i < bank; i++ {
		d.Sets = append(d.Sets, make([]CohSet, d.NumSets[i]))
	}
	d.RegionLen = regionLen
	if len(d.RegionLen) != d.NumBanks {
		panic("ERR: len(d.regionLen) doesn't match d.NumBanks")
	}

	d.Reset()

	fmt.Printf("[Directory]\tBuild new coherence directory: %d banks, %d sets, %d ways (CBF banks=%d)\n",
		d.NumBanks, d.NumSets, d.NumWays, d.numCBFBanks)

	return d
}

func (d *SuperDirectoryImpl) TotalSize() uint64 {
	// return uint64(d.NumBanks) * uint64(d.NumSets) * uint64(d.NumWays) * uint64(d.BlockSize)
	return uint64(d.NumBanks) * uint64(d.NumSets[d.NumBanks-1]*2) * uint64(d.NumWays) * uint64(d.BlockSize)
}

// GetBank returns the CBF-positive coarse banks for reqAddr.
//   Order: finest-first among CBF-enabled banks (e.g. 2, 1, 0).
//   Banks without a CBF (numBanks-1, numBanks-2) are NOT included here
//   because GetBank's contract is "banks the CBF predicts may hold data".
//   The caller (selectBank) is responsible for appending the always-probed
//   non-CBF banks to honor the requested lookup order.
//   When disableCBF=true the legacy "all banks finest-first" behavior is
//   preserved so RSB/disagreement logic continues to function.
func (d *SuperDirectoryImpl) GetBank(reqAddr uint64) (bankID []int) {
	if d.disableCBF {
		for i := d.NumBanks - 1; i >= 0; i-- {
			bankID = append(bankID, i)
		}
		return
	}

	for i := d.numCBFBanks - 1; i >= 0; i-- {
		if d.bloomFilter[i] == nil {
			continue
		}
		if d.bloomFilter[i].Search(reqAddr, d.RegionLen[i]) {
			bankID = append(bankID, i)
		}
	}
	return
}

// NumCBFBanks reports how many of the leading banks have a CBF
// (i.e. coarse banks 0..NumCBFBanks-1 do, the rest do not).
func (d *SuperDirectoryImpl) NumCBFBanks() int {
	if d.disableCBF {
		return 0
	}
	return d.numCBFBanks
}

func (d *SuperDirectoryImpl) GetSet(bankID int, reqAddr uint64) (set *CohSet, setID int) {
	// 2. bankID에 해당하는 bank에서 set 얻기
	if d.AddrConverter != nil {
		reqAddr = d.AddrConverter.ConvertExternalToInternal(reqAddr)
	}

	len := d.RegionLen[bankID] + d.log2NumSubEntry
	setID = int(reqAddr) / (1 << len) % d.NumSets[bankID]
	set = &d.Sets[bankID][setID]

	return
}

func (d *SuperDirectoryImpl) Lookup(bankID int, PID vm.PID, reqAddr uint64) (*CohEntry, int) {
	maskLen := d.RegionLen[bankID] + d.log2NumSubEntry
	set, _ := d.GetSet(bankID, reqAddr)
	for _, entry := range set.CohEntries {
		if entry.IsValid && entry.Tag>>maskLen == reqAddr>>maskLen && entry.PID == PID {
			index := (int(reqAddr) >> d.RegionLen[bankID]) % (1 << d.log2NumSubEntry)
			if !entry.SubEntry[index].IsValid {
				return nil, -1
			}
			return entry, index
		}
	}

	return nil, -1
}

func (d *SuperDirectoryImpl) FindVictim(bankID int, PID vm.PID, addr uint64) (*CohEntry, bool) {
	set, _ := d.GetSet(bankID, addr)

	// addr을 포함하는 entry가 있는 경우 해당 entry를 return
	maskLen := d.RegionLen[bankID] + d.log2NumSubEntry
	for _, entry := range set.CohEntries {
		if entry.IsValid && entry.Tag>>maskLen == addr>>maskLen && entry.PID == PID {
			return entry, false
		}
	}

	// invalid entry까지 포함하여 addr을 포함하는 entry를 search
	for _, entry := range set.CohEntries {
		if entry.Tag>>maskLen == addr>>maskLen && entry.PID == PID {
			return entry, true // 아마도 invalid 일 것 -> 초기화 후 사용해야 함
		}
	}

	if d.isIdealDirectory { // 새로운 entry를 추가하고 해당 entry를 victim으로 사용하기
		entry := d.victimFinder.FindVictim(set)
		return entry, true
	} else {
		entry := d.victimFinder.FindVictim(set)
		return entry, true
	}
}

// findVictimWithPredicate is the shared core for the Prefer* variants. If
// any LRU-ordered entry in the set satisfies pred(), the LRU-most such
// entry becomes the victim. Otherwise falls back to the default LRU
// victim. The (entry, true) contract matches FindVictim's "victim that
// needs eviction handling" semantics — callers must call needEviction to
// decide whether to issue evict messages.
func (d *SuperDirectoryImpl) findVictimWithPredicate(
	bankID int, PID vm.PID, addr uint64, pred func(*CohEntry) bool,
) (*CohEntry, bool) {
	set, _ := d.GetSet(bankID, addr)

	// Existing-entry lookup short-circuits the predicate (same contract as
	// FindVictim: a hit returns alloc=false).
	maskLen := d.RegionLen[bankID] + d.log2NumSubEntry
	for _, entry := range set.CohEntries {
		if entry.IsValid && entry.Tag>>maskLen == addr>>maskLen && entry.PID == PID {
			return entry, false
		}
	}
	for _, entry := range set.CohEntries {
		if entry.Tag>>maskLen == addr>>maskLen && entry.PID == PID {
			return entry, true
		}
	}

	// Two-pass victim search: prefer LRU-most entry satisfying pred.
	for _, entry := range set.LRUQueue {
		if entry.IsValidEntry() && !entry.IsLockedEntry() && entry.GetReadCount() == 0 && pred(entry) {
			return entry, true
		}
	}
	return d.victimFinder.FindVictim(set), true
}

// FindVictimPreferPromotable: §4 v2 finest-bank path. Among entries
// that pass PromoteOnEvictEligible (≥3 valid sub-entries share a
// common GPU), pick the one with the highest MaxSharerSubentryCount
// — i.e., the entry whose dominant GPU is shared across the most
// sub-entries. Ties are broken by LRU (earlier-in-LRUQueue wins,
// since LRUQueue is ordered LRU-most first).
//
// Falls back to the default LRU victim when no entry qualifies.
func (d *SuperDirectoryImpl) FindVictimPreferPromotable(
	bankID int, PID vm.PID, addr uint64,
) (*CohEntry, bool) {
	set, _ := d.GetSet(bankID, addr)

	// Existing-entry lookup short-circuits the search (same contract as
	// FindVictim: a hit returns alloc=false).
	maskLen := d.RegionLen[bankID] + d.log2NumSubEntry
	for _, entry := range set.CohEntries {
		if entry.IsValid && entry.Tag>>maskLen == addr>>maskLen && entry.PID == PID {
			return entry, false
		}
	}
	for _, entry := range set.CohEntries {
		if entry.Tag>>maskLen == addr>>maskLen && entry.PID == PID {
			return entry, true
		}
	}

	// Score-ranked search: pick the eligible entry with the highest
	// MaxSharerSubentryCount. LRU-most wins on ties (strict ">" keeps
	// the first-seen entry).
	var bestEntry *CohEntry
	bestScore := 0
	for _, entry := range set.LRUQueue {
		if !entry.IsValidEntry() || entry.IsLockedEntry() || entry.GetReadCount() != 0 {
			continue
		}
		score := entry.MaxSharerSubentryCount()
		if score < 3 {
			continue
		}
		if score > bestScore {
			bestScore = score
			bestEntry = entry
		}
	}
	if bestEntry != nil {
		return bestEntry, true
	}
	return d.victimFinder.FindVictim(set), true
}

// FindVictimPreferStrictlyPromotable: §4 v3 non-finest, non-coarsest
// path. Uses the stricter AbleToPromotionRelaxed predicate (all
// sub-entries valid + non-empty sharer intersection).
func (d *SuperDirectoryImpl) FindVictimPreferStrictlyPromotable(
	bankID int, PID vm.PID, addr uint64,
) (*CohEntry, bool) {
	return d.findVictimWithPredicate(bankID, PID, addr, func(e *CohEntry) bool {
		return e.AbleToPromotionRelaxed()
	})
}

func (d *SuperDirectoryImpl) Visit(bankID int, entry *CohEntry) {
	set := &d.Sets[bankID][entry.SetID]

	for i, e := range set.LRUQueue {
		if e == entry {
			set.LRUQueue = append(set.LRUQueue[:i], set.LRUQueue[i+1:]...)
			break
		}
	}

	set.LRUQueue = append(set.LRUQueue, entry)
}

func (d *SuperDirectoryImpl) GetBanks() [][]CohSet {
	return d.Sets
}

func (d *SuperDirectoryImpl) Reset() {
	numSubEntries := 1 << d.log2NumSubEntry
	for i := 0; i < d.NumBanks; i++ {
		for j := 0; j < d.NumSets[i]; j++ {
			for k := 0; k < d.NumWays; k++ {
				entry := new(CohEntry)
				entry.IsValid = false
				entry.SetID = j
				entry.WayID = k
				entry.CacheAddress = uint64(j*d.NumWays+k) * uint64(d.BlockSize) * uint64(numSubEntries)
				entry.SubEntry = make([]CohSubEntry, numSubEntries)
				d.Sets[i][j].CohEntries = append(d.Sets[i][j].CohEntries, entry)
				d.Sets[i][j].LRUQueue = append(d.Sets[i][j].LRUQueue, entry)
			}
		}
	}

	for _, f := range d.bloomFilter {
		if f == nil {
			continue
		}
		f.Reset()
	}
}

func (d *SuperDirectoryImpl) WayAssociativity() int {
	return d.NumWays
}

func (d *SuperDirectoryImpl) GetRegionLen(regionID int) int {
	if regionID >= len(d.RegionLen) {
		return -1
	}

	return d.RegionLen[regionID]
}

func (d *SuperDirectoryImpl) InsertBloomfilter(regionID int, addr uint64) {
	if d.disableCBF {
		return
	}
	if regionID < 0 || regionID >= len(d.bloomFilter) || d.bloomFilter[regionID] == nil {
		return
	}
	d.bloomFilter[regionID].Insert(addr, d.RegionLen[regionID])
	d.cbfStats[regionID].Inserts++
}

func (d *SuperDirectoryImpl) EvictBloomfilter(regionID int, addr uint64) {
	if d.disableCBF {
		return
	}
	if regionID < 0 || regionID >= len(d.bloomFilter) || d.bloomFilter[regionID] == nil {
		return
	}
	d.bloomFilter[regionID].Evict(addr, d.RegionLen[regionID])
	d.cbfStats[regionID].Evicts++
}

// HasCBF reports whether bankID has an instantiated CBF.
func (d *SuperDirectoryImpl) HasCBF(bankID int) bool {
	if d.disableCBF {
		return false
	}
	return bankID >= 0 && bankID < len(d.bloomFilter) && d.bloomFilter[bankID] != nil
}

// SampleCBFForAllBanks probes every CBF-enabled bank for reqAddr and
// records the (CBF outcome, ground-truth at that bank) pair into the
// per-bank stats. Called once per access at acceptance time — gives an
// unbiased FPR sample for *every* request, regardless of which bank
// selectBank routes the access to.
//
// Ground truth at a CBF-enabled bank: the directory has a valid sub-entry
// for (PID, reqAddr) at that bank, i.e. Lookup returns non-nil.
func (d *SuperDirectoryImpl) SampleCBFForAllBanks(pid vm.PID, reqAddr uint64) {
	for bankID := 0; bankID < d.numCBFBanks; bankID++ {
		if !d.HasCBF(bankID) {
			continue
		}
		positive := d.bloomFilter[bankID].Search(reqAddr, d.RegionLen[bankID])
		block, _ := d.Lookup(bankID, pid, reqAddr)
		hit := block != nil
		s := &d.cbfStats[bankID]
		s.Queries++
		if positive {
			s.Positives++
			if hit {
				s.TruePositives++
			} else {
				s.FalsePositives++
			}
		} else {
			if hit {
				s.FalseNegatives++ // should be 0; flags a CBF maintenance bug
			} else {
				s.TrueNegatives++
			}
		}
	}
}

// refreshCBFOverflowSnapshot fills NumOverflowed on every bank's stats.
// Cheap (O(m) per bank, no per-tick cost) but only invoked on read.
func (d *SuperDirectoryImpl) refreshCBFOverflowSnapshot() {
	for i := 0; i < len(d.bloomFilter); i++ {
		if d.bloomFilter[i] == nil {
			continue
		}
		d.cbfStats[i].NumOverflowed = d.bloomFilter[i].NumAtMax()
	}
}

// CBFStats returns a snapshot of the per-bank CBF FPR counters.
func (d *SuperDirectoryImpl) CBFStats() []CBFBankStats {
	d.refreshCBFOverflowSnapshot()
	out := make([]CBFBankStats, len(d.cbfStats))
	copy(out, d.cbfStats)
	return out
}

func (e *CohEntry) DeepCopy() *CohEntry {
	// 1. nil 포인터 방어
	if e == nil {
		return nil
	}

	// 2. 값 타입 필드 및 고정 크기 배열([4]CohSubEntry) 1차 복사
	// (이 시점에서 포인터와 슬라이스 헤더는 여전히 원본을 가리키는 상태임)
	newEntry := *e

	// 3. SubEntry 내부의 참조 타입(슬라이스)들을 순회하며 깊은 복사(Deep Copy) 수행
	for i := range newEntry.SubEntry {
		// 원본 SubEntry 참조
		origSub := &e.SubEntry[i]

		// 3-1. Sharer 슬라이스 깊은 복사
		if origSub.Sharer != nil {
			newSharer := make([]sim.RemotePort, len(origSub.Sharer))
			copy(newSharer, origSub.Sharer)
			newEntry.SubEntry[i].Sharer = newSharer
		}

		// 3-2. DirtyMask 슬라이스 깊은 복사 (구조체에 존재할 경우)
		if origSub.DirtyMask != nil {
			newDirtyMask := make([]bool, len(origSub.DirtyMask))
			copy(newDirtyMask, origSub.DirtyMask)
			newEntry.SubEntry[i].DirtyMask = newDirtyMask
		}

		// 주의: 만약 CohSubEntry 내부에 또 다른 슬라이스나 맵, 포인터가 추가된다면
		// 반드시 이곳에 동일한 방식의 복사 로직을 추가해주어야 합니다.
	}

	return &newEntry
}

func (e *CohEntry) Reset() {
	e.IsValid = false
	e.DemoteLocked = false
	for i := range e.SubEntry {
		sub := &e.SubEntry[i]
		sub.IsValid = false
		sub.IsDirty = false
		sub.ReadCount = 0
		sub.IsLocked = false
		sub.Sharer = nil
		sub.VAddr = 0
		sub.DirtyMask = nil
		sub.Accessed = false
	}
}
