package vm

import "sync"

type Block struct {
	tag   uint64 // index를 tag로 사용
	data1 *LevelPageTable
	data2 *processTable
}

type Set struct {
	sync.Mutex
	Blocks []*Block
}

type UPTC struct {
	Sets      map[string]*Set
	CapOfBlks int
	Latency   int
}

func (u *UPTC) Find(baseAddr string, index uint64) (
	data1 *LevelPageTable,
	data2 *processTable,
) {
	set, found := u.Sets[baseAddr]
	if !found {
		return
	}

	set.Lock()
	defer set.Unlock()

	for i, blk := range set.Blocks {
		if blk.tag == index {
			var temp []*Block
			temp = append(set.Blocks[:i], set.Blocks[i+1:]...)
			set.Blocks = append(temp, blk)

			if blk.data1 != nil {
				return blk.data1, nil
			} else {
				return nil, blk.data2
			}
		}
	}

	return nil, nil
}

func (u *UPTC) Insert(
	baseAddr string,
	index uint64,
	data1 *LevelPageTable,
	data2 *processTable,
) bool {
	set, found := u.Sets[baseAddr]
	if !found {
		u.Sets[baseAddr] = &Set{}
		set = u.Sets[baseAddr]
	}

	return set.Insert(baseAddr, index, data1, data2, u.CapOfBlks)
}

func (s *Set) Insert(
	baseAddr string,
	index uint64,
	data1 *LevelPageTable,
	data2 *processTable,
	cap int,
) bool {
	s.Lock()
	defer s.Unlock()

	blk := &Block{
		tag:   index,
		data1: data1,
		data2: data2,
	}
	s.Blocks = append(s.Blocks, blk)

	if len(s.Blocks) > cap {
		s.Blocks = s.Blocks[1:]
	}

	return true
}

func (u *UPTC) Clear() {
	u.Sets = make(map[string]*Set)
}
