package REC

import (
	"fmt"

	"github.com/sarchlab/akita/v4/mem/cache/REC/internal"
	"github.com/sarchlab/akita/v4/pipelining"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/akita/v4/tracing"
)

type dirPipelineItem struct {
	trans *transaction
}

func (i dirPipelineItem) TaskID() string {
	return i.trans.id + "_dir_pipeline"
}

type directoryStage struct {
	cache *Comp

	localPipeline  pipelining.Pipeline
	remotePipeline pipelining.Pipeline
	localBuf       sim.Buffer
	remoteBuf      sim.Buffer

	// [ITER20 COMPLETION-VC] Skip-head side queue for the REMOTE dir
	// pipeline only. processTransaction(isLocal=false) Peeks remoteBuf
	// strict-FIFO and used to break on any stall. A peer-incoming
	// eviction (EvictAndInsertNewEntry / write-miss / inv-class) at the
	// head — stalled on the REQUEST-path resources (remoteDirToBankBuffers,
	// MSHR, victim lock) — then strict-HoL-blocked every completion-bearing
	// peer-incoming HIT (read-hit / write-hit-full-perm fast-path, the SOLE
	// producer of the cross-GPU DataReadyRsp/WriteDoneRsp) queued behind it,
	// so the completion that frees the peer's numRemoteInflightEviction /
	// L2 writeBufferBuffer could never be produced -> symmetric cross-GPU
	// deadlock. We move such a stalled, request-path-coupled head into this
	// side queue so the HIT behind it can reach remoteBSBData (the
	// completion's own forward buffer). Cap-neutral: deferral count is
	// bounded by remoteBuf.Capacity() (an item is only ever moved FROM
	// remoteBuf INTO here, never duplicated), and we only skip past an
	// address-DISJOINT head so no same-cacheline operation is reordered.
	remoteDeferredEvict []*transaction

	returnFalse0 string
	returnFalse1 string
	returnFalse  *string
}

func (ds *directoryStage) Tick() (madeProgress bool) {
	// [FIX da6a521 #2 패턴 적용] Remote 먼저, Local 나중에 처리.
	// cross-GPU forward progress 보장 — local trans가 remote 처리를
	// 가로막아 inter-GPU cycle deadlock에 빠지는 것을 방지한다.
	madeProgress = ds.acceptNewTransaction(false) || madeProgress // Remote 먼저
	madeProgress = ds.acceptNewTransaction(true) || madeProgress  // Local 나중

	madeProgress = ds.remotePipeline.Tick() || madeProgress
	madeProgress = ds.localPipeline.Tick() || madeProgress

	madeProgress = ds.processTransaction(false) || madeProgress // Remote 먼저
	madeProgress = ds.processTransaction(true) || madeProgress  // Local 나중

	return madeProgress
}

func (ds *directoryStage) acceptNewTransaction(fromLocal bool) bool {
	madeProgress := false

	targetBuf := ds.cache.localDirStageBuffer
	targetPipeline := ds.localPipeline
	if !fromLocal {
		targetBuf = ds.cache.remoteDirStageBuffer
		targetPipeline = ds.remotePipeline
	}

	// 사이클 당 최대 처리량(numReqPerCycle)만큼 단일 루프 실행
	for i := 0; i < ds.cache.numReqPerCycle; i++ {
		// 1. 버퍼 폴링 (Ack 버퍼를 먼저 확인하여 교착 상태 방지, 이후 일반 버퍼 확인)
		item := targetBuf.Peek()
		if item == nil {
			break
		}
		trans := item.(*transaction)
		req := trans.accessReq()

		if req == nil {
			fmt.Printf("[%s][ERROR] Transaction %s has nil AccessReq. action: %d\n", ds.cache.name, trans.id, trans.action)
		}
		if !targetPipeline.CanAccept() {
			return madeProgress // 파이프라인 정체 시 조기 종료
		}

		targetPipeline.Accept(dirPipelineItem{trans})
		targetBuf.Pop()
		madeProgress = true

	}

	return madeProgress
}

func (ds *directoryStage) Reset() {
	ds.localPipeline.Clear()
	ds.remotePipeline.Clear()

	ds.localBuf.Clear()
	ds.remoteBuf.Clear()

	// [ITER20 COMPLETION-VC] drop deferred remote evictions on reset.
	ds.remoteDeferredEvict = nil

	ds.cache.localDirStageBuffer.Clear()
	ds.cache.remoteDirStageBuffer.Clear()
}

func (ds *directoryStage) processTransaction(isLocal bool) bool {
	madeProgress := false
	buf := ds.localBuf
	ds.returnFalse = &ds.returnFalse0
	if !isLocal {
		// [ITER20 COMPLETION-VC] Remote pipeline uses the skip-head variant
		// so a request-path-coupled eviction at the FIFO head cannot HoL-
		// block a completion-bearing HIT behind it. Local pipeline keeps the
		// original strict-FIFO semantics (no cross-GPU completion cycle on
		// the local plane).
		return ds.processRemoteTransaction()
	}
	*ds.returnFalse = ""

	for i := 0; i < ds.cache.numReqPerCycle; i++ {
		*ds.returnFalse += "."
		item := buf.Peek()
		if item == nil {
			break
		}

		trans := item.(dirPipelineItem).trans

		addr := trans.accessReq().GetAddress()
		cacheLineID, _ := getCacheLineID(addr, ds.cache.log2BlockSize+uint64(ds.cache.log2NumSubEntry))

		if _, evicting := ds.cache.evictingList[cacheLineID]; evicting {
			ds.cache.stallEvictingList++
			break
		}

		if ds.doWrite(trans, isLocal) {
			buf.Pop()
			madeProgress = true
		} else {
			break // 처리가 막히면 해당 Bank의 루프 중단
		}
	}

	if madeProgress {
		*ds.returnFalse = ""
	}
	return madeProgress
}

// regionCacheLineID returns the region-granular cacheline ID used for the
// evictingList / same-region hazard check (block + sub-entry span).
func (ds *directoryStage) regionCacheLineID(addr uint64) uint64 {
	id, _ := getCacheLineID(addr,
		ds.cache.log2BlockSize+uint64(ds.cache.log2NumSubEntry))
	return id
}

// deferredHasRegion reports whether any currently-deferred remote eviction
// targets the same region as addr. We must NOT skip a buffer item past a
// deferred item that shares its region, otherwise a later same-region
// request could be (re)ordered ahead of an earlier eviction of that region
// and observe stale directory state. Address-disjointness makes the skip
// provably order-preserving for every individual cacheline.
func (ds *directoryStage) deferredHasRegion(addr uint64) bool {
	region := ds.regionCacheLineID(addr)
	for _, t := range ds.remoteDeferredEvict {
		if ds.regionCacheLineID(t.accessReq().GetAddress()) == region {
			return true
		}
	}
	return false
}

// isCompletionBearingHit is a SIDE-EFFECT-FREE classification of a remote
// (peer-incoming) transaction. It returns true when doWrite would resolve to
// the read-hit / write-hit-full-permission fast-path or a read MSHR-hit —
// i.e. the cases that push to remoteBSBData and ultimately produce the
// cross-GPU DataReadyRsp/WriteDoneRsp. Such a transaction depends only on
// remoteBSBData.CanPush() (the completion's own forward buffer); skipping it
// cannot help, so the caller keeps strict ordering for it. Everything else
// (write-miss, eviction, inv-class write-hit) is REQUEST-path-coupled (needs
// remoteDirToBankBuffers / MSHR / victim lock) and is what we defer.
//
// Mirrors the read-only lookups doWrite performs (mshr.Query, directory.
// Lookup, writePermission/readPermission) BEFORE any mutation. No directory
// state, MSHR, or buffer is modified here.
func (ds *directoryStage) isCompletionBearingHit(trans *transaction) bool {
	req := trans.accessReq()
	cachelineID, _ := getCacheLineID(req.GetAddress(), ds.cache.log2BlockSize)

	if mshrEntry := ds.cache.mshr.Query(req.GetPID(), cachelineID); mshrEntry != nil {
		// Read MSHR-hit returns true (doWriteMSHRHit always succeeds);
		// a write on an MSHR hit stalls (no completion produced) -> not a
		// completion-bearing hit.
		return trans.write == nil
	}

	block, index := ds.cache.directory.Lookup(req.GetPID(), req.GetAddress())
	if block == nil || !block.SubEntry[index].IsValid {
		return false // miss -> doWriteMiss (request-path coupled)
	}

	subEntry := block.SubEntry[index]
	if subEntry.IsLocked || subEntry.ReadCount > 0 {
		return false // doWriteHit early-returns false; not a clean hit
	}

	if trans.isReadTrans() && !trans.read.FetchForWriteMiss {
		return true // read-hit fast-path -> remoteBSBData
	}

	// Write-hit: only the full-permission branch (action Nothing) is a
	// completion-bearing fast-path; otherwise it falls through to writeToBank
	// (request-path / inv emission) -> defer-eligible.
	if !trans.isReadTrans() && ds.writePermission(trans, subEntry.Sharer) {
		return true
	}

	return false
}

// processRemoteTransaction is the skip-head variant of processTransaction for
// the remote (peer-incoming) directory pipeline. Each Tick it first retries
// the oldest deferred evictions, then scans remoteBuf; a request-path-coupled
// head that stalls is moved into remoteDeferredEvict so a completion-bearing
// HIT behind it can be reached. See remoteDeferredEvict for the deadlock
// rationale and the cap-neutrality / order-preservation invariants.
func (ds *directoryStage) processRemoteTransaction() bool {
	madeProgress := false
	buf := ds.remoteBuf
	ds.returnFalse = &ds.returnFalse1
	*ds.returnFalse = ""

	// 1. Retry deferred evictions first (oldest first, in order). A deferred
	//    item is processed exactly like a normal head; on success it leaves
	//    the side queue, freeing a slot for a future deferral. Stop at the
	//    first still-stalled deferred item to preserve same-region order
	//    among deferred items.
	for len(ds.remoteDeferredEvict) > 0 {
		trans := ds.remoteDeferredEvict[0]
		region := ds.regionCacheLineID(trans.accessReq().GetAddress())
		if _, evicting := ds.cache.evictingList[region]; evicting {
			ds.cache.stallEvictingList++
			break
		}
		if ds.doWrite(trans, false) {
			ds.remoteDeferredEvict[0] = nil
			ds.remoteDeferredEvict = ds.remoteDeferredEvict[1:]
			madeProgress = true
		} else {
			break
		}
	}

	// 2. Scan remoteBuf with skip-head over request-path-coupled stalls.
	for i := 0; i < ds.cache.numReqPerCycle; i++ {
		*ds.returnFalse += "."
		item := buf.Peek()
		if item == nil {
			break
		}

		trans := item.(dirPipelineItem).trans
		addr := trans.accessReq().GetAddress()
		region := ds.regionCacheLineID(addr)

		if _, evicting := ds.cache.evictingList[region]; evicting {
			ds.cache.stallEvictingList++
			break
		}

		if ds.doWrite(trans, false) {
			buf.Pop()
			madeProgress = true
			continue
		}

		// Head stalled. Decide whether to skip it.
		//  - A completion-bearing HIT stalls only on remoteBSBData (its own
		//    forward buffer); skipping cannot help -> keep strict order, stop.
		//  - A request-path-coupled item (evict / write-miss / inv-class)
		//    blocks the request plane; defer it so a HIT behind it can run,
		//    but ONLY if it is address-disjoint from every deferred item and
		//    the side queue still has cap-neutral headroom (<= remoteBuf cap).
		if ds.isCompletionBearingHit(trans) {
			break
		}
		if ds.deferredHasRegion(addr) {
			break
		}
		if len(ds.remoteDeferredEvict) >= buf.Capacity() {
			break
		}

		buf.Pop()
		ds.remoteDeferredEvict = append(ds.remoteDeferredEvict, trans)
		ds.cache.stallEvictingList++
		madeProgress = true
		// continue scanning: the next head may be a completion-bearing HIT.
	}

	if madeProgress {
		*ds.returnFalse = ""
	}
	return madeProgress
}

func (ds *directoryStage) doWrite(trans *transaction, isLocal bool) bool {
	*ds.returnFalse += "[doWrite] "
	ds.cache.totalDoWriteCalls++
	// regionLen := ds.cache.log2BlockSize + uint64(ds.cache.log2NumSubEntry)
	req := trans.accessReq()
	// cachelineID, _ := getCacheLineID(req.GetAddress(), uint64(regionLen)) // 하위 영역은 0으로 지우기

	// MSHR is keyed at 64B granularity: the paper's REC design handles each
	// incoming request with an immediate bitwise OR on position bits (§4.2),
	// with no serialization between different sub-entries in the same 1KB block.
	// Each sub-entry fetch is independent, so each 64B address gets its own entry.
	cachelineID, _ := getCacheLineID(req.GetAddress(), ds.cache.log2BlockSize)
	mshrEntry := ds.cache.mshr.Query(req.GetPID(), cachelineID)
	// [DOWRITE-TRACE] is this the traced cache line?
	dbgLine := false
	if ds.cache.debugProcess {
		tLine, _ := getCacheLineID(ds.cache.debugAddress, ds.cache.log2BlockSize)
		dbgLine = cachelineID == tLine
	}
	if mshrEntry != nil {
		if trans.write != nil { // write 인 경우, MSHR hit이 발생하면 처리 x
			*ds.returnFalse += "MSHR hit for write request"
			// [DOWRITE-TRACE] peer eviction (write) rejected by a line-level
			// MSHR conflict (an in-flight request — usually local — holds the
			// MSHR entry for this line). Counted per origin so we can see if
			// PEER writes are being starved here. Not a capacity stall.
			if isLocal {
				ds.cache.stallMSHRHitWriteLocal++
			} else {
				ds.cache.stallMSHRHitWriteRemote++
			}
			if dbgLine {
				fmt.Printf("[%s][TRACE] doWrite addr=%x line=%x REJECT(MSHR-hit-write) isLocal=%v fromLocal=%v reqID=%s\n",
					ds.cache.name, req.GetAddress(), cachelineID, isLocal, trans.fromLocal, trans.accessReq().Meta().ID)
			}
			return false
		}

		// if trans.read.FetchForWriteMiss {
		// 	*ds.returnFalse += "MSHR hit for fetch for write miss request"
		// 	return false
		// }

		ok := ds.doWriteMSHRHit(trans, mshrEntry)
		if ok {
			if isLocal {
				ds.cache.doWriteMSHRHitLocal++
			} else {
				ds.cache.doWriteMSHRHitRemote++
			}
			if dbgLine {
				fmt.Printf("[%s][TRACE] doWrite addr=%x line=%x MSHR-HIT-COALESCE isLocal=%v fromLocal=%v\n",
					ds.cache.name, req.GetAddress(), cachelineID, isLocal, trans.fromLocal)
			}
			tracing.AddTaskStep(
				tracing.MsgIDAtReceiver(trans.accessReq(), ds.cache),
				ds.cache,
				"read-mshr-hit",
			)
		}

		return ok
	}

	block, index := ds.cache.directory.Lookup(trans.accessReq().GetPID(), req.GetAddress())
	// if block != nil {
	// 	fmt.Printf("[%s]\tDirectory Lookup: Addr %x -> %x, bank %d, set %d, way %d, index %d\n",
	// 		ds.cache.name, req.GetAddress(), block.Tag, bankID, block.SetID, block.WayID, index)
	// }

	if block != nil && block.SubEntry[index].IsValid {
		ok := ds.doWriteHit(trans, block, index, isLocal)
		if ok {
			if isLocal {
				ds.cache.doWriteHitLocal++
			} else {
				ds.cache.doWriteHitRemote++
			}
			what := "read-hit"
			if trans.write != nil {
				what = "write-hit"
			}

			tracing.AddTaskStep(
				tracing.MsgIDAtReceiver(trans.accessReq(), ds.cache),
				ds.cache,
				what,
			)
		}

		if dbgLine {
			fmt.Printf("[%s][TRACE] doWrite addr=%x line=%x HIT ok=%v isLocal=%v fromLocal=%v write=%v\n",
				ds.cache.name, req.GetAddress(), cachelineID, ok, isLocal, trans.fromLocal, trans.write != nil)
		}
		return ok
	}

	ok := ds.doWriteMiss(trans, isLocal)
	if ok {
		if isLocal {
			ds.cache.doWriteMissLocal++
		} else {
			ds.cache.doWriteMissRemote++
		}
		what := "read-miss"
		if trans.write != nil {
			what = "write-miss"
		}

		tracing.AddTaskStep(
			tracing.MsgIDAtReceiver(trans.accessReq(), ds.cache),
			ds.cache,
			what,
		)
	}

	if dbgLine {
		fmt.Printf("[%s][TRACE] doWrite addr=%x line=%x MISS ok=%v isLocal=%v fromLocal=%v write=%v\n",
			ds.cache.name, req.GetAddress(), cachelineID, ok, isLocal, trans.fromLocal, trans.write != nil)
	}

	return ok
}

func (ds *directoryStage) doWriteMSHRHit(
	trans *transaction,
	mshrEntry *internal.MSHREntry,
) bool {
	trans.mshrEntry = mshrEntry

	// regionLen := ds.cache.log2BlockSize + uint64(ds.cache.log2NumSubEntry)
	// _, offset := getCacheLineID(trans.accessReq().GetAddress(), regionLen)
	// offset = offset >> ds.cache.log2BlockSize
	// trans.blockIdx = int(offset)

	addr := trans.accessReq().GetAddress()
	trans.blockIdx = int((addr >> ds.cache.log2BlockSize) % uint64(1<<ds.cache.log2NumSubEntry))

	mshrEntry.Requests = append(mshrEntry.Requests, trans)

	if ds.cache.debugProcess && trans.read != nil && trans.read.Address == ds.cache.debugAddress {
		fmt.Printf("[%s] [directoryStage]\tReceived remote read req - 1: addr %x\n", ds.cache.name, trans.read.Address)
	}

	return true
}

func (ds *directoryStage) doWriteHit(
	trans *transaction,
	block *internal.CohEntry,
	index int,
	isLocal bool,
) bool {
	*ds.returnFalse += "[doWriteHit] "
	subEntry := block.SubEntry[index]
	if subEntry.IsLocked || subEntry.ReadCount > 0 {
		*ds.returnFalse += "Subentry is being used"
		ds.cache.stallSubEntryLocked++
		return false
	}

	if trans.isReadTrans() && !trans.read.FetchForWriteMiss {
		// Read-hit fast-path. Both branches (already-sharer / new-sharer)
		// avoid writeToBank because the directory state mutation is a
		// trivial sharer-list append (no MSHR, no sub-entry lock, no
		// bank-pipeline traversal). Without this path, the per-sub-entry
		// tracking required by REC inflates dir_avg_latency to ~78ns.
		// [R4] Read-hit fast-path is Data-class only (action is Nothing or
		// UpdateEntry — sharer-list append, no inv emission). Push to BSBData.
		targetBuffer := ds.cache.localBSBData
		if !isLocal {
			targetBuffer = ds.cache.remoteBSBData
		}
		if !targetBuffer.CanPush() {
			*ds.returnFalse += "Cannot push to bottom sender buffer (read-hit fast-path)"
			ds.cache.stallBottomBufFull++
			return false
		}

		if ds.readPermission(trans, subEntry.Sharer) {
			// Position/sharer bits already set — no mutation needed.
			trans.action = Nothing
		} else {
			// Append sharer atomically here; bottomSender forwards the
			// read to L2 with action=UpdateEntry so the existing
			// per-action accounting still works.
			ds.cache.directory.Visit(block)
			block.SubEntry[index].Sharer = appendSharerInline(
				block.SubEntry[index].Sharer,
				trans.accessReq().GetSrcRDMA(),
			)
			trans.action = UpdateEntry
		}
		trans.bottomEnterTime = ds.cache.Engine.CurrentTime()
		trans.pathCategory = "fast"
		targetBuffer.Push(trans)
		return true
	}

	if ds.writePermission(trans, subEntry.Sharer) {
		trans.action = Nothing
	} else if !trans.fromLocal {
		// Remote write hit on a valid offset: per REC paper §4.2,
		// the writer becomes the sole sharer; the position bit and
		// writer's sharer bit are preserved. This branch *moves* the
		// emit from InvalidateAndUpdateEntry to the dedicated
		// RemoteWriteHitPreserveWriter handler in bankstage; pre-fix
		// REC clobbered both the writer and the position bit here.
		trans.action = RemoteWriteHitPreserveWriter
		// invalidationList must exclude the writer; bottomSender
		// filters self-as-writer, but we set it explicitly for
		// clarity at the producer site.
		trans.invalidationList = make([]sim.RemotePort, 0, len(subEntry.Sharer))
		for _, sh := range subEntry.Sharer {
			if sh != trans.accessReq().GetSrcRDMA() && sh != "" {
				trans.invalidationList = append(trans.invalidationList, sh)
			}
		}
		trans.evictingAddr = block.Tag + uint64(index)*(1<<ds.cache.log2NumSubEntry)
		trans.evictingPID = block.PID
	} else {
		trans.action = InvalidateAndUpdateEntry
		trans.invalidationList = subEntry.Sharer
		trans.evictingAddr = block.Tag + uint64(index)*(1<<ds.cache.log2NumSubEntry)
		trans.evictingPID = block.PID
	}

	// [R4] doWriteHit fast-path reaches the push-site only when
	// trans.action is Nothing (write hit with full permission — no inv
	// needed). Inv-class actions (InvalidateAndUpdateEntry,
	// RemoteWriteHitPreserveWriter) fall through to writeToBank ->
	// bankstage, which routes them to BSBInv. So this push-site is
	// pure Data-class.
	targetBuffer := ds.cache.localBSBData
	if !isLocal {
		targetBuffer = ds.cache.remoteBSBData
	}
	if trans.action == Nothing {
		if !targetBuffer.CanPush() {
			*ds.returnFalse += "Cannot push to target bottom sender buffer"
			ds.cache.stallBottomBufFull++
			return false
		}

		trans.bottomEnterTime = ds.cache.Engine.CurrentTime()
		trans.pathCategory = "fast"
		targetBuffer.Push(trans)
		return true
	}

	return ds.writeToBank(trans, block, index, isLocal)
}

func (ds *directoryStage) doWriteMiss(trans *transaction, isLocal bool) bool {
	*ds.returnFalse += "[doWriteMiss] "
	// [R4] doWriteMiss local-write fast-path: action forced to Nothing
	// (no directory entry added for local-write miss) -> Data-class.
	targetBuffer := ds.cache.localBSBData
	if !isLocal {
		targetBuffer = ds.cache.remoteBSBData
	}
	if trans.fromLocal { // local write request에 대해 directory miss 발생 시, entry 추가 안 함
		trans.action = Nothing
		if !targetBuffer.CanPush() {
			*ds.returnFalse += "Cannot push to bottom sender buffer"
			ds.cache.stallBottomBufFull++
			return false
		}
		trans.bottomEnterTime = ds.cache.Engine.CurrentTime()
		trans.pathCategory = "fast"
		targetBuffer.Push(trans)
		return true
	}

	if ds.cache.mshr.IsFull() {
		*ds.returnFalse += "MSHR is full"
		ds.cache.stallMSHRFull++
		return false
	}

	// bankID를 PTE 또는 regionSizeBuffer에서 확인하기
	blkSize := ds.cache.log2BlockSize

	pid := trans.accessReq().GetPID()
	addr := trans.accessReq().GetAddress()
	cachelineID, _ := getCacheLineID(addr, uint64(blkSize))
	index := (addr >> blkSize) % uint64(1<<ds.cache.log2NumSubEntry)

	victim, alloc := ds.cache.directory.FindVictim(pid, cachelineID)

	if !alloc {
		trans.action = UpdateEntry
		return ds.writeToBank(trans, victim, int(index), isLocal)
	}

	if victim.IsLockedEntry() || victim.GetReadCount() > 0 {
		*ds.returnFalse += "Victim is being used"
		ds.cache.stallVictimLocked++
		return false
	}

	ds.cache.allocCount++
	if ds.needEviction(victim) {
		ds.cache.needEvictionCount++
		trans.action = EvictAndInsertNewEntry
		trans.victim = *victim.DeepCopy()
		trans.evictingAddr = victim.Tag
		trans.evictingPID = victim.PID
	} else {
		ds.cache.silentResetCount++
		trans.action = InsertNewEntry
	}

	// victim은 bloomfilter 감소, 추가되는 entry는 증가
	// if victim.IsValidEntry() {
	// 	addr := victim.Tag
	// 	diff := uint64(1 << ds.cache.log2BlockSize)
	// 	for i, e := range victim.SubEntry {
	// 		if e.IsValid {
	// 			victim.SubEntry[i].IsValid = false
	// 		}
	// 		addr += diff
	// 	}
	// }
	victim.Reset()

	return ds.writeToBank(trans, victim, int(index), isLocal)
}

func (ds *directoryStage) writeToBank(
	trans *transaction,
	block *internal.CohEntry,
	index int,
	isLocal bool,
) bool {
	*ds.returnFalse += "[writeToBank] "
	// numBanks := len(ds.cache.dirToBankBuffers)
	// bank := bankID(block, ds.cache.directory.WayAssociativity(), numBanks)

	bankBuf := ds.cache.localDirToBankBuffers
	if !isLocal {
		bankBuf = ds.cache.remoteDirToBankBuffers
	}

	if !bankBuf.CanPush() {
		*ds.returnFalse += "Cannot push to bank buffer"
		ds.cache.stallBankFull++
		return false
	}

	if ds.cache.mshr.IsFull() {
		*ds.returnFalse += "MSHR is full (writeToBank)"
		ds.cache.stallMSHRFull++
		return false
	}

	maskLen := int(ds.cache.log2BlockSize) + ds.cache.log2NumSubEntry
	addr := trans.accessReq().GetAddress()
	cachelineID, _ := getCacheLineID(addr, uint64(ds.cache.log2BlockSize))

	subEntry := &block.SubEntry[index]
	ds.cache.directory.Visit(block)
	block.Tag = cachelineID >> maskLen << maskLen
	block.IsValid = true
	block.PID = trans.accessReq().GetPID()
	subEntry.IsLocked = true
	subEntry.IsValid = true
	trans.block = block
	trans.blockIdx = index

	bankBuf.Push(trans)

	mshrEntry := ds.cache.mshr.Add(block.PID, cachelineID)
	mshrEntry.Block = block
	mshrEntry.BlockIdx = index
	trans.mshrEntry = mshrEntry
	mshrEntry.Requests = nil

	return true
}

func (ds *directoryStage) needEviction(victim *internal.CohEntry) bool {
	if !victim.IsValidEntry() {
		return false
	}

	for _, e := range victim.SubEntry {
		if e.IsValid && len(e.Sharer) > 0 {
			return true
		}
	}

	return false
}

func (ds *directoryStage) readPermission(trans *transaction, sharer []sim.RemotePort) bool {
	if !trans.fromLocal { // remote access
		// Sharer entries are recorded as the requester's *RDMA* port
		// (see bankstage.UpdateEntry: appendSharer uses GetSrcRDMA()).
		// The previous code compared against Meta().Src (the immediate
		// sender, e.g. the RDMA engine port), which never matched the
		// stored RDMA port — so this branch always returned false and
		// the action=Nothing fast-path in doWriteHit never fired.
		// Re-fetches by an existing sharer therefore traversed the full
		// writeToBank → bank → mshrStage pipeline unnecessarily.
		src := trans.accessReq().GetSrcRDMA()
		for _, sh := range sharer {
			if sh == src {
				return true
			}
		}

		return false
	} else { // local access
		return true
	}
}

// appendSharerInline appends sh to the sharer list if not already present.
// Used by the read-hit fast-path (UpdateEntry) which mutates the directory
// state in directorystage instead of routing through the bank pipeline.
// The check uses string formatting to match bankstage.sharerExist's
// equality semantics across nominally-different RemotePort instances.
func appendSharerInline(list []sim.RemotePort, sh sim.RemotePort) []sim.RemotePort {
	target := fmt.Sprintf("%s", sh)
	for _, e := range list {
		if fmt.Sprintf("%s", e) == target {
			return list
		}
	}
	return append(list, sh)
}

func (ds *directoryStage) writePermission(trans *transaction, sharer []sim.RemotePort) bool {
	if !trans.fromLocal { // remote access
		// 1. Sharer가 없거나(0명) 여러 명(2명 이상)인 경우 권한 없음
		if len(sharer) != 1 {
			return false
		}

		// 2. Sharer가 딱 1명이지만, 그게 요청자 본인이 아닌 경우 권한 없음
		// Sharer is stored as the requester's RDMA port (GetSrcRDMA),
		// not Meta().Src. See readPermission() for the matching fix
		// rationale.
		if sharer[0] != trans.accessReq().GetSrcRDMA() {
			return false
		}

		// 유일한 Sharer가 본인일 때만 권한 있음
		return true

	} else { // local access
		// Paper §4.2 "Local writes": if any sharer is recorded, an
		// invalidation must be sent. Home GPU is excluded from the
		// sharer list per §2.3, so any present sharer is a remote
		// GPU that holds a stale copy. Returning false here forces
		// doWriteHit to emit InvalidateAndUpdateEntry.
		//
		// Pre-fix code returned true here when len(sharer) <= 1,
		// silently leaving stale data on the lone remote sharer.
		// op5aShortcutWithRemoteSharer (Comp field) is the regression
		// slot for that bug; if a future change reintroduces the
		// shortcut, also reintroduce the increment site here so the
		// post-C-2 sanity dump catches it.
		return len(sharer) == 0
	}
}
