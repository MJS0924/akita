package largeblkcache

import (
	"fmt"
	"strings"

	"github.com/sarchlab/akita/v4/mem/cache/largeblkcache/internal"
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

	topPort       sim.Port
	remoteTopPort sim.Port
	bottomPort    sim.Port
	controlPort   sim.Port

	cohDirStageBuffer        sim.Buffer
	dirStageBuffer           sim.Buffer
	remoteDirStageBuffer     sim.Buffer // [추가] Remote(RDMA)에서 들어온 외부 요청 전용
	dirToBankBuffers         []sim.Buffer
	writeBufferToBankBuffers  []sim.Buffer
	mshrStageBuffer           sim.Buffer
	writeBufferBuffer         sim.Buffer // eviction 전용 (writeBufferFlush, writeBufferEvictAndFetch, writeBufferEvictAndWrite)
	writeBufferFetchBuffer    sim.Buffer // 순수 fetch 전용 (dirStage.fetch() → writeBufferFetch)

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
	log2SubBlockSize    uint64
	log2BlockSize       uint64
	log2PageSize        uint64
	numReqPerCycle      int

	state                cacheState
	inFlightTransactions []*transaction
	evictingList         map[uint64]bool
	prefetchAllowed      bool

	DirtyMask *[]map[vm.PID]map[uint64][]uint8
	ReadMask  *[]map[vm.PID]map[uint64][]uint8

	returnValue bool
	debug       int
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
	madeProgress := false

	if m.state != cacheStatePaused {
		madeProgress = m.runPipeline() || madeProgress
	} else { // pause 상태에서 들어오는 request/response 버리기
		item := m.topPort.PeekIncoming()
		if item != nil {
			for m.topPort.RetrieveIncoming() != nil {
			}
			madeProgress = true
		}

		item = m.bottomPort.PeekIncoming()
		if item != nil {
			for m.bottomPort.RetrieveIncoming() != nil {
			}
			madeProgress = true
		}
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
		for _, block := range set.Blocks {
			block.ReadCount = 0
			block.IsLocked = false
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
