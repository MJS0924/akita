package REC

import (
	"fmt"
	"log"
	"reflect"
	"strings"

	"github.com/sarchlab/akita/v4/mem/cache"
	"github.com/sarchlab/akita/v4/mem/cache/REC/internal"
	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/tracing"
)

type flusher struct {
	cache *Comp

	blockToEvict    []*internal.CohEntry
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
		if f.processingFlush.DiscardInflight {
			f.cache.discardInflightTransactions()
		} else {
			return false
		}
	}

	f.prepareBlockToFlushList()
	f.cache.state = cacheStateFlushing

	return true
}

func (f *flusher) existInflightTransaction() bool {
	if len(f.cache.bottomSender.localInflightRequest) > 0 || len(f.cache.bottomSender.remoteInflightRequest) > 0 {
		return true
	}

	if len(f.cache.bottomSender.inflightInvToBottom) > 0 {
		return true
	}

	if len(f.cache.bottomSender.inflightInvToOutside) > 0 {
		return true
	}

	return false
}

func (f *flusher) prepareBlockToFlushList() {
	sets := f.cache.directory.GetSets()
	for _, set := range sets {
		for _, block := range set.CohEntries {
			for _, entry := range block.SubEntry {
				entry.IsValid = false
				entry.IsDirty = false
				entry.IsLocked = false
				entry.ReadCount = 0
				entry.Sharer = nil
			}

			// remote data는 어차피 cache flush 과정에서 처리 됨

			// if !f.processingFlush.InvalidateAllCachelines && block.Tag != f.processingFlush.Addr {
			// 	continue
			// }

			// if block.ReadCount > 0 || block.IsLocked {
			// 	panic("all the blocks should be unlocked before flushing")
			// }

			// if len(block.Sharer) > 0 {
			// 	block.IsValid = false
			// 	f.blockToEvict = append(f.blockToEvict, block)
			// }
		}
	}
}

func (f *flusher) processFlush() bool {
	if len(f.blockToEvict) == 0 {
		return false
	}

	block := f.blockToEvict[0]
	bankBuf := f.cache.localDirToBankBuffers

	if !bankBuf.CanPush() {
		return false
	}

	dummyRead := mem.ReadReqBuilder{}.
		// WithAddress(block.CacheAddress).
		WithAddress(block.Tag).
		Build()
	trans := &transaction{
		read:   dummyRead,
		action: InvalidateEntry,
		victim: *block,
		// 나중에: bankId 잘 넣어야 됨
	}
	bankBuf.Push(trans)

	f.blockToEvict = f.blockToEvict[1:]

	// fmt.Printf("[%s]\tA.0. Push Invalidation Request(%s) for %x to bank\n", f.cache.name, trans.read.Meta().ID, block.CacheAddress)
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
	fmt.Printf("[%s]\tStart Restart\n", f.cache.name)
	if !f.cache.controlPort.CanSend() {
		return false
	}

	f.cache.state = cacheStateRunning
	f.cache.flushLocalAccess = false

	rsp := cache.RestartRspBuilder{}.
		WithSrc(f.cache.controlPort.AsRemote()).
		WithDst(req.Src).
		WithRspTo(req.ID).
		SetFromL1I(strings.Contains(f.cache.name, "L1ICache")).
		Build()
	f.cache.controlPort.Send(rsp)
	fmt.Printf("[%s]\tSend Restart Rsp\n", f.cache.name)

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
	if f.cache.localDirToBankBuffers.Size() > 0 {
		return false
	}
	if f.cache.remoteDirToBankBuffers.Size() > 0 {
		return false
	}

	if f.cache.bankStages.localInflightTransCount > 0 || f.cache.bankStages.remoteInflightTransCount > 0 {
		return false
	}

	if f.cache.localBottomSenderBuffer.Size() > 0 || f.cache.remoteBottomSenderBuffer.Size() > 0 {
		return false
	}

	// [수정] 양방향 Inflight Request 리스트 확인
	if len(f.cache.bottomSender.localInflightRequest) > 0 ||
		len(f.cache.bottomSender.remoteInflightRequest) > 0 ||
		len(f.cache.bottomSender.inflightInvToOutside) > 0 {
		return false
	}

	return true
}
