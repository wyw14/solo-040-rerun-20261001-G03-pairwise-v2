package leveling

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-96/internal/gate"
	"github.com/wyw14/cry-96/internal/hydraulics"
	"github.com/wyw14/cry-96/internal/journal"
	"github.com/wyw14/cry-96/internal/model"
)

type fakeCycles struct {
	current model.TransitCycle
	has     bool
}

func (f *fakeCycles) Current(string) (model.TransitCycle, bool) { return f.current, f.has }

type fakeRamp struct {
	calls []fakeRampCall
	err   error
}

type fakeRampCall struct {
	cycle    model.TransitCycle
	device   model.DeviceKind
	openings []float64
}

func (f *fakeRamp) RampDevice(_ context.Context, cycle model.TransitCycle, device model.DeviceKind, openings []float64) (hydraulics.RampResult, error) {
	if f.err != nil {
		return hydraulics.RampResult{}, f.err
	}
	f.calls = append(f.calls, fakeRampCall{cycle: cycle, device: device, openings: append([]float64(nil), openings...)})
	return hydraulics.RampResult{Steps: []hydraulics.RampStep{{Target: openings[0], Completed: true}}}, nil
}

type fixture struct {
	clock   *time.Time
	service *Service
	hyd     *hydraulics.Controller
	cycles  *fakeCycles
	ramp    *fakeRamp
	ctx     context.Context
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	nowTime := time.Unix(5000, 0).UTC()
	now := func() time.Time { return nowTime }
	store := journal.NewMemoryStore()
	recorder := journal.NewRecorder(store, now)
	hyd := hydraulics.NewController(gate.NewCommands(), recorder, hydraulics.NewRamp(time.Millisecond, now), now)
	cycles := &fakeCycles{}
	ramp := &fakeRamp{}
	registry := NewRegistry()
	if err := registry.Register("chamber-1", DefaultConfig()); err != nil {
		t.Fatal(err)
	}
	service := NewService(registry, NewPlanner(now), hyd, cycles, ramp, recorder, now)
	return &fixture{clock: &nowTime, service: service, hyd: hyd, cycles: cycles, ramp: ramp, ctx: context.Background()}
}

func (f *fixture) activate(cycle model.TransitCycle) {
	f.cycles.current = cycle
	f.cycles.has = true
	if err := f.hyd.BeginLeveling(cycle); err != nil {
		panic(err)
	}
}

func TestViewWithoutSamplesIsNotExecutable(t *testing.T) {
	f := newFixture(t)
	f.activate(testCycle(model.DirectionUpstream, 1))
	view := f.service.View("chamber-1")
	if view.Executable || view.PlanCurrent {
		t.Fatal("fresh service must not offer an executable plan")
	}
	if len(view.Reasons) == 0 {
		t.Fatal("expected explanatory reasons for the missing samples")
	}
}

func TestBuildPlanRejectsMissingAndStaleSamples(t *testing.T) {
	f := newFixture(t)
	cycle := testCycle(model.DirectionUpstream, 1)
	f.activate(cycle)

	if _, err := f.service.BuildPlan(f.ctx, "chamber-1"); err == nil {
		t.Fatal("expected refusal to plan without any samples")
	}

	if _, _, err := f.service.Observe(f.ctx, "chamber-1", 1.0); err != nil {
		t.Fatalf("observe: %v", err)
	}
	if _, err := f.service.BuildPlan(f.ctx, "chamber-1"); err == nil {
		t.Fatal("expected refusal with only one sample (minimum is two)")
	}

	if _, _, err := f.service.Observe(f.ctx, "chamber-1", 1.0); err != nil {
		t.Fatalf("observe: %v", err)
	}
	view, err := f.service.BuildPlan(f.ctx, "chamber-1")
	if err != nil {
		t.Fatalf("build plan with fresh samples: %v", err)
	}
	if !view.PlanCurrent || !view.Executable {
		t.Fatalf("fresh plan should be current and executable, got %+v", view)
	}

	// Telemetry ages past the configured freshness window.
	*f.clock = (*f.clock).Add(DefaultConfig().SampleMaxAge + time.Second)
	if _, err := f.service.BuildPlan(f.ctx, "chamber-1"); err == nil {
		t.Fatal("expected refusal to rebuild a plan on stale samples")
	}
}

func TestExecuteRunsOnceAndBindsGeneration(t *testing.T) {
	f := newFixture(t)
	// Vessel from the downstream pool (1 m): chamber must fill to 5 m.
	cycle := testCycle(model.DirectionDownstream, 1)
	f.activate(cycle)
	if _, _, err := f.service.Observe(f.ctx, "chamber-1", 1.0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.service.Observe(f.ctx, "chamber-1", 1.0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.BuildPlan(f.ctx, "chamber-1"); err != nil {
		t.Fatalf("build: %v", err)
	}

	plan, _, err := f.service.ExecutePlan(f.ctx, "chamber-1", cycle.ID, cycle.Generation)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !plan.Executed {
		t.Fatal("plan should be marked executed")
	}
	if len(f.ramp.calls) != 1 {
		t.Fatalf("ramp calls = %d, want 1", len(f.ramp.calls))
	}
	call := f.ramp.calls[0]
	if call.device != model.DeviceFillValve {
		t.Fatalf("ramp device = %s, want fill_valve", call.device)
	}
	if call.openings[len(call.openings)-1] != 0 {
		t.Fatal("ramp must finish by closing the valve")
	}

	if _, _, err := f.service.ExecutePlan(f.ctx, "chamber-1", cycle.ID, cycle.Generation); err == nil {
		t.Fatal("second execution of the same plan must be rejected")
	}

	// A new transit generation starts: the old plan must stay locked.
	next := testCycle(model.DirectionDownstream, 2)
	f.activate(next)
	view := f.service.View("chamber-1")
	if view.PlanCurrent || view.Executable {
		t.Fatal("plan from the previous generation must not be current/executable")
	}
	if _, _, err := f.service.ExecutePlan(f.ctx, "chamber-1", cycle.ID, cycle.Generation); err == nil {
		t.Fatal("executing the previous generation's plan must be rejected")
	}
	if _, _, err := f.service.ExecutePlan(f.ctx, "chamber-1", next.ID, next.Generation); err == nil {
		t.Fatal("stored plan must not be executed against the new generation")
	}
}

func TestExecuteRejectsUnknownGeneration(t *testing.T) {
	f := newFixture(t)
	cycle := testCycle(model.DirectionUpstream, 1)
	f.activate(cycle)
	if _, _, err := f.service.Observe(f.ctx, "chamber-1", 5.0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.service.Observe(f.ctx, "chamber-1", 5.0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.BuildPlan(f.ctx, "chamber-1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.service.ExecutePlan(f.ctx, "chamber-1", uuid.New(), 99); err == nil {
		t.Fatal("execute with an unrelated generation must fail")
	}
	if f.ramp.calls != nil {
		t.Fatal("no ramp command may be issued for a mismatched generation")
	}
}

func TestPlanWithinToleranceExecutesWithoutRamp(t *testing.T) {
	f := newFixture(t)
	// Upstream entrance; chamber already at the downstream target (1 m).
	cycle := testCycle(model.DirectionUpstream, 1)
	f.activate(cycle)
	if _, _, err := f.service.Observe(f.ctx, "chamber-1", 1.01); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.service.Observe(f.ctx, "chamber-1", 1.0); err != nil {
		t.Fatal(err)
	}
	view, err := f.service.BuildPlan(f.ctx, "chamber-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Plan.Instructions) != 0 {
		t.Fatalf("expected no instructions, got %d", len(view.Plan.Instructions))
	}
	plan, _, err := f.service.ExecutePlan(f.ctx, "chamber-1", cycle.ID, cycle.Generation)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !plan.Executed || f.ramp.calls != nil {
		t.Fatal("tolerance plan completes marked executed without issuing a ramp")
	}
}

func TestObserveRejectsSampleWithoutActiveTransit(t *testing.T) {
	f := newFixture(t)
	if _, _, err := f.service.Observe(f.ctx, "chamber-1", 3.0); err == nil {
		t.Fatal("sample without an active transit must be rejected")
	}
}

func TestBuildPlanRejectsWrongStage(t *testing.T) {
	f := newFixture(t)
	cycle, err := model.NewTransitCycle("chamber-1", uuid.New(), model.DirectionUpstream, 1, *f.clock)
	if err != nil {
		t.Fatal(err)
	}
	f.cycles.current, f.cycles.has = cycle, true
	_, err = f.service.BuildPlan(f.ctx, "chamber-1")
	if err == nil {
		t.Fatalf("expected stage conflict, got %v", err)
	}
}
