package internal

type RegionSizeBufferEntry struct {
	Addr     uint64 // page 단위로 align된 값이 들어와야 함
	RegionID int
}

type RegionSizeBuffer struct {
	numEntries   uint64
	entries      []RegionSizeBufferEntry
	log2PageSize uint64
	regionLen    []int
	disabled     bool
}

// NewCountingBloomFilter는 새로운 블룸 필터를 생성합니다.
// numEntries는 전체 카운터 배열의 크기(m)입니다.
func NewRegionSizeBuffer(numEntries uint64, pageSize uint64, regionLen []int, disabled bool) *RegionSizeBuffer {
	return &RegionSizeBuffer{
		numEntries:   numEntries,
		entries:      make([]RegionSizeBufferEntry, numEntries),
		log2PageSize: pageSize,
		regionLen:    regionLen,
		disabled:     disabled,
	}
}

func (b *RegionSizeBuffer) Reset() {
	b.entries = make([]RegionSizeBufferEntry, b.numEntries)
}

func (b *RegionSizeBuffer) Search(addr uint64) RegionSizeBufferEntry {
	if b.disabled {
		return RegionSizeBufferEntry{RegionID: -1}
	}
	addr = addr >> b.log2PageSize
	for _, entry := range b.entries {
		mask := b.regionLen[entry.RegionID]
		if entry.Addr>>mask == addr>>mask {
			return entry
		}
	}

	return RegionSizeBufferEntry{RegionID: -1}
}

func (b *RegionSizeBuffer) Push(addr uint64, regionID int) {
	addr = addr >> b.log2PageSize
	entry := RegionSizeBufferEntry{
		Addr:     addr,
		RegionID: regionID,
	}

	b.entries = append(b.entries, entry)

	over := len(b.entries) - int(b.numEntries)
	if over > 0 {
		b.entries = b.entries[over:]
	}
}

func (b *RegionSizeBuffer) Delete(e RegionSizeBufferEntry) {
	// n은 살아남을(삭제되지 않을) 요소들이 들어갈 인덱스입니다.
	n := 0
	for _, val := range b.entries {
		if val != e {
			// 삭제 대상이 아니면 앞쪽부터 차곡차곡 쌓습니다.
			b.entries[n] = val
			n++
		}
	}
	// 살아남은 요소들까지만 잘라서 슬라이스를 재할당합니다. (메모리 낭비 방지)
	b.entries = b.entries[:n]
}

func (b *RegionSizeBuffer) Update(addr uint64, regionID int) {
	if b.disabled {
		return
	}
	e := b.Search(addr)
	b.Delete(e)

	b.Push(addr, regionID)
}

func (b *RegionSizeBuffer) GetEntries() *[]RegionSizeBufferEntry {
	return &b.entries
}

// InvalidateForPromotion removes stale RSB entries that would mis-route requests
// to bank >= prevBankID when the block covering addr has been promoted to
// bank prevBankID-1. An RSB entry is stale when its RegionID >= prevBankID AND
// its page-level address falls within the same region as addr at that bank's
// granularity.
func (b *RegionSizeBuffer) InvalidateForPromotion(addr uint64, prevBankID int) {
	if b.disabled {
		return
	}
	pageAddr := addr >> b.log2PageSize
	n := 0
	for _, e := range b.entries {
		if e.RegionID >= prevBankID {
			mask := b.regionLen[e.RegionID]
			if e.Addr>>mask == pageAddr>>mask {
				continue // stale entry — drop it
			}
		}
		b.entries[n] = e
		n++
	}
	b.entries = b.entries[:n]
}
