package internal

// A VictimFinder decides with block should be evicted
type VictimFinder interface {
	FindVictim(set *CohSet) *CohEntry
}

// LRUVictimFinder evicts the least recently used block to evict
type LRUVictimFinder struct {
}

// NewLRUVictimFinder returns a newly constructed lru evictor
func NewLRUVictimFinder() *LRUVictimFinder {
	e := new(LRUVictimFinder)
	return e
}

// FindVictim returns the least recently used block in a set
func (e *LRUVictimFinder) FindVictim(set *CohSet) *CohEntry {
	// First try evicting an empty block
	for _, block := range set.LRUQueue {
		if !block.IsValidEntry() {
			return block
		}
	}

	for _, block := range set.LRUQueue {
		if !block.IsLockedEntry() {
			return block
		}
	}

	return set.LRUQueue[0]
}

// // A VictimFinder decides with block should be evicted
// type CohVictimFinder interface {
// 	FindCohVictim(set *CohSet) *CohEntry
// }

// // LRUVictimFinder evicts the least recently used block to evict
// type LRUCohVictimFinder struct {
// }

// // NewLRUVictimFinder returns a newly constructed lru evictor
// func NewLRUCohVictimFinder() *LRUCohVictimFinder {
// 	e := new(LRUCohVictimFinder)
// 	return e
// }

// // FindVictim returns the least recently used block in a set
// func (e *LRUCohVictimFinder) FindCohVictim(set *CohSet) *CohEntry {
// 	// First try evicting an empty block
// 	for _, block := range set.LRUQueue {
// 		if !block.IsValidEntry() {
// 			return block
// 		}
// 	}

// 	for _, block := range set.LRUQueue {
// 		if !block.IsLockedEntry() {
// 			return block
// 		}
// 	}

// 	return set.LRUQueue[0]
// }
