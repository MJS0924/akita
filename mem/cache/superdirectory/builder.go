package superdirectory

import (
	"fmt"

	"github.com/sarchlab/akita/v4/mem/cache/superdirectory/internal"
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
	log2NumSubEntry      uint64
	fetchSingleCacheLine bool // true이면 miss 시 64B(1 cacheline)만 fetch

	interleaving          bool
	numInterleavingBlock  int
	interleavingUnitCount int
	interleavingUnitIndex int

	numBanks            int
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

	dirtyMask *[]map[vm.PID]map[uint64][]uint8
	readMask  *[]map[vm.PID]map[uint64][]uint8

	eventLogger *EventLogger
}

// MakeBuilder creates a new builder with default configurations.
func MakeBuilder() Builder {
	return Builder{
		freq:                1 * sim.GHz,
		wayAssociativity:    4,
		log2BlockSize:       6,
		numBanks:            5,
		byteSize:            512 * mem.KB,
		numMSHREntry:        16,
		numReqPerCycle:      1,
		writeBufferCapacity: 1024,
		maxInflightFetch:    128,
		maxInflightEviction: 128,
		bankLatency:         10,
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

func (b Builder) WithLog2NumSubEntry(n uint64) Builder {
	b.log2NumSubEntry = n
	return b
}

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

// WithEventLogger attaches an EventLogger to the cache. The logger must
// already be enabled by the caller (logger.Enable()) before building.
func (b Builder) WithEventLogger(l *EventLogger) Builder {
	b.eventLogger = l
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
	regionLen := []int{}
	for i := b.numBanks - 1; i >= 0; i-- {
		regionLen = append(regionLen, int(b.log2BlockSize+uint64(i)*b.log2NumSubEntry))
		// regionLen = {14, 12, 10, 8, 6}
	}
	directory := internal.NewSuperDirectory(
		b.numBanks, numSet, b.wayAssociativity, blockSize, int(b.log2NumSubEntry), 64, vimctimFinder, regionLen)

	if b.interleaving {
		directory.AddrConverter = &mem.InterleavingConverter{
			InterleavingSize: uint64(b.numInterleavingBlock) *
				(1 << b.log2BlockSize) * (1 << b.log2NumSubEntry),
			TotalNumOfElements:  b.interleavingUnitCount,
			CurrentElementIndex: b.interleavingUnitIndex,
		}
	}

	mshr := internal.NewMSHR(b.numMSHREntry)
	storage := mem.NewStorage(b.byteSize)

	cacheModule.log2BlockSize = b.log2BlockSize
	cacheModule.log2PageSize = b.log2PageSize
	cacheModule.log2NumSubEntry = int(b.log2NumSubEntry)
	cacheModule.fetchSingleCacheLine = b.fetchSingleCacheLine
	cacheModule.numBanks = b.numBanks
	cacheModule.numReqPerCycle = b.numReqPerCycle
	cacheModule.directory = directory
	cacheModule.mshr = mshr
	cacheModule.storage = storage
	cacheModule.regionLen = regionLen

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

	cacheModule.DirtyMask = b.dirtyMask
	cacheModule.ReadMask = b.readMask

	cacheModule.regionSizeBuffer = *internal.NewRegionSizeBuffer(64, b.log2PageSize, regionLen)

	if b.eventLogger != nil {
		cacheModule.eventLogger = b.eventLogger
	} else {
		cacheModule.eventLogger = newEventLogger(b.engine, 1<<b.log2NumSubEntry)
	}
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
	cache.topParser.returnFalse = ""

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
	localBuf := []sim.Buffer{}
	remoteBuf := []sim.Buffer{}
	motionBuf := []sim.Buffer{}
	localPipeline := []pipelining.Pipeline{}
	remotePipeline := []pipelining.Pipeline{}
	motionPipeline := []pipelining.Pipeline{}

	for i := 0; i < b.numBanks; i++ {
		newBuf := sim.NewBuffer(
			cache.Name()+".DirectoryStageInternalBuffer"+fmt.Sprintf("[%d]", i),
			b.numReqPerCycle,
		)
		localBuf = append(localBuf, newBuf)

		newPipeline := pipelining.
			MakeBuilder().
			WithCyclePerStage(1).
			WithNumStage(b.dirLatency).
			WithPipelineWidth(b.numReqPerCycle).
			WithPostPipelineBuffer(newBuf).
			Build(cache.Name() + ".Dir.Pipeline" + fmt.Sprintf("[%d]", i))
		localPipeline = append(localPipeline, newPipeline)
	}

	for i := 0; i < b.numBanks; i++ {
		newBuf := sim.NewBuffer(
			cache.Name()+".DirectoryStageInternalBuffer"+fmt.Sprintf("[%d]", i),
			b.numReqPerCycle,
		)
		remoteBuf = append(remoteBuf, newBuf)

		newPipeline := pipelining.
			MakeBuilder().
			WithCyclePerStage(1).
			WithNumStage(b.dirLatency).
			WithPipelineWidth(b.numReqPerCycle).
			WithPostPipelineBuffer(newBuf).
			Build(cache.Name() + ".Dir.Pipeline" + fmt.Sprintf("[%d]", i))
		remotePipeline = append(remotePipeline, newPipeline)
	}

	for i := 0; i < b.numBanks; i++ {
		newBuf := sim.NewBuffer(
			cache.Name()+".DirectoryStageInternalBuffer"+fmt.Sprintf("[%d]", i),
			b.numReqPerCycle,
		)
		motionBuf = append(motionBuf, newBuf)

		newPipeline := pipelining.
			MakeBuilder().
			WithCyclePerStage(1).
			WithNumStage(b.dirLatency).
			WithPipelineWidth(b.numReqPerCycle).
			WithPostPipelineBuffer(newBuf).
			Build(cache.Name() + ".Dir.Pipeline" + fmt.Sprintf("[%d]", i))
		motionPipeline = append(motionPipeline, newPipeline)
	}

	cache.dirStage = &directoryStage{
		cache:          cache,
		localPipeline:  localPipeline,
		remotePipeline: remotePipeline,
		localBuf:       localBuf,
		remoteBuf:      remoteBuf,
		motionBuf:      motionBuf,
		motionPipeline: motionPipeline,
		returnFalse0:   "",
		returnFalse1:   "",
	}
}

func (b *Builder) buildBankStages(cache *Comp) {
	cache.bankStages = make([]*bankStage, b.numBanks)

	laneWidth := b.numReqPerCycle
	if laneWidth == 1 {
		laneWidth = 2
	}

	for i := 0; i < b.numBanks; i++ {
		localBuf := &bufferImpl{
			name:     fmt.Sprintf("%s.Bank.LocalPostPipelineBuffer[%d]", cache.Name(), i),
			capacity: laneWidth,
		}
		remoteBuf := &bufferImpl{
			name:     fmt.Sprintf("%s.Bank.RemotePostPipelineBuffer[%d]", cache.Name(), i),
			capacity: laneWidth,
		}

		localPipeline := pipelining.
			MakeBuilder().
			WithCyclePerStage(1).
			WithNumStage(b.bankLatency).
			WithPipelineWidth(laneWidth).
			WithPostPipelineBuffer(localBuf).
			Build(fmt.Sprintf("%s.Bank.Pipeline[%d]", cache.Name(), i))
		remotePipeline := pipelining.
			MakeBuilder().
			WithCyclePerStage(1).
			WithNumStage(b.bankLatency).
			WithPipelineWidth(laneWidth).
			WithPostPipelineBuffer(remoteBuf).
			Build(fmt.Sprintf("%s.Bank.Pipeline[%d]", cache.Name(), i))

		cache.bankStages[i] = &bankStage{
			cache:  cache,
			bankID: i,

			localPipeline:         localPipeline,
			remotePipeline:        remotePipeline,
			localPostPipelineBuf:  localBuf,
			remotePostPipelineBuf: remoteBuf,

			pipelineWidth: laneWidth,

			returnFalse0: "",
			returnFalse1: "",
		}
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
	cache.dirStageAckBuffer = sim.NewBuffer(
		cache.Name()+".DirStageAckBuffer",
		cache.numReqPerCycle,
	)
	cache.dirStageMotionBuffer = sim.NewBuffer(
		cache.Name()+".DirStageMotionBuffer",
		cache.numReqPerCycle,
	)

	// [수정] DirToBankBuffer를 Local과 Remote로 분리
	cache.localDirToBankBuffers = make([]sim.Buffer, cache.numBanks)
	for i := 0; i < cache.numBanks; i++ {
		cache.localDirToBankBuffers[i] = sim.NewBuffer(
			cache.Name()+".LocalDirToBankBuffer["+fmt.Sprintf("%d", i)+"]",
			cache.numReqPerCycle,
		)
	}

	cache.remoteDirToBankBuffers = make([]sim.Buffer, cache.numBanks)
	for i := 0; i < cache.numBanks; i++ {
		cache.remoteDirToBankBuffers[i] = sim.NewBuffer(
			cache.Name()+".RemoteDirToBankBuffer["+fmt.Sprintf("%d", i)+"]",
			cache.numReqPerCycle,
		)
	}

	cache.writeBufferToBankBuffers = make([]sim.Buffer, cache.numBanks)
	for i := 0; i < cache.numBanks; i++ {
		cache.writeBufferToBankBuffers[i] = sim.NewBuffer(
			cache.Name()+".WriteBufferToBankBuffer["+fmt.Sprintf("%d", i)+"]",
			cache.numReqPerCycle,
		)
	}

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
