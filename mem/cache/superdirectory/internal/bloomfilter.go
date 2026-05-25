package internal

// CountingBloomFilter: L-CBF style (Mehta et al. 2008), 4-bit counters.
// k=3 hash functions, m/n = 4 (target ~15% FPR). Only coarse banks
// (0,1,2) instantiate a filter; finer banks pass nil.
//
// Counter width 4 bits (max 15) — matches the original L-CBF spec.
// Simple sticky-at-max (cannot decrement once a slot reaches 15) is kept
// purely as a FN-safety guard for the rare overflow case. With m/n=4
// the steady-state probability of any slot reaching 15 is very low, so
// sticky rarely fires in practice.
type CountingBloomFilter struct {
	bankID     int
	numEntries uint64
	counters   []uint64 // 4-bit counters, value range 0..15
}

const cbfNumHashes = 3
const cbfCounterMax = 15 // 4-bit counter saturates at 15

// Three multiplicative-hash mixing constants. Each is a 64-bit odd integer
// with good avalanche behavior (golden-ratio multiplier, Murmur3 finalizer,
// xxHash finalizer). Using *different* constants guarantees the three
// hashes don't collapse to the same index for small addresses — the
// failure mode that broke the previous XOR-fold variant when called with
// addr-derived region IDs in the low-magnitude range.
const (
	cbfMix0 uint64 = 0x9E3779B185EBCA87
	cbfMix1 uint64 = 0xC2B2AE3D27D4EB4F
	cbfMix2 uint64 = 0x165667B19E3779F9
)

// NewCountingBloomFilter는 새로운 블룸 필터를 생성합니다.
// numEntries는 전체 카운터 배열의 크기(m)입니다.
func NewCountingBloomFilter(bankID int, numEntries uint64) *CountingBloomFilter {
	return &CountingBloomFilter{
		bankID:     bankID,
		numEntries: numEntries,
		counters:   make([]uint64, numEntries),
	}
}

// hash returns k=3 independent indices via multiplicative hashing.
// numMaskBit drops the in-region bits that don't distinguish entries at
// this bank's granularity. Upper 32 of the 64-bit product is used as the
// hash output (high bits absorb the avalanche from the multiplier).
func (cbf *CountingBloomFilter) hash(addr uint64, numMaskBit int) [cbfNumHashes]uint64 {
	a := addr >> uint(numMaskBit)
	return [cbfNumHashes]uint64{
		((a * cbfMix0) >> 32) % cbf.numEntries,
		((a * cbfMix1) >> 32) % cbf.numEntries,
		((a * cbfMix2) >> 32) % cbf.numEntries,
	}
}

// Insert: 캐시에 데이터가 들어올 때 호출합니다. (카운터 증가)
// 4-bit 카운터는 cbfCounterMax(=15)에 도달하면 더 이상 증가하지 않습니다.
func (cbf *CountingBloomFilter) Insert(addr uint64, numMaskBit int) {
	indices := cbf.hash(addr, numMaskBit)
	for _, idx := range indices {
		if cbf.counters[idx] < cbfCounterMax {
			cbf.counters[idx]++
		}
	}
}

// Search: 데이터가 캐시에 존재하는지 확인합니다. (존재 가능성 검사)
func (cbf *CountingBloomFilter) Search(addr uint64, numMaskBit int) bool {
	indices := cbf.hash(addr, numMaskBit)
	for _, idx := range indices {
		// k개의 위치 중 단 하나라도 0이면, 100% 없는 것(Definitely Not)
		if cbf.counters[idx] == 0 {
			return false
		}
	}
	// 모두 1 이상이면 있을 수도 있음(Possibly in set)
	return true
}

// Evict: 캐시에서 데이터가 쫓겨날 때 호출합니다. (카운터 감소)
// Sticky-at-max: counter == cbfCounterMax(=15) 인 슬롯은 decrement 하지
// 않음. m/n=4 정상 워크로드에선 발동 확률이 낮고, 만일의 overflow 시
// FN 방지를 위해 보수적으로 유지합니다.
func (cbf *CountingBloomFilter) Evict(addr uint64, numMaskBit int) {
	indices := cbf.hash(addr, numMaskBit)
	for _, idx := range indices {
		if cbf.counters[idx] == cbfCounterMax {
			continue // sticky: 진짜 overflow 시 보호
		}
		if cbf.counters[idx] > 0 {
			cbf.counters[idx]--
		}
	}
}

// NumEntries returns the configured size (m) of this filter.
func (cbf *CountingBloomFilter) NumEntries() uint64 {
	return cbf.numEntries
}

// NumAtMax returns how many slots are currently at cbfCounterMax (and
// therefore sticky for further Evicts). Useful diagnostic — large values
// indicate the m/n ratio is too tight for the workload's transient peaks.
func (cbf *CountingBloomFilter) NumAtMax() uint64 {
	var n uint64
	for _, v := range cbf.counters {
		if v == cbfCounterMax {
			n++
		}
	}
	return n
}

// Reset: 블룸 필터의 모든 카운터를 0으로 초기화합니다.
func (cbf *CountingBloomFilter) Reset() {
	for i := range cbf.counters {
		cbf.counters[i] = 0
	}
}
