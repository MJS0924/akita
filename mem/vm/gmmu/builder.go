package gmmu

import (
	"fmt"

	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
)

// A Builder can build GMMU component
type Builder struct {
	engine              sim.Engine
	freq                sim.Freq
	log2PageSize        uint64
	pageTable           vm.LevelPageTable
	maxNumReqInFlight   int
	pageWalkingLatency  int
	deviceID            uint64
	lowModule           sim.RemotePort
	accessCounter       *map[vm.PID]map[uint64]uint8
	dirtyMask           *[]map[vm.PID]map[uint64][]uint8
	readMask            *[]map[vm.PID]map[uint64][]uint8
	size                uint64
	pageMigrationPolicy uint64
}

// MakeBuilder creates a new builder
func MakeBuilder() Builder {
	return Builder{
		freq:              1 * sim.GHz,
		log2PageSize:      12,
		maxNumReqInFlight: 16,
	}
}

// WithEngine sets the engine to be used with the MMU
func (b Builder) WithEngine(engine sim.Engine) Builder {
	b.engine = engine
	return b
}

// WithFreq sets the frequency that the MMU to work at
func (b Builder) WithFreq(freq sim.Freq) Builder {
	b.freq = freq
	return b
}

// WithLog2PageSize sets the page size that the mmu support.
func (b Builder) WithLog2PageSize(log2PageSize uint64) Builder {
	b.log2PageSize = log2PageSize
	return b
}

// WithPageTable sets the page table that the MMU uses.
func (b Builder) WithPageTable(pageTable vm.LevelPageTable) Builder {
	b.pageTable = pageTable
	return b
}

func (b Builder) WithPageTableLogSize(size uint64) Builder {
	b.size = size
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

func (b Builder) WithPageMigrationPolicy(policy uint64) Builder {
	b.pageMigrationPolicy = policy
	return b
}

// WithMaxNumReqInFlight sets the number of requests can be concurrently
// processed by the MMU.
func (b Builder) WithMaxNumReqInFlight(n int) Builder {
	b.maxNumReqInFlight = n
	return b
}

// WithPageWalkingLatency sets the number of cycles required for walking a page
// table.
func (b Builder) WithPageWalkingLatency(n int) Builder {
	b.pageWalkingLatency = n
	return b
}

// WithDeviceID sets the device ID of the GMMU
func (b Builder) WithDeviceID(deviceID uint64) Builder {
	b.deviceID = deviceID
	return b
}

// WithLowModule sets the low module of the GMMU
func (b Builder) WithLowModule(p sim.RemotePort) Builder {
	b.lowModule = p
	return b
}

func (b Builder) WithAccessCounter(ac *map[vm.PID]map[uint64]uint8) Builder {
	b.accessCounter = ac
	return b
}

// Build returns a newly created GMMU component
func (b Builder) Build(name string) *Comp {
	gmmu := new(Comp)
	gmmu.TickingComponent = *sim.NewTickingComponent(
		name, b.engine, b.freq, gmmu)

	b.createPorts(name, gmmu)
	b.createPageTable(gmmu)
	b.configureInternalStates(gmmu)

	// gmmu.AddMiddleware(middleware)

	return gmmu
}

func (b Builder) configureInternalStates(gmmu *Comp) {
	gmmu.deviceID = b.deviceID
	gmmu.LowModule = b.lowModule

	gmmu.maxRequestsInFlight = b.maxNumReqInFlight
	gmmu.latency = b.pageWalkingLatency
	gmmu.PageAccessedByDeviceID = make(map[uint64][]uint64)
	gmmu.remoteMemReqs = make(map[uint64]transaction)
	gmmu.migrationReqs = make(map[uint64]transaction)
	gmmu.invalidationReqs = make(map[uint64]transaction)
	gmmu.isFlushing = false

	gmmu.log2PageSize = b.log2PageSize
	gmmu.pageTableCache.Sets = make(map[string]*vm.Set)
	gmmu.pageTableCache.CapOfBlks = 8
	gmmu.pageTableCache.Latency = 5

	gmmu.AccessCounter = b.accessCounter
	gmmu.dirtyMask = b.dirtyMask
	gmmu.readMask = b.readMask

	gmmu.pageMigrationPolicy = b.pageMigrationPolicy
	fmt.Printf("[GMMU Builder]\tGPU %d GMMU built with PageMigrationPolicy=%d\n", b.deviceID, b.pageMigrationPolicy)
}

func (b Builder) createPageTable(gmmu *Comp) {
	if b.pageTable != nil {
		gmmu.pageTable = b.pageTable
	} else {
		gmmu.pageTable = vm.NewLevelPageTable(b.log2PageSize, 6, fmt.Sprintf("GMMU[%d].PT", gmmu.deviceID))
	}
}

func (b Builder) createPorts(name string, gmmu *Comp) {
	gmmu.topPort = sim.NewPort(gmmu, 4096, 4096, name+".ToTop")
	gmmu.AddPort("Top", gmmu.topPort)
	gmmu.bottomPort = sim.NewPort(gmmu, 4096, 4096, name+".ToBottom")
	gmmu.AddPort("Bottom", gmmu.bottomPort)
	gmmu.controlPort = sim.NewPort(gmmu, 4096, 4096, name+".ToControl")
	gmmu.AddPort("Control", gmmu.controlPort)
	// gmmu.LowModule = sim.NewPort(gmmu, 1, 1, name+".ToLowModule")
	// gmmu.AddPort("LowModule", gmmu.LowModule)

	gmmu.topSender = sim.NewBufferedSender(
		gmmu.topPort, sim.NewBuffer(name+".TopSenderBuffer", 4096))
	gmmu.bottomSender = sim.NewBufferedSender(
		gmmu.bottomPort, sim.NewBuffer(name+".BottomSenderBuffer", 4096))
}
