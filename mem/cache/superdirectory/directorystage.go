package superdirectory

import (
	"fmt"
	"os"

	"github.com/sarchlab/akita/v4/mem/cache/superdirectory/internal"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/pipelining"
	"github.com/sarchlab/akita/v4/sim"
)

type dirPipelineItem struct {
	trans *transaction
}

func (i dirPipelineItem) TaskID() string {
	return i.trans.id + "_dir_pipeline"
}

type directoryStage struct {
	cache *Comp

	localPipeline  []pipelining.Pipeline
	remotePipeline []pipelining.Pipeline
	localBuf       []sim.Buffer
	remoteBuf      []sim.Buffer

	motionPipeline []pipelining.Pipeline
	motionBuf      []sim.Buffer

	// [SD-REC PARITY Fix2 = REC ITER20 COMPLETION-VC] Skip-head side queue
	// for the REMOTE dir plane only. processTransaction(isLocal=false)
	// used to break on any stalled per-bank head: a peer-incoming
	// eviction/write-miss/inv-class trans stalled on REQUEST-path
	// resources (remoteDirToBankBuffers, MSHR, victim/sub-entry lock)
	// then strict-HoL-blocked every completion-bearing HIT behind it in
	// the same bank — the sole producer of the cross-GPU DataReadyRsp/
	// WriteDoneRsp that frees the peer's eviction caps. Stalled,
	// request-path-coupled heads are moved here so HITs behind them can
	// reach remoteBottomSenderBufferData. Cap-neutral: items are only
	// ever MOVED from a remoteBuf (never duplicated) and the queue is
	// bounded by the combined remoteBuf capacity; only address-DISJOINT
	// heads (at the coarsest region span) are skipped so no same-region
	// operation is reordered.
	remoteDeferredEvict []*transaction

	returnFalse0 string
	returnFalse1 string
	returnFalse  *string
}

func (ds *directoryStage) Tick() (madeProgress bool) {
	// E-RACE-DEFER: drain deferred Fix B-2 redirects FIRST. These are
	// trans that were forced to wait by a momentarily-full coarser
	// pipeline; retrying them ahead of new acceptNewTransaction work
	// minimizes their dwell time and keeps SO invariant routing
	// honored ASAP. Returns true if any deferred trans dispatched.
	madeProgress = ds.retryDeferredRedirects() || madeProgress

	// 양쪽 모두 Tick 진행
	madeProgress = ds.acceptNewTransaction(true) || madeProgress  // Local
	madeProgress = ds.acceptNewTransaction(false) || madeProgress // Remote
	madeProgress = ds.acceptMotionTransaction() || madeProgress   // Local Motion

	for i, _ := range ds.localPipeline {
		madeProgress = ds.localPipeline[i].Tick() || madeProgress
		madeProgress = ds.remotePipeline[i].Tick() || madeProgress
		madeProgress = ds.motionPipeline[i].Tick() || madeProgress
	}

	madeProgress = ds.processTransaction(true) || madeProgress
	madeProgress = ds.processTransaction(false) || madeProgress
	madeProgress = ds.processMotionTransaction() || madeProgress

	return madeProgress
}

// retryDeferredRedirects drains the deferredRedirects queue.  Each
// entry was pushed by doWriteMiss's Fix B-2 reprobe when the targeted
// coarser bank's pipeline was momentarily full. For every queued
// entry we re-check pipeline.CanAccept and dispatch if possible;
// stuck entries are kept for the next Tick. Re-verification of the
// coarser entry (SubEntry.IsValid) is intentionally skipped: trans
// stalls here are short (until pipeline drains one stage, typically
// within a few cycles), and the coarser entry is very unlikely to
// be evicted in that window. Subsequent stages (bankStage) still
// perform their own directory operations, so the late-binding bank
// alloc is safe under SO invariant in practice.
func (ds *directoryStage) retryDeferredRedirects() bool {
	if len(ds.cache.deferredRedirects) == 0 {
		return false
	}
	madeProgress := false
	remaining := ds.cache.deferredRedirects[:0]
	for _, d := range ds.cache.deferredRedirects {
		pipeline := ds.localPipeline
		if !d.isLocal {
			pipeline = ds.remotePipeline
		}
		if !pipeline[d.targetBank].CanAccept() {
			remaining = append(remaining, d)
			continue
		}
		d.trans.bankID = d.targetBank
		d.trans.bankList = nil
		pipeline[d.targetBank].Accept(dirPipelineItem{d.trans})
		madeProgress = true
	}
	ds.cache.deferredRedirects = remaining
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
		if !fromLocal {
			ds.cache.remoteAcceptCount++
		}
		trans := item.(*transaction)
		req := trans.accessReq()

		if req == nil {
			fmt.Printf("[%s][ERROR] Transaction %s has nil AccessReq. action: %d\n", ds.cache.name, trans.id, trans.action)
		}

		addr := req.GetAddress()

		// Unbiased CBF FPR sampling: probe every CBF-enabled bank for this
		// access and record (CBF outcome, directory ground truth). Doing it
		// here (per accepted access) avoids the sampling bias of measuring
		// only at the bank selectBank ultimately routed to — that bias
		// caps measured TN at near-zero by construction.
		ds.cache.directory.SampleCBFForAllBanks(req.GetPID(), addr)

		sel := ds.selectBank(req.GetPID(), addr)
		trans.bankID = sel.bankID
		trans.bankList = sel.bankList
		trans.bfEagerInserted = sel.bfEager
		trans.rsbHintBank = sel.rsbHintBank

		if !targetPipeline[trans.bankID].CanAccept() {
			return madeProgress // 상태 미변경 — 다음 cycle에 동일 로직 재실행 가능
		}

		ds.cache.incEvent(fmt.Sprintf("GetBankCount - %d", 1+len(sel.bankList)))

		targetPipeline[trans.bankID].Accept(dirPipelineItem{trans})
		if sel.onCommit != nil {
			sel.onCommit() // ★ dispatch 성공 후에만 state mutation
		}
		targetBuf.Pop()
		madeProgress = true
	}

	return madeProgress
}

func (ds *directoryStage) acceptMotionTransaction() bool {
	madeProgress := false

	// 주의: cache 컴포넌트에 localDirStageMotionBuffer 를 추가해야 합니다.
	targetBuf := ds.cache.dirStageMotionBuffer

	for i := 0; i < ds.cache.numReqPerCycle; i++ {
		var trans *transaction
		var activeBuf sim.Buffer

		// Ack 버퍼를 최우선으로 폴링 (Ack는 Local에서만 온다고 가정)
		if ds.cache.dirStageAckBuffer.Peek() != nil {
			trans = ds.cache.dirStageAckBuffer.Peek().(*transaction)
			activeBuf = ds.cache.dirStageAckBuffer
		} else if item := targetBuf.Peek(); item != nil {
			trans = item.(*transaction)
			activeBuf = targetBuf
		} else {
			break
		}

		switch trans.action {
		case InsertPromotionEntry, InsertDemotionEntry:
			trans.fromLocal = true
			if !ds.motionPipeline[trans.bankID].CanAccept() {
				return madeProgress
			}

			ds.motionPipeline[trans.bankID].Accept(dirPipelineItem{trans})
			activeBuf.Pop()
			madeProgress = true

		default:
			fmt.Printf("[%s][ERROR] Invalid action in Motion Buffer: %d\n", ds.cache.name, trans.action)
			activeBuf.Pop() // 비정상 메시지 폐기
		}
	}

	return madeProgress
}

func (ds *directoryStage) Reset() {
	for i := range ds.localPipeline {
		ds.localPipeline[i].Clear()
		ds.remotePipeline[i].Clear()
		ds.motionPipeline[i].Clear()
	}

	for i := range ds.localBuf {
		ds.localBuf[i].Clear()
		ds.remoteBuf[i].Clear()
		ds.motionBuf[i].Clear()
	}

	ds.cache.localDirStageBuffer.Clear()
	ds.cache.remoteDirStageBuffer.Clear()

	// [COMPLETION-VC] drop deferred remote evictions on reset.
	ds.remoteDeferredEvict = nil

	if ds.cache.dirStageMotionBuffer != nil {
		ds.cache.dirStageMotionBuffer.Clear()
	}
}

func (ds *directoryStage) processTransaction(isLocal bool) bool {
	madeProgress := false
	buf := ds.localBuf
	ds.returnFalse = &ds.returnFalse0
	if !isLocal {
		// [SD-REC PARITY Fix2 = REC ITER20 COMPLETION-VC] Remote plane uses
		// the skip-head variant so a request-path-coupled eviction at a
		// bank's FIFO head cannot HoL-block a completion-bearing HIT behind
		// it. Local plane keeps the original strict-FIFO semantics (no
		// cross-GPU completion cycle on the local plane).
		return ds.processRemoteTransaction()
	}
	*ds.returnFalse = ""

	for bankID := range buf {
		for i := 0; i < ds.cache.numReqPerCycle; i++ {
			*ds.returnFalse += "."
			item := buf[bankID].Peek()
			if item == nil {
				break
			}

			trans := item.(dirPipelineItem).trans

			addr := trans.accessReq().GetAddress()
			cacheLineID, _ := getCacheLineID(addr, ds.cache.log2BlockSize)

			if _, evicting := ds.cache.evictingList[cacheLineID]; evicting {
				ds.cache.stallEvictingList++
				break
			}

			if ds.doWrite(trans, isLocal) {
				buf[bankID].Pop()
				madeProgress = true
			} else {
				break // 처리가 막히면 해당 Bank의 루프 중단
			}
		}
	}

	if madeProgress {
		*ds.returnFalse = ""
	}
	return madeProgress
}

// coarsestRegionID returns the address truncated to the COARSEST region
// span any bank tracks (max regionLen + sub-entry span). Used as the
// address-disjointness key for the COMPLETION-VC skip: two addresses with
// different coarsest-region IDs can never touch the same directory entry
// in ANY bank (including after a bankList redirect), so skipping past a
// deferred item with a different ID is provably order-preserving.
func (ds *directoryStage) coarsestRegionID(addr uint64) uint64 {
	maxRegionLen := 0
	for _, rl := range ds.cache.regionLen {
		if rl > maxRegionLen {
			maxRegionLen = rl
		}
	}
	span := uint64(maxRegionLen + ds.cache.log2NumSubEntry)
	id, _ := getCacheLineID(addr, span)
	return id
}

// deferredHasRegion reports whether any currently-deferred remote trans
// targets the same coarsest region as addr. We must NOT skip a buffer item
// past a deferred item that shares its region, otherwise a later
// same-region request could be reordered ahead of an earlier eviction of
// that region and observe stale directory state.
func (ds *directoryStage) deferredHasRegion(addr uint64) bool {
	region := ds.coarsestRegionID(addr)
	for _, t := range ds.remoteDeferredEvict {
		if ds.coarsestRegionID(t.accessReq().GetAddress()) == region {
			return true
		}
	}
	return false
}

// remoteDeferredCap bounds the side queue at the combined capacity of the
// per-bank remote post-pipeline buffers (cap-neutrality: the queue can
// never hold more than the buffers it drains from could hold).
func (ds *directoryStage) remoteDeferredCap() int {
	cap := 0
	for _, b := range ds.remoteBuf {
		cap += b.Capacity()
	}
	return cap
}

// isCompletionBearingHit is a SIDE-EFFECT-FREE classification of a remote
// (peer-incoming) transaction, mirroring the read-only prefix of doWrite/
// doWriteHit for trans.bankID. It returns true when doWrite would resolve
// to the read-hit (already-sharer) or write-hit-full-permission fast-path —
// the cases that push to remoteBottomSenderBufferData and ultimately
// produce the cross-GPU DataReadyRsp/WriteDoneRsp. Such a transaction
// stalls only on its own forward buffer, so skipping it cannot help and
// the caller keeps strict ordering for it. Everything else (miss,
// eviction, inv-class write-hit, UpdateEntry via writeToBank, bankList
// redirect, MSHR conflicts) is REQUEST-path-coupled and is what we defer.
// No directory state, MSHR, or buffer is modified here.
func (ds *directoryStage) isCompletionBearingHit(trans *transaction) bool {
	bankID := trans.bankID
	regionLen := ds.cache.regionLen[bankID]
	req := trans.accessReq()
	cachelineID, _ := getCacheLineID(req.GetAddress(), uint64(regionLen))

	// Mirror doWrite's cross-granularity MSHR check (read-only).
	var mshrEntry *internal.MSHREntry
	for _, e := range ds.cache.mshr.QueryWithMask(
		req.GetPID(), req.GetAddress(), uint64(regionLen)) {
		if e.RegionLen < uint64(regionLen) {
			return false // stalls on a finer in-flight entry: request-path
		}
		if mshrEntry == nil {
			mshrEntry = e
		}
	}
	if mshrEntry != nil {
		// Read MSHR-hit coalesces and never stalls; a write on an MSHR hit
		// stalls without producing a completion.
		return trans.write == nil
	}

	block, index := ds.cache.directory.Lookup(bankID, req.GetPID(), cachelineID)
	if block == nil || !block.SubEntry[index].IsValid {
		return false // miss or redirect path: request-path coupled
	}

	subEntry := block.SubEntry[index]
	if subEntry.IsLocked || subEntry.ReadCount > 0 {
		return false
	}

	if trans.isReadTrans() && !trans.read.FetchForWriteMiss {
		// Only the already-sharer branch fast-paths to BSBData; the
		// new-sharer (UpdateEntry) branch goes through writeToBank.
		return ds.readPermission(trans, subEntry.Sharer)
	}

	// Write-hit: only the full-permission branch (action Nothing) is a
	// completion-bearing fast-path; otherwise it falls through to
	// writeToBank (inv emission) -> defer-eligible.
	return ds.writePermission(trans, subEntry.Sharer)
}

// processRemoteTransaction is the skip-head variant of processTransaction
// for the remote (peer-incoming) plane. Each Tick it first retries the
// oldest deferred items in order, then scans each bank's remoteBuf; a
// request-path-coupled head that stalls is moved into remoteDeferredEvict
// so a completion-bearing HIT behind it can be reached. See
// remoteDeferredEvict for the deadlock rationale and invariants.
func (ds *directoryStage) processRemoteTransaction() bool {
	madeProgress := false
	buf := ds.remoteBuf
	ds.returnFalse = &ds.returnFalse1
	*ds.returnFalse = ""

	// 1. Retry deferred items first (oldest first, in order). Stop at the
	//    first still-stalled item to preserve same-region order among
	//    deferred items.
	for len(ds.remoteDeferredEvict) > 0 {
		trans := ds.remoteDeferredEvict[0]
		addr := trans.accessReq().GetAddress()
		cacheLineID, _ := getCacheLineID(addr, ds.cache.log2BlockSize)
		if _, evicting := ds.cache.evictingList[cacheLineID]; evicting {
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

	// 2. Scan each bank's remoteBuf with skip-head over request-path-
	//    coupled stalls.
	for bankID := range buf {
		for i := 0; i < ds.cache.numReqPerCycle; i++ {
			*ds.returnFalse += "."
			item := buf[bankID].Peek()
			if item == nil {
				break
			}

			trans := item.(dirPipelineItem).trans
			addr := trans.accessReq().GetAddress()
			cacheLineID, _ := getCacheLineID(addr, ds.cache.log2BlockSize)

			if _, evicting := ds.cache.evictingList[cacheLineID]; evicting {
				ds.cache.stallEvictingList++
				break
			}

			if ds.doWrite(trans, false) {
				buf[bankID].Pop()
				madeProgress = true
				continue
			}

			// Head stalled. A completion-bearing HIT stalls only on its own
			// forward buffer (skipping cannot help) -> keep strict order.
			// A request-path-coupled item is deferred so a HIT behind it
			// can run, but ONLY if address-disjoint from every deferred
			// item and within the cap-neutral headroom.
			if ds.isCompletionBearingHit(trans) {
				break
			}
			if ds.deferredHasRegion(addr) {
				break
			}
			if len(ds.remoteDeferredEvict) >= ds.remoteDeferredCap() {
				break
			}

			buf[bankID].Pop()
			ds.remoteDeferredEvict = append(ds.remoteDeferredEvict, trans)
			ds.cache.incEvent("RemoteHeadDeferred")
			madeProgress = true
			// continue scanning: the next head may be a completion-bearing HIT.
		}
	}

	if madeProgress {
		*ds.returnFalse = ""
	}
	return madeProgress
}

func (ds *directoryStage) processMotionTransaction() bool {
	madeProgress := false
	buf := ds.motionBuf

	for bankID := range buf {
		for i := 0; i < ds.cache.numReqPerCycle; i++ {
			item := buf[bankID].Peek()
			if item == nil {
				break
			}

			trans := item.(dirPipelineItem).trans

			if trans.action == InsertPromotionEntry {
				if ds.doPromotion(trans, true) {
					buf[bankID].Pop()
					madeProgress = true
				} else {
					break
				}
			} else if trans.action == InsertDemotionEntry {
				if ds.doDemotion(trans, true) {
					buf[bankID].Pop()
					madeProgress = true
				} else {
					break
				}
			}
		}
	}
	return madeProgress
}

func (ds *directoryStage) doPromotion(trans *transaction, isLocal bool) bool {
	*ds.returnFalse += "[doPromotion] "
	targetBankID := trans.bankID // 승격될 타겟 상위 bank (예: n-1)
	targetBuffer := ds.cache.localDirToBankBuffers
	if !isLocal {
		targetBuffer = ds.cache.remoteDirToBankBuffers
	}

	// 타겟 상위 뱅크 버퍼에 여유가 없으면 대기
	if !targetBuffer[targetBankID].CanPush() {
		*ds.returnFalse += "Cannot push to target dir to bank buffer"
		return false
	}

	regionLen := uint64(ds.cache.regionLen[targetBankID])
	maskLen := regionLen + uint64(ds.cache.log2NumSubEntry)
	addr := trans.fetchingAddr
	pid := trans.fetchingPID
	cachelineID, _ := getCacheLineID(addr, regionLen)

	// Target Block 찾기 및 할당 (이전 로직과 동일)
	targetBlock, index := ds.cache.directory.Lookup(targetBankID, pid, cachelineID)

	var alloc bool
	if targetBlock == nil {
		targetBlock, alloc = ds.cache.directory.FindVictim(targetBankID, pid, cachelineID)
		if alloc && (targetBlock.IsLockedEntry() || targetBlock.GetReadCount() > 0) {
			*ds.returnFalse += "Target block is used"
			return false
		}

		if alloc && ds.needEviction(targetBlock) {
			trans.action = EvictAndPromotionEntry
			trans.victim = *targetBlock.DeepCopy()
		}

		if alloc {
			evictAddr := targetBlock.Tag
			diff := uint64(1 << ds.cache.regionLen[targetBankID])
			for _, e := range targetBlock.SubEntry {
				if e.IsValid {
					ds.cache.directory.EvictBloomfilter(targetBankID, evictAddr)
				}
				// Same intent as the doWriteMiss eviction-recording site:
				// promotion-driven evictions of non-finest entries should
				// also feed RSB so the address keeps its preferred bank.
				if ds.cache.recordSilentEvict && targetBankID != ds.cache.numBanks-1 {
					ds.cache.regionSizeBuffer.Update(evictAddr, targetBankID)
				}
				evictAddr += diff
			}

			targetBlock.Reset()
		}

		index = int((cachelineID >> regionLen) % uint64(1<<ds.cache.log2NumSubEntry))
	}

	// Target Block 상태 일괄 업데이트
	targetBlock.PID = pid
	targetBlock.Tag = cachelineID >> maskLen << maskLen
	targetBlock.IsValid = true
	// H3e fix: a freshly promoted entry is not a demote-product and must
	// not carry an inherited demote lock. Reset() handles the alloc path;
	// this line covers the Lookup-hit path where Reset() is not called.
	targetBlock.DemoteLocked = false

	targetBlock.SubEntry[index].IsValid = true
	targetBlock.SubEntry[index].IsLocked = true
	copiedSh := make([]sim.RemotePort, len(trans.sharers))
	copy(copiedSh, trans.sharers)
	targetBlock.SubEntry[index].Sharer = copiedSh

	trans.block = targetBlock
	trans.blockIdx = index

	// §1 SO assertion (sub-entry granular). Active only when
	// SD_ASSERT_SINGLE_OWNER=1. Probe every other bank's directory entry
	// covering this sub-region; panic if another bank already owns a
	// valid sub-entry for the same byte range.
	if os.Getenv("SD_ASSERT_SINGLE_OWNER") == "1" {
		for bid := 0; bid < ds.cache.numBanks; bid++ {
			if bid == targetBankID {
				continue
			}
			otherRegionLen := uint64(ds.cache.regionLen[bid])
			otherCID, _ := getCacheLineID(addr, otherRegionLen)
			otherEntry, _ := ds.cache.directory.Lookup(bid, pid, otherCID)
			if otherEntry == nil || !otherEntry.IsValidEntry() {
				continue
			}
			subIdxOther := int((addr >> otherRegionLen) % uint64(1<<ds.cache.log2NumSubEntry))
			if !otherEntry.SubEntry[subIdxOther].IsValid {
				continue
			}
			panic(fmt.Sprintf(
				"[%s] SO violation (doPromotion): addr=%#x target bank=%d subIdx=%d clashes with bank=%d subIdx=%d (tag=%#x)",
				ds.cache.name, addr, targetBankID, index, bid, subIdxOther, otherEntry.Tag))
		}
	}

	ds.cache.directory.InsertBloomfilter(targetBankID, cachelineID)
	// (E-3) Data is now present at targetBankID — drop any RSB hint pointing
	// to a different bank for this addr to preserve the "RSB only for absent
	// addrs" invariant. Without this, a stale hint can route a future access
	// past this bank and allocate a parallel entry.
	ds.cache.regionSizeBuffer.InvalidateAddr(addr)

	// §1 SO-invariant fix: sweep only the sub-region that this promotion
	// actually fills, leaving other sub-regions' finer-bank entries alone.
	ds.sweepFinerBankEntriesInSubRegion(targetBankID, targetBlock, index, pid)

	targetBuffer[targetBankID].Push(trans)

	return true
}

// sweepFinerBankEntriesInSubRegion migrates sharer info from every finer-
// bank directory entry whose region overlaps a specific sub-region (a
// single sub-entry of targetBlock) into targetBlock.SubEntry[subIdx], then
// invalidates the finer entry. Without this migration the "at most one
// bank owns each coarse region" invariant is restored only at the
// directory layer; the underlying GPU L2s still hold copies that the
// directory has forgotten, breaking cross-GPU coherence on subsequent
// writes.
//
// §1 SO-invariant fix: the iteration range is restricted to a single
// sub-entry (sub-region) instead of the entire coarse region. The
// previous all-region sweep absorbed finer-bank entries belonging to
// sub-regions the current promotion does not actually fill, conflicting
// with promotions in flight at those sub-regions.
func (ds *directoryStage) sweepFinerBankEntriesInSubRegion(
	targetBankID int, targetBlock *internal.CohEntry, subIdx int, pid vm.PID,
) {
	coarseRegionStart := targetBlock.Tag
	coarseRegionLen := uint64(ds.cache.regionLen[targetBankID])
	subRegionStart := coarseRegionStart + uint64(subIdx)*(uint64(1)<<coarseRegionLen)
	subRegionSize := uint64(1) << coarseRegionLen
	for finerBankID := targetBankID + 1; finerBankID < ds.cache.numBanks; finerBankID++ {
		finerRegionLen := ds.cache.regionLen[finerBankID]
		finerMaskLen := uint64(finerRegionLen) + uint64(ds.cache.log2NumSubEntry)
		finerEntrySize := uint64(1) << finerMaskLen
		// Iterate the finer-bank cache sets whose entries can overlap
		// [subRegionStart, subRegionStart+subRegionSize). When the finer
		// entry size exceeds the sub-region (only possible if regionLen
		// ordering changes; defensive), probe at least once.
		probeStep := finerEntrySize
		if probeStep == 0 {
			probeStep = subRegionSize
		}
		for probeAddr := subRegionStart; ; probeAddr += probeStep {
			set, _ := ds.cache.directory.GetSet(finerBankID, probeAddr)
			for _, finerEntry := range set.CohEntries {
				if !finerEntry.IsValid || finerEntry.PID != pid {
					continue
				}
				finerStart := (finerEntry.Tag >> finerMaskLen) << finerMaskLen
				if finerStart+finerEntrySize <= subRegionStart || finerStart >= subRegionStart+subRegionSize {
					continue
				}
				for i := range finerEntry.SubEntry {
					if !finerEntry.SubEntry[i].IsValid {
						continue
					}
					finerSubAddr := finerStart + uint64(i)*(1<<uint64(finerRegionLen))
					if finerSubAddr < subRegionStart || finerSubAddr >= subRegionStart+subRegionSize {
						continue
					}
					coarseSub := &targetBlock.SubEntry[subIdx]
					wasValid := coarseSub.IsValid
					for _, sh := range finerEntry.SubEntry[i].Sharer {
						present := false
						for _, existing := range coarseSub.Sharer {
							if existing == sh {
								present = true
								break
							}
						}
						if !present {
							coarseSub.Sharer = append(coarseSub.Sharer, sh)
						}
					}
					if finerEntry.SubEntry[i].IsDirty {
						coarseSub.IsDirty = true
					}
					coarseSub.IsValid = true
					if !wasValid {
						ds.cache.directory.InsertBloomfilter(targetBankID, subRegionStart)
					}
					ds.cache.directory.EvictBloomfilter(finerBankID, finerSubAddr)
					finerEntry.SubEntry[i].IsValid = false
					finerEntry.SubEntry[i].Sharer = nil
				}
				// finerEntry may still have other valid sub-entries
				// belonging to different sub-regions; only invalidate
				// the entry shell when no valid sub-entries remain.
				stillValid := false
				for _, se := range finerEntry.SubEntry {
					if se.IsValid {
						stillValid = true
						break
					}
				}
				if !stillValid {
					finerEntry.IsValid = false
					finerEntry.DemoteLocked = false
				}
			}
			// Single iteration only (probe once per finer bank for the
			// sub-region — finer entries that straddle the sub-region
			// boundary are handled by GetSet's set-indexing).
			if probeAddr+probeStep >= subRegionStart+subRegionSize {
				break
			}
		}
	}
}

func (ds *directoryStage) doDemotion(trans *transaction, isLocal bool) bool {
	*ds.returnFalse += "[doDemotion] "
	bankID := trans.bankID
	targetBuffer := ds.cache.localDirToBankBuffers
	if !isLocal {
		targetBuffer = ds.cache.remoteDirToBankBuffers
	}

	// 1. Early Exit: 버퍼 확인
	if !targetBuffer[bankID].CanPush() {
		*ds.returnFalse += "Cannot push to target dir to bank buffer"
		return false
	}

	if ds.cache.debugProcess && trans.evictingAddr == ds.cache.debugAddress {
		fmt.Printf("[%s] [directoryStage]\tProcessing Demotion Request - 1: addr %x\n", ds.cache.name, trans.evictingAddr)
	}

	regionLen := ds.cache.regionLen[bankID]
	maskLen := regionLen + ds.cache.log2NumSubEntry
	addr := trans.block.Tag
	pid := trans.block.PID
	cachelineID, _ := getCacheLineID(addr, uint64(regionLen))

	// 2. Target Block 찾기
	targetBlock, _ := ds.cache.directory.Lookup(bankID, pid, cachelineID)

	if targetBlock != nil { // Hit — invariant violation; dump state then panic
		// === DIAGNOSTIC DUMP for invariant-violation race investigation ===
		fmt.Fprintf(os.Stderr, "\n==== [%s] INVARIANT VIOLATION DIAGNOSTIC DUMP ====\n", ds.cache.name)
		if ds.cache.TickingComponent != nil && ds.cache.Engine != nil {
			fmt.Fprintf(os.Stderr, "Time: %v\n", ds.cache.Engine.CurrentTime())
		}
		fmt.Fprintf(os.Stderr, "doDemotion target bank=%d cachelineID=%#x (regionLen=%d, maskLen=%d)\n",
			bankID, cachelineID, regionLen, maskLen)
		fmt.Fprintf(os.Stderr, "  trans.id=%s action=%d isLocal=%v fromLocal=%v\n",
			trans.id, trans.action, isLocal, trans.fromLocal)
		fmt.Fprintf(os.Stderr, "  trans.block (source): Tag=%#x PID=%v IsValid=%v DemoteLocked=%v",
			trans.block.Tag, trans.block.PID, trans.block.IsValid, trans.block.DemoteLocked)
		if len(trans.block.SubEntry) > 0 {
			fmt.Fprintf(os.Stderr, " SubEntryValid=[")
			for i := range trans.block.SubEntry {
				if i > 0 {
					fmt.Fprintf(os.Stderr, ",")
				}
				fmt.Fprintf(os.Stderr, "%v", trans.block.SubEntry[i].IsValid)
			}
			fmt.Fprintf(os.Stderr, "]")
		}
		fmt.Fprintf(os.Stderr, "\n")
		fmt.Fprintf(os.Stderr, "  targetBlock (existing at bank %d): Tag=%#x PID=%v IsValid=%v DemoteLocked=%v",
			bankID, targetBlock.Tag, targetBlock.PID, targetBlock.IsValid, targetBlock.DemoteLocked)
		if len(targetBlock.SubEntry) > 0 {
			fmt.Fprintf(os.Stderr, " SubEntryValid=[")
			for i := range targetBlock.SubEntry {
				if i > 0 {
					fmt.Fprintf(os.Stderr, ",")
				}
				fmt.Fprintf(os.Stderr, "%v", targetBlock.SubEntry[i].IsValid)
			}
			fmt.Fprintf(os.Stderr, "]")
		}
		fmt.Fprintf(os.Stderr, "\n")
		if trans.mshrEntry != nil {
			fmt.Fprintf(os.Stderr, "  trans.mshrEntry: addr=%#x PID=%v RegionLen=%d RegionID=%d IsAccessReq=%v IsPromotion=%v IsDemotion=%v\n",
				trans.mshrEntry.Address, trans.mshrEntry.PID,
				trans.mshrEntry.RegionLen, trans.mshrEntry.RegionID,
				trans.mshrEntry.IsAccessReq, trans.mshrEntry.IsPromotion, trans.mshrEntry.IsDemotion)
		}
		// MSHR full dump — all entries with PID/addr matching the demotion addr
		// region at any granularity (coarsest mask).
		fmt.Fprintf(os.Stderr, "  MSHR entries overlapping addr %#x at coarsest mask=%d:\n",
			cachelineID, ds.cache.regionLen[0])
		overlaps := ds.cache.mshr.QueryWithMask(pid, cachelineID, uint64(ds.cache.regionLen[0]))
		for i, e := range overlaps {
			fmt.Fprintf(os.Stderr, "    [%d] addr=%#x PID=%v RegionLen=%d RegionID=%d IsAccessReq=%v IsPromotion=%v IsDemotion=%v\n",
				i, e.Address, e.PID, e.RegionLen, e.RegionID,
				e.IsAccessReq, e.IsPromotion, e.IsDemotion)
		}
		// Also list ALL MSHR entries for context
		allEntries := ds.cache.mshr.AllEntries()
		fmt.Fprintf(os.Stderr, "  MSHR total entries: %d\n", len(allEntries))
		for i, e := range allEntries {
			fmt.Fprintf(os.Stderr, "    all[%d] addr=%#x PID=%v RegionLen=%d RegionID=%d IsAccessReq=%v IsPromotion=%v IsDemotion=%v\n",
				i, e.Address, e.PID, e.RegionLen, e.RegionID,
				e.IsAccessReq, e.IsPromotion, e.IsDemotion)
		}
		// RSB entries that overlap addr (at their own granularities)
		rsbEntries := *ds.cache.regionSizeBuffer.GetEntries()
		fmt.Fprintf(os.Stderr, "  RSB entries (%d total):\n", len(rsbEntries))
		for i, r := range rsbEntries {
			fmt.Fprintf(os.Stderr, "    rsb[%d] pageAddr=%#x RegionID=%d\n", i, r.Addr, r.RegionID)
		}
		// evictingList
		fmt.Fprintf(os.Stderr, "  evictingList size: %d\n", len(ds.cache.evictingList))
		if v, ok := ds.cache.evictingList[trans.block.Tag]; ok {
			fmt.Fprintf(os.Stderr, "    evictingList[%#x] = %v (source addr is in evictingList!)\n",
				trans.block.Tag, v)
		}
		// Cache set contents at every bank for this address — reveals every
		// way and its valid state, exposing parallel allocations that
		// shouldn't coexist.
		fmt.Fprintf(os.Stderr, "  ----- per-bank cache set contents at addr %#x -----\n", cachelineID)
		for b := 0; b < ds.cache.numBanks; b++ {
			rl := ds.cache.regionLen[b]
			mask := rl + ds.cache.log2NumSubEntry
			set, setID := ds.cache.directory.GetSet(b, cachelineID)
			fmt.Fprintf(os.Stderr, "    bank=%d setID=%d regionLen=%d maskLen=%d ways=%d\n",
				b, setID, rl, mask, len(set.CohEntries))
			for w, e := range set.CohEntries {
				validMask := ""
				for i := range e.SubEntry {
					if e.SubEntry[i].IsValid {
						validMask += "T"
					} else {
						validMask += "F"
					}
				}
				fmt.Fprintf(os.Stderr, "      way=%d Tag=%#x PID=%v IsValid=%v DemoteLocked=%v SubValid=[%s]\n",
					w, e.Tag, e.PID, e.IsValid, e.DemoteLocked, validMask)
			}
		}
		fmt.Fprintf(os.Stderr, "==== END DIAGNOSTIC DUMP ====\n\n")

		panic(fmt.Sprintf(
			"[%s] invariant violation: demotion target already present at bank %d, addr %x",
			ds.cache.name, bankID, cachelineID))
	} else { // Miss
		var alloc bool
		targetBlock, alloc = ds.cache.directory.FindVictim(bankID, pid, cachelineID)
		if alloc && (targetBlock.IsLockedEntry() || targetBlock.GetReadCount() > 0) {
			*ds.returnFalse += "Target block is being used"
			return false
		}

		if alloc && ds.needEviction(targetBlock) {
			trans.action = EvictAndDemotionEntry
			trans.victim = *targetBlock.DeepCopy()
		}

		if alloc {
			evictAddr := targetBlock.Tag
			diff := uint64(1 << ds.cache.regionLen[bankID]) // doDemotion이면 bankID 사용
			for _, e := range targetBlock.SubEntry {
				if e.IsValid {
					ds.cache.directory.EvictBloomfilter(bankID, evictAddr)
				}
				// Same intent as the doWriteMiss/doPromotion eviction-recording:
				// demotion-target replacements at non-finest banks should also
				// feed RSB. Skip when bankID is finest (RSB hint = default).
				if ds.cache.recordSilentEvict && bankID != ds.cache.numBanks-1 {
					ds.cache.regionSizeBuffer.Update(evictAddr, bankID)
				}
				evictAddr += diff
			}

			targetBlock.Reset()
		}
	}

	// 3. Target Block 상태 일괄 업데이트
	targetBlock.PID = pid
	targetBlock.Tag = cachelineID >> maskLen << maskLen
	targetBlock.IsValid = true
	// H3e fix: this entry was created by a demote → mark it so a subsequent
	// demote trigger converts to invalidate-only (cascade prevention).
	targetBlock.DemoteLocked = true

	for i, e := range trans.block.SubEntry { // mshrStage에서 생성한 block을 저장하는 과정
		if e.IsValid {
			targetBlock.SubEntry[i] = trans.block.SubEntry[i]
			targetBlock.SubEntry[i].IsLocked = true
		} else {
			targetBlock.SubEntry[i].IsValid = false
		}
		// mshrstage.insertDemotionEntry 가 4개 sub-entry 를 모두 IsValid=true 로
		// 만들어 주므로 무조건 Insert. (trigger sub-region 만 fromLocal=true 일 때
		// sharer 가 빈 상태이지만 IsValid 자체는 true 이고, BF Insert/Evict 는
		// sharer 가 아닌 IsValid 기준으로 균형이 맞는다.)
		ds.cache.directory.InsertBloomfilter(bankID, cachelineID)
		cachelineID += 1 << regionLen
	}

	trans.block = targetBlock
	// (E-3) Data is now present at bankID — drop any stale RSB hint that
	// would mis-route a future access into a parallel-entry allocation.
	ds.cache.regionSizeBuffer.InvalidateAddr(addr)

	// 4. 전송 및 마무리
	targetBuffer[bankID].Push(trans)

	return true
}

func (ds *directoryStage) doWrite(trans *transaction, isLocal bool) bool {
	*ds.returnFalse += "[doWrite] "
	ds.cache.totalDoWriteCalls++
	trans.banksChecked++ // 이 bank를 실제로 확인함
	bankID := trans.bankID
	regionLen := ds.cache.regionLen[bankID]
	req := trans.accessReq()
	cachelineID, _ := getCacheLineID(req.GetAddress(), uint64(regionLen)) // 하위 영역은 0으로 지우기

	// Cross-granularity-aware MSHR check.
	// mshr.Add uses max(e.RegionLen, newRegionLen) so it detects conflicts across
	// granularities.  The old mshr.Query only used e.RegionLen, making fine-grained
	// entries (e.g. bank-4, RegionLen=6) invisible to coarser requests (bank-2,
	// RegionLen=10) that share the same 1KB region — leading to a panic in mshr.Add.
	//
	// New logic:
	//  • If a *finer* MSHR entry (e.RegionLen < regionLen) overlaps → stall until it
	//    completes; otherwise mshr.Add would panic with "entry already in mshr".
	//  • If a *coarser or same* MSHR entry overlaps → normal MSHR hit (same as before).
	var mshrEntry *internal.MSHREntry
	for _, e := range ds.cache.mshr.QueryWithMask(req.GetPID(), req.GetAddress(), uint64(regionLen)) {
		if e.RegionLen < uint64(regionLen) {
			// Finer-grained entry overlaps this coarser request → must wait for it
			*ds.returnFalse += "stall: finer MSHR entry overlaps coarser request"
			return false
		}
		if mshrEntry == nil {
			mshrEntry = e // first coarser-or-same entry becomes the MSHR hit
		}
	}
	if mshrEntry != nil {
		if trans.write != nil { // write 인 경우, MSHR hit이 발생하면 처리 x
			*ds.returnFalse += "MSHR hit for write request"
			return false
		}

		// if trans.read.FetchForWriteMiss {
		// 	*ds.returnFalse += "MSHR hit for fetch for write miss request"
		// 	return false
		// }

		ok := ds.doWriteMSHRHit(trans, mshrEntry)
		if ok {
			ds.cache.incEvent("read-mshr-hit")
			ds.cache.incEvent(fmt.Sprintf("BankChecked - %d", trans.banksChecked))

			if ds.cache.debugProcess && trans.read != nil && trans.read.Address == ds.cache.debugAddress {
				fmt.Printf("[%s] [directoryStage]\tReceived remote read req - 1: mshr hit addr %x\n", ds.cache.name, trans.read.Address)
			}
		}

		return ok
	}

	block, index := ds.cache.directory.Lookup(bankID, trans.accessReq().GetPID(), cachelineID)
	// if block != nil {
	// 	fmt.Printf("[%s]\tDirectory Lookup: Addr %x -> %x, bank %d, set %d, way %d, index %d\n",
	// 		ds.cache.name, req.GetAddress(), block.Tag, bankID, block.SetID, block.WayID, index)
	// }

	if block != nil && block.SubEntry[index].IsValid {
		ok := ds.doWriteHit(trans, block, index, isLocal)
		if ok {
			what := "read-hit"
			if trans.write != nil {
				what = "write-hit"
			}

			ds.cache.incEvent(what)
			ds.cache.incEvent(fmt.Sprintf("BankChecked - %d", trans.banksChecked))

			if ds.cache.debugProcess && trans.read != nil && trans.read.Address == ds.cache.debugAddress {
				fmt.Printf("[%s] [directoryStage]\tReceived remote read req - 2: write hit: bank %d addr %x, action %d\n", ds.cache.name, trans.bankID, trans.read.Address, trans.action)
			}
		}

		return ok
	}

	pipeline := ds.localPipeline
	if !isLocal {
		pipeline = ds.remotePipeline
	}
	if len(trans.bankList) != 0 {
		// §3 deadlock fix: aliasingMotionTarget retry-path redirect removed.
		// In-flight motion correctness is now covered by the MSHR check at
		// doWrite's entry (above), which uses regionLen[bankID] mask so any
		// finer/coarser overlap is detected via QueryWithMask's
		// max(e.RegionLen, regionLen) semantics.
		bankID := trans.bankList[0]

		if !pipeline[bankID].CanAccept() {
			// fmt.Printf("[%s]\t[WARNING] Next pipeline[%d] is full, waiting...\n", ds.cache.name, bankID)
			*ds.returnFalse += "Pipeline is full"
			return false
		}

		trans.bankID = bankID
		trans.bankList = trans.bankList[1:]
		pipeline[bankID].Accept(dirPipelineItem{trans})
		return true
	}

	ok := ds.doWriteMiss(trans, isLocal)
	if ok {
		what := "read-miss"
		if trans.write != nil {
			what = "write-miss"
		}

		ds.cache.incEvent(what)
		ds.cache.incEvent(fmt.Sprintf("BankChecked - %d", trans.banksChecked))

		if ds.cache.debugProcess && trans.read != nil && trans.read.Address == ds.cache.debugAddress {
			fmt.Printf("[%s] [directoryStage]\tReceived remote read req - 3: write miss: bank %d addr %x, action %d\n", ds.cache.name, trans.bankID, trans.read.Address, trans.action)
		}
	}

	return ok
}

func (ds *directoryStage) doWriteMSHRHit(
	trans *transaction,
	mshrEntry *internal.MSHREntry,
) bool {
	trans.mshrEntry = mshrEntry

	// Use the MSHR entry's recorded RegionLen rather than the bank's
	// configured regionLen. During in-flight promotion/demotion the
	// entry's bankID and the bank's regionLen can diverge: the entry
	// records the granularity at the time of mshr.Add (see
	// mshrstage.go:160 demotion path which Adds with regionLen[bankID]
	// but RegionID = bankID+1). selectBank's MSHR-aware redirect can
	// also place trans.bankID at a bank whose configured regionLen
	// differs from the entry's. Anchoring on mshrEntry.RegionLen keeps
	// blockIdx calculation correct regardless of which bank the trans
	// was routed to.
	regionLen := int(mshrEntry.RegionLen)
	log2NumSubEntry := ds.cache.log2NumSubEntry
	_, offset := getCacheLineID(trans.accessReq().GetAddress(), uint64(regionLen+log2NumSubEntry))
	offset = offset >> regionLen
	trans.blockIdx = int(offset)

	mshrEntry.Requests = append(mshrEntry.Requests, trans)

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
		// Read-hit fast-path. NOTE for super: only the already-sharer
		// (action=Nothing) branch can fast-path; the new-sharer
		// (UpdateEntry) branch must still go through writeToBank because
		// super's mshrStage uses the post-bank trans to evaluate
		// promotion/demotion (block.AbleToPromotion). Bypassing it
		// would silently disable super's motion logic.
		if ds.readPermission(trans, subEntry.Sharer) {
			trans.action = Nothing
			// [BSB-CLASS-SPLIT] read-hit fast-path is data-class (Nothing).
			targetBuffer := ds.cache.localBottomSenderBufferData
			if !isLocal {
				targetBuffer = ds.cache.remoteBottomSenderBufferData
			}
			if !targetBuffer.CanPush() {
				*ds.returnFalse += "Cannot push to bottom sender buffer"
				ds.cache.stallBottomBufFull++
				return false
			}
			targetBuffer.Push(trans)
			return true
		}
		trans.action = UpdateEntry
		return ds.writeToBank(trans, block, index, isLocal)
	}

	if ds.writePermission(trans, subEntry.Sharer) {
		trans.action = Nothing
	} else {
		trans.action = InvalidateAndUpdateEntry
		trans.invalidationList = make([]sim.RemotePort, len(subEntry.Sharer))
		copy(trans.invalidationList, subEntry.Sharer)
		trans.evictingAddr = block.Tag + uint64(index<<ds.cache.regionLen[trans.bankID])
		trans.evictingPID = block.PID
	}

	// [BSB-CLASS-SPLIT] only the action==Nothing branch below pushes here,
	// and Nothing is data-class -> route to the Data lane. (The
	// InvalidateAndUpdateEntry branch falls through to writeToBank and is
	// class-routed in bankstage.finalizeTrans.)
	targetBuffer := ds.cache.localBottomSenderBufferData
	if !isLocal {
		targetBuffer = ds.cache.remoteBottomSenderBufferData
	}
	if trans.action == Nothing {
		if !targetBuffer.CanPush() {
			*ds.returnFalse += "Cannot push to target bottom sender buffer"
			ds.cache.stallBottomBufFull++
			return false
		}

		targetBuffer.Push(trans)
		return true
	}

	return ds.writeToBank(trans, block, index, isLocal)
}

func (ds *directoryStage) doWriteMiss(trans *transaction, isLocal bool) bool {
	ds.cache.doWriteMissCount++
	if !trans.fromLocal {
		ds.cache.doWriteMissRemote++
	}
	*ds.returnFalse += "[doWriteMiss] "
	// [BSB-CLASS-SPLIT] the only direct push below sets action=Nothing
	// (data-class) -> Data lane. The remote-alloc path falls through to
	// writeToBank and is class-routed in bankstage.finalizeTrans.
	targetBuffer := ds.cache.localBottomSenderBufferData
	if !isLocal {
		targetBuffer = ds.cache.remoteBottomSenderBufferData
	}
	if trans.fromLocal { // local write request에 대해 directory miss 발생 시, entry 추가 안 함
		trans.action = Nothing
		if !targetBuffer.CanPush() {
			*ds.returnFalse += "Cannot push to bottom sender buffer"
			ds.cache.stallBottomBufFull++
			return false
		}
		targetBuffer.Push(trans)
		return true
	}

	if ds.cache.mshr.IsFull() {
		*ds.returnFalse += "MSHR is full"
		ds.cache.stallMSHRFull++
		return false
	}

	// Allocation bank: honor RSB hint when present (carried via trans.rsbHintBank,
	// set by selectBank). Falls back to finest bank when there was no RSB hit.
	// Without this, every write miss allocates at finest regardless of the
	// recorded "this address used to be at bank X" hint, leaving RSB feedback
	// loop effectively broken.
	bankID := ds.cache.numBanks - 1
	if ds.cache.useRsbHintAlloc && trans.rsbHintBank >= 0 {
		bankID = trans.rsbHintBank
	}
	trans.bankID = bankID
	regionLen := ds.cache.regionLen[bankID]

	pid := trans.accessReq().GetPID()
	addr := trans.accessReq().GetAddress()
	cachelineID, _ := getCacheLineID(addr, uint64(regionLen))
	index := (addr >> regionLen) % uint64(1<<ds.cache.log2NumSubEntry)

	// §1 Fix B-2: alloc-time coarser-bank reprobe. Between selectBank's
	// routing decision and this allocation point, another transaction may
	// have committed a coarser-bank entry covering this address. Probe
	// every bank coarser than the chosen allocation bank with a sub-entry
	// granular Lookup; on hit, redirect this trans into that bank's
	// pipeline instead of allocating a parallel entry.
	{
		pipeline := ds.localPipeline
		if !isLocal {
			pipeline = ds.remotePipeline
		}
		for coarserBank := 0; coarserBank < bankID; coarserBank++ {
			coarserRegionLen := uint64(ds.cache.regionLen[coarserBank])
			coarserCID, _ := getCacheLineID(addr, coarserRegionLen)
			coarserEntry, _ := ds.cache.directory.Lookup(coarserBank, pid, coarserCID)
			if coarserEntry == nil || !coarserEntry.IsValidEntry() {
				continue
			}
			subIdx := int((addr >> coarserRegionLen) % uint64(1<<ds.cache.log2NumSubEntry))
			if !coarserEntry.SubEntry[subIdx].IsValid {
				continue
			}
			if !pipeline[coarserBank].CanAccept() {
				// E-RACE-DEFER (CYCLE_FINDINGS.md §13): the SO invariant
				// requires this trans to route to coarserBank (entry
				// exists at sub-level), so we cannot fall through to
				// finest alloc. Returning false here pins the trans at
				// the head of dirStage's remoteBuf[bankID], and when
				// many concurrent useRsbHintAlloc-driven coarse allocs
				// simultaneously hit this condition across all banks,
				// every bank's processTransaction breaks → dirStage.Tick
				// returns false → no auto-reschedule → engine halt.
				//
				// Fix: park the trans on a deferredRedirects queue and
				// return true (consume head of remoteBuf), breaking the
				// head-of-line. retryDeferredRedirects() drains the
				// queue each Tick whenever the targeted pipeline has
				// room.
				ds.cache.deferredRedirects = append(
					ds.cache.deferredRedirects,
					deferredRedirectEntry{
						trans:      trans,
						targetBank: coarserBank,
						isLocal:    isLocal,
					})
				ds.cache.deferredRedirectCount++
				*ds.returnFalse += "doWriteMiss coarser-bank redirect: pipeline full -> deferred"
				return true
			}
			trans.bankID = coarserBank
			trans.bankList = nil
			pipeline[coarserBank].Accept(dirPipelineItem{trans})
			return true
		}
	}

	// §4 victim selection switch. v2 finest-bank path uses the lenient
	// PromoteOnEvictEligible predicate. v3 non-finest non-coarsest path
	// uses the stricter AbleToPromotionRelaxed predicate. The default
	// remains the regular LRU victim finder.
	var victim *internal.CohEntry
	var alloc bool
	switch {
	case ds.cache.promoteAtEvict && ds.cache.promoteAtEvictBiasVictim && bankID == ds.cache.numBanks-1:
		victim, alloc = ds.cache.directory.FindVictimPreferPromotable(bankID, pid, cachelineID)
	case ds.cache.promoteAtEvict && ds.cache.promoteAtEvictMultiBank && bankID > 0 && bankID < ds.cache.numBanks-1:
		victim, alloc = ds.cache.directory.FindVictimPreferStrictlyPromotable(bankID, pid, cachelineID)
	default:
		victim, alloc = ds.cache.directory.FindVictim(bankID, pid, cachelineID)
	}

	// (C-4) Stale-parent guard: at a non-finest bank, FindVictim's Loop 1 may
	// return a parent whose target sub-entry is invalid (typically because a
	// recent demote moved that sub-region's data to a finer bank). Proceeding
	// with UpdateEntry would revive the sub-entry at this bank, creating a
	// second copy that conflicts with the demote-product at the finer bank
	// (eventually surfacing as a "Hit about demotion" warning). Detect and
	// re-route to the finest bank for a fresh allocation.
	if !alloc && bankID != ds.cache.numBanks-1 && !victim.SubEntry[int(index)].IsValid {
		bankID = ds.cache.numBanks - 1
		trans.bankID = bankID
		regionLen = ds.cache.regionLen[bankID]
		cachelineID, _ = getCacheLineID(addr, uint64(regionLen))
		index = (addr >> regionLen) % uint64(1<<ds.cache.log2NumSubEntry)
		victim, alloc = ds.cache.directory.FindVictim(bankID, pid, cachelineID)
	}

	if !alloc {
		trans.action = UpdateEntry
		return ds.writeToBank(trans, victim, int(index), isLocal)
	}

	if victim.IsLockedEntry() || victim.GetReadCount() > 0 {
		*ds.returnFalse += "Victim is being used"
		ds.cache.stallVictimLocked++
		return false
	}

	// §4.4 Promote-at-evict — Step A: eligibility.
	eligibleForPromoteAtEvict := false
	if ds.cache.promoteAtEvict && bankID != 0 && ds.needEviction(victim) {
		if bankID == ds.cache.numBanks-1 {
			// Finest bank: lenient predicate (v1 / v2).
			eligibleForPromoteAtEvict = victim.PromoteOnEvictEligible()
		} else if ds.cache.promoteAtEvictMultiBank {
			// Non-finest, non-coarsest: strict predicate (v3).
			eligibleForPromoteAtEvict = victim.AbleToPromotionRelaxed()
		}
	}

	promotedAtEvict := false
	if eligibleForPromoteAtEvict {
		coarseBank := bankID - 1
		coarseRegionLen := ds.cache.regionLen[coarseBank]
		coarseMaskLen := uint64(coarseRegionLen) + uint64(ds.cache.log2NumSubEntry)
		coarseStart := (victim.Tag >> coarseMaskLen) << coarseMaskLen
		coarseEnd := coarseStart + (uint64(1) << coarseMaskLen)

		// Step B: coarse-region alias check. If the current access's addr
		// falls inside the coarse region we are about to promote into,
		// allocating the new finer-bank entry would coexist with the
		// coarse-bank promoted entry — a single-owner violation. Skip
		// promotion in that case.
		if addr < coarseStart || addr >= coarseEnd {
			coarseCachelineID, _ := getCacheLineID(victim.Tag, uint64(coarseRegionLen))

			// Step C: resource & MSHR conflict guards. All three must hold:
			//   - motion buffer has room,
			//   - MSHR has room (Add below mustn't push it over),
			//   - no in-flight MSHR entry overlaps the coarse region
			//     (MSHR.Add would otherwise panic with "entry already in mshr").
			if ds.cache.dirStageMotionBuffer.CanPush() &&
				!ds.cache.mshr.IsFull() &&
				len(ds.cache.mshr.QueryWithMask(victim.PID, coarseCachelineID, uint64(coarseRegionLen))) == 0 {

				// Step D: sharer capture. Empty union means the entry has
				// no live remote consumers — promoting it just shifts an
				// inert entry up, no coherence value.
				captured := victim.SharerUnion()
				if len(captured) > 0 {
					// Step E: register promotion MSHR. IsPromotion=true is
					// the marker used by downstream stages (doWrite's MSHR
					// check returns this entry as a fast-path MSHR hit for
					// reads that arrive during the motion window).
					promMshr := ds.cache.mshr.Add(victim.PID, coarseCachelineID,
						uint64(coarseRegionLen), coarseBank)
					promMshr.IsPromotion = true

					// Step F: synthesize the promotion trans. DeepCopy
					// preserves trans.read / trans.write (so
					// bottomSender.sendInvalidationRequest's
					// accessReq() dereference does not panic if a later
					// stage ever runs it), then we overwrite the fields
					// that belong to the promotion's context.
					promTrans := trans.DeepCopy()
					promTrans.action = InsertPromotionEntry
					promTrans.bankID = coarseBank
					promTrans.fromLocal = true
					promTrans.sharers = captured
					promTrans.fetchingAddr = victim.Tag
					promTrans.fetchingPID = victim.PID
					promTrans.mshrEntry = promMshr
					promTrans.blockIdx = int((victim.Tag >> uint(coarseRegionLen)) %
						uint64(1<<ds.cache.log2NumSubEntry))
					promTrans.rsbHintBank = -1
					// Inherited fields that must not carry over into the
					// synthesized promotion context.
					promTrans.reqToBottom = nil
					promTrans.capturedPromoteSharers = nil
					promTrans.invalidationList = nil
					promTrans.pendingEviction = nil
					promTrans.entryProcessingPromotion = false
					promTrans.bfEagerInserted = false
					promTrans.bankList = nil
					promTrans.banksChecked = 0

					// Step G: enqueue into the motion buffer. doPromotion
					// (via acceptMotionTransaction → processMotionTransaction)
					// picks it up on a subsequent tick and runs the canonical
					// promotion path including sweepFinerBankEntriesInSubRegion.
					ds.cache.dirStageMotionBuffer.Push(promTrans)
					ds.cache.promoteAtEvictCount++
					promotedAtEvict = true

					// Step H: event log emit (optional). validSubs counts
					// the source entry's currently-valid sub-entries so
					// downstream analysis can distinguish full-population
					// from PromoteOnEvictEligible's lenient (≥2) case.
					if ds.cache.eventLogger != nil && ds.cache.eventLogger.IsEnabled() {
						validSubs := 0
						for _, se := range victim.SubEntry {
							if se.IsValid {
								validSubs++
							}
						}
						ds.cache.eventLogger.logPromotion(victim.Tag, bankID, coarseBank, len(captured), validSubs)
					}
				}
			}
		}
	}

	// Step I: action decision.
	//
	// When promote-at-evict fires, sharer state has been captured into the
	// promotion trans and will be installed at the coarse bank. The L2
	// caches still hold valid copies of those lines — invalidating them
	// here (via EvictAndInsertNewEntry) would silently drop coherent
	// shared copies. Use InsertNewEntry instead so the access path does
	// not emit invalidations for the displaced victim.
	if !promotedAtEvict && ds.needEviction(victim) {
		trans.action = EvictAndInsertNewEntry
		trans.victim = *victim.DeepCopy()
		trans.evictingAddr = victim.Tag
		trans.evictingPID = victim.PID
	} else {
		trans.action = InsertNewEntry
	}

	// Pre-flight check: ensure bank buffer has space BEFORE modifying BF/directory state.
	// Without this check, BF decrements on the victim happen even if writeToBank later
	// fails (buffer full), leaving the BF with a false negative.  That false negative
	// causes future requests to miss the coarser bank and fall through to a finer bank,
	// creating MSHR entries at two different granularities for the same address region,
	// which triggers the "entry already in mshr" panic in mshr.Add.
	{
		bankBuf := ds.cache.localDirToBankBuffers[bankID]
		if !isLocal {
			bankBuf = ds.cache.remoteDirToBankBuffers[bankID]
		}
		if !bankBuf.CanPush() {
			*ds.returnFalse += "pre-check: bank buffer full before victim eviction"
			ds.cache.stallBankFull++
			return false
		}
	}

	// victim은 bloomfilter 감소, 추가되는 entry는 증가
	if victim.IsValidEntry() {
		addr := victim.Tag
		diff := uint64(1 << regionLen)
		for i, e := range victim.SubEntry {
			if e.IsValid {
				ds.cache.directory.EvictBloomfilter(bankID, addr)
				victim.SubEntry[i].IsValid = false
			}

			// Record victim's bank into RSB on every silent eviction (no
			// invalidation responses required) so a future access to this
			// address can be routed back to the right bank instead of
			// defaulting to finest. Skip when bankID is finest — the RSB
			// hint would be identical to the default fallback (useless).
			if ds.cache.recordSilentEvict && bankID != ds.cache.numBanks-1 {
				ds.cache.regionSizeBuffer.Update(addr, bankID)
			}

			addr += diff
		}

	}

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

	bankID := trans.bankID
	bankBuf := ds.cache.localDirToBankBuffers[bankID]
	if !isLocal {
		bankBuf = ds.cache.remoteDirToBankBuffers[bankID]
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

	regionLen := ds.cache.regionLen[bankID]
	maskLen := regionLen + ds.cache.log2NumSubEntry
	addr := trans.accessReq().GetAddress()
	cachelineID, _ := getCacheLineID(addr, uint64(ds.cache.regionLen[bankID]))

	// PRE-FLIGHT: stall iff a subsequent mshr.Add below would panic with
	// "entry already in mshr". mshr.Add's invariant uses
	// max(e.RegionLen, new_regionLen) as the comparison mask and is
	// bank-agnostic. Mirror that exactly: QueryWithMask(pid, addr,
	// regionLen[bankID]) checks the same condition (returns each e where
	// e.Address>>max(e.RegionLen, regionLen) == addr>>max(...)).
	// This is tighter than the original coarsest-mask (regionLen[0])
	// stall — path B scenarios (X in a different sub-region than an
	// in-flight motion at finer granularity) only stall here when X's
	// allocation would actually alias an existing MSHR entry at the
	// joint granularity, not whenever they share the coarsest 16KB.
	// Deadlock-free: conflicting entry has no resource dependency on
	// this trans (which holds nothing yet); it self-cleans via mshr.Remove
	// in processOneReq{,AfterPromotion,AfterDemotion}.
	pid := trans.accessReq().GetPID()
	if conflicts := ds.cache.mshr.QueryWithMask(pid, cachelineID, uint64(regionLen)); len(conflicts) > 0 {
		*ds.returnFalse += "[writeToBank] stall: would alias existing MSHR entry (Add panic)"
		ds.cache.stallWriteToBankPreflight++
		return false
	}

	// §1 SO assertion (sub-entry granular) for the writeToBank alloc
	// path. Active only when SD_ASSERT_SINGLE_OWNER=1.
	if os.Getenv("SD_ASSERT_SINGLE_OWNER") == "1" {
		for bid := 0; bid < ds.cache.numBanks; bid++ {
			if bid == bankID {
				continue
			}
			otherRegionLen := uint64(ds.cache.regionLen[bid])
			otherCID, _ := getCacheLineID(addr, otherRegionLen)
			otherEntry, _ := ds.cache.directory.Lookup(bid, pid, otherCID)
			if otherEntry == nil || !otherEntry.IsValidEntry() {
				continue
			}
			subIdxOther := int((addr >> otherRegionLen) % uint64(1<<ds.cache.log2NumSubEntry))
			if !otherEntry.SubEntry[subIdxOther].IsValid {
				continue
			}
			panic(fmt.Sprintf(
				"[%s] SO violation (writeToBank): addr=%#x alloc bank=%d subIdx=%d clashes with bank=%d subIdx=%d (tag=%#x)",
				ds.cache.name, addr, bankID, index, bid, subIdxOther, otherEntry.Tag))
		}
	}

	if !block.SubEntry[index].IsValid && !trans.bfEagerInserted {
		ds.cache.directory.InsertBloomfilter(bankID, cachelineID)
	}

	subEntry := &block.SubEntry[index]
	ds.cache.directory.Visit(trans.bankID, block)
	block.Tag = cachelineID >> maskLen << maskLen
	block.IsValid = true
	block.PID = trans.accessReq().GetPID()
	subEntry.IsLocked = true
	subEntry.IsValid = true
	trans.block = block
	trans.blockIdx = index
	ds.cache.allocationCount++
	// (E-3) Data is now present at bankID — drop any stale RSB hint for
	// this addr to preserve the "RSB only for absent addrs" invariant.
	ds.cache.regionSizeBuffer.InvalidateAddr(addr)

	// Symmetric to doPromotion: when allocating a coarser-or-equal entry
	// at bankID, sweep any stale finer-bank entries that fall within
	// this new entry's sub-region. §1 SO-invariant fix narrows the sweep
	// to the specific sub-entry being filled (instead of the entire
	// coarse region) so concurrent promotions for other sub-regions of
	// the same coarse block are not disturbed.
	ds.sweepFinerBankEntriesInSubRegion(bankID, block, index, block.PID)

	bankBuf.Push(trans)

	mshrEntry := ds.cache.mshr.Add(block.PID, cachelineID, uint64(ds.cache.regionLen[bankID]), bankID)
	mshrEntry.IsAccessReq = true
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
		for _, sh := range sharer {
			if sh == trans.accessReq().GetSrcRDMA() {
				return true
			}
		}

		return false
	} else { // local access
		return true
	}
}

func (ds *directoryStage) writePermission(trans *transaction, sharer []sim.RemotePort) bool {
	if !trans.fromLocal { // remote access
		// 1. Sharer가 없거나(0명) 여러 명(2명 이상)인 경우 권한 없음
		if len(sharer) != 1 {
			return false
		}

		// 2. Sharer가 딱 1명이지만, 그게 요청자 본인이 아닌 경우 권한 없음
		if fmt.Sprintf("%s", sharer[0]) != fmt.Sprintf("%s", trans.accessReq().GetSrcRDMA()) {
			return false
		}

		// 유일한 Sharer가 본인일 때만 권한 있음
		return true

	} else { // local access
		// Spec: local writes to remotely-shared data must propagate
		// invalidations to every recorded sharer. Home GPU is excluded
		// from the sharer list, so any present sharer is a remote GPU
		// holding a stale copy. This decision is bank-agnostic — it
		// applies to fine-bank and coarse-bank hits identically.
		//
		// Pre-fix code returned true here when len(sharer) <= 1, silently
		// dropping the invalidation to a lone remote sharer.
		// op5aShortcutWithRemoteSharer (Comp field) is the regression slot
		// for that bug; if a future change reintroduces the shortcut, also
		// reintroduce the increment site here so the post-C-2 sanity dump
		// catches it.
		return len(sharer) == 0
	}
}

// bankSelection holds the routing decision produced by selectBank.
// onCommit must be called exactly once, immediately after the pipeline Accept
// succeeds — never before, and never on early return.
type bankSelection struct {
	bankID   int
	bankList []int
	// onCommit executes all state mutations (RSB.Delete, BF.Insert).
	// Nil for RSB-miss paths that require no state change.
	onCommit func()
	// bfEager is true when onCommit will call InsertBloomfilter.
	// writeToBank checks this to avoid double-counting the same BF counter.
	bfEager bool
	// rsbHintBank carries the RSB-recorded bank to downstream code
	// (specifically doWriteMiss) so that allocation honors the hint instead
	// of always defaulting to the finest bank. -1 = no RSB hit.
	rsbHintBank int
}

// selectBank computes the routing decision for addr without mutating any shared
// state.  All state changes are deferred to bankSelection.onCommit, which the
// caller must invoke only after the pipeline Accept succeeds ([S1] guarantee).
//
// GetBank returns banks finest-first ([0]=finest, [last]=coarsest).
//
// MSHR-aware redirect (single source of truth for in-flight motion):
//
//	When any granularity's MSHR entry overlaps addr (probed with the coarsest
//	regionLen so every granularity is visible), the routing decision is the
//	entry's RegionID — overriding BF and RSB. Rationale: mshrStage atomically
//	registers an MSHR entry at the motion's target bank the moment promotion
//	or demotion is enqueued, even though the directory entry transition spans
//	multiple cycles. Without this redirect, a new access during the motion
//	window can land at a different bank and create a cross-granularity
//	parallel entry — the exact invariant violation that surfaces as the
//	"Hit about demotion" warning and the "finer MSHR overlaps coarser"
//	stall. With the redirect, the new access joins the bank that already
//	owns the in-flight entry; it either resolves as an MSHR hit when the
//	motion completes, or stalls at that bank's doWrite check until then.
//	No deadlock: motion completion always calls mshr.Remove, releasing
//	the redirect; the redirected access holds no resources while waiting.
//
// aliasingMotionTarget returns the in-flight motion MSHR entry whose
// target block at its RegionID would alias to (have the same Tag as)
// addr's block at that bank — i.e., a future allocation for addr at the
// motion target bank would collide with the motion's block. Returns nil
// if no motion entry aliases. When multiple motions alias, returns the
// one at the coarsest (smallest) RegionID. Used by routing decisions to
// distinguish:
//   - exact alias → redirect to motion target (path A: subsequent doWrite
//     can MSHR-hit via doWriteMSHRHit once routed there).
//   - coarse-region overlap but no alias → fall through to normal BF/RSB
//     routing (path B: source bank still has valid sub-entry for the
//     different sub-region, or natural allocation at non-conflicting Tag).
func (ds *directoryStage) aliasingMotionTarget(pid vm.PID, addr uint64) *internal.MSHREntry {
	coarsestMask := uint64(ds.cache.regionLen[0])
	var best *internal.MSHREntry
	for _, e := range ds.cache.mshr.QueryWithMask(pid, addr, coarsestMask) {
		targetMaskLen := uint64(ds.cache.regionLen[e.RegionID]) + uint64(ds.cache.log2NumSubEntry)
		if addr>>targetMaskLen != e.Address>>targetMaskLen {
			continue
		}
		if best == nil || e.RegionID < best.RegionID {
			best = e
		}
	}
	return best
}

func (ds *directoryStage) selectBank(pid vm.PID, addr uint64) bankSelection {
	// §3 deadlock fix: aliasingMotionTarget redirect removed to break the
	// heterogeneous bankList traversal cycle (bank3↔bank4 pipeline cycle in
	// pagerank). Correctness is preserved by (a) doWrite's MSHR-aware check
	// at entry (mshr.QueryWithMask using max(e.RegionLen, currentBankRegionLen))
	// which catches in-flight motions at any granularity, and (b) §1's
	// alloc-time coarser-bank reprobe in doWriteMiss.

	e := ds.cache.regionSizeBuffer.Search(addr) // miss when RSB disabled
	if e.RegionID != -1 {
		// §2 simplified bank lookup: RSB hit routes exclusively to the
		// recorded bank. The bfDisagreesWithRSB cross-check is removed;
		// correctness for the "stale RSB pointing at a coarser bank while
		// a finer bank now owns the region" case is provided by §1's
		// alloc-time coarser-bank reprobe in doWriteMiss.
		if os.Getenv("SD_ASSERT_RSB_BF_DISJOINT") == "1" {
			bfList := ds.cache.directory.GetBank(addr)
			for _, probedBank := range bfList {
				probedCachelineID, _ := getCacheLineID(addr, uint64(ds.cache.regionLen[probedBank]))
				realEntry, _ := ds.cache.directory.Lookup(probedBank, pid, probedCachelineID)
				if realEntry != nil && realEntry.IsValidEntry() {
					panic(fmt.Sprintf("RSB/BF invariant violated: addr=%#x, RSB.bank=%d, real entry at bank=%d (tag=%#x)",
						addr, e.RegionID, probedBank, realEntry.Tag))
				}
			}
		}
		rsbEntry := e
		routedBank := e.RegionID
		return bankSelection{
			bankID:   routedBank,
			bankList: nil,
			onCommit: func() {
				ds.cache.directory.InsertBloomfilter(routedBank, addr)
				ds.cache.regionSizeBuffer.Delete(rsbEntry)
			},
			bfEager:     true,
			rsbHintBank: routedBank,
		}
	}

	// RSB miss: per the m/n=4 L-CBF spec, the lookup order is
	//   [CBF-positive coarse banks (finest-first)] -> Bank N-1 -> Bank N-2
	// Banks N-1 and N-2 have no CBF and are always probed last.
	list := ds.lookupOrder(addr)
	if len(list) > 0 {
		return bankSelection{bankID: list[0], bankList: list[1:], rsbHintBank: -1}
	}
	return bankSelection{bankID: ds.cache.numBanks - 1, rsbHintBank: -1}
}

// lookupOrder builds the full per-access bank probe order:
//  1. CBF-positive coarse banks (finest-first among CBF banks).
//  2. The two finest non-CBF banks (numBanks-1 then numBanks-2).
//
// This is the user-visible ordering "CBF 양성 bank > Bank 4 > Bank 3".
// When CBF is globally disabled, GetBank already returns finest-first across
// every bank, so we just pass that through.
func (ds *directoryStage) lookupOrder(addr uint64) []int {
	type cbfInfo interface{ NumCBFBanks() int }
	dir := ds.cache.directory
	out := dir.GetBank(addr)
	if info, ok := dir.(cbfInfo); ok && info.NumCBFBanks() > 0 {
		cbfBanks := info.NumCBFBanks()
		for i := ds.cache.numBanks - 1; i >= cbfBanks; i-- {
			out = append(out, i)
		}
	}
	return out
}
