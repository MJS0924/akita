package writebackcoh

import (
	"github.com/sarchlab/akita/v4/mem/vm"
)

// missTrackerKey identifies a cache line by PID and cache-line address.
// The address is masked at log2BlockSize (i.e. cacheLineID), the same key
// the directory uses internally.
type missTrackerKey struct {
	PID  vm.PID
	Addr uint64
}

// Eviction reason tags (used to classify a future re-fetch).
const (
	evictReasonLRU       = "lru"
	evictReasonInvWrite  = "inv-write"  // peer write on a shared line
	evictReasonInvEvict  = "inv-evict"  // remote directory eviction
)

// classifyAndRecordReadMiss returns the canonical miss-reason tag for a
// read-miss at (pid, addr) and updates seenAddrs so subsequent classifications
// remain accurate.
//
// Reasons:
//   "cold"       — first time this L2 has ever seen the line.
//   "capacity"   — line was previously cached then LRU-evicted. (We don't
//                  separate true capacity vs conflict misses; doing so would
//                  require a parallel fully-associative shadow LRU.)
//   "coh-write"  — line was invalidated by a peer write (write-init).
//   "coh-evict"  — line was invalidated by a remote directory eviction.
//   "other"      — unexpected: should be 0 in a healthy run.
func (c *Comp) classifyAndRecordReadMiss(pid vm.PID, addr uint64) string {
	key := missTrackerKey{PID: pid, Addr: addr}

	reason := "cold"
	if _, seen := c.seenAddrs[key]; seen {
		switch c.lastEvictionReason[key] {
		case evictReasonLRU:
			reason = "capacity"
		case evictReasonInvWrite:
			reason = "coh-write"
		case evictReasonInvEvict:
			reason = "coh-evict"
		default:
			reason = "other"
		}
	}

	// Mark seen and clear any past eviction reason — once we record this
	// miss, the line is being fetched and will live in the cache again.
	c.seenAddrs[key] = struct{}{}
	delete(c.lastEvictionReason, key)
	return reason
}

// recordEviction stamps the most recent eviction reason for (pid, addr).
// Call this with the *old* victim's PID/Tag, before they are overwritten
// for the new request.
func (c *Comp) recordEviction(pid vm.PID, addr uint64, reason string) {
	key := missTrackerKey{PID: pid, Addr: addr}
	c.lastEvictionReason[key] = reason
}
