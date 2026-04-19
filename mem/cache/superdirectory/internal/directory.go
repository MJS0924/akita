package internal

import (
	"fmt"

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
	SubEntry     [4]CohSubEntry
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

// 포인터 리시버(*CohEntry)를 사용하여 불필요한 메모리 복사를 방지합니다.
// threshold에 따라 다르게 판단하는 로직이 필요할 수도 있음..
func (c *CohEntry) AbleToPromotion() bool {
	sh0 := c.SubEntry[0].Sharer
	len0 := len(sh0)

	if len0 == 0 {
		return false
	}

	if !c.IsValid {
		return false
	}
	for i := range c.SubEntry {
		if !c.SubEntry[i].IsValid {
			return false
		}
	}

	for i := range c.SubEntry {
		shI := c.SubEntry[i].Sharer

		if len(shI) != len0 {
			return false
		}

		for _, targetSharer := range shI {
			found := false
			for _, baseSharer := range sh0 {
				if targetSharer == baseSharer {
					found = true
					break
				}
			}

			if !found {
				return false
			}
		}
	}

	return true
}

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

type SuperDirectory interface {
	Lookup(bankID int, PID vm.PID, reqAddr uint64) (*CohEntry, int)
	GetBank(reqAddr uint64) (bankID []int)
	GetSet(bankID int, reqAddr uint64) (set *CohSet, setID int)
	GetBanks() [][]CohSet

	FindVictim(bankID int, pid vm.PID, addr uint64) (*CohEntry, bool)
	Visit(bankID int, entry *CohEntry)

	TotalSize() uint64
	WayAssociativity() int
	GetRegionLen(regionID int) int

	InsertBloomfilter(regionID int, addr uint64)
	EvictBloomfilter(regionID int, addr uint64)

	Reset()
}

type SuperDirectoryImpl struct {
	NumBanks        int
	NumSets         []int
	NumWays         int
	log2NumSubEntry int
	BlockSize       int
	AddrConverter   mem.AddressConverter

	BFSize      uint64
	bloomFilter []*CountingBloomFilter

	Sets      [][]CohSet // 1st index: bank, 2nd index: set
	RegionLen []int

	victimFinder     VictimFinder
	isIdealDirectory bool
}

func NewSuperDirectory(
	bank, set, way, blockSize, log2NumSubEntry, BFSize int,
	victimFinder VictimFinder, regionLen []int,
) *SuperDirectoryImpl {
	d := new(SuperDirectoryImpl)
	d.victimFinder = victimFinder

	d.NumBanks = bank
	d.NumSets = make([]int, bank)
	d.NumWays = way
	d.BlockSize = blockSize
	d.log2NumSubEntry = log2NumSubEntry
	d.isIdealDirectory = false

	for i := 0; i < bank; i++ {
		if set>>bank<<i == 0 {
			panic("ERROR: Too small set number")
		}

		d.NumSets[i] = set >> bank << i
	}

	d.BFSize = uint64(BFSize)
	for i := 0; i < bank; i++ {
		d.bloomFilter = append(d.bloomFilter, NewCountingBloomFilter(i, d.BFSize))
	}

	for i := 0; i < bank; i++ {
		d.Sets = append(d.Sets, make([]CohSet, d.NumSets[i]))
	}
	d.RegionLen = regionLen
	if len(d.RegionLen) != d.NumBanks {
		panic("ERR: len(d.regionLen) doesn't match d.NumBanks")
	}

	d.Reset()

	fmt.Printf("[Directory]\tBuild new coherence directory: %d banks, %d sets, %d ways\n", d.NumBanks, d.NumSets, d.NumWays)

	return d
}

func (d *SuperDirectoryImpl) TotalSize() uint64 {
	// return uint64(d.NumBanks) * uint64(d.NumSets) * uint64(d.NumWays) * uint64(d.BlockSize)
	return uint64(d.NumBanks) * uint64(d.NumSets[d.NumBanks-1]*2) * uint64(d.NumWays) * uint64(d.BlockSize)
}

func (d *SuperDirectoryImpl) GetBank(reqAddr uint64) (bankID []int) {
	// fine-grain bank(높은 인덱스)부터 확인하여, data가 집중되는 finest bank를
	// acceptNewTransaction()이 가장 먼저 시도하도록 순서를 반환한다.
	for i := d.NumBanks - 1; i >= 0; i-- {
		present := d.bloomFilter[i].Search(reqAddr, d.RegionLen[i])
		if present {
			bankID = append(bankID, i)
		}
	}

	return
}

func (d *SuperDirectoryImpl) GetSet(bankID int, reqAddr uint64) (set *CohSet, setID int) {
	// 2. bankID에 해당하는 bank에서 set 얻기
	if d.AddrConverter != nil {
		reqAddr = d.AddrConverter.ConvertExternalToInternal(reqAddr)
	}

	len := d.RegionLen[bankID] + d.log2NumSubEntry
	setID = int(reqAddr) / (1 << len) % d.NumSets[bankID]
	set = &d.Sets[bankID][setID]

	return
}

func (d *SuperDirectoryImpl) Lookup(bankID int, PID vm.PID, reqAddr uint64) (*CohEntry, int) {
	maskLen := d.RegionLen[bankID] + d.log2NumSubEntry
	set, _ := d.GetSet(bankID, reqAddr)
	for _, entry := range set.CohEntries {
		if entry.IsValid && entry.Tag>>maskLen == reqAddr>>maskLen && entry.PID == PID {
			index := (int(reqAddr) >> d.RegionLen[bankID]) % (1 << d.log2NumSubEntry)
			if !entry.SubEntry[index].IsValid {
				return nil, -1
			}
			return entry, index
		}
	}

	return nil, -1
}

func (d *SuperDirectoryImpl) FindVictim(bankID int, PID vm.PID, addr uint64) (*CohEntry, bool) {
	set, _ := d.GetSet(bankID, addr)

	// addr을 포함하는 entry가 있는 경우 해당 entry를 return
	maskLen := d.RegionLen[bankID] + d.log2NumSubEntry
	for _, entry := range set.CohEntries {
		if entry.IsValid && entry.Tag>>maskLen == addr>>maskLen && entry.PID == PID {
			return entry, false
		}
	}

	// invalid entry까지 포함하여 addr을 포함하는 entry를 search
	for _, entry := range set.CohEntries {
		if entry.Tag>>maskLen == addr>>maskLen && entry.PID == PID {
			return entry, true // 아마도 invalid 일 것 -> 초기화 후 사용해야 함
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

func (d *SuperDirectoryImpl) Visit(bankID int, entry *CohEntry) {
	set := &d.Sets[bankID][entry.SetID]

	for i, e := range set.LRUQueue {
		if e == entry {
			set.LRUQueue = append(set.LRUQueue[:i], set.LRUQueue[i+1:]...)
			break
		}
	}

	set.LRUQueue = append(set.LRUQueue, entry)
}

func (d *SuperDirectoryImpl) GetBanks() [][]CohSet {
	return d.Sets
}

func (d *SuperDirectoryImpl) Reset() {
	for i := 0; i < d.NumBanks; i++ {
		for j := 0; j < d.NumSets[i]; j++ {
			for k := 0; k < d.NumWays; k++ {
				entry := new(CohEntry)
				entry.IsValid = false
				entry.SetID = j
				entry.WayID = k
				entry.CacheAddress = uint64(j*d.NumWays+k) * uint64(d.BlockSize) * (1 << d.log2NumSubEntry)
				d.Sets[i][j].CohEntries = append(d.Sets[i][j].CohEntries, entry)
				d.Sets[i][j].LRUQueue = append(d.Sets[i][j].LRUQueue, entry)
			}
		}
	}

	for _, f := range d.bloomFilter {
		f.Reset()
	}
}

func (d *SuperDirectoryImpl) WayAssociativity() int {
	return d.NumWays
}

func (d *SuperDirectoryImpl) GetRegionLen(regionID int) int {
	if regionID >= len(d.RegionLen) {
		return -1
	}

	return d.RegionLen[regionID]
}

func (d *SuperDirectoryImpl) InsertBloomfilter(regionID int, addr uint64) {
	d.bloomFilter[regionID].Insert(addr, d.RegionLen[regionID])
}

func (d *SuperDirectoryImpl) EvictBloomfilter(regionID int, addr uint64) {
	d.bloomFilter[regionID].Evict(addr, d.RegionLen[regionID])
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
