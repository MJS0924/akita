package writebackcoh

import (
	"fmt"
	"log"

	"github.com/sarchlab/akita/v4/mem/cache/writebackcoh/internal"
	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/pipelining"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/akita/v4/tracing"
)

type bankStage struct {
	cache  *Comp
	bankID int

	// [BANK LOCAL/REMOTE SPLIT] Pipeline + post buffer fully separated into
	// Local (own-L1, fromLocal=true) and Remote (peer-serve, fromLocal=false)
	// lanes, mirroring directoryStage.localPipeline/remotePipeline +
	// localBuf/remoteBuf. Remote work is ticked/pulled/finalized FIRST so a
	// stalled local lane can never block peer-serve completions (the closing
	// edge of the cross-GPU writeback deadlock). pipelineWidth is shared.
	localPipeline  pipelining.Pipeline
	remotePipeline pipelining.Pipeline
	pipelineWidth  int
	localPostBuf   *bufferImpl
	remotePostBuf  *bufferImpl

	localInflightTransCount  int
	remoteInflightTransCount int

	// Count the trans that needs to be sent to the write buffer, per lane.
	localDownwardInflightTransCount  int
	remoteDownwardInflightTransCount int
}

// inflightTransCount exposes the combined local+remote in-flight count so
// drain/flush checks (flusher.go) see total bank occupancy unchanged.
func (s *bankStage) inflightTransCount() int {
	return s.localInflightTransCount + s.remoteInflightTransCount
}

// decInflight decrements the in-flight counter for the lane the
// transaction belongs to (fromLocal -> local lane, else remote lane).
// Used by finalize* handlers in place of the pre-split single-counter
// decrement; routing by fromLocal keeps each lane's accounting exact.
func (s *bankStage) decInflight(fromLocal bool) {
	if fromLocal {
		s.localInflightTransCount--
	} else {
		s.remoteInflightTransCount--
	}
}

// decDownward decrements the downward (eviction → writeBuffer) counter for
// the lane the transaction belongs to.
func (s *bankStage) decDownward(fromLocal bool) {
	if fromLocal {
		s.localDownwardInflightTransCount--
	} else {
		s.remoteDownwardInflightTransCount--
	}
}

type bufferImpl struct {
	sim.HookableBase

	name     string
	capacity int
	elements []interface{}
}

func (b *bufferImpl) Name() string {
	return b.name
}

func (b *bufferImpl) CanPush() bool {
	return len(b.elements) < b.capacity
}

func (b *bufferImpl) Push(e interface{}) {
	if len(b.elements) >= b.capacity {
		log.Panic("buffer overflow")
	}

	b.elements = append(b.elements, e)

	if b.NumHooks() > 0 {
		b.InvokeHook(sim.HookCtx{
			Domain: b,
			Pos:    sim.HookPosBufPush,
			Item:   e,
			Detail: nil,
		})
	}
}

func (b *bufferImpl) Pop() interface{} {
	if len(b.elements) == 0 {
		return nil
	}

	e := b.elements[0]
	b.elements = b.elements[1:]

	if b.NumHooks() > 0 {
		b.InvokeHook(sim.HookCtx{
			Domain: b,
			Pos:    sim.HookPosBufPush,
			Item:   e,
			Detail: nil,
		})
	}

	return e
}

func (b *bufferImpl) Peek() interface{} {
	if len(b.elements) == 0 {
		return nil
	}

	return b.elements[0]
}

func (b *bufferImpl) Capacity() int {
	return b.capacity
}

func (b *bufferImpl) Size() int {
	return len(b.elements)
}

func (b *bufferImpl) Clear() {
	b.elements = nil
}

func (b *bufferImpl) Get(i int) interface{} {
	return b.elements[i]
}

// newBankBuffer constructs a scannable (*bufferImpl) buffer. Used for the
// remote/local dir-to-bank ingress buffers so pullFromBuf can pull a
// completion-bearing HIT past a head-of-line eviction (see pullFromBuf's
// [ITER19 DIR_L2_CYCLE FIX]). Behaves as a plain FIFO for all other
// (Peek/Pop/Push) callers, so it is a drop-in for sim.NewBuffer.
func newBankBuffer(name string, capacity int) *bufferImpl {
	return &bufferImpl{name: name, capacity: capacity}
}

func (b *bufferImpl) Remove(i int) {
	element := b.elements[i]

	b.elements = append(b.elements[:i], b.elements[i+1:]...)

	if b.NumHooks() > 0 {
		b.InvokeHook(sim.HookCtx{
			Domain: b,
			Pos:    sim.HookPosBufPush,
			Item:   element,
			Detail: nil,
		})
	}
}

type bankPipelineElem struct {
	trans *transaction
}

func (e bankPipelineElem) TaskID() string {
	return e.trans.req().Meta().ID + "_write_back_bank_pipeline"
}

func (s *bankStage) Tick() (madeProgress bool) {
	// [BANK LOCAL/REMOTE SPLIT] Tick BOTH pipelines and run the REMOTE
	// (peer-serve) finalize+pull FIRST, then LOCAL — mirroring
	// directoryStage's remote-first arbitration. Each finalize/pull loop
	// has its own numReqPerCycle commit budget per lane, so the two lanes
	// run truly in parallel and a stalled local lane can never starve
	// peer-serve completions.

	// Commit-only budget for finalize: each iteration consumes one slot
	// ONLY when a transaction actually commits out of the post-pipeline
	// buffer. If no item in the post buffer can finalize this cycle, the
	// loop terminates early — a stalled head item shouldn't keep burning
	// per-cycle budget.

	// --- REMOTE lane first ---
	for i := 0; i < s.cache.numReqPerCycle; i++ {
		if !s.finalizeTrans(true) {
			break
		}
		madeProgress = true
	}

	madeProgress = s.remotePipeline.Tick() || madeProgress

	for i := 0; i < s.cache.numReqPerCycle; i++ {
		if !s.pullFromBuf(true) {
			break
		}
		madeProgress = true
	}

	// --- LOCAL lane ---
	for i := 0; i < s.cache.numReqPerCycle; i++ {
		if !s.finalizeTrans(false) {
			break
		}
		madeProgress = true
	}

	madeProgress = s.localPipeline.Tick() || madeProgress

	// Commit-only budget for pull: pullFromBuf already arbitrates between
	// writeBufferToBankBuffers (upward, priority) and dirToBankBuffers
	// (downward) internally and returns true only on commit. If both
	// queues are empty/blocked this cycle, terminate.
	for i := 0; i < s.cache.numReqPerCycle; i++ {
		if !s.pullFromBuf(false) {
			break
		}
		madeProgress = true
	}

	return madeProgress
}

func (s *bankStage) Reset() {
	s.cache.dirToBankBuffers[s.bankID].Clear()
	s.cache.dirToBankBuffersLocal[s.bankID].Clear()
	s.cache.dirToBankBuffersRemote[s.bankID].Clear()
	// [R5 + BANK LOCAL/REMOTE SPLIT] clear all four Req/Rsp Local/Remote
	// halves (the unsuffixed Req/Rsp slices are length-only aliases that
	// receive no pushes, but clear them too for completeness).
	s.cache.writeBufferToBankBuffersReq[s.bankID].Clear()
	s.cache.writeBufferToBankBuffersRsp[s.bankID].Clear()
	s.cache.writeBufferToBankBuffersReqLocal[s.bankID].Clear()
	s.cache.writeBufferToBankBuffersReqRemote[s.bankID].Clear()
	s.cache.writeBufferToBankBuffersRspLocal[s.bankID].Clear()
	s.cache.writeBufferToBankBuffersRspRemote[s.bankID].Clear()
	s.localPipeline.Clear()
	s.remotePipeline.Clear()
	s.localPostBuf.Clear()
	s.remotePostBuf.Clear()
	s.localInflightTransCount = 0
	s.remoteInflightTransCount = 0
	s.localDownwardInflightTransCount = 0
	s.remoteDownwardInflightTransCount = 0
}

// pullFromBuf pulls one transaction into the lane selected by isRemote.
// The REMOTE lane gates on remotePipeline.CanAccept() and pulls from the
// Remote writeBuffer-to-bank halves + dirToBankBuffersRemote; the LOCAL
// lane uses the Local halves + dirToBankBuffersLocal. Each lane keeps its
// own inflight/downward counters and applies the ITER19 downward-reservation
// independently, so peer-serve and own-L1 bank work never share a resource.
func (s *bankStage) pullFromBuf(isRemote bool) bool {
	pipeline := s.localPipeline
	rspBuf := s.cache.writeBufferToBankBuffersRspLocal[s.bankID]
	reqBuf := s.cache.writeBufferToBankBuffersReqLocal[s.bankID]
	dirBuf := s.cache.dirToBankBuffersLocal[s.bankID]
	downwardCount := s.localDownwardInflightTransCount
	if isRemote {
		pipeline = s.remotePipeline
		rspBuf = s.cache.writeBufferToBankBuffersRspRemote[s.bankID]
		reqBuf = s.cache.writeBufferToBankBuffersReqRemote[s.bankID]
		dirBuf = s.cache.dirToBankBuffersRemote[s.bankID]
		downwardCount = s.remoteDownwardInflightTransCount
	}

	if !pipeline.CanAccept() {
		return false
	}

	// [R5] Drain Rsp (bankWriteFetched) lane first, then Req
	// (bankWriteHit / bankWritePrefetched). Rsp-first mirrors the
	// pendingDataReady response-priority precedent — a fetched-data
	// committing to the bank frees an MSHR slot upstream and is on the
	// critical path for L1 read-miss completion. Both buffers feed the
	// same (per-lane) pipeline; only the dequeue order changes.
	for _, inBuf := range []sim.Buffer{rspBuf, reqBuf} {
		trans := inBuf.Pop()
		if trans == nil {
			continue
		}
		pipeline.Accept(bankPipelineElem{trans: trans.(*transaction)})
		if isRemote {
			s.remoteInflightTransCount++
		} else {
			s.localInflightTransCount++
		}
		return true
	}

	// [FIX #2] Remote 먼저, Local 그 다음. dirStage 의 invBuf→remoteBuf→localBuf
	// priority 가 bank 까지 전파되도록. local 트랜잭션이 bankBuf head 에 쌓여도
	// remote 가 뒤에서 영원히 밀리는 일이 없게 함 (cross-GPU 데드락 차단). With the
	// lane split this priority is now structural: the remote lane has its
	// own pipeline and is ticked/pulled first in Tick().
	//
	// [ITER19 DIR_L2_CYCLE FIX] When the downward (eviction) lane is
	// exhausted, do NOT return false outright (the old behavior). A
	// returned-false here lets a head-of-line eviction — which is stuck
	// at finalizeBankEviction because writeBufferBufferRemote is full —
	// block the completion-bearing HITs (bankReadHit / bankWriteHit)
	// queued BEHIND it in the SAME dir-to-bank ingress buffer. Those HITs
	// are the only producers of the DataReadyRsp/WriteDoneRsp that frees
	// the originating peer GPU's numRemoteInflightEviction. With both
	// GPUs symmetric, that is the closing edge of the writeback deadlock.
	//
	// Instead, reserve the lane for UP-going work only: when the downward
	// budget is exhausted we scan past head-of-line evictions and pull
	// the first non-eviction (upward) transaction so completions always
	// drain. Reordering only ever moves a HIT ahead of an eviction to a
	// DIFFERENT line (same-line accesses are serialized upstream by
	// evictingList + block locks), so coherence is unaffected. No cap
	// change — same buffers, same capacities. Applied PER LANE against
	// each lane's own downward count and pipelineWidth.
	downwardExhausted := downwardCount >= s.pipelineWidth-1
	var t *transaction
	if downwardExhausted {
		// Pull only an upward (non-eviction) transaction, scanning past
		// blocked evictions at the head.
		t = pullFirstUpward(dirBuf)
	} else {
		if item := dirBuf.Pop(); item != nil {
			t = item.(*transaction)
		}
	}
	if t == nil {
		return false
	}

	pipeline.Accept(bankPipelineElem{trans: t})
	if isRemote {
		s.remoteInflightTransCount++
	} else {
		s.localInflightTransCount++
	}

	switch t.action {
	case bankEvict, bankEvictAndFetch, bankEvictAndWrite, bankEvictAndPrefetch:
		if isRemote {
			s.remoteDownwardInflightTransCount++
		} else {
			s.localDownwardInflightTransCount++
		}
	}

	return true
}

// isDownwardAction reports whether a bank transaction occupies a
// downward (eviction → writeBuffer) pipeline lane.
func isDownwardAction(a action) bool {
	switch a {
	case bankEvict, bankEvictAndFetch, bankEvictAndWrite, bankEvictAndPrefetch:
		return true
	}
	return false
}

// pullFirstUpward removes and returns the first non-eviction (upward,
// completion-bearing) transaction from a dir-to-bank ingress buffer,
// leaving any leading evictions in place. Returns nil if none. Only the
// scannable *bufferImpl supports the scan; a plain FIFO falls back to a
// head-only check so behavior degrades safely (never reorders, never
// panics).
func pullFirstUpward(inBuf sim.Buffer) *transaction {
	bi, ok := inBuf.(*bufferImpl)
	if !ok {
		if head := inBuf.Peek(); head != nil {
			if t := head.(*transaction); !isDownwardAction(t.action) {
				inBuf.Pop()
				return t
			}
		}
		return nil
	}
	for i := 0; i < bi.Size(); i++ {
		t := bi.Get(i).(*transaction)
		if !isDownwardAction(t.action) {
			bi.Remove(i)
			return t
		}
	}
	return nil
}

// finalizeTrans drains the post-pipeline buffer for the lane selected by
// isRemote (remotePostBuf vs localPostBuf). The finalize action handlers
// (finalizeReadHit / finalizeWriteHit / finalizeBankEviction / …) are
// unchanged — only which post buffer is iterated changes.
func (s *bankStage) finalizeTrans(isRemote bool) bool {
	postBuf := s.localPostBuf
	if isRemote {
		postBuf = s.remotePostBuf
	}

	for i := 0; i < postBuf.Size(); i++ {
		trans := postBuf.Get(i).(bankPipelineElem).trans
		if s.cache.debugProcess && trans.accessReq() != nil && trans.accessReq().GetAddress() == s.cache.debugAddress0 {
			fmt.Printf("[%s] [bankStage]\tReceived req - 2: addr %x, action %d\n", s.cache.name, trans.accessReq().GetAddress(), trans.action)
		}
		if s.cache.debugProcess && trans.accessReq() != nil && trans.accessReq().GetAddress() == s.cache.debugAddress1 {
			fmt.Printf("[%s] [bankStage]\tReceived req - 2: addr %x, action %d\n", s.cache.name, trans.accessReq().GetAddress(), trans.action)
		}
		if s.cache.debugProcess && trans.responsing {
			fmt.Printf("[%s]\tTransaction %x is responsing, discard.\n", s.cache.name, trans.accessReq().GetAddress())
			postBuf.Remove(i)
			continue
		}

		done := false

		switch trans.action {
		case bankReadHit: // action 1
			done = s.finalizeReadHit(trans)
		case bankWriteHit: // action 2
			done = s.finalizeWriteHit(trans)
		case bankWriteFetched: // action 6
			done = s.finalizeBankWriteFetched(trans)
		case bankWritePrefetched, bankEvictAndPrefetch:
			done = s.finalizeBankWritePrefetched(trans)
		case bankEvictAndFetch, bankEvictAndWrite, bankEvict: // action 5, 4, 3
			done = s.finalizeBankEviction(trans)
		default:
			panic("bank action not supported")
		}

		if done {
			postBuf.Remove(i)

			return true
		}
	}

	return false
}

func (s *bankStage) finalizeReadHit(trans *transaction) bool {
	read := trans.read

	// NoNeedToReply: sub-block prefetch request from CohDir.
	// Skip response to avoid flooding topPort and causing pipeline deadlock.
	// if read.GetNoNeedToReply() {
	// 	_, offset := getCacheLineID(read.Address, s.cache.log2BlockSize)
	// 	block := trans.block

	// 	_, err := s.cache.storage.Read(block.CacheAddress+offset, read.AccessByteSize)
	// 	if err != nil {
	// 		panic(err)
	// 	}

	// 	s.removeTransaction(trans)
	// 	s.inflightTransCount--
	// 	block.ReadCount--
	// 	block.Accessed = true

	// 	tracing.TraceReqComplete(read, s.cache)
	// 	return true
	// }

	if trans.fromLocal && !s.cache.topPort.CanSend() {
		return false
	} else if !trans.fromLocal && !s.cache.remoteTopPort.CanSend() {
		return false
	}

	addr := read.Address
	_, offset := getCacheLineID(addr, s.cache.log2BlockSize)
	block := trans.block

	data, err := s.cache.storage.Read(
		block.CacheAddress+offset, read.AccessByteSize)
	if err != nil {
		panic(err)
	}

	s.removeTransaction(trans)

	// if read.ReqFrom == "GPU[1].SA[0].L1VCache[0]" {
	// 	fmt.Printf("[%s]\tSend data ready response: Addr %x\n", s.cache.name, addr)
	// }

	var error *sim.SendError
	dataReady := mem.DataReadyRspBuilder{}.
		WithSrc(s.cache.topPort.AsRemote()).
		WithDst(read.Meta().Src).
		WithRspTo(read.Meta().ID).
		WithData(data).
		WithOrigin(read).
		Build()

	if !trans.fromLocal {
		dataReady.Src = s.cache.remoteTopPort.AsRemote()
		error = s.cache.remoteTopPort.Send(dataReady)
		if error == nil {
			s.cache.peerReadServedCount++ // [DIAG] peer read served (DataReadyRsp emitted)
		} else {
			s.cache.peerReadServeFailCount++ // [DIAG] remoteTopPort.Send failed (egress blocked)
		}
	} else {
		error = s.cache.topPort.Send(dataReady)
	}
	if error != nil {
		return false
	}

	s.decInflight(trans.fromLocal)
	block.ReadCount--
	block.Accessed = true

	tracing.TraceReqComplete(read, s.cache)

	if s.cache.debugProcess && trans.accessReq() != nil && trans.accessReq().GetAddress() == s.cache.debugAddress0 {
		fmt.Printf("[%s] [bankStage]\tSend rsp - 2.1: addr %x, dst %s, src %s, action %d, ID %s\n",
			s.cache.name, dataReady.Origin.GetAddress(), dataReady.Dst, dataReady.Src, trans.action, dataReady.Meta().ID)
	}
	if s.cache.debugProcess && trans.accessReq() != nil && trans.accessReq().GetAddress() == s.cache.debugAddress1 {
		fmt.Printf("[%s] [bankStage]\tSend rsp - 2.1: addr %x, dst %s, src %s, action %d\n, ID %s",
			s.cache.name, dataReady.Origin.GetAddress(), dataReady.Dst, dataReady.Src, trans.action, dataReady.Meta().ID)
	}
	// fmt.Printf("[%s]\tSend Data Ready Response %x to %s for reqID %s\n", s.cache.name, read.Address, read.Src, read.ID)

	// log.Printf("%.10f, %s, bank read hit finalize，"+
	// " %s, %04X, %04X, (%d, %d), %v\n",
	// 	now, s.cache.Name(),
	// 	trans.read.ID,
	// 	trans.read.Address, block.Tag,
	// 	block.SetID, block.WayID,
	// 	dataReady.Data,
	// )

	return true
}

func (s *bankStage) finalizeWriteHit(trans *transaction) bool {
	if trans.fromLocal && !s.cache.topPort.CanSend() {
		return false
	} else if !trans.fromLocal && !s.cache.remoteTopPort.CanSend() {
		return false
	}

	// [RESPONSE-DECOUPLE] The WriteDoneRsp ack below must be able to leave even
	// when the displacement victim cannot enter writeBufferBuffer right now —
	// otherwise a saturated own-eviction cap blocks the very ack that drains a
	// peer's eviction cap (the closing edge of the cross-GPU serve deadlock).
	// We only need SOMEWHERE to place the victim: writeBufferBuffer now, or the
	// bounded deferral list. If neither has room, retry next cycle (the ack has
	// not been emitted yet, so "ack emitted ⇒ victim placed" atomicity holds).
	if trans.writeToHomeNode &&
		!s.cache.writeBufferBufferCanPush(trans.fromLocal) &&
		!s.cache.writeBuffer.deferFlushCanPush(trans.fromLocal) {
		return false
	}

	// [INV-FIDELITY C3] A deferred invalidation is armed on this block and
	// this local (non-write-through) write is about to leave it dirty; the
	// kill at applyPendingInvalidation below must produce a local
	// writeback, so require flush capacity up front (same shape as the
	// writeToHomeNode gate above; retried next cycle, drains via local
	// DRAM only). writeToHomeNode writes leave the block clean and are
	// covered by their own gate.
	if trans.block.PendingInvalidation && !trans.writeToHomeNode &&
		!s.cache.writeBufferBufferCanPush(true) &&
		!s.cache.writeBuffer.deferFlushCanPush(true) {
		return false
	}

	write := trans.write
	addr := write.Address
	_, offset := getCacheLineID(addr, s.cache.log2BlockSize)
	block := trans.block

	dirtyMask := s.writeData(block, write, offset)

	block.IsValid = true
	block.IsLocked = false
	block.IsDirty = true
	block.Accessed = true
	block.DirtyMask = dirtyMask

	s.removeTransaction(trans)

	s.decInflight(trans.fromLocal)

	done := mem.WriteDoneRspBuilder{}.
		WithSrc(s.cache.topPort.AsRemote()).
		WithDst(write.Meta().Src).
		// WithRspTo(write.ID).
		WithRspTo(write.Meta().ID).
		WithOrigin(write).
		Build()
	if !trans.fromLocal {
		done.Meta().Src = s.cache.remoteTopPort.AsRemote()
		s.cache.remoteTopPort.Send(done)
		s.cache.peerWriteAckSent++ // [ITER20 DIAG A]
	} else {
		s.cache.topPort.Send(done)
	}
	// fmt.Printf("[%s]\tSend Write Done Response %x to %s for reqID %s\n", s.cache.name, write.Address, write.Src, write.ID)

	if trans.writeToHomeNode {
		data, err := s.cache.storage.Read(block.CacheAddress, 1<<s.cache.log2BlockSize)
		if err != nil {
			panic(err)
		}

		block.IsDirty = false // remote data에 write 하는 경우, home node로 write 하므로 dirty 표시를 하지 않음
		trans.evictingPID = block.PID
		trans.evictingAddr = trans.accessReq().GetAddress()
		trans.evictingData = data
		trans.evictingDirtyMask = block.DirtyMask

		trans.action = writeBufferFlush
		// [RESPONSE-DECOUPLE] The WriteDoneRsp ack already left above. Place the
		// victim in writeBufferBuffer if a slot is free, else park it in the
		// bounded deferral list (drained next ticks via drainDeferredFlush).
		// Pointer only — evictingData/Addr/... are already captured on trans.
		if s.cache.writeBufferBufferCanPush(trans.fromLocal) {
			s.cache.writeBufferBufferPush(trans, trans.fromLocal)
		} else {
			s.cache.writeBuffer.deferFlushPush(trans)
		}

		if s.cache.debugProcess && trans.action == bankWritePrefetched {
			fmt.Printf("[%s]\t[WARNING]\twrong trans action\n", s.cache.name)
		}
	}

	// Deferred-invalidation: an InvReq arrived while this block was
	// IsLocked by the write-hit op. It has already been acked upstream;
	// the writeToHomeNode flush above (if any) has already captured
	// block metadata, so zero the local copy now.
	s.applyPendingInvalidation(block)

	tracing.TraceReqComplete(write, s.cache)

	// log.Printf("%.10f, %s, bank write hit finalize， "+
	// "%s, %04X, %04X, (%d, %d), %v\n",
	// 	now, s.cache.Name(),
	// 	trans.write.ID,
	// 	trans.write.Address, block.Tag,
	// 	block.SetID, block.WayID,
	// 	write.Data,
	// )

	return true
}

func (s *bankStage) writeData(
	block *internal.Block,
	write *mem.WriteReq,
	offset uint64,
) []bool {
	data, err := s.cache.storage.Read(
		block.CacheAddress, 1<<s.cache.log2BlockSize)
	if err != nil {
		panic(err)
	}

	dirtyMask := block.DirtyMask
	if dirtyMask == nil {
		dirtyMask = make([]bool, 1<<s.cache.log2BlockSize)
	}

	for i := 0; i < len(write.Data); i++ {
		if write.DirtyMask == nil || write.DirtyMask[i] {
			index := offset + uint64(i)
			data[index] = write.Data[i]
			dirtyMask[index] = true
		}
	}

	err = s.cache.storage.Write(block.CacheAddress, data)
	if err != nil {
		panic(err)
	}

	return dirtyMask
}

func (s *bankStage) finalizeBankWriteFetched(
	trans *transaction,
) bool {
	if !s.cache.mshrStageBufferCanPush(trans.fromLocal) {
		return false
	}

	// [RESPONSE-DECOUPLE] The read completion (mshrStageBufferPush below, which
	// releases the L1 read-miss responder = the DataReadyRsp that drains the
	// home's inflight) must proceed even when the displacement victim cannot
	// enter writeBufferBuffer — the same cap-coupling that deadlocks peer-write
	// serves also strands own reads. Require only that the victim can be PLACED:
	// writeBufferBuffer now, or the bounded deferral list.
	if trans.writeToHomeNode &&
		!s.cache.writeBufferBufferCanPush(trans.fromLocal) &&
		!s.cache.writeBuffer.deferFlushCanPush(trans.fromLocal) {
		return false
	}

	mshrEntry := trans.mshrEntry
	block := mshrEntry.Block
	s.cache.mshrStageBufferPush(mshrEntry, trans.fromLocal)

	err := s.cache.storage.Write(block.CacheAddress, mshrEntry.Data)
	if err != nil {
		panic(err)
	}

	block.IsLocked = false
	block.IsValid = true

	s.decInflight(trans.fromLocal)

	if trans.writeToHomeNode {
		trans.evictingPID = block.PID
		trans.evictingAddr = block.Tag
		trans.evictingData = mshrEntry.Data
		trans.evictingDirtyMask = block.DirtyMask

		trans.action = writeBufferFlush
		// [RESPONSE-DECOUPLE] read completion already emitted (mshrStageBufferPush
		// above); place the victim now or park it in the bounded deferral list.
		if s.cache.writeBufferBufferCanPush(trans.fromLocal) {
			s.cache.writeBufferBufferPush(trans, trans.fromLocal)
		} else {
			s.cache.writeBuffer.deferFlushPush(trans)
		}
	}

	// Deferred-invalidation: see directoryStage.doInvalidation. The
	// L1 read miss responder has already been served via mshrStage drain;
	// the home-node flush above (if any) has captured block metadata.
	// Safe to zero the local L2 copy now.
	s.applyPendingInvalidation(block)

	if s.cache.debugProcess && trans.accessReq() != nil && trans.accessReq().GetAddress() == s.cache.debugAddress0 {
		fmt.Printf("[%s] [bankStage]\tReceived rsp - 2.2: addr %x, action %d, writeToHomenode %t\n",
			s.cache.name, trans.accessReq().GetAddress(), trans.action, trans.writeToHomeNode)
	}
	if s.cache.debugProcess && trans.accessReq() != nil && trans.accessReq().GetAddress() == s.cache.debugAddress1 {
		fmt.Printf("[%s] [bankStage]\tReceived rsp - 2.2: addr %x, action %d, writeToHomenode %t\n",
			s.cache.name, trans.accessReq().GetAddress(), trans.action, trans.writeToHomeNode)
	}
	// if trans.accessReq() != nil {
	// 	log.Printf("%.10f, %s, write fetched, "+
	// 		"%s, %04X, %04X, (%d, %d), %v\n",
	// 		now, s.cache.Name(),
	// 		trans.accessReq().Meta().ID,
	// 		trans.accessReq().GetAddress(), block.Tag,
	// 		block.SetID, block.WayID,
	// 		mshrEntry.Data,
	// 	)
	// }

	return true
}

func (s *bankStage) finalizeBankWritePrefetched(
	trans *transaction,
) bool {
	// 1. MSHR 버퍼와 WriteBuffer 버퍼 모두 여유 공간이 있는지 확인
	if !s.cache.mshrStageBufferCanPush(trans.fromLocal) {
		return false
	}
	if trans.action == bankEvictAndPrefetch && !s.cache.writeBufferBufferCanPush(trans.fromLocal) {
		return false
	}

	block := trans.block
	data := trans.fetchedData

	// 2. 덮어쓰기 전에 기존 데이터를 읽어내어 Eviction 준비 (EvictAndPrefetch인 경우만)
	var oldData []byte
	var err error
	if trans.action == bankEvictAndPrefetch {
		oldData, err = s.cache.storage.Read(block.CacheAddress, 1<<s.cache.log2BlockSize)
		if err != nil {
			panic(err)
		}
	}

	// 3. MSHR 깨우기 (필요 시)
	if trans.mshrEntry != nil {
		s.cache.mshrStageBufferPush(trans.mshrEntry, trans.fromLocal)
	}

	// 4. 새로운 Prefetch 데이터를 뱅크에 기록
	err = s.cache.storage.Write(block.CacheAddress, data)
	if err != nil {
		panic(err)
	}

	// ====================================================================
	// 5. [핵심 수정] 블록 상태 갱신은 조건문 "밖"에 있어야 합니다!
	// ====================================================================
	block.IsLocked = false
	block.IsValid = true
	block.IsDirty = false // (필요하다면 활성화)

	// 6. 기존 데이터를 writeBufferStage로 넘겨 하위로 Flush(Write)
	what := "Prefetch"
	if trans.action == bankEvictAndPrefetch {
		if trans.evictingAddr == 0 {
			trans.evictingAddr = block.Tag
			trans.evictingPID = block.PID
			trans.evictingDirtyMask = block.DirtyMask
		}
		trans.evictingData = oldData
		trans.action = writeBufferFlush // writeBufferStage가 이 데이터를 하단으로 내려보내도록 액션 지정

		// evictAndPrefetch()에서 추가한 evictingList 항목 제거.
		// 제거하지 않으면 해당 주소에 대한 이후 모든 요청이 영구적으로 차단됨.
		delete(s.cache.evictingList, trans.evictingAddr)

		// WriteBufferStage로 방출(Push)
		s.cache.writeBufferBufferPush(trans, trans.fromLocal)

		// [핵심 수정] 하위로 보냈으므로 Downward 카운터 차감
		s.decDownward(trans.fromLocal)
		what = "EvictAndPrefetch"

		if s.cache.debugProcess {
			fmt.Printf("[Prefetch Profiling] [%s]\tEvict And Prefetch: evict %x, prefetch %x\n", s.cache.name, trans.evictingAddr, block.Tag)
		}
	}

	s.decInflight(trans.fromLocal)

	if s.cache.debugProcess && trans.accessReq() != nil && trans.accessReq().GetAddress() == s.cache.debugAddress0 {
		fmt.Printf("[%s] [bankStage]\tReceived rsp - 2.3: addr %x, action %d\n", s.cache.name, trans.accessReq().GetAddress(), trans.action)
	}
	if s.cache.debugProcess && trans.accessReq() != nil && trans.accessReq().GetAddress() == s.cache.debugAddress1 {
		fmt.Printf("[%s] [bankStage]\tReceived rsp - 2.3: addr %x, action %d\n", s.cache.name, trans.accessReq().GetAddress(), trans.action)
	}

	// Deferred-invalidation: prefetched data was never returned to any
	// L1, so dropping the local L2 copy here is invisible to consumers.
	// For bankEvictAndPrefetch, the OLD victim was already pushed to
	// writeBufferBuffer above so its flush is unaffected.
	s.applyPendingInvalidation(block)

	s.cache.incEvent(what)
	tracing.TraceReqFinalize(trans.prefetch, s.cache)
	return true
}

// applyPendingInvalidation finalizes a deferred InvReq that was acked
// upstream by directoryStage.doInvalidation while this block was still
// IsLocked. The InvRsp has already been sent; CohDir considers this L2
// a non-sharer. Must be called AFTER the bank op consumes any block
// metadata it needs (e.g. PID/Tag/DirtyMask for the writeToHomeNode
// flush) and AFTER IsLocked has been cleared.
func (s *bankStage) applyPendingInvalidation(block *internal.Block) {
	if !block.PendingInvalidation {
		return
	}
	// [INV-FIDELITY C3] Deferred kill of a dirty line: the modified data
	// must be written back to local DRAM before the zero-out, exactly like
	// the immediate path in doInvalidation. finalizeWriteHit gates on
	// flush capacity up front; the write-through (writeToHomeNode) and
	// fetch/prefetch finalize paths leave the block clean, so in practice
	// the fallback drop counter stays 0.
	if block.IsValid && block.IsDirty {
		if s.cache.enqueueInvDirtyFlush(block) {
			s.cache.incEvent("InvDirtyWritebackDeferred")
		}
	}
	s.cache.eraseCacheLineFromRWMask(block.PID, block.VAddr)
	newBlk := &internal.Block{
		WayID:        block.WayID,
		SetID:        block.SetID,
		CacheAddress: block.CacheAddress,
	}
	*block = *newBlk
	s.cache.deferredInvApplied++
	s.cache.incEvent("DeferredInvApplied")
}

func (s *bankStage) removeTransaction(trans *transaction) {
	for i, t := range s.cache.inFlightTransactions {
		if trans == t {
			// fmt.Printf("%.10f, %s, trans %s removed in bank stage.\n",
			// 	now, s.cache.Name(), t.id)
			s.cache.inFlightTransactions = append(
				(s.cache.inFlightTransactions)[:i],
				(s.cache.inFlightTransactions)[i+1:]...)

			return
		}
	}

	now := s.cache.Engine.CurrentTime()

	fmt.Printf("%.10f, %s, Transaction %s not found\n",
		now, s.cache.Name(), trans.id)

	// panic("transaction not found")
}

func (s *bankStage) finalizeBankEviction(
	trans *transaction,
) bool {
	if !s.cache.writeBufferBufferCanPush(trans.fromLocal) {
		return false
	}

	victim := trans.victim

	data, err := s.cache.storage.Read(
		victim.CacheAddress, 1<<s.cache.log2BlockSize)
	if err != nil {
		panic(err)
	}

	trans.evictingData = data

	switch trans.action {
	case bankEvict:
		trans.action = writeBufferFlush
	case bankEvictAndFetch:
		trans.action = writeBufferEvictAndFetch
	case bankEvictAndWrite:
		trans.action = writeBufferEvictAndWrite
	default:
		panic("unsupported action")
	}

	// if trans.accessReq() != nil {
	// 	log.Printf("%.10f, %s, bank read for eviction， "+
	// 		"%s, %04X, %04X, (%d, %d), %v\n",
	// 		now, s.cache.Name(),
	// 		trans.accessReq().Meta().ID,
	// 		trans.accessReq().GetAddress(), victim.Tag,
	// 		victim.SetID, victim.WayID,
	// 		data,
	// 	)
	// }

	delete(s.cache.evictingList, trans.evictingAddr)
	s.cache.writeBufferBufferPush(trans, trans.fromLocal)

	s.decInflight(trans.fromLocal)
	s.decDownward(trans.fromLocal)

	return true
}
