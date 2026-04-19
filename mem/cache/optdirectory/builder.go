package optdirectory

import (
	"fmt"

	"github.com/sarchlab/akita/v4/mem/cache/optdirectory/internal"
	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"

	"github.com/sarchlab/akita/v4/pipelining"
	"github.com/sarchlab/akita/v4/sim"
)

// A Builder can build writeback caches
type Builder struct {
	engine              sim.Engine
	freq                sim.Freq
	deviceID            int
	addressToPortMapper mem.AddressToPortMapper
	wayAssociativity    int
	log2BlockSize       uint64
	log2PageSize        uint64
	log2UnitSize        uint64 // coherence management unit 크기 (cache block 개수)
	fetchSingleCacheLine bool  // true이면 miss 시 64B(1 cacheline)만 fetch (HMG용)

	interleaving          bool
	numInterleavingBlock  int
	interleavingUnitCount int
	interleavingUnitIndex int

	byteSize            uint64
	numMSHREntry        int
	numReqPerCycle      int
	writeBufferCapacity int
	maxInflightFetch    int
	maxInflightEviction int

	cohDirLatency int
	dirLatency    int
	bankLatency   int

	addressMapperType string

	ToRDMA sim.RemotePort

	IdealDirectory bool

	dirtyMask *[]map[vm.PID]map[uint64][]uint8
	readMask  *[]map[vm.PID]map[uint64][]uint8

	regionSizesBytes []uint64
	coalesceMode     coalesceMode
}

// MakeBuilder creates a new builder with default configurations.
func MakeBuilder() Builder {
	return Builder{
		freq:                1 * sim.GHz,
		wayAssociativity:    8,
		log2BlockSize:       6,
		byteSize:            512 * mem.KB,
		numMSHREntry:        16,
		numReqPerCycle:      1,
		writeBufferCapacity: 1024,
		maxInflightFetch:    128,
		maxInflightEviction: 128,
		bankLatency:         1,
	}
}

func (b Builder) WithDeviceID(id int) Builder {
	b.deviceID = id
	return b
}

// WithEngine sets the engine to be used by the caches.
func (b Builder) WithEngine(engine sim.Engine) Builder {
	b.engine = engine
	return b
}

// WithFreq sets the frequency to be used by the caches.
func (b Builder) WithFreq(freq sim.Freq) Builder {
	b.freq = freq
	return b
}

// WithWayAssociativity sets the way associativity.
func (b Builder) WithWayAssociativity(n int) Builder {
	b.wayAssociativity = n
	return b
}

// WithLog2BlockSize sets the cache line size as the power of 2.
func (b Builder) WithLog2BlockSize(n uint64) Builder {
	b.log2BlockSize = n
	return b
}

// WithLog2BlockSize sets the cache line size as the power of 2.
func (b Builder) WithLog2PageSize(n uint64) Builder {
	b.log2PageSize = n
	return b
}

// WithLog2BlockSize sets the cache line size as the power of 2.
func (b Builder) WithLog2UnitSize(n uint64) Builder {
	b.log2UnitSize = n
	return b
}

// WithFetchSingleCacheLine sets whether to fetch only 1 cacheline (64B) on miss
// instead of the full coherence unit region (256B). Used for HMG.
func (b Builder) WithFetchSingleCacheLine(v bool) Builder {
	b.fetchSingleCacheLine = v
	return b
}

// WithNumMSHREntry sets the number of MSHR entries.
func (b Builder) WithNumMSHREntry(n int) Builder {
	b.numMSHREntry = n
	return b
}

// WithAddressToPortMapper sets the AddressToPortMapper to be used.
func (b Builder) WithAddressToPortMapper(f mem.AddressToPortMapper) Builder {
	b.addressToPortMapper = f
	return b
}

// WithNumReqPerCycle sets the number of requests that can be processed by the
// cache in each cycle.
func (b Builder) WithNumReqPerCycle(n int) Builder {
	b.numReqPerCycle = n
	return b
}

// WithByteSize set the size of the cache.
func (b Builder) WithByteSize(byteSize uint64) Builder {
	b.byteSize = byteSize
	return b
}

// WithInterleaving sets the size that the cache is interleaved.
func (b Builder) WithInterleaving(
	numBlock, unitCount, unitIndex int,
) Builder {
	b.interleaving = true
	b.numInterleavingBlock = numBlock
	b.interleavingUnitCount = unitCount
	b.interleavingUnitIndex = unitIndex

	return b
}

// WithWriteBufferSize sets the number of cach lines that can reside in the
// writebuffer.
func (b Builder) WithWriteBufferSize(n int) Builder {
	b.writeBufferCapacity = n
	return b
}

// WithMaxInflightFetch sets the number of concurrent fetch that the write-back
// cache can issue at the same time.
func (b Builder) WithMaxInflightFetch(n int) Builder {
	b.maxInflightFetch = n
	return b
}

// WithMaxInflightEviction sets the number of concurrent eviction that the
// write buffer can write to a low-level module.
func (b Builder) WithMaxInflightEviction(n int) Builder {
	b.maxInflightEviction = n
	return b
}

// WithDirectoryLatency sets the number of cycles required to access the
// directory.
func (b Builder) WithCoherenceDirectoryLatency(n int) Builder {
	b.cohDirLatency = n
	return b
}

// WithDirectoryLatency sets the number of cycles required to access the
// directory.
func (b Builder) WithDirectoryLatency(n int) Builder {
	b.dirLatency = n
	return b
}

// WithBankLatency sets the number of cycles required to process each can
// read/write operation.
func (b Builder) WithBankLatency(n int) Builder {
	b.bankLatency = n
	return b
}

func (b Builder) WithAddressMapperType(t string) Builder {
	b.addressMapperType = t
	return b
}

func (b Builder) WithRemotePorts(ports ...sim.RemotePort) Builder {
	if b.addressMapperType == "single" {
		if len(ports) != 1 {
			panic("single address mapper requires exactly 1 port")
		}

		b.addressToPortMapper = &mem.SinglePortMapper{Port: ports[0]}
	} else if b.addressMapperType == "interleaved" {
		finder := mem.NewInterleavedAddressPortMapper(256)
		finder.LowModules = append(finder.LowModules, ports...)
		b.addressToPortMapper = finder
	} else if b.addressMapperType == "custom" {
		finder := mem.NewL2BottomMapper()
		finder.LocalBank = ports[0]
	} else {
		panic("unknown address mapper type")
	}

	return b
}

func (b Builder) WithToRDMA(port sim.RemotePort) Builder {
	b.ToRDMA = port
	return b
}

func (b Builder) WithIdealDirectory(bo bool) Builder {
	b.IdealDirectory = bo
	return b
}

func (b Builder) WithDirtyMask(mask *[]map[vm.PID]map[uint64][]uint8) Builder {
	b.dirtyMask = mask
	return b
}

func (b Builder) WithReadMask(mask *[]map[vm.PID]map[uint64][]uint8) Builder {
	b.readMask = mask
	return b
}

// WithRegionSizesBytes overrides the default coalescability region granularities.
// Default: {64, 256, 1024, 4096, 16384}.
func (b Builder) WithRegionSizesBytes(sizes []uint64) Builder {
	b.regionSizesBytes = sizes
	return b
}

// WithCoalesceMode selects strict / relaxed / both metric emission. Default: both.
func (b Builder) WithCoalesceMode(m coalesceMode) Builder {
	b.coalesceMode = m
	return b
}

// Build creates a usable writeback cache.
func (b Builder) Build(name string) *Comp {
	cache := new(Comp)
	cache.name = name
	cache.TickingComponent = sim.NewTickingComponent(
		name, b.engine, b.freq, cache)

	b.configureCache(cache)
	b.createPorts(cache)
	b.createInternalStages(cache)
	b.createInternalBuffers(cache)

	middleware := &middleware{Comp: cache}
	cache.AddMiddleware(middleware)

	return cache
}

func (b *Builder) configureCache(cacheModule *Comp) {
	cacheModule.deviceID = b.deviceID
	blockSize := 1 << b.log2BlockSize
	vimctimFinder := internal.NewLRUVictimFinder()
	numSet := int(b.byteSize / uint64(b.wayAssociativity*blockSize))
	directory := internal.NewCohDirectory(
		numSet, b.wayAssociativity, blockSize, b.log2UnitSize, vimctimFinder)
	directory.Ideal = b.IdealDirectory

	if b.interleaving {
		directory.AddrConverter = &mem.InterleavingConverter{
			InterleavingSize: uint64(b.numInterleavingBlock) *
				(1 << (b.log2BlockSize + b.log2UnitSize)),
			TotalNumOfElements:  b.interleavingUnitCount,
			CurrentElementIndex: b.interleavingUnitIndex,
		}
	}

	mshr := internal.NewMSHR(b.numMSHREntry)
	storage := mem.NewStorage(b.byteSize)

	cacheModule.log2BlockSize = b.log2BlockSize
	cacheModule.log2PageSize = b.log2PageSize
	cacheModule.log2UnitSize = b.log2UnitSize
	cacheModule.fetchSingleCacheLine = b.fetchSingleCacheLine
	cacheModule.numReqPerCycle = b.numReqPerCycle
	cacheModule.directory = directory
	cacheModule.mshr = mshr
	cacheModule.storage = storage

	if b.addressToPortMapper == nil {
		// panic(
		// 	"addressToPortMapper is nil. " +
		// 		"WithRemotePorts or WithAddressMapperType not set",
		// )
	} else {
		cacheModule.addressToPortMapper = b.addressToPortMapper
	}

	cacheModule.state = cacheStateRunning
	cacheModule.evictingList = make(map[uint64]bool)

	cacheModule.accessBitmask = make(map[uint64]uint8)

	// R-coalescability metrics (see design_document.md)
	cacheModule.sharerSet = make(map[uint64]uint64)
	cacheModule.cohState = make(map[uint64]uint8)
	cacheModule.writeMaskKernel = make(map[uint64]bool)
	cacheModule.accessMaskKernel = make(map[uint64]bool)

	if len(b.regionSizesBytes) == 0 {
		cacheModule.regionSizesBytes = []uint64{64, 256, 1024, 4096, 16384}
	} else {
		cacheModule.regionSizesBytes = b.regionSizesBytes
	}
	cacheModule.coalesceMode = b.coalesceMode
}

func (b *Builder) createPorts(cache *Comp) {
	cache.topPort = sim.NewPort(cache,
		cache.numReqPerCycle*2, cache.numReqPerCycle*2,
		cache.Name()+".ToTop")
	cache.AddPort("Top", cache.topPort)

	cache.bottomPort = sim.NewPort(cache,
		cache.numReqPerCycle*2, cache.numReqPerCycle*2,
		cache.Name()+".BottomPort")
	cache.AddPort("Bottom", cache.bottomPort)

	cache.remoteBottomPort = sim.NewPort(cache,
		cache.numReqPerCycle*2, cache.numReqPerCycle*2,
		cache.Name()+".RemoteBottomPort")
	cache.AddPort("RemoteBottom", cache.remoteBottomPort)

	cache.controlPort = sim.NewPort(cache,
		cache.numReqPerCycle*2, cache.numReqPerCycle*2,
		cache.Name()+".ControlPort")
	cache.AddPort("Control", cache.controlPort)

	cache.RDMAPort = sim.NewPort(cache,
		cache.numReqPerCycle*2, cache.numReqPerCycle*2,
		cache.Name()+".RDMAPort")
	cache.AddPort("RDMA", cache.RDMAPort)

	cache.RDMAInvPort = sim.NewPort(cache,
		cache.numReqPerCycle*2, cache.numReqPerCycle*2,
		cache.Name()+".RDMAInvPort")
	cache.AddPort("RDMAInv", cache.RDMAInvPort)

	cache.ToRDMA = b.ToRDMA
}

func (b *Builder) createInternalStages(cache *Comp) {
	cache.topParser = &topParser{cache: cache}
	b.buildDirectoryStage(cache)
	b.buildBankStages(cache)
	cache.mshrStage = &mshrStage{cache: cache}
	cache.flusher = &flusher{cache: cache}
	cache.bottomSender = &bottomSender{
		cache:                   cache,
		writeBufferCapacity:     b.writeBufferCapacity,
		maxInflightRequest:      b.maxInflightFetch,
		maxInflightInvalidation: b.maxInflightEviction,
	}
}

func (b *Builder) buildDirectoryStage(cache *Comp) {
	// [수정] 내부 버퍼(buf) 분리
	localBuf := sim.NewBuffer(
		cache.Name()+".LocalDirectoryStageInternalBuffer",
		b.numReqPerCycle,
	)
	remoteBuf := sim.NewBuffer(
		cache.Name()+".RemoteDirectoryStageInternalBuffer",
		b.numReqPerCycle,
	)

	// [수정] 파이프라인 분리
	localPipeline := pipelining.MakeBuilder().
		WithCyclePerStage(1).
		WithNumStage(b.dirLatency).
		WithPipelineWidth(b.numReqPerCycle).
		WithPostPipelineBuffer(localBuf).
		Build(cache.Name() + ".LocalDir.Pipeline")

	remotePipeline := pipelining.MakeBuilder().
		WithCyclePerStage(1).
		WithNumStage(b.dirLatency).
		WithPipelineWidth(b.numReqPerCycle).
		WithPostPipelineBuffer(remoteBuf).
		Build(cache.Name() + ".RemoteDir.Pipeline")

	cache.dirStage = &directoryStage{
		cache:          cache,
		localPipeline:  localPipeline,
		remotePipeline: remotePipeline,
		localBuf:       localBuf,
		remoteBuf:      remoteBuf,
	}
}

func (b *Builder) buildBankStages(cache *Comp) {
	cache.bankStages = make([]*bankStage, 1)

	laneWidth := b.numReqPerCycle
	if laneWidth == 1 {
		laneWidth = 2
	}

	// [수정] 버퍼(bufferImpl) 분리
	localBuf := &bufferImpl{
		name:     fmt.Sprintf("%s.LocalBank.PostPipelineBuffer", cache.Name()),
		capacity: laneWidth,
	}
	remoteBuf := &bufferImpl{
		name:     fmt.Sprintf("%s.RemoteBank.PostPipelineBuffer", cache.Name()),
		capacity: laneWidth,
	}

	// [수정] 파이프라인 분리
	localPipeline := pipelining.MakeBuilder().
		WithCyclePerStage(1).
		WithNumStage(b.bankLatency).
		WithPipelineWidth(laneWidth).
		WithPostPipelineBuffer(localBuf).
		Build(fmt.Sprintf("%s.LocalBank.Pipeline", cache.Name()))

	remotePipeline := pipelining.MakeBuilder().
		WithCyclePerStage(1).
		WithNumStage(b.bankLatency).
		WithPipelineWidth(laneWidth).
		WithPostPipelineBuffer(remoteBuf).
		Build(fmt.Sprintf("%s.RemoteBank.Pipeline", cache.Name()))

	cache.bankStages[0] = &bankStage{
		cache:                 cache,
		bankID:                0,
		localPipeline:         localPipeline,
		remotePipeline:        remotePipeline,
		localPostPipelineBuf:  localBuf,
		remotePostPipelineBuf: remoteBuf,
		pipelineWidth:         laneWidth,
	}
}

func (b *Builder) createInternalBuffers(cache *Comp) {
	// [수정] DirStageBuffer를 Local과 Remote로 분리
	cache.localDirStageBuffer = sim.NewBuffer(
		cache.Name()+".LocalDirStageBuffer",
		cache.numReqPerCycle,
	)
	cache.remoteDirStageBuffer = sim.NewBuffer(
		cache.Name()+".RemoteDirStageBuffer",
		cache.numReqPerCycle,
	)

	// [수정] DirToBankBuffer를 Local과 Remote로 분리
	cache.localDirToBankBuffers = make([]sim.Buffer, 1)
	cache.localDirToBankBuffers[0] = sim.NewBuffer(
		cache.Name()+".LocalDirToBankBuffer",
		cache.numReqPerCycle,
	)

	cache.remoteDirToBankBuffers = make([]sim.Buffer, 1)
	cache.remoteDirToBankBuffers[0] = sim.NewBuffer(
		cache.Name()+".RemoteDirToBankBuffer",
		cache.numReqPerCycle,
	)

	cache.writeBufferToBankBuffers = make([]sim.Buffer, 1)
	cache.writeBufferToBankBuffers[0] = sim.NewBuffer(
		cache.Name()+".WriteBufferToBankBuffer",
		cache.numReqPerCycle,
	)

	// [수정] MSHRStageBuffer를 Local과 Remote로 분리
	cache.localMshrStageBuffer = sim.NewBuffer(
		cache.Name()+".LocalMSHRStageBuffer",
		cache.numReqPerCycle,
	)
	cache.remoteMshrStageBuffer = sim.NewBuffer(
		cache.Name()+".RemoteMSHRStageBuffer",
		cache.numReqPerCycle,
	)

	// [수정] BottomSenderBuffer를 Local과 Remote로 분리
	cache.localBottomSenderBuffer = sim.NewBuffer(
		cache.Name()+".LocalBottomSenderBuffer",
		cache.numReqPerCycle,
	)
	cache.remoteBottomSenderBuffer = sim.NewBuffer(
		cache.Name()+".RemoteBottomSenderBuffer",
		cache.numReqPerCycle,
	)

	cache.invReqBuffer = sim.NewBuffer(
		cache.Name()+".InvReqBuffer",
		cache.numReqPerCycle,
	)
	cache.invRspBuffer = sim.NewBuffer(
		cache.Name()+".InvRspBuffer",
		cache.numReqPerCycle,
	)
	cache.localBypassBuffer = sim.NewBuffer(
		cache.Name()+".LocalBypassBuffer",
		cache.numReqPerCycle,
	)
}
