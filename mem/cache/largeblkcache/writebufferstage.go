package largeblkcache

import (
	"fmt"
	"strings"

	"github.com/sarchlab/akita/v4/mem/cache/largeblkcache/internal"
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
}

func (wb *writeBufferStage) Tick() bool {
	madeProgress := false

	madeProgress = wb.write() || madeProgress
	madeProgress = wb.processReturnRsp() || madeProgress
	madeProgress = wb.processNewTransaction() || madeProgress
	// [FIX: head-of-line] writeBufferFetchBuffer(fetch 전용)를 writeBufferBuffer(eviction 전용)와
	// 독립적으로 처리. fetch 블로킹이 eviction 진행을 막지 않도록 분리.
	madeProgress = wb.processNewFetch() || madeProgress

	return madeProgress
}

func (wb *writeBufferStage) processNewTransaction() bool {
	item := wb.cache.writeBufferBuffer.Peek()
	if item == nil {
		return false
	}
	if wb.cache.debug > 0 {
		fmt.Printf("[%s]\tDEBUG: Send to bottom - 3\n", wb.cache.name)
		wb.cache.debug--
	}

	progress := false
	trans := item.(*transaction)

	// [추가] 이미 응답 처리가 진행 중인 트랜잭션 폐기 (Prefetch 관련)
	if trans.responsing {
		wb.cache.writeBufferBuffer.Pop()
		return false
	}

	switch trans.action {
	case writeBufferFetch:
		progress = wb.processWriteBufferFetch(trans)
	case writeBufferEvictAndWrite:
		progress = wb.processWriteBufferEvictAndWrite(trans)
	case writeBufferEvictAndFetch:
		progress = wb.processWriteBufferFetchAndEvict(trans)
	case writeBufferEvictAndPrefetch:
		progress = wb.processWriteBufferEvictAndPrefetch(trans)
	case writeBufferFlush:
		progress = wb.processWriteBufferFlush(trans, true)
	default:
		// [추가] 디버깅을 위한 에러 로깅 및 무한 루프
		fmt.Printf("[%s]\t[WARNING]\tUnknown transaction action %d, trans: %x\n", wb.cache.name, trans.action, trans)
		for true {
		}
		panic("unknown transaction action")
	}

	return progress
}

func (wb *writeBufferStage) processWriteBufferFetch(
	trans *transaction,
) bool {
	// EvictAndFetch 변환 케이스: writeBufferBuffer에서 pop
	if wb.findDataLocally(trans) {
		return wb.sendFetchedDataToBank(trans, wb.cache.writeBufferBuffer)
	}
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

	wb.cache.mshr.Remove(trans.mshrEntry.PID, trans.mshrEntry.Address)

	bankBuf.Push(trans)

	buf.Pop()

	if wb.cache.deviceID == 3 && trans.accessReq() != nil && trans.accessReq().GetAddress() == 12888246464 {
		fmt.Printf("[%s][DEBUG]\tReadReq received - 4.3.1: %d\n", wb.cache.name, trans.accessReq().GetAddress())
	}
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
		Build()
	if trans.accessReq() != nil {
		read.VAddr = trans.accessReq().GetVAddr()
	}
	if trans.fetchAddress%(1<<wb.cache.log2BlockSize) != 0 {
		fmt.Printf("[%s][fetchFromBottom]\tDEBUG: not aligned to cache line\n")
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
	tracing.AddTaskStep(read.ID, wb.cache, what)

	if wb.cache.deviceID == 3 && trans.accessReq() != nil && trans.accessReq().GetAddress() == 12888246464 {
		fmt.Printf("[%s][DEBUG]\tReadReq received - 4.3.2: %d\n", wb.cache.name, trans.accessReq().GetAddress())
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

func (wb *writeBufferStage) processWriteBufferEvictAndPrefetch(
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

	trans.action = bankWritePrefetched
	bankBuf.Push(trans)

	wb.pendingEvictions = append(wb.pendingEvictions, trans)

	wb.cache.writeBufferBuffer.Pop()

	return true
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

	return true
}

func (wb *writeBufferStage) write() bool {
	if len(wb.pendingEvictions) == 0 {
		return false
	}

	trans := wb.pendingEvictions[0]

	if wb.tooManyInflightEvictions() {
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
	if trans.evictingAddr%(1<<wb.cache.log2BlockSize)+uint64(len(trans.evictingData)) > 1<<wb.cache.log2BlockSize {
		fmt.Printf("[%s][write]\tDEBUG: not aligned to cache line\n", wb.cache.name)
	}

	if trans.accessReq() != nil {
		write.VAddr = trans.accessReq().GetVAddr()
	}

	wb.cache.bottomPort.Send(write)

	trans.evictionWriteReq = write
	wb.pendingEvictions = wb.pendingEvictions[1:]
	wb.inflightEviction = append(wb.inflightEviction, trans)

	tracing.TraceReqInitiate(write, wb.cache,
		tracing.MsgIDAtReceiver(trans.req(), wb.cache))

	what := ""
	if strings.Contains(fmt.Sprintf("%s", write.Meta().Dst), "DRAM") {
		what = "ToLocal"
	} else {
		what = "ToRemote"
	}
	tracing.AddTaskStep(write.ID, wb.cache, what)

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

func (wb *writeBufferStage) processReturnRsp() bool {
	msg := wb.cache.bottomPort.PeekIncoming()
	if msg == nil {
		return false
	}

	switch msg := msg.(type) {
	case *mem.DataReadyRsp:
		return wb.processDataReadyRsp(msg)
	case *mem.WriteDoneRsp:
		return wb.processWriteDoneRsp(msg)
	default:
		panic("unknown msg type")
	}
}

func (wb *writeBufferStage) processDataReadyRsp(
	dataReady *mem.DataReadyRsp,
) bool {
	trans := wb.findInflightFetchByFetchReadReqID(dataReady.RespondTo)

	// [추가] 이미 응답이 도착하여 처리 중인 트랜잭션은 버퍼만 비우고 Discard
	if trans != nil && trans.responsing {
		wb.cache.bottomPort.RetrieveIncoming()
		return true
	}

	// [추가] Inflight 목록에 없으면 Prefetch(선탑재) 패킷으로 간주하고 전용 처리 로직으로 넘김
	if trans == nil {
		return wb.processPrefetch(dataReady)
	}

	if trans == nil {
		wb.cache.bottomPort.RetrieveIncoming()
		fmt.Printf("[%s]\t[Warning] DataReadyRsp with ID %s not found in inflight fetches. Ignored. len inflight: %d\n",
			wb.cache.Name(), dataReady.RespondTo, len(wb.inflightFetch))
		// return false
		return true
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
	wb.combineData(trans.mshrEntry)

	wb.cache.mshr.Remove(trans.mshrEntry.PID, trans.mshrEntry.Address)

	bankBuf.Push(trans)

	wb.removeInflightFetch(trans)
	wb.cache.bottomPort.RetrieveIncoming()

	tracing.TraceReqFinalize(trans.fetchReadReq, wb.cache)

	if wb.cache.deviceID == 3 && trans.accessReq() != nil && trans.accessReq().GetAddress() == 12888246464 {
		fmt.Printf("[%s][DEBUG]\tReadReq received - 4.3.3: %d\n", wb.cache.name, trans.accessReq().GetAddress())
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

// [추가] Prefetch 처리 함수 이식
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
		tr.responsing = true

		bankIndex := bankID(
			trans.block,
			wb.cache.directory.WayAssociativity(),
			len(wb.cache.dirToBankBuffers),
		)
		bankBuf := wb.cache.writeBufferToBankBuffers[bankIndex]

		if !bankBuf.CanPush() {
			return false
		}

		trans.fetchedData = rsp.Data
		trans.action = bankWritePrefetched
		trans.mshrEntry.Data = rsp.Data
		trans.fetchedData = rsp.Data
		wb.combineData(trans.mshrEntry)
		wb.cache.mshr.Remove(trans.mshrEntry.PID, trans.mshrEntry.Address)

		// 포인터를 Push해야 하므로 &trans 넘김
		bankBuf.Push(&trans)

		wb.cache.bottomPort.RetrieveIncoming()

		if trans.fetchReadReq != nil {
			tracing.TraceReqFinalize(trans.fetchReadReq, wb.cache)
		}

		return true
	}

	// 3. MSHR miss
	// directory, bank에서 prefetch 처리 수행
	trans := &transaction{
		id:           sim.GetIDGenerator().Generate(),
		read:         nil,
		write:        nil, // 2개 모두 nil인 경우, directory에서 prefetch로 처리
		fetchPID:     rsp.Origin.GetPID(),
		fetchAddress: rsp.Origin.GetAddress(),
		fetchedData:  rsp.Data,
		action:       bankWritePrefetched,
	}

	if !wb.cache.dirStageBuffer.CanPush() {
		// 1. 버리기
		// wb.cache.bottomPort.RetrieveIncoming()
		// return false

		// 2. Buffer의 tail에 삽입하기 (하드웨어 시뮬레이터 포트 버퍼에 재삽입하여 Stall)
		wb.cache.bottomPort.RetrieveIncoming()
		wb.cache.bottomPort.GetIncomingBuf().Push(rsp)
		return false

		// 3. 별도의 queue에 저장, background로 insert 수행
	}

	wb.cache.dirStageBuffer.Push(trans)
	wb.cache.bottomPort.RetrieveIncoming()

	return false
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
		if t.fetchReadReq.Meta().ID == id {
			return t
		}
	}

	return nil
	// panic("inflight read not found")
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

func (wb *writeBufferStage) processWriteDoneRsp(
	writeDone *mem.WriteDoneRsp,
) bool {
	for i := len(wb.inflightEviction) - 1; i >= 0; i-- {
		e := wb.inflightEviction[i]
		if e.evictionWriteReq.ID == writeDone.RespondTo {
			wb.inflightEviction = append(
				wb.inflightEviction[:i],
				wb.inflightEviction[i+1:]...,
			)
			wb.cache.bottomPort.RetrieveIncoming()
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

	fmt.Printf("[%s]\t[Warning] WriteDoneRsp with ID %s not found in inflight evictions. Ignored. len inflight: %d\n",
		wb.cache.Name(), writeDone.RespondTo, len(wb.inflightEviction))
	wb.cache.bottomPort.RetrieveIncoming()
	return true
	// return false
}

func (wb *writeBufferStage) writeBufferFull() bool {
	numEntry := len(wb.pendingEvictions) + len(wb.inflightEviction)
	return numEntry >= wb.writeBufferCapacity
}

func (wb *writeBufferStage) tooManyInflightFetches() bool {
	return len(wb.inflightFetch) >= wb.maxInflightFetch
}

func (wb *writeBufferStage) tooManyInflightEvictions() bool {
	return len(wb.inflightEviction) >= wb.maxInflightEviction
}

func (wb *writeBufferStage) Reset() {
	wb.cache.writeBufferBuffer.Clear()
	wb.cache.writeBufferFetchBuffer.Clear()
	wb.pendingEvictions = nil
	wb.inflightFetch = nil
	wb.inflightEviction = nil
}
