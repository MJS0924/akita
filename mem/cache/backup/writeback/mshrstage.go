package writeback

import (
	"fmt"

	"github.com/sarchlab/akita/v4/mem/cache"
	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/tracing"
)

type mshrStage struct {
	cache *Comp

	processingMSHREntry *cache.MSHREntry
}

func (s *mshrStage) Tick() bool {
	if s.processingMSHREntry != nil {
		return s.processOneReq()
	}

	item := s.cache.mshrStageBuffer.Pop()
	if item == nil {
		return false
	}

	s.processingMSHREntry = item.(*cache.MSHREntry)

	return s.processOneReq()
}

func (s *mshrStage) Reset() {
	s.processingMSHREntry = nil
	s.cache.mshrStageBuffer.Clear()
}

func (s *mshrStage) processOneReq() bool {
	if !s.cache.topPort.CanSend() {
		return false
	}

	mshrEntry := s.processingMSHREntry
	trans := mshrEntry.Requests[0].(*transaction)

	transIndex := s.findTransaction(trans)

	if transIndex != -1 {
		s.removeTransaction(trans)

		if trans.read != nil {
			s.respondRead(trans.read, mshrEntry.Data)
		} else {
			s.respondWrite(trans.write)
		}

		mshrEntry.Requests = mshrEntry.Requests[1:]
		if len(mshrEntry.Requests) == 0 {
			s.processingMSHREntry = nil
		}

		return true
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
) {
	_, offset := getCacheLineID(read.Address, s.cache.log2BlockSize)
	dataReady := mem.DataReadyRspBuilder{}.
		WithSrc(s.cache.topPort.AsRemote()).
		WithDst(read.Src).
		WithRspTo(read.ID).
		WithData(data[offset : offset+read.AccessByteSize]).
		WithOrigin(read).
		Build()
	s.cache.topPort.Send(dataReady)
	// fmt.Printf("[%s]\tSend Data Ready Response %x to %s for reqID %s\n", s.cache.name, read.Address, read.Src, read.ID)

	tracing.TraceReqComplete(read, s.cache)
}

func (s *mshrStage) respondWrite(write *mem.WriteReq) {
	writeDoneRsp := mem.WriteDoneRspBuilder{}.
		WithSrc(s.cache.topPort.AsRemote()).
		WithDst(write.Src).
		WithRspTo(write.ID).
		WithOrigin(write).
		Build()
	s.cache.topPort.Send(writeDoneRsp)
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
