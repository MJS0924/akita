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

type Comp struct {
	*sim.TickingComponent
	sim.MiddlewareHolder

	name     string
	deviceID int

	topPort          sim.Port
	bottomPort       sim.Port
	remoteBottomPort sim.Port
	controlPort      sim.Port
	RDMAPort         sim.Port
	RDMAInvPort      sim.Port
	ToRDMA           sim.RemotePort
	ToRDMAInv        sim.RemotePort

	// [수정 코드] 자원을 Local과 Remote로 완전 분리
	localDirStageBuffer  sim.Buffer
	remoteDirStageBuffer sim.Buffer
	dirStageAckBuffer    sim.Buffer // [추가] Ack 전용 물리적 분리 버퍼
	dirStageMotionBuffer sim.Buffer // [추가] promotion/demotion 전용 물리적 분리 버퍼

	localDirToBankBuffers  []sim.Buffer
	remoteDirToBankBuffers []sim.Buffer

	localMshrStageBuffer  sim.Buffer
	remoteMshrStageBuffer sim.Buffer

	localBottomSenderBuffer  sim.Buffer
	remoteBottomSenderBuffer sim.Buffer

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
func (c *Comp) EventCounts() map[string]uint64 {
	out := make(map[string]uint64, len(c.eventCounts)+32)
	for k, v := range c.eventCounts {
		out[k] = v
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
