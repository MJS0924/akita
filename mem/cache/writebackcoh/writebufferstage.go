package writebackcoh

import (
	"fmt"
	"os"
	"strings"

	"github.com/sarchlab/akita/v4/mem/cache/writebackcoh/internal"
	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/akita/v4/tracing"
)

type writeBufferStage struct {
	cache *Comp

	writeBufferCapacity int
	maxInflightFetch    int
	maxInflightEviction int

	pendingEvictions []*transaction
	inflightFetch    []*transaction
	inflightEviction []*transaction

	// Local/remote split mirrors the fetch quota in superdirectory's
	// bottomSender (75% local / 25% remote). Without this split,
	// remote-routed evictions (cross-GPU writebacks via SD/REC) and
	// local DRAM evictions share one cap; a backpressure burst on
	// the cross-GPU path can fill the cap and HoL-block local evictions
	// (and vice versa), producing cross-GPU circular wait deadlocks
	// observed under stencil2d SD.
	numLocalInflightEviction  int
	numRemoteInflightEviction int

	// Typed sub-queues for bottomPort response handling. Without these,
	// processDataReadyRsp may fail (writeBufferToBankBuffers cap full →
	// returns false without RetrieveIncoming), leaving a DataReadyRsp
	// stuck at bottomPort head. WriteDoneRsp behind it in the FIFO is
	// then never observed, so inflightEviction never frees and the
	// writeBuffer cap → write-through stall → cross-GPU cyclic deadlock
	// (observed under CD coherence-unit-size=0 page-migration burst).
	// Caps match the actual port buffer (4 each) so total cache buffer
	// stays nearly unchanged — invalidation/write-through cost models
	// are not affected.
	pendingDataReady []*mem.DataReadyRsp
	pendingWriteDone []*mem.WriteDoneRsp
}


func (wb *writeBufferStage) Tick() bool {
	madeProgress := false

	madeProgress = wb.write() || madeProgress

	// Drain bottomPort head into type-classified sub-queues, then process
	// each type from its own queue. Separates DataReadyRsp from
	// WriteDoneRsp so a stuck DataReadyRsp (bankBuf full, processPrefetch
	// retry, etc.) cannot HoL-block WriteDoneRsp behind it in the port
	// FIFO. WriteDoneRsp processing must always make progress to free
	// inflightEviction slots; otherwise the writeBuffer caps and the
	// cross-GPU write-through cycle deadlocks.
	madeProgress = wb.drainBottomTyped() || madeProgress
	madeProgress = wb.processPendingDataReady() || madeProgress
	madeProgress = wb.processPendingWriteDone() || madeProgress

	madeProgress = wb.processNewTransaction() || madeProgress
	// [FIX: head-of-line] writeBufferFetchBuffer(fetch 전용)를 writeBufferBuffer(eviction 전용)와
	// 독립적으로 처리. fetch 블로킹이 eviction 진행을 막지 않도록 분리.
	madeProgress = wb.processNewFetch() || madeProgress

	return madeProgress
}

func (wb *writeBufferStage) drainBottomTyped() bool {
	// Drain bottomPort head unconditionally into typed sub-queues. Caps
	// are intentionally NOT applied here — capping the typed queue
	// would re-introduce the same HoL behavior the queues are meant to
	// eliminate (a stuck DataReadyRsp at port head behind which
	// WriteDoneRsp cannot be observed). The typed queues are normally
	// near-empty (drained the same cycle by processPending* helpers);
	// they only grow during the rare downstream-stall window the fix
	// is designed to break. No inflight cap is being raised — these
	// are pure ingress sub-queues with no model semantics.
	msg := wb.cache.bottomPort.PeekIncoming()
	if msg == nil {
		return false
	}

	switch m := msg.(type) {
	case *mem.DataReadyRsp:
		wb.pendingDataReady = append(wb.pendingDataReady, m)
	case *mem.WriteDoneRsp:
		wb.pendingWriteDone = append(wb.pendingWriteDone, m)
	default:
		panic("unknown msg type on bottomPort")
	}

	wb.cache.bottomPort.RetrieveIncoming()
	return true
}

func (wb *writeBufferStage) processPendingDataReady() bool {
	if len(wb.pendingDataReady) == 0 {
		return false
	}

	head := wb.pendingDataReady[0]
	if !wb.tryProcessDataReadyRsp(head) {
		return false
	}

	wb.pendingDataReady = wb.pendingDataReady[1:]
	return true
}

func (wb *writeBufferStage) processPendingWriteDone() bool {
	if len(wb.pendingWriteDone) == 0 {
		return false
	}

	head := wb.pendingWriteDone[0]
	wb.applyWriteDoneRsp(head)
	wb.pendingWriteDone = wb.pendingWriteDone[1:]
	return true
}

func (wb *writeBufferStage) processNewTransaction() bool {
	item := wb.cache.writeBufferBuffer.Peek()
	if item == nil {
		return false
	}

	trans := item.(*transaction)
	if wb.cache.debugProcess && trans.accessReq() != nil && trans.accessReq().GetAddress() == wb.cache.debugAddress0 {
		fmt.Printf("[%s] [writebufferstage]\tReceived read req - 3: addr %x, action %d\n", wb.cache.name, trans.accessReq().GetAddress(), trans.action)
	}
	if wb.cache.debugProcess && trans.accessReq() != nil && trans.accessReq().GetAddress() == wb.cache.debugAddress1 {
		fmt.Printf("[%s] [writebufferstage]\tReceived read req - 3: addr %x, action %d\n", wb.cache.name, trans.accessReq().GetAddress(), trans.action)
	}
	if wb.cache.debugProcess && trans.responsing {
		fmt.Printf("[%s]\tTransaction %x is responsing, discard.\n", wb.cache.name, trans.accessReq().GetAddress())
		wb.cache.writeBufferBuffer.Pop()
		return true
	}
	switch trans.action {
	case writeBufferFetch:
		return wb.processWriteBufferFetch(trans)
	case writeBufferEvictAndWrite:
		return wb.processWriteBufferEvictAndWrite(trans)
	case writeBufferEvictAndFetch:
		return wb.processWriteBufferFetchAndEvict(trans)
	case writeBufferEvictAndPrefetch:
		return wb.processWriteBufferEvictAndPrefetch(trans)
	case writeBufferFlush:
		return wb.processWriteBufferFlush(trans, true)
	default:
		fmt.Printf("[%s]\t[WARNING]\tUnknown transaction action %d, trans: %x\n", wb.cache.name, trans.action, trans)
		for true {
		}
		panic("unknown transaction action")
	}
}

func (wb *writeBufferStage) processWriteBufferFetch(
	trans *transaction,
) bool {
	if wb.findDataLocally(trans) {
		if wb.cache.debugProcess && trans.accessReq() != nil && trans.accessReq().GetAddress() == wb.cache.debugAddress0 {
			fmt.Printf("[%s] [writebufferstage]\tReceived req - 3.0: addr %x, action %d\n", wb.cache.name, trans.accessReq().GetAddress(), trans.action)
		}
		if wb.cache.debugProcess && trans.accessReq() != nil && trans.accessReq().GetAddress() == wb.cache.debugAddress1 {
			fmt.Printf("[%s] [writebufferstage]\tReceived req - 3.0: addr %x, action %d\n", wb.cache.name, trans.accessReq().GetAddress(), trans.action)
		}
		// EvictAndFetch 변환 케이스: writeBufferBuffer에서 pop
		return wb.sendFetchedDataToBank(trans, wb.cache.writeBufferBuffer)
	}

	if wb.cache.debugProcess && trans.accessReq() != nil && trans.accessReq().GetAddress() == wb.cache.debugAddress0 {
		fmt.Printf("[%s] [writebufferstage]\tReceived req - 3.1: addr %x, action %d\n", wb.cache.name, trans.accessReq().GetAddress(), trans.action)
	}
	if wb.cache.debugProcess && trans.accessReq() != nil && trans.accessReq().GetAddress() == wb.cache.debugAddress1 {
		fmt.Printf("[%s] [writebufferstage]\tReceived req - 3.1: addr %x, action %d\n", wb.cache.name, trans.accessReq().GetAddress(), trans.action)
	}
	// EvictAndFetch 변환 케이스: writeBufferBuffer에서 pop
	return wb.fetchFromBottom(trans, wb.cache.writeBufferBuffer)
}

// [FIX: head-of-line] dirStage.fetch()가 직접 push한 순수 fetch 항목을 처리.
// writeBufferBuffer(eviction 전용)와 독립적으로 동작하므로 fetch 블로킹이
// finalizeBankEviction을 막지 않는다.
func (wb *writeBufferStage) processNewFetch() bool {
	item := wb.cache.writeBufferFetchBuffer.Peek()
	if item == nil {
		return false
	}
	trans := item.(*transaction)

	if wb.findDataLocally(trans) {
		return wb.sendFetchedDataToBank(trans, wb.cache.writeBufferFetchBuffer)
	}
	return wb.fetchFromBottom(trans, wb.cache.writeBufferFetchBuffer)
}

func (wb *writeBufferStage) findDataLocally(trans *transaction) bool {
	for _, e := range wb.inflightEviction {
		if e.evictingAddr == trans.fetchAddress {
			trans.fetchedData = e.evictingData
			return true
		}
	}

	for _, e := range wb.pendingEvictions {
		if e.evictingAddr == trans.fetchAddress {
			trans.fetchedData = e.evictingData
			return true
		}
	}

	return false
}

func (wb *writeBufferStage) sendFetchedDataToBank(
	trans *transaction,
	buf sim.Buffer,
) bool {
	bankNum := bankID(trans.block,
		wb.cache.directory.WayAssociativity(),
		len(wb.cache.dirToBankBuffers))
	bankBuf := wb.cache.writeBufferToBankBuffers[bankNum]

	if !bankBuf.CanPush() {
		trans.fetchedData = nil
		return false
	}

	trans.mshrEntry.Data = trans.fetchedData
	trans.action = bankWriteFetched
	wb.combineData(trans.mshrEntry)

	if trans.fromLocal {
		wb.cache.mshrLocalRemoved++
	} else {
		wb.cache.mshrRemoteRemoved++
	}
	wb.cache.mshr.Remove(trans.mshrEntry.PID, trans.mshrEntry.Address)

	bankBuf.Push(trans)

	buf.Pop()

	// log.Printf("%.10f, %s, wb data fetched locally，" +
	// " %s, %04X, %04X, (%d, %d), %v\n",
	// 	now, wb.cache.Name(),
	// 	trans.accessReq().Meta().ID,
	// 	trans.accessReq().GetAddress(), trans.block.Tag,
	// 	trans.block.SetID, trans.block.WayID,
	// 	trans.fetchedData,
	// )

	return true
}

func (wb *writeBufferStage) fetchFromBottom(
	trans *transaction,
	buf sim.Buffer,
) bool {
	if wb.tooManyInflightFetches() {
		return false
	}

	if !wb.cache.bottomPort.CanSend() {
		return false
	}

	lowModulePort := wb.cache.addressToPortMapper.Find(trans.fetchAddress)
	// if strings.Contains(fmt.Sprintf("%s", lowModulePort), "RDMA") {
	// 	fmt.Printf("[%s]\tFetch %x from %s\n", wb.cache.Name(), trans.fetchAddress, lowModulePort)
	// }
	read := mem.ReadReqBuilder{}.
		WithSrc(wb.cache.bottomPort.AsRemote()).
		WithDst(lowModulePort).
		WithPID(trans.fetchPID).
		WithAddress(trans.fetchAddress).
		WithByteSize(1 << wb.cache.log2BlockSize).
		WithFetchForWriteMiss(trans.write != nil).
		Build()
	if trans.accessReq() != nil {
		read.VAddr = trans.accessReq().GetVAddr()
	}

	wb.cache.bottomPort.Send(read)

	trans.fetchReadReq = read
	wb.inflightFetch = append(wb.inflightFetch, trans)
	buf.Pop()

	tracing.TraceReqInitiate(read, wb.cache,
		tracing.MsgIDAtReceiver(trans.req(), wb.cache))

	what := ""
	if strings.Contains(fmt.Sprintf("%s", read.Meta().Dst), "DRAM") {
		what = "ToLocal"
	} else {
		what = "ToRemote"
	}
	wb.cache.incEvent(what)

	if wb.cache.debugProcess && trans.accessReq() != nil && trans.accessReq().GetAddress() == wb.cache.debugAddress0 {
		fmt.Printf("[%s] [writebufferstage]\tReceived req - 3.1.0: addr %x, action %d\n", wb.cache.name, trans.accessReq().GetAddress(), trans.action)
	}
	if wb.cache.debugProcess && trans.accessReq() != nil && trans.accessReq().GetAddress() == wb.cache.debugAddress1 {
		fmt.Printf("[%s] [writebufferstage]\tReceived req - 3.1.0: addr %x, action %d\n", wb.cache.name, trans.accessReq().GetAddress(), trans.action)
	}
	return true
}

func (wb *writeBufferStage) processWriteBufferEvictAndWrite(
	trans *transaction,
) bool {
	if wb.writeBufferFull() {
		return false
	}

	bankNum := bankID(
		trans.block,
		wb.cache.directory.WayAssociativity(),
		len(wb.cache.dirToBankBuffers),
	)
	bankBuf := wb.cache.writeBufferToBankBuffers[bankNum]

	if !bankBuf.CanPush() {
		return false
	}

	trans.action = bankWriteHit
	bankBuf.Push(trans)

	wb.pendingEvictions = append(wb.pendingEvictions, trans)
	wb.cache.writeBufferBuffer.Pop()

	// log.Printf("%.10f, %s, wb evict and write，" +
	// " %s, %04X, %04X, (%d, %d), %v\n",
	// 	now, wb.cache.Name(),
	// 	trans.accessReq().Meta().ID,
	// 	trans.accessReq().GetAddress(), trans.block.Tag,
	// 	trans.block.SetID, trans.block.WayID,
	// 	trans.evictingData,
	// )

	return true
}

func (wb *writeBufferStage) processWriteBufferEvictAndPrefetch(
	trans *transaction,
) bool {
	if true {
		panic("invalid function called")
	}

	if wb.writeBufferFull() {
		return false
	}

	bankNum := bankID(
		trans.block,
		wb.cache.directory.WayAssociativity(),
		len(wb.cache.dirToBankBuffers),
	)
	bankBuf := wb.cache.writeBufferToBankBuffers[bankNum]

	if !bankBuf.CanPush() {
		return false
	}

	trans.action = bankWritePrefetched
	bankBuf.Push(trans)

	wb.pendingEvictions = append(wb.pendingEvictions, trans)

	wb.cache.writeBufferBuffer.Pop()

	// log.Printf("%.10f, %s, wb evict and write，" +
	// " %s, %04X, %04X, (%d, %d), %v\n",
	// 	now, wb.cache.Name(),
	// 	trans.accessReq().Meta().ID,
	// 	trans.accessReq().GetAddress(), trans.block.Tag,
	// 	trans.block.SetID, trans.block.WayID,
	// 	trans.evictingData,
	// )

	return true
}

func (wb *writeBufferStage) processWriteBufferFetchAndEvict(
	trans *transaction,
) bool {
	ok := wb.processWriteBufferFlush(trans, false)
	if ok {
		trans.action = writeBufferFetch
		return true
	}

	// log.Printf("%.10f, %s, wb fetch and evict， " +
	// "%s, %04X, %04X, (%d, %d), %v\n",
	// 	now, wb.cache.Name(),
	// 	trans.write.ID,
	// 	trans.write.Address, trans.block.Tag,
	// 	trans.block.SetID, trans.block.WayID,
	// 	trans.evictingData,
	// )

	return false
}

func (wb *writeBufferStage) processWriteBufferFlush(
	trans *transaction,
	popAfterDone bool,
) bool {
	if wb.writeBufferFull() {
		return false
	}

	wb.pendingEvictions = append(wb.pendingEvictions, trans)

	if popAfterDone {
		wb.cache.writeBufferBuffer.Pop()
	}

	if wb.cache.debugProcess && trans.evictingAddr == wb.cache.debugAddress0 {
		fmt.Printf("[%s] [writebufferstage]\tReceived req - 3.3: addr %x, action %d\n", wb.cache.name, trans.evictingAddr, trans.action)
	}
	if wb.cache.debugProcess && trans.evictingAddr == wb.cache.debugAddress1 {
		fmt.Printf("[%s] [writebufferstage]\tReceived req - 3.3: addr %x, action %d\n", wb.cache.name, trans.evictingAddr, trans.action)
	}

	return true
}

func (wb *writeBufferStage) write() bool {
	if len(wb.pendingEvictions) == 0 {
		return false
	}

	trans := wb.pendingEvictions[0]

	isLocal := wb.cache.toLocal(trans.evictingAddr)
	if wb.tooManyInflightEvictions(isLocal) {
		return false
	}

	if !wb.cache.bottomPort.CanSend() {
		return false
	}

	lowModulePort := wb.cache.addressToPortMapper.Find(trans.evictingAddr)
	write := mem.WriteReqBuilder{}.
		WithSrc(wb.cache.bottomPort.AsRemote()).
		WithDst(lowModulePort).
		WithPID(trans.evictingPID).
		WithAddress(trans.evictingAddr).
		WithData(trans.evictingData).
		WithDirtyMask(trans.evictingDirtyMask).
		Build()

	if trans.accessReq() != nil {
		write.VAddr = trans.accessReq().GetVAddr()
	}

	wb.cache.bottomPort.Send(write)

	trans.evictionWriteReq = write
	trans.evictionToLocal = isLocal
	wb.pendingEvictions = wb.pendingEvictions[1:]
	wb.inflightEviction = append(wb.inflightEviction, trans)
	if isLocal {
		wb.numLocalInflightEviction++
	} else {
		wb.numRemoteInflightEviction++
	}

	tracing.TraceReqInitiate(write, wb.cache,
		tracing.MsgIDAtReceiver(write, wb.cache))

	what := ""
	if strings.Contains(fmt.Sprintf("%s", write.Meta().Dst), "DRAM") {
		what = "ToLocal"
	} else {
		what = "ToRemote"
	}
	wb.cache.incEvent(what)

	// if trans.writeToHomeNode {
	// 	fmt.Printf("[%s]\tWrite(%s -> %s) %x to %s\n",
	// 		wb.cache.Name(), trans.req().Meta().ID, write.Meta().ID, trans.evictingAddr, lowModulePort)
	// }
	// log.Printf("%.10f, %s, wb write to bottom， "+
	// " %s, %04X, %04X, (%d, %d), %v\n",
	// 	now, wb.cache.Name(),
	// 	trans.accessReq().Meta().ID,
	// 	trans.evictingAddr, trans.evictingAddr,
	// 	trans.block.SetID, trans.block.WayID,
	// 	trans.evictingData,findInflightFetchByFetchReadReqID
	// )

	return true
}

// tryProcessDataReadyRsp processes a DataReadyRsp already drained from
// bottomPort. Returns true on success (dispatched to bank), false if it
// must be retried later (downstream buffer full). The caller decides
// whether to pop from pendingDataReady based on the return value.
func (wb *writeBufferStage) tryProcessDataReadyRsp(
	dataReady *mem.DataReadyRsp,
) bool {
	return wb.processDataReadyRsp(dataReady)
}

// applyWriteDoneRsp processes a WriteDoneRsp already drained from
// bottomPort. Always succeeds (only mutates internal state).
func (wb *writeBufferStage) applyWriteDoneRsp(writeDone *mem.WriteDoneRsp) {
	wb.processWriteDoneRsp(writeDone)
}

func (wb *writeBufferStage) processDataReadyRsp(
	dataReady *mem.DataReadyRsp,
) bool {
	trans := wb.findInflightFetchByFetchReadReqID(dataReady.RespondTo)

	if trans != nil && trans.responsing { // 이미 응답이 도착하여 처리 중인 trans -> discard
		wb.removeInflightFetch(trans)
		return true
	}

	if trans == nil { // prefetch 처리
		return wb.processPrefetch(dataReady)
	}

	bankIndex := bankID(
		trans.block,
		wb.cache.directory.WayAssociativity(),
		len(wb.cache.dirToBankBuffers),
	)
	bankBuf := wb.cache.writeBufferToBankBuffers[bankIndex]

	if !bankBuf.CanPush() {
		return false
	}

	trans.fetchedData = dataReady.Data
	trans.action = bankWriteFetched
	trans.mshrEntry.Data = dataReady.Data
	// trans.responsing = true
	wb.combineData(trans.mshrEntry)

	if trans.fromLocal {
		wb.cache.mshrLocalRemoved++
	} else {
		wb.cache.mshrRemoteRemoved++
	}
	wb.cache.mshr.Remove(trans.mshrEntry.PID, trans.mshrEntry.Address)

	bankBuf.Push(trans)

	wb.removeInflightFetch(trans)

	tracing.TraceReqFinalize(trans.fetchReadReq, wb.cache)

	if wb.cache.debugProcess && trans.accessReq() != nil && trans.accessReq().GetAddress() == wb.cache.debugAddress0 {
		fmt.Printf("[%s] [writebufferstage]\tReceived read rsp - 3.2.0: addr %x, action %d\n", wb.cache.name, trans.accessReq().GetAddress(), trans.action)
	}
	if wb.cache.debugProcess && trans.accessReq() != nil && trans.accessReq().GetAddress() == wb.cache.debugAddress1 {
		fmt.Printf("[%s] [writebufferstage]\tReceived read rsp - 3.2.0: addr %x, action %d\n", wb.cache.name, trans.accessReq().GetAddress(), trans.action)
	}
	// log.Printf("%.10f, %s, wb data fetched from bottom, "+
	//" %s, %04X, %04X, (%d, %d), %v\n",
	// 	now, wb.cache.Name(),
	// 	trans.accessReq().Meta().ID,
	// 	trans.accessReq().GetAddress(), trans.block.Tag,
	// 	trans.block.SetID, trans.block.WayID,
	// 	trans.fetchedData,
	// )

	return true
}

func (wb *writeBufferStage) combineData(mshrEntry *internal.MSHREntry) {
	mshrEntry.Block.DirtyMask = make([]bool, 1<<wb.cache.log2BlockSize)
	for _, t := range mshrEntry.Requests {
		trans := t.(*transaction)
		if trans.read != nil {
			continue
		}

		mshrEntry.Block.IsDirty = true
		write := trans.write
		_, offset := getCacheLineID(write.Address, wb.cache.log2BlockSize)

		for i := 0; i < len(write.Data); i++ {
			if write.DirtyMask == nil || write.DirtyMask[i] {
				index := offset + uint64(i)
				mshrEntry.Data[index] = write.Data[i]
				mshrEntry.Block.DirtyMask[index] = true
			}
		}
	}
}

func (wb *writeBufferStage) findInflightFetchByFetchReadReqID(
	id string,
) *transaction {
	for _, t := range wb.inflightFetch {
		if t.fetchReadReq.ID == id {
			return t
		}
	}

	return nil
	// panic("inflight read not found")
}

// findInflightFetchByAddress fallback used when RspTo-based match fails.
// If the response carries the expected fetch address, we can still
// identify the right transaction even if the ID got mangled in the
// RDMA→CD chain.
func (wb *writeBufferStage) findInflightFetchByAddress(addr uint64) *transaction {
	for _, t := range wb.inflightFetch {
		if t.fetchAddress == addr {
			return t
		}
	}
	return nil
}

func (wb *writeBufferStage) removeInflightFetch(f *transaction) {
	for i, trans := range wb.inflightFetch {
		if trans == f {
			wb.inflightFetch = append(
				wb.inflightFetch[:i],
				wb.inflightFetch[i+1:]...,
			)

			return
		}
	}

	panic("not found")
}

func (wb *writeBufferStage) processPrefetch(
	rsp *mem.DataReadyRsp,
) bool {
	// 1. MSHR 확인하기
	entry := wb.cache.mshr.Query(rsp.Origin.GetPID(), rsp.Origin.GetAddress())

	// 2. MSHR hit
	// 해당하는 trans에 대해 fetch가 온 것처럼 처리
	// 기존 trans는 따로 responsing 기록 (기존 trans에 대한 요청이 들어오면 prefetch처럼 처리하지 않고 discard하기 위해)
	if entry != nil {
		tr := entry.Requests[0].(*transaction)
		trans := *tr

		bankIndex := bankID(
			trans.block,
			wb.cache.directory.WayAssociativity(),
			len(wb.cache.dirToBankBuffers),
		)
		bankBuf := wb.cache.writeBufferToBankBuffers[bankIndex]

		if !bankBuf.CanPush() {
			return false
		}

		tr.responsing = true
		trans.fetchedData = rsp.Data
		trans.action = bankWriteFetched
		trans.mshrEntry.Data = rsp.Data
		trans.fetchedData = rsp.Data
		wb.combineData(trans.mshrEntry)
		if trans.fromLocal {
			wb.cache.mshrLocalRemoved++
		} else {
			wb.cache.mshrRemoteRemoved++
		}
		wb.cache.mshr.Remove(trans.mshrEntry.PID, trans.mshrEntry.Address)

		bankBuf.Push(&trans)


		if trans.fetchReadReq != nil {
			tracing.TraceReqFinalize(trans.fetchReadReq, wb.cache)
		}

		if wb.cache.debugProcess && trans.accessReq() != nil && trans.accessReq().GetAddress() == wb.cache.debugAddress0 {
			fmt.Printf("[%s] [writebufferstage]\tReceived read rsp - 3.2.1: addr %x, action %d\n", wb.cache.name, trans.accessReq().GetAddress(), trans.action)
		}
		if wb.cache.debugProcess && trans.accessReq() != nil && trans.accessReq().GetAddress() == wb.cache.debugAddress1 {
			fmt.Printf("[%s] [writebufferstage]\tReceived read rsp - 3.2.1: addr %x, action %d\n", wb.cache.name, trans.accessReq().GetAddress(), trans.action)
		}

		return true
	}

	// 3. MSHR miss
	// directory, bank에서 prefetch 처리 수행
	if true {
		fmt.Fprintf(os.Stdout, "[%s][processPrefetch]\tStart prefetching: %x\n", wb.cache.name, rsp.Origin.GetAddress())
	}
	trans := &transaction{
		id:           sim.GetIDGenerator().Generate(),
		read:         nil,
		write:        nil, // 2개 모두 nil인 경우, directory에서 prefetch로 처리
		fetchPID:     rsp.Origin.GetPID(),
		fetchAddress: rsp.Origin.GetAddress(),
		fetchedData:  rsp.Data,
		action:       bankWritePrefetched,
		prefetch:     rsp,
	}

	tracing.TraceReqReceive(rsp, wb.cache)
	wb.cache.incEvent("PrefetchStart")

	if !wb.cache.dirStageBuffer.CanPush() {
		// 1. 버리기

		wb.cache.incEvent("PrefetchDiscard - Busy")
		tracing.TraceReqFinalize(trans.prefetch, wb.cache)

		return true

		// 2. Buffer의 tail에 삽입하기
		// wb.cache.bottomPort.RetrieveIncoming()
		// wb.cache.bottomPort.GetIncomingBuf().Push(rsp)
		// return false

		// 3. 별도의 queue에 저장, background로 insert 수행
	}

	wb.cache.dirStageBuffer.Push(trans)

	if wb.cache.debugProcess && trans.accessReq() != nil && trans.accessReq().GetAddress() == wb.cache.debugAddress0 {
		fmt.Printf("[%s] [writebufferstage]\tReceived read prefetch - 3.3: addr %x, action %d\n", wb.cache.name, trans.accessReq().GetAddress(), trans.action)
	}
	if wb.cache.debugProcess && trans.accessReq() != nil && trans.accessReq().GetAddress() == wb.cache.debugAddress1 {
		fmt.Printf("[%s] [writebufferstage]\tReceived read prefetch - 3.3: addr %x, action %d\n", wb.cache.name, trans.accessReq().GetAddress(), trans.action)
	}

	return true
}

func (wb *writeBufferStage) processWriteDoneRsp(
	writeDone *mem.WriteDoneRsp,
) bool {
	for i := len(wb.inflightEviction) - 1; i >= 0; i-- {
		e := wb.inflightEviction[i]
		if e.evictionWriteReq.ID == writeDone.RespondTo {
			if e.evictionToLocal {
				wb.numLocalInflightEviction--
			} else {
				wb.numRemoteInflightEviction--
			}
			wb.inflightEviction = append(
				wb.inflightEviction[:i],
				wb.inflightEviction[i+1:]...,
			)
				tracing.TraceReqFinalize(e.evictionWriteReq, wb.cache)

			// log.Printf("%.10f, %s, wb write to bottom，
			//  %s, %04X, %04X, (%d, %d), %v\n",
			//  now, wb.cache.Name(),
			//  e.accessReq().Meta().ID,
			//  e.evictingAddr, e.evictingAddr,
			//  e.block.SetID, e.block.WayID,
			//  e.evictingData,
			// )

			return true
		}
	}

	return true
}

func (wb *writeBufferStage) writeBufferFull() bool {
	numEntry := len(wb.pendingEvictions) + len(wb.inflightEviction)
	return numEntry >= wb.writeBufferCapacity
}

func (wb *writeBufferStage) tooManyInflightFetches() bool {
	return len(wb.inflightFetch) >= wb.maxInflightFetch
}

func (wb *writeBufferStage) tooManyInflightEvictions(isLocal bool) bool {
	// Asymmetric soft cap: remote can use the full maxInflightEviction
	// budget; local is bounded at 3/4 of it. The remaining 1/4 is
	// always reserved for remote so local cannot starve cross-GPU
	// traffic, while remote-dominant workloads (e.g., stencil2d on
	// multi-GPU where all evictions go cross-GPU) can still use the
	// full cap when local is idle. Total inflight is hard-capped by
	// maxInflightEviction so the writeBuffer's accounting stays sound.
	total := wb.numLocalInflightEviction + wb.numRemoteInflightEviction
	if total >= wb.maxInflightEviction {
		return true
	}
	if isLocal {
		localLimit := wb.maxInflightEviction - wb.maxInflightEviction/4
		return wb.numLocalInflightEviction >= localLimit
	}
	return false
}

func (wb *writeBufferStage) Reset() {
	wb.cache.writeBufferBuffer.Clear()
	wb.cache.writeBufferFetchBuffer.Clear()
	wb.pendingEvictions = nil
	wb.inflightFetch = nil
	wb.inflightEviction = nil
	wb.numLocalInflightEviction = 0
	wb.numRemoteInflightEviction = 0
	wb.pendingDataReady = nil
	wb.pendingWriteDone = nil
}
