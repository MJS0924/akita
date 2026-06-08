package writebackcoh

import (
	"fmt"
	"log"
	"reflect"
	"strings"

	"github.com/sarchlab/akita/v4/mem/cache"
	"github.com/sarchlab/akita/v4/mem/cache/writebackcoh/internal"
	"github.com/sarchlab/akita/v4/tracing"
)

type flusher struct {
	cache *Comp

	blockToEvict    []*internal.Block
	processingFlush *cache.FlushReq
}

func (f *flusher) Tick() bool {
	if f.processingFlush != nil && f.cache.state == cacheStatePreFlushing {
		return f.processPreFlushing()
	}

	madeProgress := false
	if f.processingFlush != nil && f.cache.state == cacheStateFlushing {
		madeProgress = f.finalizeFlushing() || madeProgress
		madeProgress = f.processFlush() || madeProgress

		return madeProgress
	}

	return f.extractFromPort()
}

func (f *flusher) processPreFlushing() bool {
	if f.existInflightTransaction() {
		return false
	}

	f.prepareBlockToFlushList()
	f.cache.state = cacheStateFlushing

	return true
}

func (f *flusher) existInflightTransaction() bool {
	if len(f.cache.inFlightTransactions) > 0 {
		return true
	}

	if len(f.cache.writeBuffer.inflightEviction) > 0 {
		return true
	}

	if len(f.cache.writeBuffer.inflightFetch) > 0 {
		return true
	}

	return false
}

func (f *flusher) prepareBlockToFlushList() {
	sets := f.cache.directory.GetSets()
	for _, set := range sets {
		for _, block := range set.Blocks {
			if block.ReadCount > 0 || block.IsLocked {
				fmt.Printf("[%s]\t[WARNING]\tall the blocks should be unlocked before flushing\n", f.cache.name)
				for true {
				}
				panic("all the blocks should be unlocked before flushing")
			}

			if !block.IsValid || !block.IsDirty {
				continue
			}

			port := fmt.Sprintf("%s", f.cache.addressToPortMapper.Find(block.Tag))
			if !strings.Contains(port, "DRAM") { // remote data는 모두 invalidate -> Coh Dir는 모두 초기화
				block.IsValid = false
				block.IsDirty = false
				continue
			}

			if block.IsValid && block.IsDirty {
				f.blockToEvict = append(f.blockToEvict, block)
			}
		}
	}
}

func (f *flusher) processFlush() bool {
	if len(f.blockToEvict) == 0 {
		return false
	}

	block := f.blockToEvict[0]
	bankNum := bankID(
		block,
		f.cache.directory.WayAssociativity(),
		len(f.cache.dirToBankBuffers))
	// flusher 가 push 하는 evict 는 이 cache 자체의 flush — local 취급.
	bankBuf := f.cache.dirToBankBuffersLocal[bankNum]

	if !bankBuf.CanPush() {
		return false
	}

	trans := &transaction{
		flush:             f.processingFlush,
		victim:            block,
		action:            bankEvict,
		evictingAddr:      block.Tag,
		evictingDirtyMask: block.DirtyMask,
	}
	bankBuf.Push(trans)

	f.blockToEvict = f.blockToEvict[1:]

	return true
}

func (f *flusher) extractFromPort() bool {
	item := f.cache.controlPort.PeekIncoming()
	if item == nil {
		return false
	}

	switch req := item.(type) {
	case *cache.FlushReq:
		return f.startProcessingFlush(req)
	case *cache.RestartReq:
		return f.handleCacheRestart(req)
	default:
		log.Panicf("Cannot process request of %s", reflect.TypeOf(req))
	}

	return true
}

func (f *flusher) startProcessingFlush(
	req *cache.FlushReq,
) bool {
	f.processingFlush = req
	if req.DiscardInflight {
		f.cache.discardInflightTransactions()
		clearPort(f.cache.topPort)
		clearPort(f.cache.bottomPort)
	}

	f.cache.state = cacheStatePreFlushing
	f.cache.controlPort.RetrieveIncoming()

	tracing.TraceReqReceive(req, f.cache)

	return true
}

func (f *flusher) handleCacheRestart(
	req *cache.RestartReq,
) bool {
	if !f.cache.controlPort.CanSend() {
		return false
	}

	clearPort(f.cache.topPort)
	clearPort(f.cache.bottomPort)

	f.cache.state = cacheStateRunning

	rsp := cache.RestartRspBuilder{}.
		WithSrc(f.cache.controlPort.AsRemote()).
		WithDst(req.Src).
		WithRspTo(req.ID).
		SetFromL1I(strings.Contains(f.cache.name, "L1ICache")).
		Build()
	f.cache.controlPort.Send(rsp)

	f.cache.controlPort.RetrieveIncoming()

	return true
}

func (f *flusher) finalizeFlushing() bool {
	if len(f.blockToEvict) > 0 {
		return false
	}

	if !f.flushCompleted() {
		return false
	}

	if !f.cache.controlPort.CanSend() {
		return false
	}

	rsp := cache.FlushRspBuilder{}.
		WithSrc(f.cache.controlPort.AsRemote()).
		WithDst(f.processingFlush.Src).
		WithRspTo(f.processingFlush.ID).
		SetFromL1I(strings.Contains(f.cache.name, "L1ICache")).
		Build()
	f.cache.controlPort.Send(rsp)

	f.cache.mshr.Reset()
	f.cache.directory.Reset()

	if f.processingFlush.PauseAfterFlushing {
		f.cache.state = cacheStatePaused
	} else {
		f.cache.state = cacheStateRunning
	}

	tracing.TraceReqComplete(f.processingFlush, f.cache)
	f.processingFlush = nil

	return true
}

func (f *flusher) flushCompleted() bool {
	for _, b := range f.cache.dirToBankBuffersLocal {
		if b.Size() > 0 {
			return false
		}
	}
	for _, b := range f.cache.dirToBankBuffersRemote {
		if b.Size() > 0 {
			return false
		}
	}

	// [ITER19b R5] writeBufferToBankBuffers split into Req + Rsp halves.
	// Both must be drained — Req side carries peer-bypass/regular admit
	// pushes; Rsp side carries fetched-data and DataReadyRsp pushes.
	// PRE-iter19 this slice was never checked at all (latent bug).
	for _, b := range f.cache.writeBufferToBankBuffersReq {
		if b.Size() > 0 {
			return false
		}
	}
	for _, b := range f.cache.writeBufferToBankBuffersRsp {
		if b.Size() > 0 {
			return false
		}
	}

	// [ITER19b LATENT] pure-fetch ingress from dirStage.fetch().
	if f.cache.writeBufferFetchBuffer.Size() > 0 {
		return false
	}

	for _, b := range f.cache.bankStages {
		if b.inflightTransCount > 0 {
			return false
		}
	}

	// [ITER13 fix #2] sum both split eviction-ingress buffers.
	if f.cache.writeBufferBufferTotalSize() > 0 {
		return false
	}

	// [ITER19b LATENT] mshrStageBuffer was split earlier; check both halves.
	if f.cache.mshrStageBuffer.Size() > 0 ||
		f.cache.mshrStageBufferRemote.Size() > 0 {
		return false
	}

	if len(f.cache.writeBuffer.inflightFetch) > 0 ||
		len(f.cache.writeBuffer.inflightEviction) > 0 ||
		// [ITER10] check both split pending queues.
		len(f.cache.writeBuffer.pendingLocalEvictions) > 0 ||
		len(f.cache.writeBuffer.pendingRemoteEvictions) > 0 {
		return false
	}

	// [ITER19b R6] pendingDataReady / pendingWriteDone split into
	// Local + Remote halves. Pre-R6 the single queues were never checked
	// here, allowing a DataReadyRsp / WriteDoneRsp to sit at ingress while
	// flushCompleted returned true (silent transaction loss). Post-R6 both
	// halves must drain before the flush is declared complete.
	if len(f.cache.writeBuffer.pendingDataReadyLocal) > 0 ||
		len(f.cache.writeBuffer.pendingDataReadyRemote) > 0 ||
		len(f.cache.writeBuffer.pendingWriteDoneLocal) > 0 ||
		len(f.cache.writeBuffer.pendingWriteDoneRemote) > 0 {
		return false
	}

	return true
}
