package globalmshr

import (
	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"

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

	DirtyMask *[]map[vm.PID]map[uint64][]uint8
	ReadMask  *[]map[vm.PID]map[uint64][]uint8
}

// MakeBuilder creates a new builder with default configurations.
func MakeBuilder() Builder {
	return Builder{
		freq:                1 * sim.GHz,
		wayAssociativity:    4,
		log2BlockSize:       6,
		byteSize:            512 * mem.KB,
		numMSHREntry:        16,
		numReqPerCycle:      1,
		writeBufferCapacity: 1024,
		maxInflightFetch:    128,
		maxInflightEviction: 128,
		bankLatency:         10,
	}
}
