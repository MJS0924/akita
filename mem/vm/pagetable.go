package vm

import (
	"container/list"
	"fmt"
	"strings"
	"sync"
)

// PID stands for Process ID.
type PID uint32

// A Page is an entry in the page table, maintaining the information about how
// to translate a virtual address to a physical address.
type Page struct {
	PID         PID
	PAddr       uint64
	VAddr       uint64
	PageSize    uint64
	Valid       bool
	DeviceID    uint64
	Unified     bool
	IsMigrating bool
	IsPinned    bool
	IsShared    bool
	SharedPages []Page

	regionLen int
}

// A PageTable holds the a list of pages.
type PageTable interface {
	Insert(page Page)
	Remove(pid PID, vAddr uint64)
	Find(pid PID, Addr uint64) (Page, bool)
	Update(page Page)
	ReverseLookup(pAddr uint64) (Page, bool)
}

// NewPageTable creates a new PageTable.
func NewPageTable(log2PageSize uint64) PageTable {
	return &pageTableImpl{
		log2PageSize: log2PageSize,
		tables:       make(map[PID]*processTable),
	}
}

// pageTableImpl is the default implementation of a Page Table
type pageTableImpl struct {
	sync.Mutex

	log2PageSize uint64
	tables       map[PID]*processTable
}

func (pt *pageTableImpl) getTable(pid PID) *processTable {
	pt.Lock()
	defer pt.Unlock()

	table, found := pt.tables[pid]
	if !found {
		table = &processTable{
			entries:      list.New(),
			entriesTable: make(map[uint64]*list.Element),
		}
		pt.tables[pid] = table
	}

	return table
}

func (pt *pageTableImpl) alignToPage(addr uint64) uint64 {
	return (addr >> pt.log2PageSize) << pt.log2PageSize
}

// GetLog2PageSize returns the log2 page size for this page table.
// This method allows the MMU to validate page size consistency.
func (pt *pageTableImpl) GetLog2PageSize() uint64 {
	return pt.log2PageSize
}

// Insert put a new page into the PageTable
func (pt *pageTableImpl) Insert(page Page) {
	table := pt.getTable(page.PID)
	table.insert(page)
}

// Remove removes the entry in the page table that contains the target
// address.
func (pt *pageTableImpl) Remove(pid PID, vAddr uint64) {
	table := pt.getTable(pid)
	table.remove(vAddr)
}

// Find returns the page that contains the given virtual address. The bool
// return value invicates if the page is found or not.
func (pt *pageTableImpl) Find(pid PID, vAddr uint64) (Page, bool) {
	table := pt.getTable(pid)
	vAddr = pt.alignToPage(vAddr)

	return table.find(vAddr)
}

// Update changes the field of an existing page. The PID and the VAddr field
// will be used to locate the page to update.
func (pt *pageTableImpl) Update(page Page) {
	table := pt.getTable(page.PID)
	table.update(page)
}

// ReverseLookup finds a page by its physical address across all processes.
func (pt *pageTableImpl) ReverseLookup(pAddr uint64) (Page, bool) {
	pt.Lock()
	defer pt.Unlock()

	for _, processTable := range pt.tables {
		if page, found := processTable.reverseLookup(pAddr); found {
			return page, true
		}
	}
	return Page{}, false
}

type processTable struct {
	sync.Mutex

	entries      *list.List
	entriesTable map[uint64]*list.Element
}

func (t *processTable) insert(page Page) bool {
	t.Lock()
	defer t.Unlock()

	if !t.pageMustNotExist(page.VAddr) {
		return false
	}

	elem := t.entries.PushBack(page)
	t.entriesTable[page.VAddr] = elem

	return true
}

func (t *processTable) remove(vAddr uint64) {
	t.Lock()
	defer t.Unlock()

	t.pageMustExist(vAddr)

	elem := t.entriesTable[vAddr]
	t.entries.Remove(elem)
	delete(t.entriesTable, vAddr)
}

func (t *processTable) update(page Page) {
	t.Lock()
	defer t.Unlock()

	t.pageMustExist(page.VAddr)

	elem := t.entriesTable[page.VAddr]
	elem.Value = page
}

func (t *processTable) find(vAddr uint64) (Page, bool) {
	t.Lock()
	defer t.Unlock()

	elem, found := t.entriesTable[vAddr]
	if found {
		return elem.Value.(Page), true
	}

	return Page{}, false
}

func (t *processTable) reverseLookup(pAddr uint64) (Page, bool) {
	t.Lock()
	defer t.Unlock()

	for elem := t.entries.Front(); elem != nil; elem = elem.Next() {
		page := elem.Value.(Page)
		if page.PAddr == pAddr {
			return page, true
		}
	}

	return Page{}, false
}

func (t *processTable) pageMustExist(vAddr uint64) {
	_, found := t.entriesTable[vAddr]
	if !found {
		panic("page does not exist")
	}
}

func (t *processTable) pageMustNotExist(vAddr uint64) bool {
	_, found := t.entriesTable[vAddr]
	if found {
		panic("[PageTable]\tWarning: Try to insert exist page\n")
	}

	return true
}

// // A PageTable holds the a list of pages.
type LevelPageTable interface {
	Insert(page Page) bool
	Remove(pid PID, vAddr uint64)
	Find(pid PID, Addr uint64) (Page, bool)
	FindStepByStep(pid PID, Addr uint64) (LevelPageTable, bool)
	Update(page Page)
	ReverseLookup(pAddr uint64) (Page, bool)
	GetLevel() int
	GetName() string
}

// NewPageTable creates a new PageTable.
func NewLevelPageTable(log2PageSize uint64, level int, name string) LevelPageTable {
	if level == 1 {
		return &LevelPageTableImpl{
			name:          name,
			level:         level,
			log2PageSize:  log2PageSize,
			nextPageTable: nil,
			pageTable: &processTable{
				entries:      &list.List{},
				entriesTable: make(map[uint64]*list.Element),
			},
		}
	} else {
		return &LevelPageTableImpl{
			name:          name,
			level:         level,
			log2PageSize:  log2PageSize,
			nextPageTable: make(map[uint64]LevelPageTable, 512),
			pageTable:     nil,
		}
	}
}

type LevelPageTableImpl struct {
	sync.Mutex

	name          string
	log2PageSize  uint64
	level         int
	nextPageTable map[uint64]LevelPageTable
	pageTable     *processTable
}

// GetLog2PageSize returns the log2 page size for this page table.
// This method allows the MMU to validate page size consistency.
func (pt *LevelPageTableImpl) GetLog2PageSize() uint64 {
	return pt.log2PageSize
}

func AddrSlice(VAddr uint64, lv int, log2PageSize uint64) uint64 {
	var mask, addr uint64
	if lv == 1 {
		mask = (1 << 21) - 1
		mask = mask >> log2PageSize
		addr = VAddr >> log2PageSize
	} else {
		mask = (1 << 9) - 1
		addr = VAddr >> (3 + lv*9)
	}
	// level 1: 12 + (1-1)*9 = 12
	// level 2: 12 + (2-1)*9 = 21
	// level 3: 12 + (3-1)*9 = 30
	// level 4: 12 + (4-1)*9 = 39
	// level 5: 12 + (5-1)*9 = 48

	return mask & addr
}

func (pt *LevelPageTableImpl) Insert(page Page) bool {
	pt.Lock()
	defer pt.Unlock()

	if pt.level == 1 {
		return pt.pageTable.insert(page)
	} else {
		var index uint64
		if pt.level == 6 {
			index = uint64(page.PID)
		} else {
			index = AddrSlice(page.VAddr, pt.level, pt.log2PageSize)
		}

		if pt.nextPageTable[index] == nil {
			pt.nextPageTable[index] = NewLevelPageTable(
				pt.log2PageSize, pt.level-1, fmt.Sprintf("%s[%d].L%d", pt.name, index, pt.level-1))
		}

		return pt.nextPageTable[index].Insert(page)
	}
}

func (pt *LevelPageTableImpl) Remove(pid PID, vAddr uint64) {
	pt.Lock()
	defer pt.Unlock()

	if pt.level == 1 {
		pt.pageTable.remove(vAddr)
	} else {
		var index uint64
		if pt.level == 6 {
			index = uint64(pid)
		} else {
			index = AddrSlice(vAddr, pt.level, pt.log2PageSize)
		}

		if pt.nextPageTable[index] == nil {
			if strings.Contains(pt.name, "GMMU[1].PT") {
				fmt.Printf("[PageTable]\tWarning: Try to remove absence PTE: %d - %x\n", pid, vAddr)
			}
			return
		}

		pt.nextPageTable[index].Remove(pid, vAddr)
	}
}

func (pt *LevelPageTableImpl) Find(pid PID, Addr uint64) (Page, bool) {
	pt.Lock()
	defer pt.Unlock()

	var rslt Page

	if pt.level == 1 {
		alignedAddr := (Addr >> pt.log2PageSize) << pt.log2PageSize
		page, found := pt.pageTable.find(alignedAddr)
		return page, found
	} else {
		var index uint64
		if pt.level == 6 {
			index = uint64(pid)
		} else {
			index = AddrSlice(Addr, pt.level, pt.log2PageSize)
		}

		if pt.nextPageTable[index] == nil {
			return rslt, false
		}

		return pt.nextPageTable[index].Find(pid, Addr)
	}
}

func (pt *LevelPageTableImpl) FindStepByStep(pid PID, Addr uint64) (LevelPageTable, bool) {
	// L2 entry까지만 지원
	pt.Lock()
	defer pt.Unlock()

	if pt.level == 1 {
		return nil, false
	} else {
		var index uint64
		if pt.level == 6 {
			index = uint64(pid)
		} else {
			index = AddrSlice(Addr, pt.level, pt.log2PageSize)
		}

		nextPT, found := pt.nextPageTable[index]

		if !found {
			return nil, false
		}
		return nextPT, true
	}
}

func (pt *LevelPageTableImpl) Update(page Page) {
	pt.Lock()
	defer pt.Unlock()

	if pt.level == 1 {
		pt.pageTable.update(page)
	} else {
		var index uint64
		if pt.level == 6 {
			index = uint64(page.PID)
		} else {
			index = AddrSlice(page.VAddr, pt.level, pt.log2PageSize)
		}

		if pt.nextPageTable[index] == nil {
			return
		}

		pt.nextPageTable[index].Update(page)
	}
}

func (pt *LevelPageTableImpl) ReverseLookup(pAddr uint64) (Page, bool) {
	pt.Lock()
	defer pt.Unlock()

	var temp Page

	if pt.level == 1 {
		if pt.pageTable != nil {
			return pt.pageTable.reverseLookup(pAddr)
		}
	} else {
		for _, npt := range pt.nextPageTable {
			if npt == nil {
				continue
			}

			page, found := npt.ReverseLookup(pAddr)
			if found {
				return page, found
			}
		}
	}

	return temp, false
}

func (pt *LevelPageTableImpl) GetLevel() int {
	return pt.level
}

func (pt *LevelPageTableImpl) GetName() string {
	return pt.name
}
