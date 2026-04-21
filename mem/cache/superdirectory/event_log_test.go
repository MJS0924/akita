package superdirectory

import (
	"testing"

	"github.com/sarchlab/akita/v4/mem/cache/superdirectory/internal"
	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/sim"
)

// mockClock implements sim.TimeTeller with a fixed time.
type mockClock struct{ time sim.VTimeInSec }

func (c *mockClock) CurrentTime() sim.VTimeInSec { return c.time }

// newTestMshrStage builds a minimal mshrStage with an EventLogger for tests.
func newTestMshrStage(enableLogging bool) (*mshrStage, *EventLogger) {
	clk := &mockClock{time: 1.0}
	logger := newEventLogger(clk, 4) // numSubEntry = 4
	if enableLogging {
		logger.Enable()
	}

	motionBuf := sim.NewBuffer("TestMotionBuf", 16)

	comp := &Comp{
		numBanks:             testNumBanks,
		regionLen:            testRegionLen,
		log2NumSubEntry:      2, // 1<<2 = 4 subentries
		mshr:                 &mockMSHR{},
		dirStageMotionBuffer: motionBuf,
		eventLogger:          logger,
	}

	return &mshrStage{cache: comp}, logger
}

// buildPromotionTrans creates a transaction whose block passes AbleToPromotion
// in bank 2 (so bankID-1=1 is a valid target).
func buildPromotionTrans(sharer sim.RemotePort) *transaction {
	blk := &internal.CohEntry{IsValid: true}
	for i := range blk.SubEntry {
		blk.SubEntry[i].IsValid = true
		blk.SubEntry[i].Sharer = []sim.RemotePort{sharer}
	}
	blk.Tag = testAddr

	return &transaction{
		bankID:    2,
		block:     blk,
		mshrEntry: internal.NewMSHREntry(),
		read:      mem.ReadReqBuilder{}.WithAddress(testAddr).WithByteSize(64).Build(),
	}
}

// buildDemotionTrans creates a demotion candidate in bank 1 with all 4
// sub-entries valid before the targeted one is cleared.
func buildDemotionTrans(owner sim.RemotePort) *transaction {
	blk := &internal.CohEntry{IsValid: true}
	for i := range blk.SubEntry {
		blk.SubEntry[i].IsValid = true
	}

	read := mem.ReadReqBuilder{}.WithAddress(testAddr).WithByteSize(64).Build()
	read.SetSrcRDMA(owner)
	return &transaction{
		bankID:           1,
		blockIdx:         0,
		block:            blk,
		mshrEntry:        internal.NewMSHREntry(),
		evictingAddr:     testAddr,
		invalidationList: []sim.RemotePort{owner},
		read:             read,
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// D.1.a  Promotion event recorded when logging is ON
// ──────────────────────────────────────────────────────────────────────────────

func TestEventLog_PromotionRecorded(t *testing.T) {
	s, logger := newTestMshrStage(true)

	sharer := sim.RemotePort("GPU0")
	trans := buildPromotionTrans(sharer)
	s.promotionQueue = []*transaction{trans}

	progress := s.insertPromotionEntry()
	if !progress {
		t.Fatal("insertPromotionEntry returned false")
	}

	evs := logger.Events()
	if len(evs) != 1 {
		t.Fatalf("expected 1 event, got %d", len(evs))
	}
	ev := evs[0]
	if ev.Type != MotionEventPromotion {
		t.Errorf("expected Promotion type, got %d", ev.Type)
	}
	if ev.FromBank != 2 || ev.ToBank != 1 {
		t.Errorf("expected bank 2→1, got %d→%d", ev.FromBank, ev.ToBank)
	}
	if ev.SharerCount != 1 {
		t.Errorf("expected SharerCount=1, got %d", ev.SharerCount)
	}
	if ev.ValidSubs != 4 {
		t.Errorf("expected ValidSubs=4, got %d", ev.ValidSubs)
	}
	if ev.Utilization != 1.0 {
		t.Errorf("expected Utilization=1.0, got %f", ev.Utilization)
	}
	if ev.Address != testAddr {
		t.Errorf("expected Address=%x, got %x", testAddr, ev.Address)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// D.1.b  Demotion event recorded when logging is ON
// ──────────────────────────────────────────────────────────────────────────────

func TestEventLog_DemotionRecorded(t *testing.T) {
	s, logger := newTestMshrStage(true)

	owner := sim.RemotePort("GPU0")
	trans := buildDemotionTrans(owner)
	s.demotionQueue = []*transaction{trans}

	progress := s.insertDemotionEntry()
	if !progress {
		t.Fatal("insertDemotionEntry returned false")
	}

	evs := logger.Events()
	if len(evs) != 1 {
		t.Fatalf("expected 1 event, got %d", len(evs))
	}
	ev := evs[0]
	if ev.Type != MotionEventDemotion {
		t.Errorf("expected Demotion type, got %d", ev.Type)
	}
	if ev.FromBank != 1 || ev.ToBank != 2 {
		t.Errorf("expected bank 1→2, got %d→%d", ev.FromBank, ev.ToBank)
	}
	if ev.ValidSubs != 4 {
		t.Errorf("expected ValidSubs=4 (all subs valid before modification), got %d", ev.ValidSubs)
	}
	if ev.Utilization < 0 || ev.Utilization > 1.0 {
		t.Errorf("Utilization out of range: %f", ev.Utilization)
	}
	if ev.Address != testAddr {
		t.Errorf("expected Address=%x, got %x", testAddr, ev.Address)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// D.1.c  Logger OFF → zero events (overhead guard)
// ──────────────────────────────────────────────────────────────────────────────

func TestEventLog_LoggerOff_NoEvents(t *testing.T) {
	s, logger := newTestMshrStage(false) // disabled

	sharer := sim.RemotePort("GPU0")
	trans := buildPromotionTrans(sharer)
	s.promotionQueue = []*transaction{trans}
	s.insertPromotionEntry()

	if len(logger.Events()) != 0 {
		t.Errorf("expected 0 promotion events when logger is off, got %d", len(logger.Events()))
	}

	owner := sim.RemotePort("GPU1")
	dtrans := buildDemotionTrans(owner)
	s.demotionQueue = []*transaction{dtrans}
	s.insertDemotionEntry()

	if len(logger.Events()) != 0 {
		t.Errorf("expected 0 demotion events when logger is off, got %d", len(logger.Events()))
	}
}
