package internal

// CountingBloomFilter 구조체 정의
type CountingBloomFilter struct {
	bankID     int
	numEntries uint64
	counters   []uint64 // 9비트 카운터를 모사하기 위해 uint16 사용 (값은 0~511만 허용)
}

// NewCountingBloomFilter는 새로운 블룸 필터를 생성합니다.
// numEntries는 전체 카운터 배열의 크기(m)입니다.
func NewCountingBloomFilter(bankID int, numEntries uint64) *CountingBloomFilter {
	return &CountingBloomFilter{
		bankID:     bankID,
		numEntries: numEntries,
		counters:   make([]uint64, numEntries),
	}
}

// hash는 42비트 주소(addr)를 기반으로 5개의 독립적인 해시 인덱스를 반환합니다.
// 하드웨어 구현이 쉬운 XOR 폴딩 기법을 모사했습니다.
func (cbf *CountingBloomFilter) hash(addr uint64, numMaskBit int) [5]uint64 {
	mask := uint64(1<<32 - 1) // 하위 32비트 사용
	addr &= mask
	addr >>= numMaskBit

	var indices [5]uint64

	// H0: 6비트 단위의 정석적인 폴딩
	indices[0] = (addr ^ (addr >> 6) ^ (addr >> 12) ^ (addr >> 18)) % cbf.numEntries

	// H1: 3비트 단위로 엇갈리게 섞음 (H0와 비트 중첩 방지)
	indices[1] = (addr ^ (addr >> 3) ^ (addr >> 9) ^ (addr >> 15)) % cbf.numEntries

	// H2: 소수(Prime) 간격을 이용한 폴딩 (5, 10, 16)
	indices[2] = (addr ^ (addr >> 5) ^ (addr >> 10) ^ (addr >> 16)) % cbf.numEntries

	// H3: H2와 다른 비대칭 간격 (2, 7, 14)
	indices[3] = (addr ^ (addr >> 2) ^ (addr >> 7) ^ (addr >> 14)) % cbf.numEntries

	// H4: 넓은 간격과 좁은 간격의 혼합 (4, 11, 17)
	indices[4] = (addr ^ (addr >> 4) ^ (addr >> 11) ^ (addr >> 17)) % cbf.numEntries

	return indices
}

// Insert: 캐시에 데이터가 들어올 때 호출합니다. (카운터 증가)
func (cbf *CountingBloomFilter) Insert(addr uint64, numMaskBit int) {
	indices := cbf.hash(addr, numMaskBit)
	for _, idx := range indices {
		// 4비트 카운터의 최댓값은 15
		if cbf.counters[idx] < 511 {
			cbf.counters[idx]++
		}
	}

	// fmt.Fprintf(os.Stdout, "[CBF %d]\taddr %x, mask %d, indices: %v, counters: %d %d %d %d %d\n",
	// 	cbf.bankID, addr, numMaskBit, indices, cbf.counters[indices[0]], cbf.counters[indices[1]], cbf.counters[indices[2]], cbf.counters[indices[3]], cbf.counters[indices[4]])
}

// Search: 데이터가 캐시에 존재하는지 확인합니다. (존재 가능성 검사)
func (cbf *CountingBloomFilter) Search(addr uint64, numMaskBit int) bool {
	indices := cbf.hash(addr, numMaskBit)
	for _, idx := range indices {
		// 5개의 위치 중 단 하나라도 0이면, 100% 없는 것(Definitely Not)
		if cbf.counters[idx] == 0 {
			return false
		}
	}
	// 모두 1 이상이면 있을 수도 있음(Possibly in set)
	return true
}

// Evict: 캐시에서 데이터가 쫓겨날 때 호출합니다. (카운터 감소)
func (cbf *CountingBloomFilter) Evict(addr uint64, numMaskBit int) {
	indices := cbf.hash(addr, numMaskBit)
	for _, idx := range indices {
		// [핵심] Sticky Counter 방어 로직: 카운터가 15에 도달했다면 값을 빼지 않음 (False Negative 방지)
		if cbf.counters[idx] == 511 {
			continue
		}

		if cbf.counters[idx] > 0 {
			cbf.counters[idx]--
		} else {
			// 시뮬레이터 디버깅용: 0인 카운터를 빼려고 하는 비정상 상황 감지
			// fmt.Printf("[Warning] Eviction applied to address %x, but counter at idx %d is already 0!\n", addr, idx)
		}
	}
}

// Reset: 블룸 필터의 모든 카운터를 0으로 초기화합니다.
func (cbf *CountingBloomFilter) Reset() {
	for i := range cbf.counters {
		cbf.counters[i] = 0
	}
}
