package coherence

import (
	"fmt"
	"log"
	"reflect"
	"strings"

	"github.com/sarchlab/akita/v4/mem/cache/cohdirectory/internal"
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

	topPort     sim.Port
	bottomPort  sim.Port
	controlPort sim.Port
	RDMAPort    sim.Port
	ToRDMA      sim.RemotePort

	cohDirStageBuffer        sim.Buffer
	dirStageBuffer           sim.Buffer
	dirToBankBuffers         []sim.Buffer
	writeBufferToBankBuffers []sim.Buffer
	mshrStageBuffer          sim.Buffer
	bottomSenderBuffer       sim.Buffer
	invRspBuffer             sim.Buffer

	topParser    *topParser
	bottomSender *bottomSender
	dirStage     *directoryStage
	bankStages   []*bankStage
	mshrStage    *mshrStage
	flusher      *flusher

	storage               *mem.Storage
	addressToPortMapper   mem.AddressToPortMapper
	l2AddressToPortMapper mem.AddressToPortMapper // incoming request가 remote/local data에 대한 것인지 판단
	directory             internal.CohDirectory
	mshr                  internal.MSHR
	log2BlockSize         uint64
	log2PageSize          uint64
	numReqPerCycle        int

	state            cacheState
	flushLocalAccess bool
	evictingList     map[uint64]bool

	DirtyMask *[]map[vm.PID]map[uint64][]uint8
	ReadMask  *[]map[vm.PID]map[uint64][]uint8

	returnValue bool
	printReturn bool
	recordTime  sim.VTimeInSec
}

func (c *Comp) SetAddressToPortMapper(lmf mem.AddressToPortMapper) {
	c.addressToPortMapper = lmf
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
	now := m.Engine.CurrentTime()
	m.printReturn = false
	if now >= m.recordTime+0.000001 && (m.deviceID == 5 || m.deviceID == 4) {
		m.recordTime = now
		m.printReturn = true
	}

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

	// for test connectivity
	// if m.state != cacheStatePaused {
	// 	madeProgress = m.SendToBottom() || madeProgress
	// 	madeProgress = m.SendToTop() || madeProgress
	// }

	m.returnValue = madeProgress
	return madeProgress
}

func (m *middleware) runPipeline() bool {
	madeProgress := false

	temp := m.runStage(m.mshrStage)
	madeProgress = temp || madeProgress
	if m.printReturn {
		fmt.Printf("[DEBUG CohDir %d]\treturn 1.1: %v\n", m.deviceID, temp)
	}

	for i, bs := range m.bankStages {
		temp = bs.Tick()
		madeProgress = temp || madeProgress
		if m.printReturn {
			fmt.Printf("[DEBUG CohDir %d]\treturn 1.2.%d: %v\n", m.deviceID, i, temp)
		}
	}

	temp = m.runStage(m.bottomSender)
	madeProgress = temp || madeProgress
	if m.printReturn {
		fmt.Printf("[DEBUG CohDir %d]\treturn 1.3: %v\n", m.deviceID, temp)
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
	c.bottomSender.Reset()

	clearPort(c.topPort)

	// for _, t := range c.inFlightTransactions {
	// 	fmt.Printf("%.10f, %s, transaction %s discarded due to flushing\n",
	// 		now, c.Name(), t.id)
	// }
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

// for test connectivity
// func (c *Comp) SendToBottom() bool {
// 	if c.bottomPort.CanSend() {
// 		if c.topPort.PeekIncoming() == nil {
// 			return false
// 		}

// 		req := c.topPort.RetrieveIncoming()

// 		switch req := req.(type) {
// 		case *mem.ReadReq:
// 			read := mem.ReadReqBuilder{}.
// 				WithSrc(c.bottomPort.AsRemote()).
// 				WithDst(c.addressToPortMapper.Find(req.GetAddress())).
// 				WithPID(req.GetPID()).
// 				WithAddress(req.GetAddress()).
// 				WithByteSize(1 << c.log2BlockSize).
// 				Build()

// 			fmt.Printf("[%s]\tRead Req %x from %s to %s\n", c.Name(), read.GetAddress(), req.Meta().Src, read.Meta().Dst)
// 			c.bottomPort.Send(read)

// 			inflightTrans := &transaction{
// 				read:           read,
// 				readReqFromTop: req,
// 			}
// 			c.inFlightTransactions = append(c.inFlightTransactions, inflightTrans)
// 		case *mem.WriteReq:
// 			write := mem.WriteReqBuilder{}.
// 				WithSrc(c.bottomPort.AsRemote()).
// 				WithDst(c.addressToPortMapper.Find(req.GetAddress())).
// 				WithPID(req.GetPID()).
// 				WithAddress(req.GetAddress()).
// 				WithData(req.Data).
// 				WithDirtyMask(req.DirtyMask).
// 				Build()

// 			fmt.Printf("[%s]\tWrite Req %x from %s to %s\n", c.Name(), write.GetAddress(), req.Meta().Src, write.Meta().Dst)
// 			c.bottomPort.Send(write)

// 			inflightTrans := &transaction{
// 				write:           write,
// 				writeReqFromTop: req,
// 			}
// 			c.inFlightTransactions = append(c.inFlightTransactions, inflightTrans)
// 		}

// 		return true
// 	}

// 	return false
// }

// func (c *Comp) SendToTop() bool {
// 	if c.topPort.CanSend() {
// 		if c.bottomPort.PeekIncoming() == nil {
// 			return false
// 		}
// 		msg := c.bottomPort.RetrieveIncoming()

// 		switch msg := msg.(type) {
// 		case *mem.DataReadyRsp:
// 			return c.dataRdyRsp(msg)
// 		case *mem.WriteDoneRsp:
// 			return c.dataWrtRsp(msg)
// 		default:
// 			panic("unknown msg type")
// 		}
// 	}

// 	return false
// }

// func (c *Comp) dataRdyRsp(
// 	dataReady *mem.DataReadyRsp,
// ) bool {
// 	trans := c.findInFlightTrans(dataReady.RespondTo)

// 	rsp := mem.DataReadyRspBuilder{}.
// 		WithSrc(c.topPort.AsRemote()).
// 		WithDst(trans.readReqFromTop.Src).
// 		WithRspTo(trans.readReqFromTop.Meta().ID).
// 		WithData(dataReady.Data).
// 		Build()

// 	fmt.Printf("[%s]\tRead Rsp %x from %s to %s\n",
// 		c.Name(), trans.read.GetAddress(), dataReady.Meta().Src, rsp.Meta().Dst)

// 	c.topPort.Send(rsp)

// 	return true
// }

// func (c *Comp) dataWrtRsp(
// 	writeDone *mem.WriteDoneRsp,
// ) bool {
// 	trans := c.findInFlightTrans(writeDone.RespondTo)

// 	rsp := mem.WriteDoneRspBuilder{}.
// 		WithSrc(c.topPort.AsRemote()).
// 		WithDst(trans.writeReqFromTop.Src).
// 		WithRspTo(trans.writeReqFromTop.Meta().ID).
// 		Build()

// 	fmt.Printf("[%s]\tWrite Rsp %x from %s to %s\n",
// 		c.Name(), trans.write.GetAddress(), writeDone.Meta().Src, rsp.Meta().Dst)

// 	c.topPort.Send(rsp)

// 	return true
// }

// func (c *Comp) findInFlightTrans(
// 	id string,
// ) *transaction {
// 	for i, t := range c.inFlightTransactions {
// 		if t.read != nil && t.read.Meta().ID == id {
// 			c.inFlightTransactions = append(c.inFlightTransactions[:i], c.inFlightTransactions[i+1:]...)
// 			return t
// 		} else if t.write != nil && t.write.Meta().ID == id {
// 			c.inFlightTransactions = append(c.inFlightTransactions[:i], c.inFlightTransactions[i+1:]...)
// 			return t
// 		}
// 	}

// 	panic("inflight read not found")
// }

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
