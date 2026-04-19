package largeblkcache

import (
	"fmt"

	"github.com/sarchlab/akita/v4/mem/cache/largeblkcache/internal"
	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/tracing"
)

type mshrStage struct {
	cache *Comp

	processingMSHREntry *internal.MSHREntry
}

func (s *mshrStage) Tick() bool {
	if s.processingMSHREntry != nil {
		return s.processOneReq()
	}

	item := s.cache.mshrStageBuffer.Pop()
	if item == nil {
		return false
	}

	s.processingMSHREntry = item.(*internal.MSHREntry)

	return s.processOneReq()
}

func (s *mshrStage) Reset() {
	s.processingMSHREntry = nil
	s.cache.mshrStageBuffer.Clear()
}

func (s *mshrStage) processOneReq() bool {
	mshrEntry := s.processingMSHREntry
	trans := mshrEntry.Requests[0].(*transaction)

	if trans.fromLocal && !s.cache.topPort.CanSend() {
		return false
	} else if !trans.fromLocal && !s.cache.remoteTopPort.CanSend() {
		return false
	}

	transIndex := s.findTransaction(trans)

	if transIndex != -1 {
		s.removeTransaction(trans)

		if trans.read != nil {
			s.respondRead(trans.read, mshrEntry.Data, trans.fromLocal)
		} else {
			s.respondWrite(trans.write, trans.fromLocal)
		}

		mshrEntry.Requests = mshrEntry.Requests[1:]
		if len(mshrEntry.Requests) == 0 {
			s.processingMSHREntry = nil
		}

		return true
	} else {
		fmt.Printf("[%s]\t[Warning] Transaction %s not found in mshr stage. Ignored.\n",
			s.cache.Name(), trans.accessReq().GetAddress())
	}

	mshrEntry.Requests = mshrEntry.Requests[1:]
	if len(mshrEntry.Requests) == 0 {
		s.processingMSHREntry = nil
	}

	return true
}

func (s *mshrStage) respondRead(
	read *mem.ReadReq,
	data []byte,
	fromLocal bool,
) {
	_, offset := getCacheLineID(read.Address, s.cache.log2BlockSize)
	if offset+read.AccessByteSize > 1<<s.cache.log2BlockSize {
		fmt.Printf("[%s]\t[ERROR]\tNot Aligned Access Req from %s\n", s.cache.name, read.Src)
	}
	dataReady := mem.DataReadyRspBuilder{}.
		WithSrc(s.cache.topPort.AsRemote()).
		WithDst(read.Src).
		WithRspTo(read.Meta().ID).
		WithData(data[offset : offset+read.AccessByteSize]).
		WithOrigin(read).
		Build()
	if !fromLocal {
		dataReady.Src = s.cache.remoteTopPort.AsRemote()
		s.cache.remoteTopPort.Send(dataReady)
	} else {
		s.cache.topPort.Send(dataReady)
	}
	// fmt.Printf("[%s]\tSend Data Ready Response %x to %s for reqID %s\n", s.cache.name, read.Address, read.Src, read.ID)

	if s.cache.deviceID == 3 && read.GetAddress() == 12888246464 {
		fmt.Printf("[%s][DEBUG]\tReadReq received - 4.9: %d\n", s.cache.name, read.GetAddress())
	}
	tracing.TraceReqComplete(read, s.cache)
}

func (s *mshrStage) respondWrite(write *mem.WriteReq, fromLocal bool) {
	writeDoneRsp := mem.WriteDoneRspBuilder{}.
		WithSrc(s.cache.topPort.AsRemote()).
		WithDst(write.Src).
		WithRspTo(write.Meta().ID).
		WithOrigin(write).
		Build()
	if !fromLocal {
		writeDoneRsp.Src = s.cache.remoteTopPort.AsRemote()
		s.cache.remoteTopPort.Send(writeDoneRsp)
	} else {
		s.cache.topPort.Send(writeDoneRsp)
	}
	// fmt.Printf("[%s]\tSend Write Done Response %x to %s for reqID %s\n", s.cache.name, write.Address, write.Src, write.ID)

	tracing.TraceReqComplete(write, s.cache)
}

func (s *mshrStage) removeTransaction(trans *transaction) {
	for i, t := range s.cache.inFlightTransactions {
		if trans == t {
			// fmt.Printf("%.10f, %s, transaction %s removed in mshr stage.\n",
			// now, s.cache.Name(), t.id)
			s.cache.inFlightTransactions = append(
				(s.cache.inFlightTransactions)[:i],
				(s.cache.inFlightTransactions)[i+1:]...)

			return
		}
	}

	// panic("transaction not found")
	fmt.Printf("[%s]\t[Warning] transaction %s not found in removeTransaction. Ignored.\n", s.cache.Name(), trans.id)
}

func (s *mshrStage) findTransaction(trans *transaction) int {
	for i, t := range s.cache.inFlightTransactions {
		if trans == t {
			return i
		}
	}

	return -1
}
