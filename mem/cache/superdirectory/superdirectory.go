package superdirectory

import (
	"fmt"
	"log"
	"reflect"
	"strings"

	"github.com/sarchlab/akita/v4/mem/cache/superdirectory/internal"
	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"

	"github.com/sarchlab/akita/v4/sim"
)

type cacheState int

const (
	cacheStateInvalid cacheState = iota
	cacheStateRunning
	cacheStatePreFlushing
	cacheStateFlushing
	cacheStatePaused
)

// deferredRedirectEntry tracks a doWriteMiss trans whose Fix B-2
// coarser-bank reprobe found an existing coarser entry but couldn't
// dispatch because that coarser bank's pipeline was full. The trans
// is parked here until the targeted pipeline drains, breaking the
// head-of-line deadlock that arises when many concurrent
// useRsbHintAlloc-driven coarse allocs saturate every coarser
// bank's pipeline simultaneously (A2 fir / matmul deadlock pattern,
// CYCLE_FINDINGS.md §13).
type deferredRedirectEntry struct {
	trans      *transaction
	targetBank int
	isLocal    bool
}

type Comp struct {
	*sim.TickingComponent
	sim.MiddlewareHolder

	name     string
	deviceID int

	topPort          sim.Port
	// D4: dedicated L1-facing InvRsp ingress. L1 sends InvRsp back to
	// the directory using `WithDst(req.Src)` where req.Src was set to
	// this port's AsRemote() (see bottomSender sendInvalidationRequest /
	// sendInvalidationRequestByWrite / processInvalidationReq). Without
	// this split, L1's InvRsp competed with ReadReq at topPort's FIFO;
	// when localBypassBuffer was full, an L1 ReadReq stalled at the head
	// blocked InvRsp delivery → inflightInvToOutside never drained →
	// cross-GPU backpressure (mirror of D1's RDMA-side split, but on
	// the L1-facing side).
	topInvRspPort    sim.Port
	bottomPort       sim.Port
	remoteBottomPort sim.Port
	controlPort      sim.Port
	RDMAPort         sim.Port
	RDMAInvPort      sim.Port
	// D1: separate InvRsp incoming channel. RDMAInvPort previously
	// carried both InvReq and InvRsp in the same FIFO; when the head
	// was an InvReq and invReqBuffer was full, InvRsp behind it could
	// not be retrieved → upstream SD's inflightInvToBottom never
	// drained → cross-GPU cyclic backpressure (A2 fir deadlock at
	// win=495 when useRsbHintAlloc=true triggers coarse-alloc InvReq
	// broadcast bursts).
	RDMAInvRspPort   sim.Port
	// S1: dedicated egress port for outbound InvRsp (peer-bound). The
	// old design drained outbound InvRsp via RDMAInvPort (shared with
	// outbound InvReq); under a broadcast InvReq burst, the InvRsp
	// behind queued InvReqs at sendToRDMAInvQue suffered HoL stall.
	// Splitting the queue + draining via this port lets InvRsp egress
	// proceed independently of InvReq backpressure.
	RDMAInvRspOutPort sim.Port
	ToRDMA           sim.RemotePort
	ToRDMAInv        sim.RemotePort
	// D1: remote-side endpoint name for the new InvRsp channel; set by
	// the r9nano builder when plugging RDMAToCohDirForInvRsp.
	ToRDMAInvRsp     sim.RemotePort

	// [수정 코드] 자원을 Local과 Remote로 완전 분리
	localDirStageBuffer  sim.Buffer
	remoteDirStageBuffer sim.Buffer
	dirStageAckBuffer    sim.Buffer // [추가] Ack 전용 물리적 분리 버퍼
	dirStageMotionBuffer sim.Buffer // [추가] promotion/demotion 전용 물리적 분리 버퍼

	localDirToBankBuffers  []sim.Buffer
	remoteDirToBankBuffers []sim.Buffer

	localMshrStageBuffer  sim.Buffer
	remoteMshrStageBuffer sim.Buffer

	// [BSB-CLASS-SPLIT] The single localBottomSenderBuffer /
	// remoteBottomSenderBuffer was a request-only FIFO that mixed
	// data-class transactions (Nothing/InsertNewEntry/UpdateEntry ->
	// sendRequestToBottom) with inv-class transactions
	// (EvictAndInsertNewEntry/InvalidateEntry/InvalidateAndUpdateEntry/
	// EvictAndPromotionEntry/EvictAndDemotionEntry ->
	// sendInvalidationRequest). When the head was an inv-class trans
	// stalled on tooManyInflightInvalidation() (or a data-class trans
	// stalled on tooManyInflightRequest()), it head-of-line blocked
	// every trans behind it in the SAME buffer -> the buffer never
	// drained -> bankStage.finalizeTrans saw !CanPush -> backpressure
	// climbed remoteDirToBankBuffers -> remoteDirStageBuffer ->
	// RDMAPort.IncomingBuf (the relu_sd ~win134 deadlock). Splitting by
	// class (mirroring REC remoteBSBData/remoteBSBInv) lets a data-class
	// trans drain on its own Peek/Pop even while an inv-class trans is
	// capped, and vice versa. The two lanes PARTITION the original 16-deep
	// capacity (12 data + 4 inv) so total admission budget is unchanged.
	localBottomSenderBufferData  sim.Buffer
	localBottomSenderBufferInv   sim.Buffer
	remoteBottomSenderBufferData sim.Buffer
	remoteBottomSenderBufferInv  sim.Buffer

	writeBufferToBankBuffers []sim.Buffer
	invReqBuffer             sim.Buffer
	invRspBuffer             sim.Buffer
	localBypassBuffer        sim.Buffer // [추가] Local-to-Local Read 전용 고속 우회 버퍼

	topParser    *topParser
	bottomSender *bottomSender
	dirStage     *directoryStage
	bankStages   []*bankStage
	mshrStage    *mshrStage
	flusher      *flusher

	storage                         *mem.Storage
	addressToPortMapper             mem.AddressToPortMapper
	addressToPortMapperForRemoteReq mem.AddressToPortMapper // remote에서 온 요청이 L2 cache의 remoteTopPort로 routing 되도록
	l2AddressToPortMapper           mem.AddressToPortMapper // incoming request가 remote/local data에 대한 것인지 판단
	directory                       internal.SuperDirectory
	mshr                            internal.MSHR
	regionSizeBuffer                internal.RegionSizeBuffer
	log2BlockSize                   uint64
	log2PageSize                    uint64
	log2NumSubEntry                 int
	fetchSingleCacheLine            bool // true이면 miss 시 64B(1 cacheline)만 fetch
	numReqPerCycle                  int
	numBanks                        int
	regionLen                       []int

	state            cacheState
	flushLocalAccess bool
	evictingList     map[uint64]bool

	DirtyMask *[]map[vm.PID]map[uint64][]uint8
	ReadMask  *[]map[vm.PID]map[uint64][]uint8

	tickReturn     bool
	printReturn    bool
	debugPromotion bool
	debugProcess   bool
	debugAddress   uint64
	recordTime     sim.VTimeInSec

	eventLogger *EventLogger

	evictEntryUtilSum float64
	evictEntryCount   uint64

	allocationCount    uint64
	remoteAcceptCount  uint64 // diagnostic: how many times acceptNewTransaction(false) fires
	doWriteMissCount   uint64 // diagnostic: how many times doWriteMiss is reached
	doWriteMissRemote  uint64 // diagnostic: doWriteMiss with fromLocal=false

	// Stall cause counters (Method E — mirrors REC/optdirectory for
	// one-to-one comparison). Each increments when the named back-pressure
	// forces the transaction to retry next cycle.
	stallMSHRFull       uint64 // writeToBank: mshr.IsFull()
	stallSubEntryLocked uint64 // doWriteHit: sub-entry IsLocked or ReadCount > 0
	stallBankFull       uint64 // writeToBank: bankBuf.CanPush() == false
	stallEvictingList   uint64 // processTransaction: addr in evictingList
	stallVictimLocked   uint64 // doWriteMiss: victim entry locked
	stallBottomBufFull  uint64 // fast-path push to bottomSenderBuffer rejected
	stallMshrBufFull    uint64 // bankstage push to mshrStageBuffer rejected
	stallInflightFetch  uint64 // bottomSender: tooManyInflightRequest
	stallInflightInv    uint64 // bottomSender: tooManyInflightInvalidation
	stallBottomPortBusy uint64 // bottomSender: bottomPort/RDMAPort can't send
	stallTopPortBusy    uint64 // doInvalidation / response: topPort/RDMAInv can't send
	stallWriteToBankPreflight uint64 // writeToBank: MSHR cross-granularity conflict caught before mutation
	totalDoWriteCalls   uint64 // every entry into doWrite (success+retry)

	// E-RACE-DEFER: deferred redirect queue. Fix B-2's coarser-bank
	// reprobe MUST route to the existing coarser entry for SO invariant.
	// When that coarser bank's pipeline is full, the old code returned
	// false from doWriteMiss, which kept the trans pinned at the head
	// of dirStage's per-bank remoteBuf. Under useRsbHintAlloc=true,
	// many trans simultaneously hit this condition across all banks,
	// and the pipeline can't drain because trans behind them (waiting
	// for the same pipeline) block bankStage from working through
	// remoteBuf entries — closed cycle.
	//
	// Fix: when Fix B-2 hits a full pipeline, push the trans onto
	// deferredRedirects (consuming the head of remoteBuf via
	// `return true`) and retry from this queue each Tick. This breaks
	// the head-of-line block — other trans in remoteBuf can now make
	// progress, draining the saturated pipeline so the deferred trans
	// eventually get their target slot.
	deferredRedirects []deferredRedirectEntry
	deferredRedirectCount uint64 // diagnostic: how many trans were ever deferred

	// H3e fix counter: number of demote triggers that hit a DemoteLocked
	// entry and were converted to invalidate-only. Reports cascade-prevention
	// work performed by the demote-lock fix.
	demoteLockHits uint64

	// disableDemoteLock bypasses the demote-lock skip in mshrStage so every
	// demote trigger goes down the cascading path. Used for A/B comparison
	// against the lock-on default. The DemoteLocked flag itself is still
	// set/cleared as usual so the diagnostic counter remains comparable.
	disableDemoteLock bool

	// promoteRelaxed switches the promotion eligibility check from the strict
	// AbleToPromotion (all sub-entries valid AND all sharer sets equal) to
	// AbleToPromotionRelaxed (any valid sub-entry, sharer = union of valid
	// sub-entry sharers). Lets entries return to a finer region after
	// coarsening; gated for A/B comparison.
	promoteRelaxed bool

	// useRsbHintAlloc makes doWriteMiss honor the RSB hint (carried on the
	// transaction) when allocating a new entry, instead of always defaulting
	// to the finest bank. Closes the RSB feedback loop: previously RSB hits
	// were consumed by lookup-only and the allocation always landed at finest.
	useRsbHintAlloc bool

	// recordSilentEvict makes the directory write its non-finest victim's
	// bank into RSB on every eviction (write-miss / promotion / demotion
	// target replacements), not just on eviction-with-sharers. Without this,
	// RSB.Update only fires from processInvRsp which requires invalidation
	// responses — for workloads with little cross-GPU sharing, RSB stays
	// effectively empty even though evictions are common.
	recordSilentEvict bool

	// §4 promote-at-evict configuration. promoteAtEvict (v1) enables the
	// feature at the finest bank with the lenient PromoteOnEvictEligible
	// predicate. promoteAtEvictBiasVictim (v2) additionally biases finest-
	// bank victim selection toward promote-eligible entries. The default
	// for v2 is true when v1 is on. promoteAtEvictMultiBank (v3) extends
	// the feature to non-finest, non-coarsest banks using the stricter
	// AbleToPromotionRelaxed predicate.
	promoteAtEvict           bool
	promoteAtEvictBiasVictim bool
	promoteAtEvictMultiBank  bool
	// promoteAtEvictCount counts how many times doWriteMiss successfully
	// converted a victim into a promotion at eviction time.
	promoteAtEvictCount uint64

	// [BANK-LEVEL PROMOTE/DEMOTE TRACKING]
	// Per-bank counters: index = bankID (0 coarsest .. numBanks-1 finest).
	// Incremented in bankStage.FinalizePromotionEntry / FinalizeDemotionEntry
	// at the point the action actually commits.
	//
	// SD promotes/demotes move an entry across banks (coarser ↔ finer).
	// Tracking per-bank rates exposes where the hierarchy is most active
	// (e.g., heavy promote from bank 4 → 3 indicates locality coarsening,
	// heavy demote from bank 0 → 1 indicates utilization collapse). The
	// global promoteAtEvictCount only counts evict-time promotions at the
	// finest bank under the §4 feature; these per-bank counters cover ALL
	// promotion/demotion paths regardless of trigger.
	//
	// Allocated to size numBanks in builder after numBanks is set.
	promoteCountByBank []uint64
	demoteCountByBank  []uint64

	// OP5 deviation regression slots (PHASE C-2). Increment sites are
	// intentionally absent in the post-fix code: a non-zero value means
	// either (a) a future change reintroduced the buggy branch and wired
	// the counter back, or (b) someone added a new code path that
	// re-exhibits the deviation. Either case is a regression.
	op5aShortcutWithRemoteSharer uint64 // local write hit took the no-inv shortcut despite a remote sharer
	// Note: superdirectory's OP5b is PROTOCOL-INTENTIONAL (paper-correct
	// at the finest bank, by-design demote at coarser banks — see
	// cross_model_op5_audit.md C1.3). This counter records the "writer
	// cleared at finest bank" case only, which should never fire.
	op5bWriterClearedAtFinestBank uint64

	// eventCounts replaces high-frequency tracing.AddTaskStep calls with
	// in-memory counters to keep akita_sim_*.sqlite trace files small.
	// report.go's eventCountsProvider reads this map at simulation end.
	eventCounts map[string]uint64
}

func (c *Comp) incEvent(name string) {
	if c.eventCounts == nil {
		c.eventCounts = make(map[string]uint64)
	}
	c.eventCounts[name]++
}

// EventCounts returns a copy of the in-memory event counters. Per-bank
// CBF FPR counters are merged in under the "CBF_*_<bank>" namespace so
// they flow through the existing eventCountsProvider -> sqlite pipeline.
// [BANK-LEVEL TRACKING] Per-bank promote/demote counters are merged in
// under the "promote_bank<N>" / "demote_bank<N>" namespace using the
// same mechanism, so they reach summary.csv and sqlite without any
// changes to the reporting layer.
func (c *Comp) EventCounts() map[string]uint64 {
	out := make(map[string]uint64, len(c.eventCounts)+32)
	for k, v := range c.eventCounts {
		out[k] = v
	}
	// Per-bank promote/demote counters.
	for bank, v := range c.promoteCountByBank {
		out[fmt.Sprintf("promote_bank%d", bank)] = v
	}
	for bank, v := range c.demoteCountByBank {
		out[fmt.Sprintf("demote_bank%d", bank)] = v
	}
	// [BANK-LEVEL ENTRY ACTIVITY + COVERAGE — END-OF-SIM SNAPSHOT]
	// At the moment EventCounts() is called (typically at simulation
	// end via reportingProvider), capture per-bank entry counts and
	// cache-line coverage so the sqlite/summary.csv pipeline records
	// the final directory layout per bank. Per-window evolution is
	// recorded separately by runner/windowsnapshot.go.
	if c.directory != nil {
		validEntries := c.PerBankValidEntries()
		maxEntries := c.PerBankMaxEntries()
		validSub := c.PerBankValidSubEntries()
		coverage := c.CoverageByBank()
		maxCoverage := c.MaxCoverageByBank()
		// Per-bank util ×1e6 (avoid float in uint64 map; consumers
		// divide by 1e6 to recover ratio).
		utilPpm := c.PerBankUtilization()
		for i := 0; i < len(validEntries); i++ {
			prefix := fmt.Sprintf("bank%d_", i)
			out[prefix+"valid_entries"] = uint64(validEntries[i])
			out[prefix+"max_entries"] = uint64(maxEntries[i])
			out[prefix+"valid_subentries"] = uint64(validSub[i])
			out[prefix+"coverage_cachelines"] = uint64(coverage[i])
			out[prefix+"max_coverage_cachelines"] = uint64(maxCoverage[i])
			out[prefix+"util_ppm"] = uint64(utilPpm[i] * 1e6)
		}
	}
	if c.directory != nil {
		for bank, s := range c.directory.CBFStats() {
			if s.NumEntries == 0 {
				continue // no CBF on this bank
			}
			prefix := fmt.Sprintf("CBF_bank%d_", bank)
			out[prefix+"queries"] = s.Queries
			out[prefix+"positives"] = s.Positives
			out[prefix+"true_positives"] = s.TruePositives
			out[prefix+"false_positives"] = s.FalsePositives
			out[prefix+"true_negatives"] = s.TrueNegatives
			out[prefix+"false_negatives"] = s.FalseNegatives
			out[prefix+"num_entries"] = s.NumEntries
			out[prefix+"inserts"] = s.Inserts
			out[prefix+"evicts"] = s.Evicts
			out[prefix+"overflowed"] = s.NumOverflowed
			// Embed FPR×1e6 as an integer so the count-typed metric carries
			// a usable ratio in the same sqlite column.
			negatives := s.FalsePositives + s.TrueNegatives
			if negatives > 0 {
				out[prefix+"fpr_ppm"] = uint64(float64(s.FalsePositives) /
					float64(negatives) * 1e6)
			}
		}
	}
	return out
}

// ActionCounts returns the diagnostic counters in a uniform map for
// summary.csv emission (matches the keys used by REC and optdirectory).
// op5a_/op5b_ keys are PHASE C-2 regression slots and are expected to be 0
// in the post-fix codebase.
func (c *Comp) ActionCounts() map[string]uint64 {
	return map[string]uint64{
		"allocation_count":     c.allocationCount,
		"remote_accept_count":  c.remoteAcceptCount,
		"do_write_miss":        c.doWriteMissCount,
		"do_write_miss_remote": c.doWriteMissRemote,
		"stall_mshr_full":          c.stallMSHRFull,
		"stall_subentry_locked":    c.stallSubEntryLocked,
		"stall_bank_full":          c.stallBankFull,
		"stall_evicting_list":      c.stallEvictingList,
		"stall_victim_locked":      c.stallVictimLocked,
		"stall_bottom_buf_full":    c.stallBottomBufFull,
		"stall_mshr_buf_full":      c.stallMshrBufFull,
		"stall_inflight_fetch":     c.stallInflightFetch,
		"stall_inflight_inv":       c.stallInflightInv,
		"stall_bottom_port_busy":   c.stallBottomPortBusy,
		"stall_top_port_busy":      c.stallTopPortBusy,
		"stall_write_to_bank_preflight": c.stallWriteToBankPreflight,
		"total_dowrite_calls":      c.totalDoWriteCalls,
		"demote_lock_hits":         c.demoteLockHits,
		"op5a_shortcut_with_remote_sharer":     c.op5aShortcutWithRemoteSharer,
		"op5b_writer_cleared_at_finest_bank":   c.op5bWriterClearedAtFinestBank,
		"promote_at_evict_count":               c.promoteAtEvictCount,
	}
}

func (c *Comp) AvgEvictUtilization() float64 {
	if c.evictEntryCount == 0 {
		return 0
	}
	return c.evictEntryUtilSum / float64(c.evictEntryCount)
}

// CurrentValidEntryUtilization scans every directory entry that is
// currently valid (IsValidEntry == true) and averages the per-entry
// sub-entry utilization (validSubEntries / totalSubEntries). Invalid
// entries are excluded from the average. Returns (avgUtil, validEntries)
// where avgUtil ∈ [0,1] and validEntries is the count of valid entries
// observed. Safe to call between simulation ticks (SerialEngine guarantee).
//
// Use case: per-window snapshot wants the live utilization of the
// directory at every checkpoint — not just the eviction-time samples
// recorded by AvgEvictUtilization.
//
// Returns (avgUtil, validEntries, totalCacheLines). totalCacheLines is
// the directory's cache-line coverage capacity. UNLIKE REC, each SD
// sub-entry covers a region of size 2^regionLen[bankID] bytes — the
// number of cache lines a sub-entry can cover therefore depends on
// which bank the parent entry sits in:
//
//	cachelines_per_subentry(bankID) = 1 << (regionLen[bankID] - log2BlockSize)
//
// builder.go sets regionLen = {14,12,10,8,6} (bank 0 coarsest .. bank 4
// finest at log2BlockSize=6,log2NumSubEntry=2), so coverage per valid
// sub-entry ranges from 1 cacheline (finest) up to 256 cachelines
// (coarsest). Earlier versions of this method counted each valid
// sub-entry as 1 cacheline irrespective of bank, which under-reports
// coverage by a factor of up to 256× for coarse banks.
//
// [COVERAGE FORMULA VERIFICATION]
// totalCacheLines = Σ (per valid entry: validSubEntries × cachelinesPerSub)
//                 = Σ (per valid entry: validSubEntries × regionSize / cacheLineSize)
// where regionSize = 1<<regionLen[bankID], cacheLineSize = 1<<log2BlockSize.
// This MATCHES the user-specified formula
//   coverage = Σ (subentries × regionSize/cacheLineSize)
// when "subentries" means VALID sub-entries (the only sub-entries that
// actually track address space) and the sum is over all valid entries
// across all banks. See also MaxCoverageCacheLines below for the
// always-valid (= directory capacity) variant.
func (c *Comp) CurrentValidEntryUtilization() (float64, int, int) {
	banks := c.directory.GetBanks()
	var sum float64
	count := 0
	totalCacheLines := 0
	for bankIdx, bank := range banks {
		// 1 << (regionLen[bankIdx] - log2BlockSize) cache lines per
		// valid sub-entry in this bank.
		cachelinesPerSub := 1 << (c.regionLen[bankIdx] - int(c.log2BlockSize))
		for _, set := range bank {
			for _, entry := range set.CohEntries {
				if entry == nil || !entry.IsValidEntry() {
					continue
				}
				numSub := len(entry.SubEntry)
				if numSub == 0 {
					continue
				}
				validSub := 0
				for k := 0; k < numSub; k++ {
					if entry.SubEntry[k].IsValid {
						validSub++
					}
				}
				sum += float64(validSub) / float64(numSub)
				count++
				totalCacheLines += validSub * cachelinesPerSub
			}
		}
	}
	if count == 0 {
		return 0, 0, 0
	}
	return sum / float64(count), count, totalCacheLines
}

// [COVERAGE FORMULA VERIFICATION HELPERS]

// MaxCoverageCacheLines returns the theoretical maximum number of cache
// lines this directory could track when every entry's every sub-entry
// is valid:
//
//	max_coverage = Σ_banks (numEntriesInBank × numSubEntriesPerEntry × cachelinesPerSubentry)
//	             = Σ_banks (numEntriesInBank × (1<<log2NumSubEntry) × (1<<(regionLen[bank]-log2BlockSize)))
//
// This is a static configuration property (set by builder); useful as a
// denominator for "how much of the address space is the directory
// CURRENTLY tracking" ratio.
func (c *Comp) MaxCoverageCacheLines() int {
	banks := c.directory.GetBanks()
	numSub := 1 << c.log2NumSubEntry
	total := 0
	for bankIdx, bank := range banks {
		cachelinesPerSub := 1 << (c.regionLen[bankIdx] - int(c.log2BlockSize))
		// numEntriesInBank = (number of sets in this bank) × wayAssociativity
		nEntries := 0
		for _, set := range bank {
			nEntries += len(set.CohEntries)
		}
		total += nEntries * numSub * cachelinesPerSub
	}
	return total
}

// CoverageByBank returns the per-bank breakdown of current valid cache-
// line coverage (valid sub-entries × cachelinesPerSub). The slice index
// is bankID. The sum over the slice equals the totalCacheLines that
// CurrentValidEntryUtilization() returns, providing the contribution of
// each bank to the directory's tracked address-space coverage.
func (c *Comp) CoverageByBank() []int {
	banks := c.directory.GetBanks()
	out := make([]int, len(banks))
	for bankIdx, bank := range banks {
		cachelinesPerSub := 1 << (c.regionLen[bankIdx] - int(c.log2BlockSize))
		for _, set := range bank {
			for _, entry := range set.CohEntries {
				if entry == nil || !entry.IsValidEntry() {
					continue
				}
				validSub := 0
				for k := 0; k < len(entry.SubEntry); k++ {
					if entry.SubEntry[k].IsValid {
						validSub++
					}
				}
				out[bankIdx] += validSub * cachelinesPerSub
			}
		}
	}
	return out
}

// MaxCoverageByBank returns per-bank static maximum cache-line coverage
// (= numEntriesInBank × numSubEntriesPerEntry × cachelinesPerSub).
// Useful as a per-bank denominator. Sum matches MaxCoverageCacheLines().
func (c *Comp) MaxCoverageByBank() []int {
	banks := c.directory.GetBanks()
	numSub := 1 << c.log2NumSubEntry
	out := make([]int, len(banks))
	for bankIdx, bank := range banks {
		cachelinesPerSub := 1 << (c.regionLen[bankIdx] - int(c.log2BlockSize))
		nEntries := 0
		for _, set := range bank {
			nEntries += len(set.CohEntries)
		}
		out[bankIdx] = nEntries * numSub * cachelinesPerSub
	}
	return out
}

// PerBankValidEntries returns per-bank counts of parent entries with
// IsValidEntry()==true. Index = bankID.
func (c *Comp) PerBankValidEntries() []int {
	banks := c.directory.GetBanks()
	out := make([]int, len(banks))
	for bankIdx, bank := range banks {
		for _, set := range bank {
			for _, entry := range set.CohEntries {
				if entry != nil && entry.IsValidEntry() {
					out[bankIdx]++
				}
			}
		}
	}
	return out
}

// PerBankMaxEntries returns the static maximum number of entries per
// bank (= numSets × wayAssociativity).
func (c *Comp) PerBankMaxEntries() []int {
	banks := c.directory.GetBanks()
	out := make([]int, len(banks))
	for bankIdx, bank := range banks {
		for _, set := range bank {
			out[bankIdx] += len(set.CohEntries)
		}
	}
	return out
}

// PerBankValidSubEntries returns the total number of valid sub-entries
// per bank. CoverageByBank[i] = PerBankValidSubEntries[i] × (regionSize
// at bank i / cacheLineSize).
func (c *Comp) PerBankValidSubEntries() []int {
	banks := c.directory.GetBanks()
	out := make([]int, len(banks))
	for bankIdx, bank := range banks {
		for _, set := range bank {
			for _, entry := range set.CohEntries {
				if entry == nil || !entry.IsValidEntry() {
					continue
				}
				for k := 0; k < len(entry.SubEntry); k++ {
					if entry.SubEntry[k].IsValid {
						out[bankIdx]++
					}
				}
			}
		}
	}
	return out
}

// PerBankUtilization returns per-bank average sub-entry fill ratio
// (validSub/numSub averaged across valid entries in that bank). 0 if no
// valid entries.
func (c *Comp) PerBankUtilization() []float64 {
	banks := c.directory.GetBanks()
	numSub := 1 << c.log2NumSubEntry
	out := make([]float64, len(banks))
	for bankIdx, bank := range banks {
		var sum float64
		count := 0
		for _, set := range bank {
			for _, entry := range set.CohEntries {
				if entry == nil || !entry.IsValidEntry() {
					continue
				}
				validSub := 0
				for k := 0; k < len(entry.SubEntry); k++ {
					if entry.SubEntry[k].IsValid {
						validSub++
					}
				}
				sum += float64(validSub) / float64(numSub)
				count++
			}
		}
		if count > 0 {
			out[bankIdx] = sum / float64(count)
		}
	}
	return out
}

// PerBankPromoteCounts returns a copy of bank-level cumulative promote
// counts for external samplers (per-window snapshot). Index = bankID.
func (c *Comp) PerBankPromoteCounts() []uint64 {
	out := make([]uint64, len(c.promoteCountByBank))
	copy(out, c.promoteCountByBank)
	return out
}

// PerBankDemoteCounts returns a copy of bank-level cumulative demote
// counts. Index = bankID.
func (c *Comp) PerBankDemoteCounts() []uint64 {
	out := make([]uint64, len(c.demoteCountByBank))
	copy(out, c.demoteCountByBank)
	return out
}

func (c *Comp) EvictCount() uint64 {
	return c.evictEntryCount
}

// AllocationCount returns the total number of directory entries allocated during the simulation.
func (c *Comp) AllocationCount() uint64 {
	return c.allocationCount
}

// DiagCounts returns diagnostic counters for investigation.
func (c *Comp) DiagCounts() (remoteAccept, doWriteMiss, doWriteMissRemote uint64) {
	return c.remoteAcceptCount, c.doWriteMissCount, c.doWriteMissRemote
}

// EventLogger returns the EventLogger attached to this cache component.
// The caller can call Enable() on it before simulation starts, and read
// Events() after simulation completes.
func (c *Comp) EventLogger() *EventLogger { return c.eventLogger }

// BankEntryCount returns the number of valid entries in bankID.
// Safe to call only between simulation ticks (SerialEngine guarantee).
func (c *Comp) BankEntryCount(bankID int) int {
	banks := c.directory.GetBanks()
	if bankID < 0 || bankID >= len(banks) {
		return 0
	}
	count := 0
	for _, set := range banks[bankID] {
		for _, entry := range set.CohEntries {
			if entry.IsValidEntry() {
				count++
			}
		}
	}
	return count
}

// BankMaxCapacity returns the maximum number of entries (sets × ways) in bankID.
func (c *Comp) BankMaxCapacity(bankID int) int {
	banks := c.directory.GetBanks()
	if bankID < 0 || bankID >= len(banks) {
		return 0
	}
	total := 0
	for _, set := range banks[bankID] {
		total += len(set.CohEntries)
	}
	return total
}

// NumBanks returns the number of banks in this superdirectory.
func (c *Comp) NumBanks() int { return c.numBanks }

// DeviceID returns the GPU device ID this superdirectory belongs to.
func (c *Comp) DeviceID() int { return c.deviceID }

func (c *Comp) SetAddressToPortMapper(lmf mem.AddressToPortMapper) {
	c.addressToPortMapper = lmf
}

func (c *Comp) SetAddressToPortMapperForRemoteReq(lmf mem.AddressToPortMapper) {
	c.addressToPortMapperForRemoteReq = lmf
}

func (c *Comp) SetL2AddressToPortMapper(lmf mem.AddressToPortMapper) {
	c.l2AddressToPortMapper = lmf
}

func (c *Comp) Tick() bool {
	return c.MiddlewareHolder.Tick()
}

type middleware struct {
	*Comp
}

func (m *middleware) Tick() bool {
	m.printReturn = false
	m.debugPromotion = false
	m.debugProcess = false
	m.debugAddress = 21475446080
	// now := m.Engine.CurrentTime()
	// if now >= m.recordTime+0.00002 {
	// 	m.recordTime = now
	// m.printReturn = true
	// }

	madeProgress := false

	if m.state != cacheStatePaused {
		madeProgress = m.runPipeline() || madeProgress
	}

	madeProgress = m.flusher.Tick() || madeProgress

	m.tickReturn = madeProgress
	return madeProgress
}

func (m *middleware) runPipeline() bool {
	madeProgress := false

	madeProgress = m.runStage(m.mshrStage) || madeProgress
	madeProgress = m.runStage(m.bottomSender) || madeProgress

	for _, bs := range m.bankStages {
		madeProgress = bs.Tick() || madeProgress
	}

	madeProgress = m.runStage(m.dirStage) || madeProgress
	madeProgress = m.runStage(m.topParser) || madeProgress

	return madeProgress
}

func (m *middleware) runStage(stage sim.Ticker) bool {
	madeProgress := false
	for i := 0; i < m.numReqPerCycle; i++ {
		madeProgress = stage.Tick() || madeProgress
	}

	return madeProgress
}

func (c *Comp) discardInflightTransactions() {
	// banks := c.directory.GetBanks()
	// for _, sets := range banks {
	// 	for _, set := range sets {
	// 		for _, block := range set.CohEntries {
	// 			for _, subEntry := range block.SubEntry {
	// 				subEntry.ReadCount = 0
	// 				subEntry.IsLocked = false
	// 			}
	// 		}
	// 	}
	// }

	c.directory.Reset()
	c.dirStage.Reset()
	c.regionSizeBuffer.Reset()

	for _, bs := range c.bankStages {
		bs.Reset()
	}

	c.mshrStage.Reset()
	c.bottomSender.Reset()

	clearPort(c.topPort)

}

func (c *Comp) discardMsgToLocal() {
	temp := []sim.Msg{}

	for c.topPort.PeekOutgoing() != nil {
		temp = append(temp, c.topPort.RetrieveOutgoing())
	}

	for _, msg := range temp {
		if strings.Contains(fmt.Sprintf("%s", msg.Meta().Dst), "RDMA") {
			c.topPort.Send(msg)
		}
	}
}

func (c *Comp) cloneReq(origin mem.AccessReq) mem.AccessReq {
	switch origin := origin.(type) {
	case *mem.ReadReq:
		read := mem.ReadReqBuilder{}.
			WithSrc(origin.Src).
			WithDst(origin.Dst).
			WithReqFrom(origin.ReqFrom).
			WithPID(origin.GetPID()).
			WithAddress(origin.Address).
			WithVAddr(origin.GetVAddr()).
			WithByteSize(origin.AccessByteSize).
			Build()
		read.SetSrcRDMA(origin.SrcRDMA)
		return read
	case *mem.WriteReq:
		write := mem.WriteReqBuilder{}.
			WithSrc(origin.Src).
			WithDst(origin.Dst).
			WithReqFrom(origin.ReqFrom).
			WithPID(origin.GetPID()).
			WithAddress(origin.Address).
			WithVAddr(origin.GetVAddr()).
			WithData(origin.Data).
			WithDirtyMask(origin.DirtyMask).
			// WithInfo((*(c.dirtyMask))[c.deviceID-1][origin.GetPID()][origin.GetVAddr()>>c.log2PageSize]).
			Build()
		write.SetSrcRDMA(origin.SrcRDMA)
		return write
	default:
		log.Panicf("cannot clone request of type %s",
			reflect.TypeOf(origin))
	}
	return nil
}

func (c *Comp) cloneRsp(origin mem.AccessRsp, rspTo string, addr uint64) mem.AccessRsp {
	switch origin := origin.(type) {
	case *mem.DataReadyRsp:
		rsp := mem.DataReadyRspBuilder{}.
			WithSrc(origin.Src).
			WithDst(origin.Dst).
			WithRspTo(rspTo).
			WithData(origin.Data).
			WithOrigin(origin.Origin).
			Build()
		return rsp
	case *mem.WriteDoneRsp:
		rsp := mem.WriteDoneRspBuilder{}.
			WithSrc(origin.Src).
			WithDst(origin.Dst).
			WithRspTo(rspTo).
			WithOrigin(origin.Origin).
			Build()
		return rsp
	default:
		log.Panicf("cannot clone request of type %s",
			reflect.TypeOf(origin))
	}
	return nil
}

func (c *Comp) fromLocal(msg sim.Msg) bool {
	if !strings.Contains(fmt.Sprintf("%s", msg.Meta().Src), "RDMA") {
		return true
	}

	return false
}

func (c *Comp) toLocal(addr uint64) bool {
	port := c.l2AddressToPortMapper.Find(addr)
	if !strings.Contains(fmt.Sprintf("%s", port), "RDMA") {
		return true
	}

	return false
}
