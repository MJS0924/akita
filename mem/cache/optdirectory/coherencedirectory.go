package optdirectory

import (
	"fmt"
	"log"
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

	// --- Exp-W: write workload tracking ---
	writeEventCountKernel     int               // total write events in current kernel
	sharerSetChurnCountKernel int               // times sharerSet[blockID] changed value this kernel
	prevSharerSetKernel       map[uint64]uint64 // sharerSet at start of write event (for churn detection)
	falseInvalidCountKernel   int               // writes to different GPUs in same 4-CL group
	hmg4CLWritersKernel       map[uint64]uint64 // 4-CL group → union of writer GPU bits this kernel
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

	temp := m.runStage(m.bottomSender)
	madeProgress = temp || madeProgress
	if m.printReturn {
		fmt.Printf("[DEBUG CohDir %d]\treturn 1.1: %v\n", m.deviceID, temp)
	}

	temp = m.runStage(m.mshrStage)
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
	if id == 999 {
		fmt.Printf("[%s][recordAccessMask]\tImpossible GPU ID\n", c.name)
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
