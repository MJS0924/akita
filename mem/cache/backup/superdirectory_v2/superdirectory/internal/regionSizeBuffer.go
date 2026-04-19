package internal

type RegionSizeBufferEntry struct {
	Addr     uint64 // page 단위로 align된 값이 들어와야 함
	RegionID int
}

type RegionSizeBuffer struct {
	numEntries   uint64
	entries      []RegionSizeBufferEntry
	log2PageSize uint64
}

// NewCountingBloomFilter는 새로운 블룸 필터를 생성합니다.
// numEntries는 전체 카운터 배열의 크기(m)입니다.
func NewRegionSizeBuffer(numEntries uint64, pageSize uint64) *RegionSizeBuffer {
	return &RegionSizeBuffer{
		numEntries:   numEntries,
		entries:      make([]RegionSizeBufferEntry, numEntries),
		log2PageSize: pageSize,
	}
}

func (b *RegionSizeBuffer) Search(addr uint64) RegionSizeBufferEntry {
	addr = addr >> b.log2PageSize
	for _, entry := range b.entries {
		if entry.Addr == addr {
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
	for i := 0; i < len(b.entries); i++ {
		if b.entries[i] == e {
			b.entries = append(b.entries[:i], b.entries[i+1:]...)
		}

		i--
	}
}

func (b *RegionSizeBuffer) Update(addr uint64, regionID int) {
	e := b.Search(addr)
	b.Delete(e)

	b.Push(addr, regionID)
}

func (b *RegionSizeBuffer) GetEntries() *[]RegionSizeBufferEntry {
	return &b.entries
}
