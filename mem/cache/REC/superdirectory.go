package REC

import (
	"fmt"
	"log"
	"reflect"
	"strings"

	"github.com/sarchlab/akita/v4/mem/cache/REC/internal"
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
	// [R2] Split peer-facing data ingress into REQ vs RSP ports so a
	// stalled REQ stream cannot HoL-block returning RSPs (and vice
	// versa).  Paired with R1's split RDMA outside ports
	// (RDMADataReqInside / RDMADataRspInside).  RDMAPort is retained
	// only as a legacy field; new traffic flows through the split
	// ports.
	RDMADataReqPort  sim.Port
	RDMADataRspPort  sim.Port
	// RDMAInvPort is conceptually the InvReq ingress (carried under
	// the historical iter7 field name); RDMAInvRspPort is the InvRsp
	// ingress added in iter7.
	RDMAInvPort      sim.Port
	RDMAInvRspPort   sim.Port
	// [ITER19] Dedicated InvRsp EGRESS port on the ForInvRsp connection
	// (mirrors SD/HMG RDMAInvRspOutPort). Keeps outbound InvRsp off the
	// InvReq connection (RDMAInvPort/ForInv) and off the InvRsp ingress
	// (RDMAInvRspPort, polled by topparser), so req/rsp stay fully split.
	RDMAInvRspOutPort sim.Port
	ToRDMA           sim.RemotePort
	ToRDMADataReq    sim.RemotePort // [R2] paired with R1 RDMADataReqInside
	ToRDMADataRsp    sim.RemotePort // [R2] paired with R1 RDMADataRspInside
	ToRDMAInv        sim.RemotePort
	ToRDMAInvRsp     sim.RemotePort

	// [수정 코드] 자원을 Local과 Remote로 완전 분리
	localDirStageBuffer  sim.Buffer
	remoteDirStageBuffer sim.Buffer
	dirStageAckBuffer    sim.Buffer // [추가] Ack 전용 물리적 분리 버퍼
	dirStageMotionBuffer sim.Buffer // [추가] promotion/demotion 전용 물리적 분리 버퍼

	localDirToBankBuffers  sim.Buffer
	remoteDirToBankBuffers sim.Buffer

	localMshrStageBuffer  sim.Buffer
	remoteMshrStageBuffer sim.Buffer

	// [R4] BottomSenderBuffer split by trans.action class to avoid HoL
	// blocking between data-path trans (Nothing/InsertNewEntry/UpdateEntry)
	// and inv-path trans (EvictAndInsertNewEntry/InvalidateEntry/
	// InvalidateAndUpdateEntry/RemoteWriteHitPreserveWriter). Inv-path
	// trans can stall on inflightInvToOutside / maxInflightInvalidation
	// caps; before the split, that stall HoL-blocked data-path trans at
	// the BSB head, dropping useful work behind unrelated inv pressure.
	// processInputReq drains both classes per tick (inv first within each
	// side, remote-side first overall to preserve cross-GPU progress).
	localBSBData  sim.Buffer
	localBSBInv   sim.Buffer
	remoteBSBData sim.Buffer
	remoteBSBInv  sim.Buffer

	writeBufferToBankBuffers sim.Buffer
	invReqBuffer             sim.Buffer
	invRspBuffer             sim.Buffer
	localBypassBuffer        sim.Buffer // [추가] Local-to-Local Read 전용 고속 우회 버퍼
	// [R2] Dedicated drain target for the RDMADataRspPort topparser
	// loop.  Returning peer data RSPs land here so they are isolated
	// from data REQ flow (remoteDirStageBuffer) and INV traffic
	// (invReqBuffer / invRspBuffer).
	remoteBypassBuffer sim.Buffer

	topParser    *topParser
	bottomSender *bottomSender
	dirStage     *directoryStage
	bankStages   *bankStage
	mshrStage    *mshrStage
	flusher      *flusher

	storage                         *mem.Storage
	addressToPortMapper             mem.AddressToPortMapper
	addressToPortMapperForRemoteReq mem.AddressToPortMapper // remote에서 온 요청이 L2 cache의 remoteTopPort로 routing 되도록
	l2AddressToPortMapper           mem.AddressToPortMapper // incoming request가 remote/local data에 대한 것인지 판단
	directory                       internal.RECDirectory
	mshr                            internal.MSHR
	log2BlockSize                   uint64
	log2PageSize                    uint64
	log2NumSubEntry                 int
	numReqPerCycle                  int
	numBanks                        int

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

	evictEntryUtilSum float64
	evictEntryCount   uint64

	// Diagnostic counters for silent eviction analysis
	allocCount             uint64 // FindVictim returned alloc=true (need to allocate new slot)
	needEvictionCount      uint64 // alloc=true && needEviction=true (real eviction with sharers)
	silentResetCount       uint64 // alloc=true && needEviction=false (Reset clears entry without inv)
	defensiveCleanupCount  uint64 // "혹시 몰라서" loop in InvalidateAndUpdateEntry fired
	invSentCount           uint64 // sendInvalidationRequest sent at least one inv message
	invSkippedSelfOnlyCount uint64 // sendInvalidationRequest had no targets (only-self sharer)

	// Per-action dispatch counts (counted when transaction successfully advances)
	actNothing       uint64
	actInsertNew     uint64
	actUpdate        uint64
	actEvictInsert   uint64
	actInvalidateEnt uint64
	actInvUpdate     uint64
	actBypass        uint64

	// Forwarding counts: how many requests actually reach L2 / RDMA
	bottomSendCount uint64 // sendRequestToBottom returned true
	mshrFwdCount    uint64 // mshrStage pushed a coalesced request to bottomSenderBuffer

	// Stall cause counters (Method E — directory bottleneck analysis).
	// Each counter increments when the named back-pressure forces the
	// transaction to retry next cycle. Naming is shared with optdirectory
	// so REC vs CD comparison is one-to-one.
	stallMSHRFull       uint64 // writeToBank: mshr.IsFull()
	stallSubEntryLocked uint64 // doWriteHit: sub-entry IsLocked or ReadCount > 0
	stallBankFull       uint64 // writeToBank: bankBuf.CanPush() == false (alias: stallBankBufFull)
	stallEvictingList   uint64 // processTransaction: addr in evictingList
	stallVictimLocked   uint64 // doWriteMiss: victim entry locked (split from stallMSHRFull)
	stallBottomBufFull  uint64 // fast-path push to bottomSenderBuffer rejected
	stallMshrBufFull    uint64 // bankstage push to mshrStageBuffer rejected
	stallInflightFetch  uint64 // bottomSender: tooManyInflightRequest
	stallInflightInv    uint64 // bottomSender: tooManyInflightInvalidation
	stallBottomPortBusy uint64 // bottomSender: bottomPort/RDMAPort can't send
	stallTopPortBusy    uint64 // doInvalidation / response: topPort/RDMAInv can't send
	totalDoWriteCalls   uint64 // every entry into doWrite (success+retry)

	// [DOWRITE-TRACE] origin-split doWrite outcome counters. The shared
	// stall counters above cannot tell whether a PEER (remote, fromLocal=
	// false) request is being rejected/served vs a LOCAL one. These split by
	// origin to confirm whether peer evictions are rejected at the MSHR-hit-
	// for-write branch (directorystage.go doWrite). Removable; diagnostic-only.
	stallMSHRHitWriteLocal  uint64 // doWrite: write hit an existing MSHR entry -> return false (local origin)
	stallMSHRHitWriteRemote uint64 // doWrite: write hit an existing MSHR entry -> return false (PEER origin) <-- key
	doWriteMSHRHitLocal     uint64 // doWrite -> doWriteMSHRHit success (local)
	doWriteMSHRHitRemote    uint64 // doWrite -> doWriteMSHRHit success (peer)
	doWriteHitLocal         uint64 // doWrite -> doWriteHit (local)
	doWriteHitRemote        uint64 // doWrite -> doWriteHit (peer)
	doWriteMissLocal        uint64 // doWrite -> doWriteMiss (local)
	doWriteMissRemote       uint64 // doWrite -> doWriteMiss (peer)

	// Queueing-delay accumulators (Method E2). Track per-trans wait time
	// in two phases:
	//   waitDir   = bottomEnterTime - enterTime (in dirStage + bank)
	//   waitBottom = exitTime - bottomEnterTime (in bottomSender)
	// Sum is in seconds (sim.VTimeInSec). For breakdown, separate sums by
	// path category (bypass / fast / bank).
	waitDirSum_bypass    sim.VTimeInSec
	waitBottomSum_bypass sim.VTimeInSec
	waitCount_bypass     uint64

	waitDirSum_fast      sim.VTimeInSec
	waitBottomSum_fast   sim.VTimeInSec
	waitCount_fast       uint64

	waitDirSum_bank      sim.VTimeInSec
	waitBottomSum_bank   sim.VTimeInSec
	waitCount_bank       uint64

	// OP5 deviation regression slots (PHASE C-2). Increment sites are
	// intentionally absent in the post-fix code: a non-zero value means
	// either (a) a future change reintroduced the buggy branch and wired
	// the counter back, or (b) someone added a new code path that
	// re-exhibits the deviation. Either case is a regression.
	op5aShortcutWithRemoteSharer uint64 // local write hit took the no-inv shortcut despite a remote sharer (paper §4.2 OP5a)
	op5bRemoteWriteHitClearedWriter uint64 // remote write hit on valid offset cleared the writer's sharer bit (paper §4.2 OP5b)
}

func (c *Comp) AvgEvictUtilization() float64 {
	if c.evictEntryCount == 0 {
		return 0
	}
	return c.evictEntryUtilSum / float64(c.evictEntryCount)
}

func (c *Comp) EvictCount() uint64 {
	return c.evictEntryCount
}

// CurrentValidEntryUtilization scans every directory entry that is
// currently valid (IsValidEntry == true) and averages the per-entry
// sub-entry utilization (validSubEntries / totalSubEntries). Invalid
// entries are excluded from the average. Returns (avgUtil, validEntries)
// where avgUtil ∈ [0,1] and validEntries is the count of valid entries
// observed. Safe to call between simulation ticks (SerialEngine guarantee).
//
// REC stores entries directly under directory.GetSets() (no per-bank
// nesting like SuperDirectory). SubEntry is a fixed-size [16]CohSubEntry.
// Returns (avgUtil, validEntries, totalValidSubEntries). totalValidSubEntries
// is the directory's cache-line coverage — each REC sub-entry tracks one
// cache line within the region, so summing valid sub-entries gives the
// number of cache lines currently covered.
func (c *Comp) CurrentValidEntryUtilization() (float64, int, int) {
	sets := c.directory.GetSets()
	var sum float64
	count := 0
	totalValidSub := 0
	for i := range sets {
		for _, entry := range sets[i].CohEntries {
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
			totalValidSub += validSub
		}
	}
	if count == 0 {
		return 0, 0, 0
	}
	return sum / float64(count), count, totalValidSub
}

// DiagCounts returns the silent-eviction diagnostic counters.
func (c *Comp) DiagCounts() (alloc, needEvict, silentReset, defCleanup, invSent, invSkippedSelf uint64) {
	return c.allocCount, c.needEvictionCount, c.silentResetCount,
		c.defensiveCleanupCount, c.invSentCount, c.invSkippedSelfOnlyCount
}

// ActionCounts returns dispatch counts by transaction action type and the
// total bottom-sender forwarding count and MSHR-stage forwarding count.
// op5a_/op5b_ keys are PHASE C-2 regression slots and are expected to be 0
// in the post-fix codebase.
func (c *Comp) ActionCounts() map[string]uint64 {
	return map[string]uint64{
		"act_Nothing":              c.actNothing,
		"act_InsertNewEntry":       c.actInsertNew,
		"act_UpdateEntry":          c.actUpdate,
		"act_EvictAndInsertNew":    c.actEvictInsert,
		"act_InvalidateEntry":      c.actInvalidateEnt,
		"act_InvalidateAndUpdate":  c.actInvUpdate,
		"act_BypassingDirectory":   c.actBypass,
		"bottom_send_count":        c.bottomSendCount,
		"mshr_forward_count":       c.mshrFwdCount,
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
		"total_dowrite_calls":      c.totalDoWriteCalls,
		// Queueing-delay sums emitted in nanoseconds (for analysis;
		// divide by *_count to get average per-trans dwell time).
		"wait_dir_ns_bypass":       uint64(c.waitDirSum_bypass * 1e9),
		"wait_bottom_ns_bypass":    uint64(c.waitBottomSum_bypass * 1e9),
		"wait_count_bypass":        c.waitCount_bypass,
		"wait_dir_ns_fast":         uint64(c.waitDirSum_fast * 1e9),
		"wait_bottom_ns_fast":      uint64(c.waitBottomSum_fast * 1e9),
		"wait_count_fast":          c.waitCount_fast,
		"wait_dir_ns_bank":         uint64(c.waitDirSum_bank * 1e9),
		"wait_bottom_ns_bank":      uint64(c.waitBottomSum_bank * 1e9),
		"wait_count_bank":          c.waitCount_bank,
		"op5a_shortcut_with_remote_sharer":   c.op5aShortcutWithRemoteSharer,
		"op5b_remote_write_hit_cleared_writer": c.op5bRemoteWriteHitClearedWriter,
	}
}

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
	m.debugAddress = 12884956160
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

	madeProgress = m.bankStages.Tick() || madeProgress

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
	sets := c.directory.GetSets()
	for _, set := range sets {
		for _, block := range set.CohEntries {
			for _, subEntry := range block.SubEntry {
				subEntry.ReadCount = 0
				subEntry.IsLocked = false
			}
		}
	}

	c.dirStage.Reset()
	c.bankStages.Reset()
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
