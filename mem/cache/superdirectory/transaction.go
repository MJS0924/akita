package superdirectory

import (
	"github.com/sarchlab/akita/v4/mem/cache"
	"github.com/sarchlab/akita/v4/mem/cache/superdirectory/internal"
	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
)

type action int

const (
	Nothing action = iota

	InsertNewEntry
	EvictAndInsertNewEntry
	UpdateEntry
	InvalidateAndUpdateEntry

	InsertDemotionEntry
	InsertPromotionEntry
	EvictAndPromotionEntry
	EvictAndDemotionEntry

	InvalidateEntry
	BypassingDirectory

	QueryLowerBankForPromotion // [추가] 하위 뱅크에 Sharer 탐색 요청
	PromotionGatherAck         // [추가] 하위 뱅크로부터 Sharer 수집 완료 응답
)

type transaction struct {
	action

	id        string
	fromLocal bool
	toLocal   bool

	bankID      int
	bankList    []int
	banksChecked int // doWrite()가 호출된 횟수 (실제로 몇 개의 bank를 확인했는지)
	read     *mem.ReadReq
	write    *mem.WriteReq
	flush    *cache.FlushReq
	block    *internal.CohEntry
	blockIdx int
	// EvictAndInsertNewEntry인 경우, entry 전체를 eviction
	victim internal.CohEntry // bankID로 bank 정보 알 수 있음

	// InvalidateAndUpdateEntry인 경우, 하나의 subentry에 대해 invalidation
	evictingPID      vm.PID
	evictingAddr     uint64
	invalidationList []sim.RemotePort
	pendingEviction  []sim.RemotePort
	numInv           uint64
	accessed         uint64

	fetchingPID  vm.PID
	fetchingAddr uint64
	reqToBottom  []*mem.AccessReq
	ack          uint64

	mshrEntry *internal.MSHREntry

	entryProcessingPromotion bool
	sharers                  []sim.RemotePort
	needToDemotion           bool

	// bfEagerInserted is set true when acceptNewTransaction eagerly called
	// InsertBloomfilter before the transaction enters the bank pipeline.
	// writeToBank checks this flag to avoid incrementing the same BF counter twice.
	bfEagerInserted bool
}

func (t transaction) accessReq() mem.AccessReq {
	if t.read != nil {
		return t.read
	}

	if t.write != nil {
		return t.write
	}

	return nil
}

func (t transaction) isReadTrans() bool {
	if t.read != nil {
		return true
	}

	return false
}

func (t transaction) req() sim.Msg {
	if t.accessReq() != nil {
		return t.accessReq()
	}

	if t.flush != nil {
		return t.flush
	}

	return nil
}

func (t *transaction) DeepCopy() *transaction {
	if t == nil {
		return nil
	}

	// 1. 값 타입(Value type) 및 단순 포인터 1차 복사 (얕은 복사 베이스)
	// (action, id, bankID, blockIdx, evictingPID 등 기본형 필드 일괄 복사)
	newTrans := *t

	// 2. 슬라이스(Slice) 필드 완벽한 깊은 복사 (메모리 완전 분리)
	if t.bankList != nil {
		newTrans.bankList = make([]int, len(t.bankList))
		copy(newTrans.bankList, t.bankList)
	}

	if t.invalidationList != nil {
		newTrans.invalidationList = make([]sim.RemotePort, len(t.invalidationList))
		copy(newTrans.invalidationList, t.invalidationList)
	}

	if t.pendingEviction != nil {
		newTrans.pendingEviction = make([]sim.RemotePort, len(t.pendingEviction))
		copy(newTrans.pendingEviction, t.pendingEviction)
	}

	if t.reqToBottom != nil {
		// 포인터 배열 자체를 분리 (포인터가 가리키는 Request 객체 자체는 공유됨)
		newTrans.reqToBottom = make([]*mem.AccessReq, len(t.reqToBottom))
		copy(newTrans.reqToBottom, t.reqToBottom)
	}

	if t.sharers != nil {
		newTrans.sharers = make([]sim.RemotePort, len(t.sharers))
		copy(newTrans.sharers, t.sharers)
	}

	// 3. 값 타입(Value)인 victim 깊은 복사
	// victim은 포인터가 아니므로 주소값(&)을 넘겨 DeepCopy 후 반환된 포인터를 역참조(*)하여 저장
	copiedVictim := (&t.victim).DeepCopy()
	if copiedVictim != nil {
		newTrans.victim = *copiedVictim
	}

	// 참고: read, write, flush, mshrEntry block 등은 복사하지 않고 원본 주소를 그대로 유지합니다.
	// (시뮬레이터에서 원본 Request나 MSHR 큐의 흐름(ID 매핑 등)은 추적되어야 하기 때문입니다.)

	return &newTrans
}

type invTrans struct {
	req      *mem.InvReq
	ack      int
	numInv   uint64
	accessed uint64
}
