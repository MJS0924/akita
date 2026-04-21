package superdirectory

import "github.com/sarchlab/akita/v4/sim"

// MotionEventType distinguishes promotion from demotion.
type MotionEventType int8

const (
	MotionEventPromotion MotionEventType = 0
	MotionEventDemotion  MotionEventType = 1
)

// MotionEvent records one promotion or demotion at the moment it is committed
// to the dirStageMotionBuffer.
type MotionEvent struct {
	Type        MotionEventType
	Time        float64 // sim.VTimeInSec cast to float64
	Address     uint64
	FromBank    int32
	ToBank      int32
	SharerCount int32   // number of sharers (promotion) or 0 (demotion)
	ValidSubs   int32   // valid sub-entries at emission time
	Utilization float64 // validSubs / numSubEntry
}

// EventLogger accumulates MotionEvents in an in-memory slice.
// When disabled (the default) every method is a no-op, so there is zero
// overhead on production runs.
type EventLogger struct {
	enabled     bool
	clock       sim.TimeTeller
	numSubEntry int
	buf         []MotionEvent
}

func newEventLogger(clock sim.TimeTeller, numSubEntry int) *EventLogger {
	return &EventLogger{clock: clock, numSubEntry: numSubEntry}
}

// Enable activates event collection.
func (l *EventLogger) Enable() {
	l.enabled = true
	if l.buf == nil {
		l.buf = make([]MotionEvent, 0, 4096)
	}
}

// IsEnabled reports whether the logger is active.
func (l *EventLogger) IsEnabled() bool { return l.enabled }

// Events returns the accumulated slice. Do not modify the slice.
func (l *EventLogger) Events() []MotionEvent { return l.buf }

// Reset clears all buffered events without releasing the backing array.
func (l *EventLogger) Reset() { l.buf = l.buf[:0] }

func (l *EventLogger) logPromotion(addr uint64, fromBank, toBank, sharerCount, validSubs int) {
	if !l.enabled {
		return
	}
	util := 1.0
	if l.numSubEntry > 0 {
		util = float64(validSubs) / float64(l.numSubEntry)
	}
	l.buf = append(l.buf, MotionEvent{
		Type:        MotionEventPromotion,
		Time:        float64(l.clock.CurrentTime()),
		Address:     addr,
		FromBank:    int32(fromBank),
		ToBank:      int32(toBank),
		SharerCount: int32(sharerCount),
		ValidSubs:   int32(validSubs),
		Utilization: util,
	})
}

func (l *EventLogger) logDemotion(addr uint64, fromBank, toBank, validSubs int) {
	if !l.enabled {
		return
	}
	util := 0.0
	if l.numSubEntry > 0 {
		util = float64(validSubs) / float64(l.numSubEntry)
	}
	l.buf = append(l.buf, MotionEvent{
		Type:        MotionEventDemotion,
		Time:        float64(l.clock.CurrentTime()),
		Address:     addr,
		FromBank:    int32(fromBank),
		ToBank:      int32(toBank),
		ValidSubs:   int32(validSubs),
		Utilization: util,
	})
}
