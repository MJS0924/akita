package REC

import (
	"fmt"
	"strings"
	"testing"

	"github.com/sarchlab/akita/v4/mem/cache/REC/internal"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
)

// TestREC_OP1_CoalescingMicrotest is the Phase-B B4 micro-test from the audit
// for OP1 (remote-read coalescing on existing 1 kB region entry).
//
// Scenario: a single home-GPU directory receives three remote reads from the
// same source GPU, all targeting addresses inside the same 1 kB region
// (0x1000, 0x1040, 0x1080). After all three reads, the directory must hold
// exactly one entry, with three position-bit sub-entries valid, each recording
// the source GPU as sharer. This validates the paper's §4.2 "Remote reads"
// behaviour for entry coalescing.
func TestREC_OP1_CoalescingMicrotest(t *testing.T) {
	const (
		log2Block    = 6           // 64 B cache line
		log2SubEntry = 4           // 16 sub-entries → 1 kB region
		regionBase   = uint64(0x1000)
		pid          = vm.PID(1)
	)
	var sharer sim.RemotePort = "GPU1.RDMA"

	// Build a minimal directory: 1 set, 4 ways. Single set keeps every address
	// in the same set, so capacity contention is the only failure mode.
	dir := internal.NewRECDirectory(
		1, 4, 1<<log2Block, log2SubEntry, log2Block,
		internal.NewLRUVictimFinder(),
	)

	// Three addresses inside region 0x1000 (all share base 0x1000 because
	// regionBase >> 10 == 0x4 for all three).
	addrs := []uint64{regionBase, regionBase + 0x40, regionBase + 0x80}

	// Each "remote read" exercises the same code path used by the runtime
	// when topparser routes a !fromLocal && toLocal ReadReq into the
	// directory pipeline:
	//   Lookup → (miss, sub-entry invalid) → FindVictim → updateOrInsert.
	for _, addr := range addrs {
		_, _ = dir.Lookup(pid, addr) // expect miss for first addr; for the
		// second/third it returns nil because SubEntry[idx].IsValid==false.

		victim, alloc := dir.FindVictim(pid, addr)
		if victim == nil {
			t.Fatalf("FindVictim returned nil for addr %#x", addr)
		}

		// The crucial assertion: addr 0x1040 and 0x1080 must reuse the
		// entry created for addr 0x1000. If alloc==true on those, REC
		// failed to coalesce and instead allocated a fresh entry.
		if addr != addrs[0] && alloc {
			t.Errorf("OP1 violation: FindVictim for addr %#x in same 1KB "+
				"region returned alloc=true (expected coalescing onto "+
				"existing entry)", addr)
		}

		// Reset only on first allocation (matches doWriteMiss flow).
		if alloc && addr == addrs[0] {
			victim.Reset()
		}

		// Compute sub-entry index and mark it (mirrors writeToBank).
		idx := int((addr >> log2Block) % (1 << log2SubEntry))
		victim.IsValid = true
		victim.PID = pid
		// Tag with low 10 bits cleared — the paper's "base address".
		victim.Tag = addr >> (log2Block + log2SubEntry) << (log2Block + log2SubEntry)
		victim.SubEntry[idx].IsValid = true
		// Append sharer if not already present.
		exists := false
		for _, s := range victim.SubEntry[idx].Sharer {
			if s == sharer {
				exists = true
				break
			}
		}
		if !exists {
			victim.SubEntry[idx].Sharer = append(victim.SubEntry[idx].Sharer, sharer)
		}
		dir.Visit(victim)
	}

	// ── Verify directory state ────────────────────────────────────────────
	sets := dir.GetSets()
	validEntries := 0
	var hot *internal.CohEntry
	for i := range sets[0].CohEntries {
		e := sets[0].CohEntries[i]
		if e.IsValid {
			validEntries++
			hot = e
		}
	}
	if validEntries != 1 {
		t.Errorf("OP1 coalescing failed: expected 1 valid entry, got %d", validEntries)
	}
	if hot == nil {
		t.Fatal("no valid entry found")
	}
	if hot.Tag != regionBase {
		t.Errorf("expected entry Tag=%#x (1 kB-aligned base), got %#x",
			regionBase, hot.Tag)
	}

	// Position bits 0, 1, 2 must be valid; 3..15 must be invalid.
	wantValid := map[int]bool{0: true, 1: true, 2: true}
	for i := 0; i < 16; i++ {
		got := hot.SubEntry[i].IsValid
		want := wantValid[i]
		if got != want {
			t.Errorf("SubEntry[%d].IsValid: want %v, got %v", i, want, got)
		}
	}

	// Each valid sub-entry must list exactly the source GPU as sharer.
	for i := 0; i < 3; i++ {
		s := hot.SubEntry[i].Sharer
		if len(s) != 1 || s[0] != sharer {
			t.Errorf("SubEntry[%d].Sharer: want [%s], got %v", i, sharer, s)
		}
	}

	t.Log(dumpEntry(hot))
}

// dumpEntry produces a human-readable single-line dump of the entry's
// position-bit / sharer-bit pattern, matching the audit's "entry dump" format.
func dumpEntry(e *internal.CohEntry) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Entry{Tag=%#x, Valid=%v, [", e.Tag, e.IsValid)
	for i := 0; i < 16; i++ {
		if e.SubEntry[i].IsValid {
			fmt.Fprintf(&b, "p%d=1{", i)
			for j, s := range e.SubEntry[i].Sharer {
				if j > 0 {
					b.WriteString(",")
				}
				fmt.Fprintf(&b, "%s", s)
			}
			b.WriteString("} ")
		}
	}
	b.WriteString("]}")
	return b.String()
}
