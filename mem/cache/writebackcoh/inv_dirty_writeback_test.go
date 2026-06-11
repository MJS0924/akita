package writebackcoh

import (
	"bytes"
	"testing"

	"github.com/sarchlab/akita/v4/mem/cache/writebackcoh/internal"
	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
)

// [INV-FIDELITY C3 TEST]
//
// An invalidation that kills a dirty (M-state) line must produce a local
// victim writeback before the block is zeroed — real L2s have no mode that
// discards modified data, and the writeback is the largest per-inv cost the
// old model skipped (writeBuffer slot + NoC + DRAM write). These tests guard
// the Comp-level enqueueInvDirtyFlush helper and the deferred-kill path in
// bankStage.applyPendingInvalidation.

func newCacheForDirtyInvTest(wbbCap int) *Comp {
	cache := &Comp{}
	cache.deviceID = 1
	cache.log2BlockSize = 6
	cache.log2PageSize = 12
	cache.storage = mem.NewStorage(1024)
	cache.writeBufferBuffer = sim.NewBuffer("WBB", wbbCap)
	cache.writeBufferBufferRemote = sim.NewBuffer("WBBRemote", 4)
	cache.writeBuffer = &writeBufferStage{
		cache:            cache,
		maxDeferredFlush: 4, // own half = 2
	}
	readMask := make([]map[vm.PID]map[uint64][]uint8, 1)
	dirtyMask := make([]map[vm.PID]map[uint64][]uint8, 1)
	cache.ReadMask = &readMask
	cache.DirtyMask = &dirtyMask
	return cache
}

func newDirtyBlock(t *testing.T, cache *Comp, payload byte) *internal.Block {
	t.Helper()
	line := make([]byte, 64)
	for i := range line {
		line[i] = payload
	}
	if err := cache.storage.Write(0, line); err != nil {
		t.Fatal(err)
	}
	dirtyMask := make([]bool, 64)
	dirtyMask[3] = true
	return &internal.Block{
		PID:          1,
		Tag:          0x1000,
		CacheAddress: 0,
		IsValid:      true,
		IsDirty:      true,
		DirtyMask:    dirtyMask,
	}
}

// The helper must capture the line into a LOCAL-origin writeBufferFlush
// transaction carrying the block's data, address, and a copy of the dirty
// mask — the exact shape tryWriteOneFrom turns into the DRAM WriteReq.
func TestInvDirtyFlushCapturesLine(t *testing.T) {
	cache := newCacheForDirtyInvTest(4)
	block := newDirtyBlock(t, cache, 0xAB)

	if !cache.enqueueInvDirtyFlush(block) {
		t.Fatal("enqueueInvDirtyFlush should succeed with a free slot")
	}

	item := cache.writeBufferBuffer.Peek()
	if item == nil {
		t.Fatal("flush transaction should be in writeBufferBuffer (local bucket)")
	}
	flush := item.(*transaction)
	if flush.action != writeBufferFlush {
		t.Fatalf("action = %d, want writeBufferFlush", flush.action)
	}
	if !flush.fromLocal {
		t.Fatal("dirty-inv flush must be LOCAL origin (never touches cross-GPU caps)")
	}
	if flush.evictingAddr != 0x1000 || flush.evictingPID != 1 {
		t.Fatalf("evicting identity = (%x, %d), want (0x1000, 1)",
			flush.evictingAddr, flush.evictingPID)
	}
	want := bytes.Repeat([]byte{0xAB}, 64)
	if !bytes.Equal(flush.evictingData, want) {
		t.Fatal("flush must carry the dirty line's data")
	}
	if len(flush.evictingDirtyMask) != 64 || !flush.evictingDirtyMask[3] {
		t.Fatal("flush must carry a copy of the block's DirtyMask")
	}
	// Mask must be a COPY: zeroing the block later must not mutate it.
	block.DirtyMask[3] = false
	if !flush.evictingDirtyMask[3] {
		t.Fatal("DirtyMask must be copied, not aliased")
	}
}

// When writeBufferBuffer is momentarily full the victim parks in the bounded
// own-origin deferred-flush list (same shock absorber the displacement
// flushes use) instead of being dropped.
func TestInvDirtyFlushFallsBackToDeferList(t *testing.T) {
	cache := newCacheForDirtyInvTest(1)
	cache.writeBufferBuffer.Push(&transaction{fromLocal: true})
	block := newDirtyBlock(t, cache, 0x5A)

	if !cache.enqueueInvDirtyFlush(block) {
		t.Fatal("helper should fall back to the deferred-flush list")
	}
	if len(cache.writeBuffer.deferredFlushOwn) != 1 {
		t.Fatalf("deferredFlushOwn len = %d, want 1",
			len(cache.writeBuffer.deferredFlushOwn))
	}
}

// Both targets full → helper refuses (returns false) and counts the event;
// callers gate on capacity first, so this is a never-drop-silently backstop.
func TestInvDirtyFlushNeverDropsSilently(t *testing.T) {
	cache := newCacheForDirtyInvTest(1)
	cache.writeBufferBuffer.Push(&transaction{fromLocal: true})
	cache.writeBuffer.deferFlushPush(&transaction{fromLocal: true})
	cache.writeBuffer.deferFlushPush(&transaction{fromLocal: true})
	block := newDirtyBlock(t, cache, 0x77)

	if cache.enqueueInvDirtyFlush(block) {
		t.Fatal("helper must report failure when both targets are full")
	}
	if cache.EventCounts()["InvDirtyWritebackDropped"] != 1 {
		t.Fatal("a refused dirty flush must be counted, never silent")
	}
}

// Deferred kill (PendingInvalidation armed by doInvalidation while the block
// was locked / had an MSHR entry): applyPendingInvalidation must enqueue the
// writeback BEFORE zeroing the block.
func TestApplyPendingInvalidationFlushesDirtyLine(t *testing.T) {
	cache := newCacheForDirtyInvTest(4)
	bank := &bankStage{cache: cache}
	block := newDirtyBlock(t, cache, 0xCD)
	block.PendingInvalidation = true

	bank.applyPendingInvalidation(block)

	if block.IsValid || block.PendingInvalidation {
		t.Fatal("block must be zeroed after apply")
	}
	item := cache.writeBufferBuffer.Peek()
	if item == nil {
		t.Fatal("dirty deferred kill must enqueue a writeback flush")
	}
	flush := item.(*transaction)
	if !bytes.Equal(flush.evictingData, bytes.Repeat([]byte{0xCD}, 64)) {
		t.Fatal("flush must carry the dirty data captured before the zero-out")
	}
	if cache.EventCounts()["InvDirtyWritebackDeferred"] != 1 {
		t.Fatal("deferred dirty writeback must be counted")
	}
}

// A clean deferred kill enqueues nothing — the writeback applies only to
// modified lines (write-through writeToHomeNode lines arrive here clean).
func TestApplyPendingInvalidationCleanLineNoFlush(t *testing.T) {
	cache := newCacheForDirtyInvTest(4)
	bank := &bankStage{cache: cache}
	block := newDirtyBlock(t, cache, 0xEE)
	block.IsDirty = false
	block.PendingInvalidation = true

	bank.applyPendingInvalidation(block)

	if cache.writeBufferBuffer.Peek() != nil {
		t.Fatal("clean line kill must not produce a writeback")
	}
	if !block.IsValid && block.PendingInvalidation {
		t.Fatal("block must still be zeroed")
	}
}
