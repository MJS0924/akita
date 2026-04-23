package superdirectory

import (
	"testing"

	"github.com/sarchlab/akita/v4/mem/mem"
	"github.com/sarchlab/akita/v4/mem/vm"
	"github.com/sarchlab/akita/v4/sim"
	"github.com/sarchlab/akita/v4/tracing"
)

var _ = vm.PID(0) // ensure vm import is used

// mockTimeTeller controls the current simulation time in tests.
type mockTimeTeller struct {
	currentTime sim.VTimeInSec
}

func (m *mockTimeTeller) CurrentTime() sim.VTimeInSec { return m.currentTime }

// testComp is a minimal NamedHookable that satisfies tracing.NamedHookable.
type testComp struct {
	sim.HookableBase
	name string
}

func (c *testComp) Name() string              { return c.name }
func (c *testComp) InvokeHook(ctx sim.HookCtx) { c.HookableBase.InvokeHook(ctx) }

func newTestComp(name string) *testComp {
	c := &testComp{name: name}
	c.HookableBase = *sim.NewHookableBase()
	return c
}

func dummyReadReq(addr uint64) *mem.ReadReq {
	// RemotePort is a string alias; use named values to avoid sim.NewPort overhead.
	src := sim.RemotePort("GPU[0].L2Cache[0].TopPort")
	dst := sim.RemotePort("GPU[0].SuperDir.TopPort")
	return mem.ReadReqBuilder{}.
		WithSrc(src).
		WithDst(dst).
		WithPID(vm.PID(1)).
		WithAddress(addr).
		WithByteSize(64).
		Build()
}

// TestDirLatencyTracedByTraceReqComplete verifies that AverageTimeTracer
// measures time between TraceReqReceive and TraceReqComplete correctly.
// This is the fix for directories that were missing TraceReqComplete calls.
func TestDirLatencyTracedByTraceReqComplete(t *testing.T) {
	tt := &mockTimeTeller{}
	comp := newTestComp("GPU[0].SuperDir")

	tracer := tracing.NewAverageTimeTracer(
		tt,
		func(task tracing.Task) bool { return task.Kind == "req_in" },
	)
	tracing.CollectTrace(comp, tracer)

	// request 1: arrives at t=0, completes at t=4ns (2 bank checks × 2ns)
	req1 := dummyReadReq(0x1000)
	tt.currentTime = 0
	tracing.TraceReqReceive(req1, comp)
	tt.currentTime = 4e-9
	tracing.TraceReqComplete(req1, comp)

	if got := tracer.AverageTime(); got != 4e-9 {
		t.Errorf("req1: expected 4ns, got %v", got)
	}

	// request 2: arrives at t=10ns, completes at t=20ns (5 bank checks × 2ns)
	req2 := dummyReadReq(0x2000)
	tt.currentTime = 10e-9
	tracing.TraceReqReceive(req2, comp)
	tt.currentTime = 20e-9
	tracing.TraceReqComplete(req2, comp)

	// average of 4ns and 10ns = 7ns
	const wantAvg = 7e-9
	const tol = 1e-15
	got := float64(tracer.AverageTime())
	if diff := got - wantAvg; diff > tol || diff < -tol {
		t.Errorf("average: expected 7ns, got %vns", got)
	}
	if tracer.TotalCount() != 2 {
		t.Errorf("TotalCount: expected 2, got %d", tracer.TotalCount())
	}
}

// TestDirLatencyNotMeasuredWithoutTraceReqComplete verifies that without
// TraceReqComplete the tracer records nothing — confirming the original bug.
func TestDirLatencyNotMeasuredWithoutTraceReqComplete(t *testing.T) {
	tt := &mockTimeTeller{}
	comp := newTestComp("GPU[0].SuperDir")

	tracer := tracing.NewAverageTimeTracer(
		tt,
		func(task tracing.Task) bool { return task.Kind == "req_in" },
	)
	tracing.CollectTrace(comp, tracer)

	req := dummyReadReq(0x3000)
	tt.currentTime = 0
	tracing.TraceReqReceive(req, comp)

	// Advance time but call only TraceReqFinalize (old behaviour — no TraceReqComplete).
	// TraceReqFinalize ends the "_req_out" task, NOT the "req_in" task.
	tt.currentTime = 10e-9
	tracing.TraceReqFinalize(req, comp)

	if tracer.TotalCount() != 0 {
		t.Errorf("expected 0 completed tasks without TraceReqComplete, got %d", tracer.TotalCount())
	}
	if tracer.AverageTime() != 0 {
		t.Errorf("expected 0 average time without TraceReqComplete, got %v", tracer.AverageTime())
	}
}
