package leveling

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-96/internal/model"
)

func testCycle(direction model.Direction, generation uint64) model.TransitCycle {
	cycle, err := model.NewTransitCycle("chamber-1", uuid.New(), direction, generation, time.Unix(1000, 0).UTC())
	if err != nil {
		panic(err)
	}
	if err := cycle.Advance(model.StageAdmitting, "", time.Unix(1000, 1).UTC()); err != nil {
		panic(err)
	}
	if err := cycle.Advance(model.StageSealed, "", time.Unix(1000, 2).UTC()); err != nil {
		panic(err)
	}
	if err := cycle.Advance(model.StageLeveling, "", time.Unix(1000, 3).UTC()); err != nil {
		panic(err)
	}
	return cycle
}

func samples(cycle model.TransitCycle, meters ...float64) []model.LevelSample {
	result := make([]model.LevelSample, 0, len(meters))
	for i, value := range meters {
		result = append(result, model.LevelSample{
			CycleID: cycle.ID, Generation: cycle.Generation, Meters: value,
			ObservedAt: time.Unix(2000+int64(i), 0).UTC(),
		})
	}
	return result
}

func TestPlannerDrainsForVesselFromUpstream(t *testing.T) {
	cycle := testCycle(model.DirectionUpstream, 1)
	plan, err := NewPlanner(nil).Build(DefaultConfig(), cycle, samples(cycle, 5.0, 5.0))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	// Vessel entered from the 5 m pool; the chamber must drain to 1 m before exit.
	if plan.TargetLevelM != 1.0 {
		t.Fatalf("target = %v, want 1.0 (opposite/downstream reach)", plan.TargetLevelM)
	}
	if plan.DeltaM != -4.0 {
		t.Fatalf("delta = %v, want -4.0", plan.DeltaM)
	}
	if len(plan.Instructions) == 0 {
		t.Fatal("expected ramp instructions for a 4 m drain")
	}
	for i, instruction := range plan.Instructions {
		if instruction.Device != model.DeviceDrainValve {
			t.Fatalf("step %d device = %s, want drain_valve", i, instruction.Device)
		}
		if instruction.Opening > 0 {
			t.Fatalf("step %d opening = %v, drain ramps must be non-positive", i, instruction.Opening)
		}
		if instruction.Rationale == "" {
			t.Fatalf("step %d has no rationale", i)
		}
	}
	last := plan.Instructions[len(plan.Instructions)-1]
	if last.Opening != 0 {
		t.Fatalf("final opening = %v, want valve closed (0)", last.Opening)
	}
	if plan.CycleID != cycle.ID || plan.Generation != 1 {
		t.Fatal("plan is not bound to the transit generation")
	}
	if len(plan.Reasons) == 0 {
		t.Fatal("plan must carry human-readable reasons")
	}
}

func TestPlannerFillsForVesselFromDownstream(t *testing.T) {
	cycle := testCycle(model.DirectionDownstream, 1)
	plan, err := NewPlanner(nil).Build(DefaultConfig(), cycle, samples(cycle, 1.0, 1.0))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if plan.TargetLevelM != 5.0 || plan.DeltaM != 4.0 {
		t.Fatalf("target/delta = %v/%v, want 5.0/4.0", plan.TargetLevelM, plan.DeltaM)
	}
	for _, instruction := range plan.Instructions {
		if instruction.Device != model.DeviceFillValve {
			t.Fatalf("device = %s, want fill_valve", instruction.Device)
		}
	}
}

func TestPlannerWithinToleranceIssuesNoValveSteps(t *testing.T) {
	// Upstream entrance: chamber already at the downstream pool level.
	cycle := testCycle(model.DirectionUpstream, 1)
	plan, err := NewPlanner(nil).Build(DefaultConfig(), cycle, samples(cycle, 1.02, 1.01))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(plan.Instructions) != 0 {
		t.Fatalf("expected no valve steps within tolerance, got %d", len(plan.Instructions))
	}
}

func TestPlannerRejectsEmptySampleWindow(t *testing.T) {
	cycle := testCycle(model.DirectionUpstream, 1)
	if _, err := NewPlanner(nil).Build(DefaultConfig(), cycle, nil); err == nil {
		t.Fatal("expected error when no samples are available")
	}
}

func TestRampInstructionCountIsBounded(t *testing.T) {
	config := DefaultConfig()
	config.LevelStepM = 0.01
	got := rampInstructions(config, 5.0)
	if len(got) > config.MaxRampSteps {
		t.Fatalf("ramp produced %d steps, limit is %d", len(got), config.MaxRampSteps)
	}
	if got[len(got)-1].Opening != 0 {
		t.Fatal("ramp must end with the valve closed")
	}
}
