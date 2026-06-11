package writebackcoh

import (
	"fmt"
	"strings"
	"sync"

	"github.com/sarchlab/akita/v4/mem/cache/writebackcoh/internal"
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

// Comp in the writeback package is a cache that performs the write-back policy.
type Comp struct {
	*sim.TickingComponent
	sim.MiddlewareHolder

	name     string
	deviceID int

	remoteTopPort sim.Port
	topPort       sim.Port
	bottomPort    sim.Port
	// [L2 LOCAL/REMOTE SPLIT] dedicated egress for REMOTE-destined (cross-GPU
	// RDMA) evictions/fetches, mirroring the REC dir's BottomPort/RemoteBottom
	// Port split. The single bottomPort's one CanSend/tick re-coupled local-DRAM
	// and remote-RDMA traffic (own-DRAM-evict could starve the remote-RDMA-evict
	// / serve flush). With its own port + CanSend, remote drains independently.
	remoteBottomPort sim.Port
	controlPort      sim.Port

	cohDirStageBuffer    sim.Buffer
	dirStageBuffer       sim.Buffer
	remoteDirStageBuffer sim.Buffer
	// Phase F — separate priority ingress queue for InvReq. topparser
	// routes incoming InvReq messages here; dirStage drains this buffer
	// before processing the regular FIFO so that read/write requests
	// stalled on bankBuf or MSHR cannot block invalidations behind them.
	invStageBuffer   sim.Buffer
	dirToBankBuffers []sim.Buffer
	// [FIX #2: priority preserved past dirStage] dirStage 가 fromLocal 기준으로
	// 분기 push. bankStage.pullFromBuf 가 Remote 를 먼저 drain → cross-GPU 데드락
	// 회피. dirToBankBuffers 는 length(numBanks) 계산 용도로만 유지.
	dirToBankBuffersLocal  []sim.Buffer
	dirToBankBuffersRemote []sim.Buffer
	// [R5] writeBufferToBankBuffers split into REQ / RSP queues per bank.
	// Req lane = bankWriteHit / bankWritePrefetched (write admit path).
	// Rsp lane = bankWriteFetched (fetch-response path).
	// Splitting prevents a write-admit head from blocking a
	// fetched-response behind it in the same FIFO.
	//
	// [BANK LOCAL/REMOTE SPLIT] Each Req/Rsp lane is further split into
	// Local (fromLocal=true: own-L1 traffic) and Remote (fromLocal=false:
	// peer-serve traffic) halves, mirroring dirToBankBuffersLocal/Remote and
	// the directoryStage local/remote pipeline split. This lets the bank's
	// REMOTE (peer-serve) pull proceed independently of LOCAL bank work,
	// breaking the cross-GPU writeback deadlock. The unsuffixed
	// writeBufferToBankBuffersReq/Rsp slices are retained ONLY for length
	// queries (writeBufferToBankBuffersSize / flusher drain); pushes and
	// pulls route through the Local/Remote halves.
	writeBufferToBankBuffersReq       []sim.Buffer
	writeBufferToBankBuffersRsp       []sim.Buffer
	writeBufferToBankBuffersReqLocal  []sim.Buffer
	writeBufferToBankBuffersReqRemote []sim.Buffer
	writeBufferToBankBuffersRspLocal  []sim.Buffer
	writeBufferToBankBuffersRspRemote []sim.Buffer
	writeBufferToBankBuffers          []sim.Buffer
	// [ITER13 fix #2 — local/remote split]
	// 'mshrStageBuffer' and 'writeBufferBuffer' historically multiplexed
	// both local (own L1 originated) and peer-incoming (from peer L2
	// via RDMA→remoteTopPort→dirStage→bankStage) transactions in the
	// same single-slot FIFO (cap=numReqPerCycle=4). Under cross-GPU
	// contention, local-eviction can fully occupy the 4 slots while
	// peer's bankStage.finalize* paths see CanPush()=false and stall —
	// preventing peer ACK and closing the symmetric cycle (stencil2d
	// sim 17.50 ms hang). The buffer with no suffix retains the LOCAL
	// (fromLocal=true) traffic; the *Remote variant absorbs the peer
	// (fromLocal=false) traffic. Each is cap=numReqPerCycle. Pop sites
	// drain Remote first (mirroring dirToBankBuffersRemote precedent).
	mshrStageBuffer         sim.Buffer
	mshrStageBufferRemote   sim.Buffer
	writeBufferBuffer       sim.Buffer // eviction 전용 (writeBufferFlush, writeBufferEvictAndFetch, writeBufferEvictAndWrite) — LOCAL only
	writeBufferBufferRemote sim.Buffer // peer-incoming eviction 전용
	writeBufferFetchBuffer  sim.Buffer // 순수 fetch 전용 (dirStage.fetch() → writeBufferFetch)

	topParser   *topParser
	writeBuffer *writeBufferStage
	dirStage    *directoryStage
	bankStages  []*bankStage
	mshrStage   *mshrStage
	flusher     *flusher

	storage             *mem.Storage
	addressToPortMapper mem.AddressToPortMapper
	directory           internal.Directory
	mshr                internal.MSHR
	maxLocalMshr        int // [추가] Local 요청이 점유할 수 있는 최대 MSHR 개수 (예약 제어용): 전체의 75%로 설정
	// [ORIGIN-SPLIT] Symmetric reservation for fromLocal=false (peer-serve)
	// misses. Previously ONLY fromLocal=true was bounded (maxLocalMshr) and
	// the IsFull residual was shared, so peer-serve could consume the whole
	// residual (own starves) and own filling to maxLocalMshr left peer-serve
	// only the gap (peer starves). maxLocalMshr + maxRemoteMshr partition
	// numMSHREntry (sum = numMSHREntry, NO net increase): own gets a 3/4
	// quota, peer-serve a guaranteed 1/4 reserve, so neither can monopolize.
	maxRemoteMshr int
	// MSHR 분포 추적 카운터 (deadlock 분석용).
	mshrLocalAdded    uint64 // fromLocal=true 로 추가된 MSHR entry 총 횟수
	mshrRemoteAdded   uint64 // fromLocal=false 로 추가된 MSHR entry 총 횟수
	mshrLocalRemoved  uint64 // fromLocal=true MSHR entry 제거 총 횟수
	mshrRemoteRemoved uint64 // fromLocal=false MSHR entry 제거 총 횟수
	// [ITER20 DIAG A] WriteDoneRsp produced for a peer-incoming (fromLocal=false)
	// write — home L2 served a remote GPU's dirty eviction and emitted the ack
	// via remoteTopPort. Compare with the sender's writeDoneReceivedCount.
	peerWriteAckSent uint64
	// [DIAG] peer READ served: home L2 read its data and emitted DataReadyRsp
	// via remoteTopPort. peerReadServeFailCount = remoteTopPort.Send returned
	// an error (egress blocked). If served is high but the dir's
	// numPeerInflightRequest stays pinned, responses are produced but not
	// clearing the dir (routing/match). If served is ~0, the L2 never reaches
	// the serve point (stuck upstream).
	peerReadServedCount    uint64
	peerReadServeFailCount uint64
	stallMSHRTotalFull     uint64 // IsFull로 reject된 횟수 (모두에게 적용)
	stallMSHRLocalCap      uint64 // local cap으로 reject된 횟수
	stallMSHRRemoteCap     uint64 // [ORIGIN-SPLIT] remote(peer-serve) cap으로 reject된 횟수
	// Deferred-invalidation counters. Armed: doInvalidation acked an
	// InvReq on a still-locked block and set PendingInvalidation.
	// Applied: bankStage.applyPendingInvalidation consumed the flag and
	// zeroed the block. End-of-run should have armed ≈ applied (the
	// difference is blocks that still have PendingInvalidation pending
	// because their owning bank op never finalized — should be 0 in a
	// healthy run, non-zero indicates the deadlock pattern returned).
	deferredInvArmed   uint64
	deferredInvApplied uint64
	log2BlockSize      uint64
	log2PageSize       uint64
	log2UnitSize       uint64
	numReqPerCycle     int

	state                     cacheState
	inFlightTransactions      []*transaction
	shadowInFlightTransaction []*transaction
	evictingList              map[uint64]bool

	// Miss-reason tracking (Method D). seenAddrs is the set of cache-line
	// keys this L2 has ever served; lastEvictionReason holds, for each
	// key recently evicted, the cause (LRU vs invalidation) so the next
	// re-fetch can be classified. Both maps grow with the working-set
	// size, which is acceptable for analysis runs.
	seenAddrs          map[missTrackerKey]struct{}
	lastEvictionReason map[missTrackerKey]string

	DirtyMask *[]map[vm.PID]map[uint64][]uint8
	ReadMask  *[]map[vm.PID]map[uint64][]uint8

	// eventCounts replaces high-frequency tracing.AddTaskStep calls with
	// in-memory counters. Reduces akita_sim_*.sqlite trace size dramatically
	// for events that report.go only reads as totals at the end of the run.
	// See report.go's eventCountsProvider for consumption.
	// [FIX #4] eventCountsMu guards incEvent vs EventCounts. The visualizer
	// HTTP server (separate goroutine) can call EventCounts() concurrently
	// with simulator ticks that call incEvent() — without this mutex,
	// Go runtime detects "concurrent map iteration and map write" and panics
	// in the final report stage. Observed in REC_halfset/CD_2/CD_1/CD_6 runs.
	eventCounts   map[string]uint64
	eventCountsMu sync.RWMutex

	returnValue   bool
	debugProcess  bool
	debugAddress0 uint64
	debugAddress1 uint64
}

func (c *Comp) incEvent(name string) {
	c.eventCountsMu.Lock()
	defer c.eventCountsMu.Unlock()
	if c.eventCounts == nil {
		c.eventCounts = make(map[string]uint64)
	}
	c.eventCounts[name]++
}

// EventCounts returns a copy of the in-memory event counters.
func (c *Comp) EventCounts() map[string]uint64 {
	c.eventCountsMu.RLock()
	defer c.eventCountsMu.RUnlock()
	out := make(map[string]uint64, len(c.eventCounts))
	for k, v := range c.eventCounts {
		out[k] = v
	}
	return out
}

// SetAddressToPortMapper sets the AddressToPortMapper used by the cache.
func (c *Comp) SetAddressToPortMapper(lmf mem.AddressToPortMapper) {
	c.addressToPortMapper = lmf
}

func (c *Comp) Tick() bool {
	return c.MiddlewareHolder.Tick()
}

type middleware struct {
	*Comp
}

// Tick updates the internal states of the Cache.
func (m *middleware) Tick() bool {
	m.debugProcess = false
	m.debugAddress0 = 12884921984
	m.debugAddress1 = 0xFFFFFFFFF
	madeProgress := false

	if m.state != cacheStatePaused {
		madeProgress = m.runPipeline() || madeProgress
	}

	madeProgress = m.flusher.Tick() || madeProgress

	m.returnValue = madeProgress
	return madeProgress
}

func (m *middleware) runPipeline() bool {
	madeProgress := false

	madeProgress = m.runStage(m.mshrStage) || madeProgress
	madeProgress = m.runStage(m.writeBuffer) || madeProgress

	for _, bs := range m.bankStages {
		madeProgress = bs.Tick() || madeProgress
	}

	// [INV-FIDELITY C1] dirStage is ticked exactly ONCE per cycle, like the
	// bank stages. It is internally budgeted per Tick (pipelines advance one
	// stage, acceptNewTransaction admits up to numReqPerCycle, and
	// processTransaction commits up to numReqPerCycle slots), so the previous
	// runStage(m.dirStage) — numReqPerCycle subticks per cycle — silently
	// quartered the dirLatency pipeline traversal time (16 stages in ~4
	// cycles) and inflated the commit bandwidth to numReqPerCycle² slots per
	// cycle, which made invCostInSlots structurally non-binding and diluted
	// the per-invalidation tag-access latency 4×. With a single Tick the
	// r9nano calibration holds as documented: L2 hit = dirLatency(16) +
	// bankLatency(184) = 200 cycles, and the commit budget is a true
	// numReqPerCycle slots/cycle shared by reads, writes, and invalidations.
	madeProgress = m.dirStage.Tick() || madeProgress
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
		for _, block := range set.Blocks {
			block.ReadCount = 0
			block.IsLocked = false
			block.PendingInvalidation = false
		}
	}

	c.dirStage.Reset()

	for _, bs := range c.bankStages {
		bs.Reset()
	}

	c.mshrStage.Reset()
	c.writeBuffer.Reset()

	clearPort(c.topPort)

	// for _, t := range c.inFlightTransactions {
	// 	fmt.Printf("%.10f, %s, transaction %s discarded due to flushing\n",
	// 		now, c.Name(), t.id)
	// }

	c.inFlightTransactions = nil
	// for {
	// 	if len(c.inFlightTransactions) == 0 {
	// 		break
	// 	}

	// 	trans := c.inFlightTransactions[0]
	// 	c.shadowInFlightTransaction = append(c.shadowInFlightTransaction, trans)

	// 	c.inFlightTransactions = c.inFlightTransactions[1:]
	// }
}

func (c *Comp) eraseRWMask(trans *transaction) {
	startPage := trans.read.GetVAddr() / (1 << c.log2PageSize)
	startIndex := trans.read.GetVAddr() % (1 << c.log2PageSize) / uint64(1<<c.log2BlockSize)
	endPage := (trans.read.GetVAddr() + trans.read.AccessByteSize - 1) / (1 << c.log2PageSize)
	endIndex := trans.read.GetVAddr() + trans.read.AccessByteSize - 1
	endIndex = endIndex % (1 << c.log2PageSize) / uint64(1<<c.log2BlockSize)

	for page := startPage; page <= endPage; page++ {
		if (*(c.ReadMask))[c.deviceID-1] == nil {
			continue
		}
		if (*(c.ReadMask))[c.deviceID-1][trans.read.GetPID()] == nil {
			continue
		}
		if (*(c.ReadMask))[c.deviceID-1][trans.read.GetPID()][page] == nil {
			continue
		}

		rm := (*(c.ReadMask))[c.deviceID-1][trans.read.GetPID()][page]
		wm := (*(c.DirtyMask))[c.deviceID-1][trans.read.GetPID()][page]

		var start, end uint64
		if page == startPage {
			start = startIndex
		} else {
			start = 0
		}

		if page == endPage {
			end = endIndex
		} else {
			end = (1<<c.log2PageSize)/(1<<c.log2BlockSize) - 1
		}

		for i := start; i <= end; i++ {
			rm[i] = 0
			wm[i] = 0
		}
	}
}

// [ITER13 fix #2 — split buffer helpers] Route Push/CanPush by
// trans.fromLocal so peer-incoming traffic cannot be HoL-blocked by
// local in mshrStageBuffer or writeBufferBuffer.
func (c *Comp) writeBufferBufferCanPush(fromLocal bool) bool {
	if fromLocal {
		return c.writeBufferBuffer.CanPush()
	}
	return c.writeBufferBufferRemote.CanPush()
}
func (c *Comp) writeBufferBufferPush(item interface{}, fromLocal bool) {
	if fromLocal {
		c.writeBufferBuffer.Push(item)
	} else {
		c.writeBufferBufferRemote.Push(item)
	}
}

// enqueueInvDirtyFlush builds a synthetic local-origin writeBufferFlush
// transaction carrying the dirty line an invalidation is about to destroy,
// and admits it to the LOCAL writeBuffer path (writeBufferBuffer, falling
// back to the bounded deferred-flush list). Returns false only when both
// targets are full (callers that gate on capacity never see this). The
// flush follows the normal pendingLocalEvictions → bottomPort → local-DRAM
// eviction path and terminates at processWriteDoneRsp — the same lifecycle
// as a displacement flush, so the kill pays the real writeBuffer slot, NoC,
// and DRAM-write cost a hardware victim writeback pays. Dirty lines are
// locally-homed by construction (RDMA-mapped writes are write-through to
// the home node and clear IsDirty), so this path never touches the
// cross-GPU eviction caps. [INV-FIDELITY C3]
func (c *Comp) enqueueInvDirtyFlush(block *internal.Block) bool {
	data, err := c.storage.Read(block.CacheAddress, 1<<c.log2BlockSize)
	if err != nil {
		panic(err)
	}

	var dirtyMask []bool
	if block.DirtyMask != nil {
		dirtyMask = make([]bool, len(block.DirtyMask))
		copy(dirtyMask, block.DirtyMask)
	}

	flushTrans := &transaction{
		id:                sim.GetIDGenerator().Generate(),
		action:            writeBufferFlush,
		fromLocal:         true,
		evictingPID:       block.PID,
		evictingAddr:      block.Tag,
		evictingData:      data,
		evictingDirtyMask: dirtyMask,
	}

	if c.writeBufferBufferCanPush(true) {
		c.writeBufferBufferPush(flushTrans, true)
	} else if c.writeBuffer.deferFlushCanPush(true) {
		c.writeBuffer.deferFlushPush(flushTrans)
	} else {
		// Unreachable when callers gate on capacity; never drop silently.
		c.incEvent("InvDirtyWritebackDropped")
		return false
	}

	return true
}
func (c *Comp) mshrStageBufferCanPush(fromLocal bool) bool {
	if fromLocal {
		return c.mshrStageBuffer.CanPush()
	}
	return c.mshrStageBufferRemote.CanPush()
}
func (c *Comp) mshrStageBufferPush(item interface{}, fromLocal bool) {
	if fromLocal {
		c.mshrStageBuffer.Push(item)
	} else {
		c.mshrStageBufferRemote.Push(item)
	}
}

// Combined Size for flusher.go drain checks.
func (c *Comp) writeBufferBufferTotalSize() int {
	return c.writeBufferBuffer.Size() + c.writeBufferBufferRemote.Size()
}

// [R5] writeBufferToBankBuffersSize returns the combined Req+Rsp depth
// for a given bank — used by flusher.go / drain checks that previously
// queried the single writeBufferToBankBuffers[bank].Size().
//
// [BANK LOCAL/REMOTE SPLIT] Now sums all four Local/Remote halves.
func (c *Comp) writeBufferToBankBuffersSize(bank int) int {
	return c.writeBufferToBankBuffersReqLocal[bank].Size() +
		c.writeBufferToBankBuffersReqRemote[bank].Size() +
		c.writeBufferToBankBuffersRspLocal[bank].Size() +
		c.writeBufferToBankBuffersRspRemote[bank].Size()
}

// [BANK LOCAL/REMOTE SPLIT] Route the Req-lane bank buffer for a bank by
// fromLocal. Mirrors directoryStage.bankBufFor for the dir-to-bank lane.
func (c *Comp) writeBufferToBankBufferReq(bank int, fromLocal bool) sim.Buffer {
	if fromLocal {
		return c.writeBufferToBankBuffersReqLocal[bank]
	}
	return c.writeBufferToBankBuffersReqRemote[bank]
}

// [BANK LOCAL/REMOTE SPLIT] Route the Rsp-lane bank buffer for a bank by
// fromLocal.
func (c *Comp) writeBufferToBankBufferRsp(bank int, fromLocal bool) sim.Buffer {
	if fromLocal {
		return c.writeBufferToBankBuffersRspLocal[bank]
	}
	return c.writeBufferToBankBuffersRspRemote[bank]
}

func (c *Comp) eraseCacheLineFromRWMask(pid vm.PID, addr uint64) {
	page := addr / (1 << c.log2PageSize)
	idx := addr % (1 << c.log2PageSize) / uint64(1<<c.log2BlockSize)

	if (*(c.ReadMask))[c.deviceID-1] == nil {
		return
	}
	if (*(c.ReadMask))[c.deviceID-1][pid] == nil {
		return
	}
	if (*(c.ReadMask))[c.deviceID-1][pid][page] == nil {
		return
	}

	rm := (*(c.ReadMask))[c.deviceID-1][pid][page]
	wm := (*(c.DirtyMask))[c.deviceID-1][pid][page]

	rm[idx] = 0
	wm[idx] = 0
}

func (c *Comp) printRWMask(pid vm.PID, VA uint64) {
	if VA == 0 {
		fmt.Printf("[%s]\tVA is %x, Do not print RW Mask\n", c.name, VA)
		return
	}

	fmt.Printf("\nVA %x ================================================================================\n", VA)
	vpn := VA >> c.log2PageSize
	for i, list := range *(c.DirtyMask) {
		fmt.Printf("\t\tDirtyMask [%x] GPU %d: %v\n", vpn, i+1, list[pid][vpn])
	}
	for i, list := range *(c.ReadMask) {
		fmt.Printf("\t\tReadMask  [%x] GPU %d: %v\n", vpn, i+1, list[pid][vpn])
	}
	fmt.Printf("======================================================================================\n\n")
}

func (c *Comp) toLocal(addr uint64) bool {
	port := c.addressToPortMapper.Find(addr)
	if !strings.Contains(fmt.Sprintf("%s", port), "RDMA") {
		return true
	}

	return false
}
