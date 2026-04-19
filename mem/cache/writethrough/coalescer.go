package writethrough

import (
	"log"
	"reflect"

	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/akita/v4/tracing"
)

type coalescer struct {
	cache      *Comp
	toCoalesce []*transaction
}

func (c *coalescer) Reset() {
	c.toCoalesce = nil
}

func (c *coalescer) Tick() bool {
	req := c.cache.topPort.PeekIncoming()
	if req == nil {
		return false
	}

	return c.processReq(req.(mem.AccessReq))
}

func (c *coalescer) processReq(
	req mem.AccessReq,
) bool {
	if len(c.cache.transactions) >= c.cache.maxNumConcurrentTrans {
		return false
	}

	// read or write mask를 cache line 단위로 기록
	trans := c.createTransaction(req)
	c.recordRWMask(trans)

	if c.isReqLastInWave(req) {
		if len(c.toCoalesce) == 0 || c.canReqCoalesce(req) {
			return c.processReqLastInWaveCoalescable(req)
		}

		return c.processReqLastInWaveNoncoalescable(req)
	}

	if len(c.toCoalesce) == 0 || c.canReqCoalesce(req) {
		return c.processReqCoalescable(req)
	}

	return c.processReqNoncoalescable(req)
}

func (c *coalescer) processReqCoalescable(
	req mem.AccessReq,
) bool {
	trans := c.createTransaction(req)
	c.toCoalesce = append(c.toCoalesce, trans)
	c.cache.transactions = append(c.cache.transactions, trans)
	c.cache.topPort.RetrieveIncoming()

	tracing.TraceReqReceive(req, c.cache)

	return true
}

func (c *coalescer) processReqNoncoalescable(
	req mem.AccessReq,
) bool {
	if !c.cache.dirBuf.CanPush() {
		return false
	}

	c.coalesceAndSend()

	trans := c.createTransaction(req)
	c.toCoalesce = append(c.toCoalesce, trans)
	c.cache.transactions = append(c.cache.transactions, trans)
	c.cache.topPort.RetrieveIncoming()

	tracing.TraceReqReceive(req, c.cache)

	return true
}

func (c *coalescer) processReqLastInWaveCoalescable(
	req mem.AccessReq,
) bool {
	if !c.cache.dirBuf.CanPush() {
		return false
	}

	trans := c.createTransaction(req)
	c.toCoalesce = append(c.toCoalesce, trans)
	c.cache.transactions = append(c.cache.transactions, trans)
	c.coalesceAndSend()
	c.cache.topPort.RetrieveIncoming()

	tracing.TraceReqReceive(req, c.cache)

	return true
}

func (c *coalescer) processReqLastInWaveNoncoalescable(
	req mem.AccessReq,
) bool {
	if !c.cache.dirBuf.CanPush() {
		return false
	}

	c.coalesceAndSend()

	if !c.cache.dirBuf.CanPush() {
		return true
	}

	trans := c.createTransaction(req)
	c.toCoalesce = append(c.toCoalesce, trans)
	c.cache.transactions = append(c.cache.transactions, trans)
	c.coalesceAndSend()
	c.cache.topPort.RetrieveIncoming()

	tracing.TraceReqReceive(req, c.cache)

	return true
}

func (c *coalescer) createTransaction(req mem.AccessReq) *transaction {
	switch req := req.(type) {
	case *mem.ReadReq:
		t := &transaction{
			read: req,
		}

		return t
	case *mem.WriteReq:
		t := &transaction{
			write: req,
		}

		return t
	default:
		log.Panicf("cannot process request of type %s\n", reflect.TypeOf(req))
		return nil
	}
}

func (c *coalescer) isReqLastInWave(req mem.AccessReq) bool {
	switch req := req.(type) {
	case *mem.ReadReq:
		return !req.CanWaitForCoalesce
	case *mem.WriteReq:
		return !req.CanWaitForCoalesce
	default:
		panic("unknown type")
	}
}

func (c *coalescer) canReqCoalesce(req mem.AccessReq) bool {
	blockSize := uint64(1 << c.cache.log2BlockSize)
	return req.GetAddress()/blockSize == c.toCoalesce[0].Address()/blockSize
}

func (c *coalescer) coalesceAndSend() bool {
	var trans *transaction
	if c.toCoalesce[0].read != nil {
		trans = c.coalesceRead()
		tracing.StartTaskWithSpecificLocation(trans.id,
			tracing.MsgIDAtReceiver(c.toCoalesce[0].read, c.cache),
			c.cache, "cache_transaction", "read",
			c.cache.Name()+".Local",
			nil)
	} else {
		trans = c.coalesceWrite()
		tracing.StartTaskWithSpecificLocation(trans.id,
			tracing.MsgIDAtReceiver(c.toCoalesce[0].write, c.cache),
			c.cache, "cache_transaction", "write",
			c.cache.Name()+".Local",
			nil)
	}

	c.cache.dirBuf.Push(trans)
	c.cache.postCoalesceTransactions =
		append(c.cache.postCoalesceTransactions, trans)
	c.toCoalesce = nil

	return true
}

func (c *coalescer) coalesceRead() *transaction {
	blockSize := uint64(1 << c.cache.log2BlockSize)
	cachelineID := c.toCoalesce[0].Address() / blockSize * blockSize
	coalescedRead := mem.ReadReqBuilder{}.
		WithAddress(cachelineID).
		WithVAddr(c.toCoalesce[0].read.GetVAddr()).
		WithByteSize(blockSize).
		WithPID(c.toCoalesce[0].PID()).
		Build()

	return &transaction{
		id:                      sim.GetIDGenerator().Generate(),
		read:                    coalescedRead,
		preCoalesceTransactions: c.toCoalesce,
	}
}

func (c *coalescer) coalesceWrite() *transaction {
	blockSize := uint64(1 << c.cache.log2BlockSize)
	cachelineID := c.toCoalesce[0].Address() / blockSize * blockSize
	write := mem.WriteReqBuilder{}.
		WithAddress(cachelineID).
		WithVAddr(c.toCoalesce[0].write.GetVAddr()).
		WithPID(c.toCoalesce[0].PID()).
		WithData(make([]byte, blockSize)).
		WithDirtyMask(make([]bool, blockSize)).
		Build()

	for _, t := range c.toCoalesce {
		w := t.write
		offset := int(w.Address - cachelineID)

		for i := 0; i < len(w.Data); i++ {
			if w.DirtyMask == nil || w.DirtyMask[i] {
				write.Data[i+offset] = w.Data[i]
				write.DirtyMask[i+offset] = true
			}
		}
	}

	return &transaction{
		id:                      sim.GetIDGenerator().Generate(),
		write:                   write,
		preCoalesceTransactions: c.toCoalesce,
	}
}

func (c *coalescer) recordRWMask(trans *transaction) {
	if trans.read != nil {
		startPage := trans.read.GetVAddr() / (1 << c.cache.log2PageSize)
		startIndex := trans.read.GetVAddr() % (1 << c.cache.log2PageSize) / uint64(1<<c.cache.log2BlockSize)
		endPage := (trans.read.GetVAddr() + trans.read.AccessByteSize - 1) / (1 << c.cache.log2PageSize)
		endIndex := trans.read.GetVAddr() + trans.read.AccessByteSize - 1
		endIndex = endIndex % (1 << c.cache.log2PageSize) / uint64(1<<c.cache.log2BlockSize)

		for page := startPage; page <= endPage; page++ {
			if (*(c.cache.readMask))[c.cache.deviceID-1] == nil {
				(*(c.cache.readMask))[c.cache.deviceID-1] = make(map[vm.PID]map[uint64][]uint8)
				(*(c.cache.dirtyMask))[c.cache.deviceID-1] = make(map[vm.PID]map[uint64][]uint8)
			}
			if (*(c.cache.readMask))[c.cache.deviceID-1][trans.read.GetPID()] == nil {
				(*(c.cache.readMask))[c.cache.deviceID-1][trans.read.GetPID()] = make(map[uint64][]uint8)
				(*(c.cache.dirtyMask))[c.cache.deviceID-1][trans.read.GetPID()] = make(map[uint64][]uint8)
			}
			if (*(c.cache.readMask))[c.cache.deviceID-1][trans.read.GetPID()][page] == nil {
				(*(c.cache.readMask))[c.cache.deviceID-1][trans.read.GetPID()][page] = make([]uint8, (1<<c.cache.log2PageSize)/(1<<c.cache.log2BlockSize))
				(*(c.cache.dirtyMask))[c.cache.deviceID-1][trans.read.GetPID()][page] = make([]uint8, (1<<c.cache.log2PageSize)/(1<<c.cache.log2BlockSize))
			}

			mask := (*(c.cache.readMask))[c.cache.deviceID-1][trans.read.GetPID()][page]

			var start, end uint64
			if page == startPage {
				start = startIndex
			} else {
				start = 0
			}

			if page == endPage {
				end = endIndex
			} else {
				end = (1<<c.cache.log2PageSize)/(1<<c.cache.log2BlockSize) - 1
			}

			for i := start; i <= end; i++ {
				mask[i] = 1
			}
		}

		// fmt.Printf("[%s]\tUpdate ReadMask for Read Req VA %x, ReadMask: %v\n", c.cache.Name(), trans.read.GetVAddr()/(1<<c.cache.log2PageSize),
		// 	(*(c.cache.readMask))[c.cache.deviceID-1][trans.read.GetPID()][trans.read.GetVAddr()/(1<<c.cache.log2PageSize)])
	} else {
		startPage := trans.write.GetVAddr() / (1 << c.cache.log2PageSize)
		startIndex := trans.write.GetVAddr() % (1 << c.cache.log2PageSize) / uint64(1<<c.cache.log2BlockSize)
		numBlocks := len(trans.write.Data) / (1 << c.cache.log2BlockSize)

		for i := 0; i < numBlocks; i++ {
			page := startPage + (startIndex+uint64(i))/(1<<(c.cache.log2PageSize-c.cache.log2BlockSize))
			index := (startIndex + uint64(i)) % (1 << (c.cache.log2PageSize - c.cache.log2BlockSize))

			if (*(c.cache.dirtyMask))[c.cache.deviceID-1] == nil {
				(*(c.cache.readMask))[c.cache.deviceID-1] = make(map[vm.PID]map[uint64][]uint8)
				(*(c.cache.dirtyMask))[c.cache.deviceID-1] = make(map[vm.PID]map[uint64][]uint8)
			}
			if (*(c.cache.dirtyMask))[c.cache.deviceID-1][trans.write.GetPID()] == nil {
				(*(c.cache.readMask))[c.cache.deviceID-1][trans.write.GetPID()] = make(map[uint64][]uint8)
				(*(c.cache.dirtyMask))[c.cache.deviceID-1][trans.write.GetPID()] = make(map[uint64][]uint8)
			}
			if (*(c.cache.dirtyMask))[c.cache.deviceID-1][trans.write.GetPID()][page] == nil {
				(*(c.cache.readMask))[c.cache.deviceID-1][trans.write.GetPID()][page] = make([]uint8, (1<<c.cache.log2PageSize)/(1<<c.cache.log2BlockSize))
				(*(c.cache.dirtyMask))[c.cache.deviceID-1][trans.write.GetPID()][page] = make([]uint8, (1<<c.cache.log2PageSize)/(1<<c.cache.log2BlockSize))
			}

			mask := (*(c.cache.dirtyMask))[c.cache.deviceID-1][trans.write.GetPID()][page]
			mask[index] = 1
		}

		// fmt.Printf("[%s]\tUpdate DirtyMask for Write Req VA %x, DirtyMask: %v\n", c.cache.Name(), trans.write.GetVAddr()/(1<<c.cache.log2PageSize),
		// 	(*(c.cache.dirtyMask))[c.cache.deviceID-1][trans.write.GetPID()][trans.write.GetVAddr()/(1<<c.cache.log2PageSize)])
	}
}
