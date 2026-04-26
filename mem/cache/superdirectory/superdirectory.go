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
		"op5a_shortcut_with_remote_sharer":     c.op5aShortcutWithRemoteSharer,
		"op5b_writer_cleared_at_finest_bank":   c.op5bWriterClearedAtFinestBank,
	}
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
