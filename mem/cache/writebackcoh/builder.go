package writebackcoh

import (
	"fmt"

	"github.com/sarchlab/akita/v4/mem/cache/writebackcoh/internal"
	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"

	"github.com/sarchlab/akita/v4/pipelining"
	"github.com/sarchlab/akita/v4/sim"
)

// A Builder can build writeback caches
type Builder struct {
	engine   sim.Engine
	freq     sim.Freq
	deviceID int

	addressToPortMapper mem.AddressToPortMapper
	wayAssociativity    int
	log2BlockSize       uint64
	log2PageSize        uint64
	log2UnitSize        uint64

	interleaving          bool
	numInterleavingBlock  int
	interleavingUnitCount int
	interleavingUnitIndex int

	byteSize                 uint64
	numMSHREntry             int
	numReqPerCycle           int
	writeBufferCapacity      int // local-side cap (= legacy)
	writeBufferPeerCapacity  int // peer-side cap (iter17 F2)
	maxPeerIncomingPending   int // peer-bypass admit cap (iter17 F1)
	maxInflightFetch         int
	maxInflightEviction      int
	maxOutgoingRemotePending int

	cohDirLatency int
	dirLatency    int
	bankLatency   int
	snoopLatency  int

	addressMapperType string

	DirtyMask *[]map[vm.PID]map[uint64][]uint8
	ReadMask  *[]map[vm.PID]map[uint64][]uint8
}

// MakeBuilder creates a new builder with default configurations.
func MakeBuilder() Builder {
	return Builder{
		freq:                    1 * sim.GHz,
		wayAssociativity:        4,
		log2BlockSize:           6,
		byteSize:                512 * mem.KB,
		numMSHREntry:            16,
		numReqPerCycle:          1,
		writeBufferCapacity:     1024, // local-side cap
		writeBufferPeerCapacity: 256,  // [ITER17 F2] peer-side cap
		maxPeerIncomingPending:  256,  // [ITER17 F1] peer-bypass admit cap (raised iter19 F1)
		maxInflightFetch:        128,
		maxInflightEviction:     128,
		bankLatency:             10,
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

// WithMaxOutgoingRemotePending caps the sum of pending+inflight remote-
// bound evictions per L2 cache. Smaller than writeBufferCapacity so that
// the per-cache wB total cannot reach the full-cap from sender-side
// remote evictions alone — guarantees headroom at the receiver L2 for
// incoming-write-triggered evictions, closing the symmetric cross-GPU
// wB-saturation cycle (stencil2d REC sim 19.95ms hang). n=0 disables.
func (b Builder) WithMaxOutgoingRemotePending(n int) Builder {
	b.maxOutgoingRemotePending = n
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

// WithSnoopLatency sets the number of *additional* cycles an invalidation
// request spends in the directory stage on top of the regular dirLatency.
// This models the per-snoop tag-array lookup, state-bit write, and snoop
// response generation cost a real L2 pays regardless of whether the
// targeted line is present (i.e., wasted invalidations cost the same as
// productive ones).
//
// Default: 0 — invalidations share the regular local/remote pipelines and
// behavior is byte-identical to the historical implementation.
//
// When n > 0, two dedicated snoop pipelines (local + remote) of
// (dirLatency + n) stages each are created. acceptNewTransaction routes
// InvReq items into them; the existing local/remote post-pipeline buffers
// are reused as the merge point, so processTransaction is unchanged.
// Pipeline width matches numReqPerCycle, so the per-cycle inv processing
// throughput cap is unchanged — only the latency floor is added.
//
// Deadlock safety: the pipelining package keeps madeProgress=true while a
// stage's cycleLeft countdown is in flight, which keeps the cache ticking
// until any deferred inv reaches the post-buffer. There is no extra
// queue or shared resource introduced beyond the pipelines themselves,
// so no new HoL blocking path is created.
func (b Builder) WithSnoopLatency(n int) Builder {
	b.snoopLatency = n
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

func (b Builder) WithDirtyMask(mask *[]map[vm.PID]map[uint64][]uint8) Builder {
	b.DirtyMask = mask
	return b
}

func (b Builder) WithReadMask(mask *[]map[vm.PID]map[uint64][]uint8) Builder {
	b.ReadMask = mask
	return b
}

// Build creates a usable writeback cache.
func (b Builder) Build(name string) *Comp {
	cache := new(Comp)
	cache.name = name
	cache.TickingComponent = sim.NewTickingComponent(
		name, b.engine, b.freq, cache)
	cache.deviceID = b.deviceID

	b.configureCache(cache)
	b.createPorts(cache)
	b.createInternalStages(cache)
	b.createInternalBuffers(cache)

	middleware := &middleware{Comp: cache}
	cache.AddMiddleware(middleware)

	return cache
}

func (b *Builder) configureCache(cacheModule *Comp) {
	blockSize := 1 << b.log2BlockSize
	vimctimFinder := internal.NewLRUVictimFinder()
	numSet := int(b.byteSize / uint64(b.wayAssociativity*blockSize))
	directory := internal.NewDirectory(
		numSet, b.wayAssociativity, blockSize, vimctimFinder)

	if b.interleaving {
		directory.AddrConverter = &mem.InterleavingConverter{
			InterleavingSize: uint64(b.numInterleavingBlock) *
				(1 << b.log2BlockSize),
			TotalNumOfElements:  b.interleavingUnitCount,
			CurrentElementIndex: b.interleavingUnitIndex,
		}

		// fmt.Printf("[CohDir %d Builder]\tBuild directory %d %d %d\n",
		// 	b.deviceID, uint64(b.numInterleavingBlock)*(1<<b.log2BlockSize), b.interleavingUnitCount, b.interleavingUnitIndex)
	}

	mshr := internal.NewMSHR(b.numMSHREntry, b.log2UnitSize)
	storage := mem.NewStorage(b.byteSize)

	cacheModule.log2PageSize = b.log2PageSize
	cacheModule.log2BlockSize = b.log2BlockSize
	cacheModule.log2UnitSize = b.log2UnitSize
	cacheModule.numReqPerCycle = b.numReqPerCycle
	cacheModule.directory = directory
	cacheModule.mshr = mshr
	// 50/50 split between fromLocal (this GPU's L1) and fromRemote
	// (external GPU's incoming via RDMA). Previously 75/25; under CD_0
	// (cache-line granularity invalidation) the 25% remote reserve was
	// too small — cross-GPU cyclic fetch dependency stalled when every
	// GPU's outgoing fetches saturated their 75% and incoming WriteReq
	// from peers couldn't allocate the last 25%. With 50/50, each GPU
	// remote 요청은 MSHR 전체(numMSHREntry)를 사용 가능, local 요청은 항상 16개 슬롯을 remote용으로 예약.
	// numMSHREntry/2(=32) 정책은 bypass 요청이 많을 때 local soft cap에 너무 빨리 도달해
	// remote 요청이 진행되지 못하는 deadlock을 유발함.
	cacheModule.maxLocalMshr = b.numMSHREntry - 16
	cacheModule.storage = storage

	if b.addressToPortMapper == nil {
		panic(
			"addressToPortMapper is nil. " +
				"WithRemotePorts or WithAddressMapperType not set",
		)
	}

	cacheModule.addressToPortMapper = b.addressToPortMapper
	cacheModule.state = cacheStateRunning
	cacheModule.evictingList = make(map[uint64]bool)

	// Method D: miss-reason tracking — initialize empty.
	cacheModule.seenAddrs = make(map[missTrackerKey]struct{})
	cacheModule.lastEvictionReason = make(map[missTrackerKey]string)

	cacheModule.DirtyMask = b.DirtyMask
	cacheModule.ReadMask = b.ReadMask
}

func (b *Builder) createPorts(cache *Comp) {
	cache.topPort = sim.NewPort(cache,
		cache.numReqPerCycle*2, cache.numReqPerCycle*2,
		cache.Name()+".ToTop")
	cache.AddPort("Top", cache.topPort)

	cache.remoteTopPort = sim.NewPort(cache,
		cache.numReqPerCycle*2, cache.numReqPerCycle*2,
		cache.Name()+".ToRemoteTop")
	cache.AddPort("RemoteTop", cache.remoteTopPort)

	cache.bottomPort = sim.NewPort(cache,
		cache.numReqPerCycle*2, cache.numReqPerCycle*2,
		cache.Name()+".BottomPort")
	cache.AddPort("Bottom", cache.bottomPort)

	cache.controlPort = sim.NewPort(cache,
		cache.numReqPerCycle*2, cache.numReqPerCycle*2,
		cache.Name()+".ControlPort")
	cache.AddPort("Control", cache.controlPort)
}

func (b *Builder) createInternalStages(cache *Comp) {
	cache.topParser = &topParser{cache: cache}
	b.buildDirectoryStage(cache)
	b.buildBankStages(cache)
	cache.mshrStage = &mshrStage{cache: cache}
	cache.flusher = &flusher{cache: cache}
	cache.writeBuffer = &writeBufferStage{
		cache:                    cache,
		writeBufferCapacity:      b.writeBufferCapacity,
		writeBufferPeerCapacity:  b.writeBufferPeerCapacity,
		maxPeerIncomingPending:   b.maxPeerIncomingPending,
		maxInflightFetch:         b.maxInflightFetch,
		maxInflightEviction:      b.maxInflightEviction,
		maxOutgoingRemotePending: b.maxOutgoingRemotePending,
	}
}

func (b *Builder) buildDirectoryStage(cache *Comp) {
	// 1. 내부(Local) 요청을 처리하기 위한 파이프라인과 버퍼 생성
	localBuf := sim.NewBuffer(
		cache.Name()+".LocalDirectoryStageInternalBuffer",
		b.numReqPerCycle,
	)
	localPipeline := pipelining.
		MakeBuilder().
		WithCyclePerStage(1).
		WithNumStage(b.dirLatency).
		WithPipelineWidth(b.numReqPerCycle).
		WithPostPipelineBuffer(localBuf).
		Build(cache.Name() + ".Dir.LocalPipeline")

	// 2. 외부(Remote) 요청을 처리하기 위한 파이프라인과 버퍼 생성
	remoteBuf := sim.NewBuffer(
		cache.Name()+".RemoteDirectoryStageInternalBuffer",
		b.numReqPerCycle,
	)
	remotePipeline := pipelining.
		MakeBuilder().
		WithCyclePerStage(1).
		WithNumStage(b.dirLatency).
		WithPipelineWidth(b.numReqPerCycle). // 필요시 Remote의 Width를 더 넓게 줄 수도 있습니다.
		WithPostPipelineBuffer(remoteBuf).
		Build(cache.Name() + ".Dir.RemotePipeline")

	// 3. (Optional) Snoop pipelines for InvReq with extended latency.
	// Built only when snoopLatency > 0; otherwise nil and invalidations
	// flow through localPipeline/remotePipeline as before. Pipelines feed
	// the same localBuf/remoteBuf so the downstream commit loop is
	// unchanged. Their total stage count is dirLatency+snoopLatency so
	// the snoop latency adds *on top of* the regular tag-access time.
	var localSnoopPipeline, remoteSnoopPipeline pipelining.Pipeline
	if b.snoopLatency > 0 {
		localSnoopPipeline = pipelining.
			MakeBuilder().
			WithCyclePerStage(1).
			WithNumStage(b.dirLatency + b.snoopLatency).
			WithPipelineWidth(b.numReqPerCycle).
			WithPostPipelineBuffer(localBuf).
			Build(cache.Name() + ".Dir.LocalSnoopPipeline")
		remoteSnoopPipeline = pipelining.
			MakeBuilder().
			WithCyclePerStage(1).
			WithNumStage(b.dirLatency + b.snoopLatency).
			WithPipelineWidth(b.numReqPerCycle).
			WithPostPipelineBuffer(remoteBuf).
			Build(cache.Name() + ".Dir.RemoteSnoopPipeline")
	}

	// 4. Phase F dedicated InvReq pipeline. ingress: invStageBuffer → invPipeline
	// (dirLatency stages baseline so tag lookup time is honored), post-pipeline:
	// invBuf. processTransaction drains invBuf BEFORE local/remote bufs so
	// invs preempt stalled read/write at the commit stage. The +snoopLatency
	// term lets experiments dial inv-handling cost up without touching the
	// regular read/write pipelines.
	invBuf := sim.NewBuffer(
		cache.Name()+".InvDirectoryStageInternalBuffer",
		b.numReqPerCycle,
	)
	invPipeline := pipelining.
		MakeBuilder().
		WithCyclePerStage(1).
		WithNumStage(b.dirLatency + b.snoopLatency).
		WithPipelineWidth(b.numReqPerCycle).
		WithPostPipelineBuffer(invBuf).
		Build(cache.Name() + ".Dir.InvPipeline")

	// 5. directoryStage 구조체 초기화 및 할당
	cache.dirStage = &directoryStage{
		cache:               cache,
		localPipeline:       localPipeline,
		remotePipeline:      remotePipeline,
		localSnoopPipeline:  localSnoopPipeline,
		remoteSnoopPipeline: remoteSnoopPipeline,
		invPipeline:         invPipeline,
		localBuf:            localBuf,
		remoteBuf:           remoteBuf,
		invBuf:              invBuf,
		activeBuf:           localBuf, // [추가] 현재 활성화된 버퍼를 가리키는 포인터 (Local이 기본)
		returnFalse0:        "",
		returnFalse1:        "",
	}
}

func (b *Builder) buildBankStages(cache *Comp) {
	cache.bankStages = make([]*bankStage, 1)

	laneWidth := b.numReqPerCycle
	if laneWidth == 1 {
		laneWidth = 2
	}

	buf := &bufferImpl{
		name:     fmt.Sprintf("%s.Bank.PostPipelineBuffer", cache.Name()),
		capacity: laneWidth,
	}
	pipeline := pipelining.
		MakeBuilder().
		WithCyclePerStage(1).
		WithNumStage(b.bankLatency).
		WithPipelineWidth(laneWidth).
		WithPostPipelineBuffer(buf).
		Build(fmt.Sprintf("%s.Bank.Pipeline", cache.Name()))
	cache.bankStages[0] = &bankStage{
		cache:           cache,
		bankID:          0,
		pipeline:        pipeline,
		postPipelineBuf: buf,
		pipelineWidth:   laneWidth,
	}
}

func (b *Builder) createInternalBuffers(cache *Comp) {
	cache.cohDirStageBuffer = sim.NewBuffer(
		cache.Name()+".CohDirStageBuffer",
		cache.numReqPerCycle,
	)
	cache.dirStageBuffer = sim.NewBuffer( // coherence directoy to directory
		cache.Name()+".DirStageBuffer",
		cache.numReqPerCycle,
	)
	cache.remoteDirStageBuffer = sim.NewBuffer( // coherence directoy to directory
		cache.Name()+".RemoteDirStageBuffer",
		cache.numReqPerCycle,
	)
	// Phase F priority queue for InvReq. Capacity = numReqPerCycle*4 to
	// absorb burst (a single eviction at home directory can fan out to
	// multiple sharers; bursts of evict-driven invs are common). dirStage
	// drains this BEFORE the regular FIFO each Tick.
	cache.invStageBuffer = sim.NewBuffer(
		cache.Name()+".InvStageBuffer",
		cache.numReqPerCycle*4,
	)
	cache.dirToBankBuffers = make([]sim.Buffer, 1)
	cache.dirToBankBuffers[0] = sim.NewBuffer(
		cache.Name()+".DirToBankBuffer",
		cache.numReqPerCycle,
	)
	cache.dirToBankBuffersLocal = make([]sim.Buffer, 1)
	cache.dirToBankBuffersLocal[0] = sim.NewBuffer(
		cache.Name()+".DirToBankBufferLocal",
		cache.numReqPerCycle,
	)
	cache.dirToBankBuffersRemote = make([]sim.Buffer, 1)
	cache.dirToBankBuffersRemote[0] = sim.NewBuffer(
		cache.Name()+".DirToBankBufferRemote",
		cache.numReqPerCycle,
	)
	// [R5] writeBufferToBankBuffers split into Req / Rsp lanes per bank.
	// Req = bankWriteHit / bankWritePrefetched (write admit path).
	// Rsp = bankWriteFetched (fetch-response path).
	// Backward-compat alias `writeBufferToBankBuffers` retained for length
	// queries (bankID / dirToBank fan-out) — points at the Req slice.
	cache.writeBufferToBankBuffersReq = make([]sim.Buffer, 1)
	cache.writeBufferToBankBuffersReq[0] = sim.NewBuffer(
		cache.Name()+".WriteBufferToBankBufferReq",
		cache.numReqPerCycle,
	)
	cache.writeBufferToBankBuffersRsp = make([]sim.Buffer, 1)
	cache.writeBufferToBankBuffersRsp[0] = sim.NewBuffer(
		cache.Name()+".WriteBufferToBankBufferRsp",
		cache.numReqPerCycle,
	)
	cache.writeBufferToBankBuffers = cache.writeBufferToBankBuffersReq
	// [ITER13 fix #2] local/remote split — each cap=numReqPerCycle so
	// total slots stay 2× the original, but local cannot HoL-block peer.
	cache.mshrStageBuffer = sim.NewBuffer(
		cache.Name()+".MSHRStageBuffer",
		cache.numReqPerCycle,
	)
	cache.mshrStageBufferRemote = sim.NewBuffer(
		cache.Name()+".MSHRStageBufferRemote",
		cache.numReqPerCycle,
	)
	cache.writeBufferBuffer = sim.NewBuffer(
		cache.Name()+".WriteBufferBuffer",
		cache.numReqPerCycle,
	)
	cache.writeBufferBufferRemote = sim.NewBuffer(
		cache.Name()+".WriteBufferBufferRemote",
		cache.numReqPerCycle,
	)
	// [FIX: head-of-line] writeBufferFetch 항목 전용 버퍼.
	// writeBufferBuffer(eviction 전용)와 분리하여 fetch 블로킹이
	// finalizeBankEviction을 막는 순환 의존성을 제거한다.
	cache.writeBufferFetchBuffer = sim.NewBuffer(
		cache.Name()+".WriteBufferFetchBuffer",
		cache.numReqPerCycle,
	)
}
