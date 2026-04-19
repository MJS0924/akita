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

	bankID   int
	bankList []int
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
	gatheredSharers          [][]sim.RemotePort // 하위 뱅크에서 응답받은 sharer들 누적
	gatherWaitCnt            int                // 응답 대기 중인 하위 뱅크 수
	isGatherDone             bool               // 모든 하위 뱅크 탐색 완료 여부
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

type invTrans struct {
	req      *mem.InvReq
	ack      int
	numInv   uint64
	accessed uint64
}
