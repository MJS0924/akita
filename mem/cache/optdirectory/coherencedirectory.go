package optdirectory

import (
	"fmt"
	"log"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/sarchlab/akita/v4/mem/cache/optdirectory/internal"
	"github.com/sarchlab/akita/v4/mem/mem"

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

	topPort sim.Port
	// D4 (ported from SD): dedicated L1-facing InvRsp ingress so L1's
	// InvRsp is not head-blocked by a full localBypassBuffer behind a
	// queued ReadReq at topPort. Mirror of D1 on the L1-facing side.
	topInvRspPort    sim.Port
	bottomPort       sim.Port
	remoteBottomPort sim.Port
	controlPort      sim.Port
	RDMAPort         sim.Port
	RDMAInvPort      sim.Port
	RDMAInvRspPort   sim.Port
	// S1 (ported from SD): dedicated egress port for outbound InvRsp.
	// Separates outbound InvRsp from outbound InvReq (sendToRDMAInvQue),
	// and complies with RDMA's contract: processFromInvInside panics
	// on InvRsp; only RDMAInvRspInside accepts InvRsp.
	RDMAInvRspOutPort sim.Port
	ToRDMA            sim.RemotePort
	ToRDMAInv         sim.RemotePort
	ToRDMAInvRsp      sim.RemotePort

	// [수정 코드] 자원을 Local과 Remote로 완전 분리
	localDirStageBuffer  sim.Buffer
	remoteDirStageBuffer sim.Buffer

	localDirToBankBuffers  []sim.Buffer
	remoteDirToBankBuffers []sim.Buffer

	localMshrStageBuffer  sim.Buffer
	remoteMshrStageBuffer sim.Buffer

	// [BSB-CLASS-SPLIT] The single localBottomSenderBuffer /
	// remoteBottomSenderBuffer was a request-only FIFO mixing data-class
	// transactions (Nothing/InsertNewEntry/UpdateEntry -> sendRequestToBottom)
	// with inv-class transactions (EvictAndInsertNewEntry/InvalidateEntry/
	// InvalidateAndUpdateEntry -> sendInvalidationRequest). An inv-class head
	// stalled on tooManyInflightInvalidation() (or a data-class head stalled
	// on tooManyInflightRequest()) head-of-line blocked every trans behind it
	// in the SAME buffer, so the buffer never drained -> bankStage backpressure
	// climbed to remoteDirStageBuffer -> RDMAPort.IncomingBuf (relu_hmg
	// ~win134 deadlock). Splitting by class (mirroring REC) decouples the two
	// drain paths. The two lanes PARTITION the original numReqPerCycle-deep
	// capacity (12 data + 4 inv at numReqPerCycle=16) so total admission is
	// unchanged. Identical to the superdirectory change (shared remote-path
	// design across HMG unit-size 4 / CD 0 / largeblock 1).
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
	directory                       internal.CohDirectory
	mshr                            internal.MSHR
	log2BlockSize                   uint64
	log2PageSize                    uint64
	log2UnitSize                    uint64 // coherence management unit 크기 (cache block 개수)
	fetchSingleCacheLine            bool   // true이면 miss 시 64B(1 cacheline)만 fetch (HMG용)
	numReqPerCycle                  int

	state            cacheState
	flushLocalAccess bool
	evictingList     map[uint64]bool

	returnValue bool
	printReturn bool
	recordTime  sim.VTimeInSec

	printMaskOption bool
	accessBitmask   map[uint64]uint8 // Figure b 용 (sub-block touch, debug-only print path)
	printMask       bool
	debugProcess    bool
	debugAddress    uint64

	// --- R-coalescability metrics (see design_document.md) ---
	sharerSet        map[uint64]uint64 // Figure c/d/M4: GPU bitmask, bit (gpuID-2)
	cohState         map[uint64]uint8  // Figure d: 1=Valid, 0=Invalid
	writeMaskKernel  map[uint64]bool   // blocks written in current kernel window
	accessMaskKernel map[uint64]bool   // blocks read or written in current kernel window

	regionSizesBytes []uint64     // {64, 256, 1024, 4096, 16384}
	coalesceMode     coalesceMode // strict / relaxed / both

	cumulativeSnapshots []coalescabilitySnapshot
	currentKernelID     int

	// --- Sharer heatmap (per-window cacheline × time visualization) ---
	// All zero-cost when sharerHeatmapEnabled is false.
	sharerHeatmapEnabled bool
	accessMaskWindow     map[uint64]uint32 // blockID → access count this window
	blockTotalAccess     map[uint64]uint64 // cumulative across whole run
	heatmapCSVFile       *os.File          // per-GPU sharer heatmap CSV
	heatmapWindowID      int

	// --- Exp-W: write workload tracking ---
	writeEventCountKernel     int               // total write events in current kernel
	sharerSetChurnCountKernel int               // times sharerSet[blockID] changed value this kernel
	prevSharerSetKernel       map[uint64]uint64 // sharerSet at start of write event (for churn detection)
	falseInvalidCountKernel   int               // writes to different GPUs in same 4-CL group
	hmg4CLWritersKernel       map[uint64]uint64 // 4-CL group → union of writer GPU bits this kernel

	// --- Diagnostic counters (mirror REC for direct comparison) ---
	actNothing       uint64
	actInsertNew     uint64
	actUpdate        uint64
	actEvictInsert   uint64
	actInvalidateEnt uint64
	actInvUpdate     uint64
	actBypass        uint64
	bottomSendCount  uint64
	// remoteBottomPort/bottomPort 별도 사용량 추적 (deadlock 분석용).
	localSendCount  uint64 // sendRequestToBottom/sendMultipleRequest with isLocal=true
	remoteSendCount uint64 // sendRequestToBottom/sendMultipleRequest with isLocal=false
	bypassSendCount uint64 // processBypassReq sends (always local)
	mshrFwdCount    uint64

	// MSHR local/remote soft cap (mirrors writebackcoh L2 정책):
	// remote 요청은 전체 numMSHREntry 사용 가능; local 요청은 항상 16개 슬롯을 예약.
	maxLocalMshr   int // numMSHREntry - 16
	localMshrCount int // 현재 in-flight local MSHR entry 수

	// Stall cause counters (Method E — mirrors REC for one-to-one comparison).
	stallMSHRFull       uint64 // writeToBank: mshr.IsFull() or local soft cap
	stallBlockLocked    uint64 // doWriteHit: block IsLocked or ReadCount > 0
	stallBankFull       uint64 // writeToBank: bankBuf.CanPush() == false
	stallEvictingList   uint64 // processTransaction: addr in evictingList
	stallVictimLocked   uint64 // doWriteMiss: victim entry locked
	stallBottomBufFull  uint64 // fast-path push to bottomSenderBuffer rejected
	stallMshrBufFull    uint64 // bankstage push to mshrStageBuffer rejected
	stallInflightFetch  uint64 // bottomSender: tooManyInflightRequest
	stallInflightInv    uint64 // bottomSender: tooManyInflightInvalidation
	stallBottomPortBusy uint64 // bottomSender: bottomPort/RDMAPort can't send
	stallTopPortBusy    uint64 // doInvalidation / response: topPort/RDMAInv can't send
	stallInvEmitPeer    uint64 // [INV-FIDELITY C4] peer-lane InvReq head deferred by emit budget
	invEmittedPeer      uint64 // [INV-FIDELITY C4] InvReqs emitted on the dir→peer-dir lane
	remoteHeadDeferred  uint64 // [SD-REC PARITY Fix2] COMPLETION-VC skip-head deferrals
	totalDoWriteCalls   uint64 // every entry into doWrite (success+retry)

	// [CD8 RESPONSE-TRACE] per-egress-site response stall counters, split from
	// the lumped stallTopPortBusy. They reveal WHICH response lane cannot get a
	// send credit at the deadlock. stallRemoteRspRDMABusy + stallRDMADataRspBusy
	// both stall on RDMAPort, which is ALSO the inbound peer-REQUEST port: if
	// these two rise together, RDMAPort is the response-starvation site (REC uses
	// a dedicated RDMADataRspPort). stallInvRspOutBusy = InvRsp (slot-freeing)
	// egress on its own dedicated port. stallInflightInvOwn/Remote split the
	// inflightInvToOutside (256) cap stall by origin.
	stallRemoteRspRDMABusy uint64 // sendRemoteRspToTop: RDMAPort.CanSend() false (write-evict WriteDoneRsp/DataReadyRsp)
	stallTopRspBusy        uint64 // sendToTopRspQue → topPort full (local L1 rsp)
	stallRDMADataRspBusy   uint64 // sendToRDMADataRspQue → RDMAPort full (peer data/write rsp)
	stallInvRspOutBusy     uint64 // sendToRDMAInvRspQue → RDMAInvRspOutPort full (InvRsp)
	stallInflightInvOwn    uint64 // cap stall, fromLocal=true
	stallInflightInvRemote uint64 // cap stall, fromLocal=false (peer-serve)

	// Queueing-delay accumulators (Method E2). See REC Comp for naming.
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
	op5aShortcutWithRemoteSharer    uint64 // local write hit took the no-inv shortcut despite a remote sharer
	op5bRemoteWriteHitClearedWriter uint64 // remote write hit on valid offset cleared the writer's sharer bit (kept at 0 by optdirectory's existing writer-survives behavior — see audit C1.3)
}

// ActionCounts returns dispatch counts by transaction action type and the
// total bottom-sender forwarding count and MSHR-stage forwarding count.
// op5a_/op5b_ keys are PHASE C-2 regression slots and are expected to be 0
// in the post-fix codebase.
func (c *Comp) ActionCounts() map[string]uint64 {
	return map[string]uint64{
		"act_Nothing":                          c.actNothing,
		"act_InsertNewEntry":                   c.actInsertNew,
		"act_UpdateEntry":                      c.actUpdate,
		"act_EvictAndInsertNew":                c.actEvictInsert,
		"act_InvalidateEntry":                  c.actInvalidateEnt,
		"act_InvalidateAndUpdate":              c.actInvUpdate,
		"act_BypassingDirectory":               c.actBypass,
		"bottom_send_count":                    c.bottomSendCount,
		"mshr_forward_count":                   c.mshrFwdCount,
		"stall_mshr_full":                      c.stallMSHRFull,
		"stall_block_locked":                   c.stallBlockLocked,
		"stall_bank_full":                      c.stallBankFull,
		"stall_evicting_list":                  c.stallEvictingList,
		"stall_victim_locked":                  c.stallVictimLocked,
		"stall_bottom_buf_full":                c.stallBottomBufFull,
		"stall_mshr_buf_full":                  c.stallMshrBufFull,
		"stall_inflight_fetch":                 c.stallInflightFetch,
		"stall_inflight_inv":                   c.stallInflightInv,
		"stall_bottom_port_busy":               c.stallBottomPortBusy,
		"stall_top_port_busy":                  c.stallTopPortBusy,
		"stall_remote_rsp_rdma_busy":           c.stallRemoteRspRDMABusy,
		"stall_top_rsp_busy":                   c.stallTopRspBusy,
		"stall_rdma_data_rsp_busy":             c.stallRDMADataRspBusy,
		"stall_inv_rsp_out_busy":               c.stallInvRspOutBusy,
		"stall_inflight_inv_own":               c.stallInflightInvOwn,
		"stall_inflight_inv_remote":            c.stallInflightInvRemote,
		"stall_inv_emit_peer":                  c.stallInvEmitPeer,
		"inv_emitted_peer":                     c.invEmittedPeer,
		"remote_head_deferred":                 c.remoteHeadDeferred,
		"total_dowrite_calls":                  c.totalDoWriteCalls,
		"wait_dir_ns_bypass":                   uint64(c.waitDirSum_bypass * 1e9),
		"wait_bottom_ns_bypass":                uint64(c.waitBottomSum_bypass * 1e9),
		"wait_count_bypass":                    c.waitCount_bypass,
		"wait_dir_ns_fast":                     uint64(c.waitDirSum_fast * 1e9),
		"wait_bottom_ns_fast":                  uint64(c.waitBottomSum_fast * 1e9),
		"wait_count_fast":                      c.waitCount_fast,
		"wait_dir_ns_bank":                     uint64(c.waitDirSum_bank * 1e9),
		"wait_bottom_ns_bank":                  uint64(c.waitBottomSum_bank * 1e9),
		"wait_count_bank":                      c.waitCount_bank,
		"op5a_shortcut_with_remote_sharer":     c.op5aShortcutWithRemoteSharer,
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
	// now := m.Engine.CurrentTime()
	m.printReturn = false
	// if now >= m.recordTime+0.00002 {
	// 	m.recordTime = now
	// 	m.printMask = true
	// 	// m.printReturn = true
	// }
	m.debugProcess = false
	m.debugAddress = 12884921984

	madeProgress := false

	if m.state != cacheStatePaused {
		temp := m.runPipeline()
		madeProgress = temp || madeProgress
		// if m.printReturn {
		// 	fmt.Printf("[DEBUG CohDir 5]\treturn 1: %v\n", temp)
		// }
	}

	temp := m.flusher.Tick()
	madeProgress = temp || madeProgress
	if m.printReturn {
		fmt.Printf("[DEBUG CohDir 5]\treturn 2: %v\n", temp)
	}

	m.returnValue = madeProgress

	return madeProgress
}

func (m *middleware) runPipeline() bool {
	madeProgress := false

	// Cross-variant fairness: tick mshrStage BEFORE bottomSender to
	// match SD/REC ordering. With CD's previous order, an mshr-merged
	// transaction could not be sent to bottom in the same cycle —
	// adding a 1-cycle skew that doesn't exist in SD/REC.
	temp := m.runStage(m.mshrStage)
	madeProgress = temp || madeProgress
	if m.printReturn {
		fmt.Printf("[DEBUG CohDir %d]\treturn 1.1: %v\n", m.deviceID, temp)
	}

	temp = m.runStage(m.bottomSender)
	madeProgress = temp || madeProgress
	if m.printReturn {
		fmt.Printf("[DEBUG CohDir %d]\treturn 1.2: %v\n", m.deviceID, temp)
	}

	for i, bs := range m.bankStages {
		temp = bs.Tick()
		madeProgress = temp || madeProgress
		if m.printReturn {
			fmt.Printf("[DEBUG CohDir %d]\treturn 1.3.%d: %v\n", m.deviceID, i, temp)
		}
	}

	temp = m.runStage(m.dirStage)
	madeProgress = temp || madeProgress
	if m.printReturn {
		fmt.Printf("[DEBUG CohDir %d]\treturn 1.4: %v\n", m.deviceID, temp)
	}

	temp = m.runStage(m.topParser)
	madeProgress = temp || madeProgress
	if m.printReturn {
		fmt.Printf("[DEBUG CohDir %d]\treturn 1.5: %v\n", m.deviceID, temp)
	}

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
	// sets := c.directory.GetSets()
	// for _, set := range sets {
	// 	for _, block := range set.Blocks {
	// 		block.ReadCount = 0
	// 		block.IsLocked = false
	// 	}
	// }

	c.directory.Reset()
	c.dirStage.Reset()

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

func (c *Comp) recordAccessMask(addr uint64, srcPort sim.RemotePort) {
	src := fmt.Sprintf("%s", srcPort)
	id := srcToGPUID(src)
	// Guard: 999 = unparseable, <2 = driver/host-side access. Both produce
	// `1 << (id-2)` with negative shift -> runtime panic on some Go versions
	// (observed with xor/dnn workloads where trainer.go drives the directory).
	if id == 999 || id < 2 {
		return
	}

	blockID := addr >> c.log2BlockSize
	item, f := c.accessBitmask[blockID]

	if !f {
		item = 0
	}

	item |= 1 << (id - 2)
	c.accessBitmask[blockID] = item
}

func (c *Comp) eraseAccessMask(addr uint64) {
	blockID := addr >> c.log2BlockSize
	_, f := c.accessBitmask[blockID]

	if f {
		c.accessBitmask[blockID] = 0
	}
}

func (c *Comp) printAccessMask() {
	if !c.printMask {
		return
	}
	c.printMask = false

	// 출력할 데이터가 없으면 바로 종료 (안전 장치)
	if len(c.accessBitmask) == 0 {
		c.accessBitmask = make(map[uint64]uint8)
		return
	}

	// 1. 맵의 Key(blockID)들만 뽑아서 슬라이스에 담기
	keys := make([]uint64, 0, len(c.accessBitmask))
	for id := range c.accessBitmask {
		keys = append(keys, id)
	}

	// 2. Key 슬라이스를 오름차순으로 정렬 (uint64 타입 비교)
	sort.Slice(keys, func(i, j int) bool {
		return keys[i] < keys[j]
	})

	mask := ""
	var prev uint64
	count := 0
	first := true // 첫 번째 요소 처리를 위한 플래그

	// 3. 정렬된 Key 배열을 순회하며 맵의 Value(item)에 접근
	for _, id := range keys {
		item := c.accessBitmask[id]

		if first {
			mask += fmt.Sprintf("%x: [%x", id, item)
			first = false
		} else if id-prev == 1 {
			// 주소가 연속된 경우: 괄호를 닫지 않고 값만 추가
			mask += fmt.Sprintf(" %x", item)
		} else {
			// 주소가 건너뛴 경우: 기존 괄호를 닫고, 새로운 주소 블록 시작
			mask += "], "
			count++
			if count >= 5 { // 5개 블록마다 줄바꿈
				mask += "\n"
				count = 0
			}
			mask += fmt.Sprintf("%x: [%x", id, item)
		}
		prev = id
	}
	mask += "]"

	fmt.Printf("\nGPU %d Access Mask ==============================================\n", c.deviceID)
	fmt.Printf("%s\n", mask)
	fmt.Printf("================================================================\n\n")

	c.accessBitmask = make(map[uint64]uint8) // 맵 초기화
}

// recordSharerRead is called on a read access (InsertNewEntry / UpdateEntry /
// EvictAndInsertNewEntry). The block becomes (additionally) shared by gpuBit.
func (c *Comp) recordSharerRead(blockID uint64, gpuBit uint) {
	c.sharerSet[blockID] |= uint64(1) << gpuBit
	c.cohState[blockID] = 1
	c.accessMaskKernel[blockID] = true
	if c.sharerHeatmapEnabled {
		c.accessMaskWindow[blockID]++
		c.blockTotalAccess[blockID]++
	}
}

// recordSharerWrite is called on a write access (InvalidateAndUpdateEntry).
// All other sharers are invalidated; the writer becomes the sole sharer.
func (c *Comp) recordSharerWrite(blockID uint64, gpuBit uint) {
	prevSet := c.sharerSet[blockID]
	newSet := uint64(1) << gpuBit

	// Exp-W: churn = sharerSet actually changes value
	if prevSet != newSet {
		c.sharerSetChurnCountKernel++
	}
	c.prevSharerSetKernel[blockID] = prevSet

	c.sharerSet[blockID] = newSet
	c.cohState[blockID] = 1
	c.writeMaskKernel[blockID] = true
	c.accessMaskKernel[blockID] = true
	c.writeEventCountKernel++
	if c.sharerHeatmapEnabled {
		c.accessMaskWindow[blockID]++
		c.blockTotalAccess[blockID]++
	}

	// Exp-W: false invalidation detection (HMG 4-CL basis).
	// A false invalidation occurs when two different GPUs write to different
	// blocks within the same 4-CL group in the same kernel.
	group4CL := blockID / 4
	existingWriters := c.hmg4CLWritersKernel[group4CL]
	writerBit := uint64(1) << gpuBit
	if existingWriters != 0 && (existingWriters&writerBit) == 0 {
		// A different GPU already wrote to this 4-CL group this kernel
		c.falseInvalidCountKernel++
	}
	c.hmg4CLWritersKernel[group4CL] |= writerBit
}

// recordSharerInvalidate is called when a block is fully invalidated
// (InvalidateEntry). Sharer set cleared, state -> Invalid.
func (c *Comp) recordSharerInvalidate(blockID uint64) {
	c.sharerSet[blockID] = 0
	c.cohState[blockID] = 0
	if c.sharerHeatmapEnabled {
		// Mark this block as "touched this window" so the per-window
		// dump emits a row with sharer=0. Without this, downstream
		// forward-fill would carry the pre-invalidation sharer set
		// across the gap, overestimating sharer presence.
		c.accessMaskWindow[blockID]++
		c.blockTotalAccess[blockID]++
	}
}

// gpuBitFromPort returns the bit position (gpuID-2) for a given remote port.
// Returns ok=false if the port name does not parse to a valid GPU id.
func (c *Comp) gpuBitFromPort(srcPort sim.RemotePort) (uint, bool) {
	id := srcToGPUID(fmt.Sprintf("%s", srcPort))
	if id == 999 || id < 2 {
		return 0, false
	}
	return uint(id - 2), true
}

// OnKernelBoundary is called by the runner at every kernel boundary.
// It emits per-(view, R) coalescability metrics and write metrics, then
// resets kernel-local state.
func (c *Comp) OnKernelBoundary(simTime sim.VTimeInSec, kernelID int) {
	c.emitWriteMetrics(simTime, kernelID)
	c.emitCoalescabilityMetrics(simTime, kernelID)
}

func srcToGPUID(src string) int {
	// 1. "GPU[" 문자열이 시작하는 인덱스를 찾습니다.
	startIdx := strings.Index(src, "GPU[")
	if startIdx == -1 {
		return 999 // "GPU["가 포함되어 있지 않음
	}

	// 2. 실제 숫자가 시작되는 위치 ("GPU["의 길이인 4를 더함)
	numStart := startIdx + 4

	// 3. 숫자 시작점 이후부터 나타나는 첫 번째 "]"의 위치를 찾습니다.
	endOffset := strings.Index(src[numStart:], "]")
	if endOffset == -1 {
		return 999 // 닫는 대괄호가 없음
	}

	// 4. 괄호 안의 숫자 부분만 잘라냅니다. (예: "3")
	idStr := src[numStart : numStart+endOffset]

	// 5. 잘라낸 문자열을 정수(int)로 변환합니다.
	id, err := strconv.Atoi(idStr)
	if err != nil {
		return 999 // 괄호 안에 숫자가 아닌 값(예: GPU[A])이 있을 경우
	}

	return id
}
