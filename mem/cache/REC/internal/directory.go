package internal

import (
	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
)

// A CohEntry of a cache is the information that is associated with a cache line
type CohSubEntry struct {
	IsValid   bool
	IsDirty   bool
	ReadCount int
	IsLocked  bool
	Sharer    []sim.RemotePort
	VAddr     uint64
	DirtyMask []bool
	Accessed  bool
}

type CohEntry struct {
	PID          vm.PID
	Tag          uint64
	WayID        int
	SetID        int
	CacheAddress uint64
	SubEntry     [16]CohSubEntry
	IsValid      bool
	// lock을 subentry가 아니라 entry 단위로 걸어야 할 수도 있음
}

func (c CohEntry) IsValidEntry() bool {
	if !c.IsValid {
		return false
	}

	for _, e := range c.SubEntry {
		if e.IsValid {
			return true
		}
	}

	return false
}

func (c CohEntry) IsLockedEntry() bool {
	for _, e := range c.SubEntry {
		if e.IsLocked {
			return true
		}
	}

	return false
}

func (c CohEntry) GetReadCount() int {
	n := 0
	for _, e := range c.SubEntry {
		n += e.ReadCount
	}

	return n
}

// func (c CohEntry) AbleToPromotion() bool {
// 	count := 0
// 	sh := []sim.RemotePort{}

// 	for _, e := c.SubEntry {
// 		if !e.IsValid {
// 			continue
// 		}

// 		// 각 subentry가 가진 sharer 수를 count
// 		count += len(c.SubEntry.Sharer)

// 		// 모든 subentry에 대해 sharer list 생성
// 		for _, sharer := range c.SubEntry.Sharer {
// 			if !Present(sh, sharer) {
// 				sh = append(sh, sharer)
// 			}
// 		}
// 	}

// 	max := len(sh) * 4
// 	if count*8 >= max*7 {	// threshold: 7/8
// 		return true
// 	}

// 	return false
// }

func Present(list []sim.RemotePort, sh sim.RemotePort) bool {
	for _, entry := range list {
		if entry == sh {
			return true
		}
	}

	return false
}

// A Set is a list of CohEntrys where a certain piece memory can be stored at
type CohSet struct {
	CohEntries []*CohEntry
	LRUQueue   []*CohEntry
}

type RECDirectory interface {
	Lookup(PID vm.PID, reqAddr uint64) (*CohEntry, int)
	GetSet(reqAddr uint64) (set *CohSet, setID int)
	GetSets() []CohSet

	FindVictim(pid vm.PID, addr uint64) (*CohEntry, bool)
	Visit(entry *CohEntry)

	TotalSize() uint64
	WayAssociativity() int

	Reset()
}

type RECDirectoryImpl struct {
	NumSets         int
	NumWays         int
	log2NumSubEntry int
	log2BlockSize   int
	BlockSize       int
	AddrConverter   mem.AddressConverter

	Sets      []CohSet // 1st index: bank, 2nd index: set
	RegionLen []int

	victimFinder     VictimFinder
	isIdealDirectory bool
}

func NewRECDirectory(
	set, way, blockSize, log2NumSubEntry, log2BlockSize int,
	victimFinder VictimFinder,
) *RECDirectoryImpl {
	d := new(RECDirectoryImpl)
	d.victimFinder = victimFinder

	d.NumSets = set
	d.NumWays = way
	d.BlockSize = blockSize
	d.log2BlockSize = log2BlockSize
	d.log2NumSubEntry = log2NumSubEntry
	d.isIdealDirectory = false

	d.Sets = make([]CohSet, set)

	d.Reset()

	return d
}

func (d *RECDirectoryImpl) TotalSize() uint64 {
	return uint64(d.NumSets) * uint64(d.NumWays) * uint64(d.BlockSize)

}

func (d *RECDirectoryImpl) GetSet(reqAddr uint64) (set *CohSet, setID int) {
	// 2. bankID에 해당하는 bank에서 set 얻기
	if d.AddrConverter != nil {
		reqAddr = d.AddrConverter.ConvertExternalToInternal(reqAddr)
	}

	len := d.log2BlockSize + d.log2NumSubEntry
	setID = int(reqAddr) / (1 << len) % d.NumSets
	set = &d.Sets[setID]

	return
}

func (d *RECDirectoryImpl) Lookup(PID vm.PID, reqAddr uint64) (*CohEntry, int) {
	maskLen := d.log2BlockSize + d.log2NumSubEntry
	set, _ := d.GetSet(reqAddr)
	for _, entry := range set.CohEntries {
		if entry.IsValid && entry.Tag>>maskLen == reqAddr>>maskLen && entry.PID == PID {
			index := (int(reqAddr) >> d.log2BlockSize) % (1 << d.log2NumSubEntry)
			return entry, index
		}
	}

	return nil, -1
}

func (d *RECDirectoryImpl) FindVictim(PID vm.PID, addr uint64) (*CohEntry, bool) {
	set, _ := d.GetSet(addr)

	// addr을 포함하는 entry가 있는 경우 해당 entry를 return
	maskLen := d.log2BlockSize + d.log2NumSubEntry
	for _, entry := range set.CohEntries {
		if entry.IsValid && entry.Tag>>maskLen == addr>>maskLen && entry.PID == PID {
			return entry, false
		}
	}

	// invalid entry까지 포함하여 addr을 포함하는 entry를 search
	for _, entry := range set.CohEntries {
		if entry.Tag>>maskLen == addr>>maskLen && entry.PID == PID {
			return entry, true
		}
	}

	if d.isIdealDirectory { // 새로운 entry를 추가하고 해당 entry를 victim으로 사용하기
		entry := d.victimFinder.FindVictim(set)
		return entry, true
	} else {
		entry := d.victimFinder.FindVictim(set)
		return entry, true
	}
}

func (d *RECDirectoryImpl) Visit(entry *CohEntry) {
	set := &d.Sets[entry.SetID]

	for i, e := range set.LRUQueue {
		if e == entry {
			set.LRUQueue = append(set.LRUQueue[:i], set.LRUQueue[i+1:]...)
			break
		}
	}

	set.LRUQueue = append(set.LRUQueue, entry)
}

func (d *RECDirectoryImpl) GetSets() []CohSet {
	return d.Sets
}

func (d *RECDirectoryImpl) Reset() {
	for j := 0; j < d.NumSets; j++ {
		for k := 0; k < d.NumWays; k++ {
			entry := new(CohEntry)
			entry.IsValid = false
			entry.SetID = j
			entry.WayID = k
			entry.CacheAddress = uint64(j*d.NumWays+k) * uint64(d.BlockSize) * (1 << d.log2NumSubEntry)
			d.Sets[j].CohEntries = append(d.Sets[j].CohEntries, entry)
			d.Sets[j].LRUQueue = append(d.Sets[j].LRUQueue, entry)
		}
	}
}

func (d *RECDirectoryImpl) WayAssociativity() int {
	return d.NumWays
}

func (e *CohEntry) DeepCopy() *CohEntry {
	// 1. nil 포인터 방어
	if e == nil {
		return nil
	}

	// 2. 값 타입 필드 및 고정 크기 배열([4]CohSubEntry) 1차 복사
	// (이 시점에서 포인터와 슬라이스 헤더는 여전히 원본을 가리키는 상태임)
	newEntry := *e

	// 3. SubEntry 내부의 참조 타입(슬라이스)들을 순회하며 깊은 복사(Deep Copy) 수행
	for i := range newEntry.SubEntry {
		// 원본 SubEntry 참조
		origSub := &e.SubEntry[i]

		// 3-1. Sharer 슬라이스 깊은 복사
		if origSub.Sharer != nil {
			newSharer := make([]sim.RemotePort, len(origSub.Sharer))
			copy(newSharer, origSub.Sharer)
			newEntry.SubEntry[i].Sharer = newSharer
		}

		// 3-2. DirtyMask 슬라이스 깊은 복사 (구조체에 존재할 경우)
		if origSub.DirtyMask != nil {
			newDirtyMask := make([]bool, len(origSub.DirtyMask))
			copy(newDirtyMask, origSub.DirtyMask)
			newEntry.SubEntry[i].DirtyMask = newDirtyMask
		}

		// 주의: 만약 CohSubEntry 내부에 또 다른 슬라이스나 맵, 포인터가 추가된다면
		// 반드시 이곳에 동일한 방식의 복사 로직을 추가해주어야 합니다.
	}

	return &newEntry
}

func (e *CohEntry) Reset() {
	e.IsValid = false
	for i := range e.SubEntry {
		sub := &e.SubEntry[i]

		// IsValid   bool
		// IsDirty   bool
		// ReadCount int
		// IsLocked  bool
		// Sharer    []sim.RemotePort
		// VAddr     uint64
		// DirtyMask []bool
		// Accessed  bool
		sub.IsValid = false
		sub.IsDirty = false
		sub.ReadCount = 0
		sub.IsLocked = false
		sub.Sharer = nil
		sub.VAddr = 0
		sub.DirtyMask = nil
		sub.Accessed = false
	}
}
