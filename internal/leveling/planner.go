package leveling

import (
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-96/internal/model"
)

// ValveInstruction is one explainable step of the equalization ramp. Opening
// follows the hydraulics ramp convention: positive drives the fill valve,
// negative the drain valve, and zero closes the active valve.
type ValveInstruction struct {
	Sequence  int              `json:"sequence"`
	Device    model.DeviceKind `json:"device"`
	Action    string           `json:"action"`
	Opening   float64          `json:"opening"`
	Rationale string           `json:"rationale"`
}

// Plan is the explainable leveling target computed for one transit generation.
type Plan struct {
	ChamberID     string              `json:"chamber_id"`
	CycleID       uuid.UUID           `json:"cycle_id"`
	Generation    uint64              `json:"generation"`
	Direction     model.Direction     `json:"direction"`
	Stage         model.TransitStage  `json:"stage"`
	TargetLevelM  float64             `json:"target_level_m"`
	CurrentLevelM float64             `json:"current_level_m"`
	ToleranceM    float64             `json:"tolerance_m"`
	DeltaM        float64             `json:"delta_m"`
	BasisSamples  []model.LevelSample `json:"basis_samples"`
	Instructions  []ValveInstruction  `json:"instructions"`
	Reasons       []string            `json:"reasons"`
	CreatedAt     time.Time           `json:"created_at"`
	Executed      bool                `json:"executed"`
	ExecutedAt    *time.Time          `json:"executed_at,omitempty"`
}

// Planner turns chamber configuration, the active transit direction and a
// fresh sample sequence into an explainable target and ramp profile.
type Planner struct {
	now func() time.Time
}

func NewPlanner(now func() time.Time) *Planner {
	if now == nil {
		now = time.Now
	}
	return &Planner{now: now}
}

// Build is pure: it never issues commands. Freshness and generation ownership
// are checked by the service before this is called.
func (p *Planner) Build(config ChamberConfig, cycle model.TransitCycle, samples []model.LevelSample) (Plan, error) {
	if len(samples) == 0 {
		return Plan{}, fmt.Errorf("no level samples for transit %s", cycle.ID)
	}
	latest := samples[len(samples)-1]
	target, err := config.TargetLevel(cycle.Direction)
	if err != nil {
		return Plan{}, err
	}
	current := latest.Meters
	delta := target - current

	plan := Plan{
		ChamberID:     cycle.ChamberID,
		CycleID:       cycle.ID,
		Generation:    cycle.Generation,
		Direction:     cycle.Direction,
		Stage:         cycle.Stage,
		TargetLevelM:  target,
		CurrentLevelM: current,
		ToleranceM:    config.LevelToleranceM,
		DeltaM:        delta,
		BasisSamples:  append([]model.LevelSample(nil), samples...),
		CreatedAt:     p.now().UTC(),
	}

	reachName := string(cycle.Direction)
	plan.Reasons = append(plan.Reasons, fmt.Sprintf(
		"vessel enters from the %s reach: equalize chamber to %.2f m", reachName, target))
	plan.Reasons = append(plan.Reasons, fmt.Sprintf(
		"latest sample at %s reads %.2f m (%d sample(s) in window)",
		latest.ObservedAt.UTC().Format(time.RFC3339), current, len(samples)))

	if math.Abs(delta) <= config.LevelToleranceM {
		plan.Reasons = append(plan.Reasons, fmt.Sprintf(
			"chamber already within %.2f m tolerance: no valve movement required", config.LevelToleranceM))
		return plan, nil
	}

	plan.Instructions = rampInstructions(config, delta)
	device := model.DeviceFillValve
	motion := "raise"
	if delta < 0 {
		device = model.DeviceDrainValve
		motion = "lower"
	}
	plan.Reasons = append(plan.Reasons, fmt.Sprintf(
		"%s chamber by %.2f m using the %s in %d ramped step(s), then close",
		motion, math.Abs(delta), device, len(plan.Instructions)))
	return plan, nil
}

// rampInstructions builds a bell-shaped opening profile: valve authority grows
// in ValveIncrement steps, is backed off near the target, and ends closed.
func rampInstructions(config ChamberConfig, delta float64) []ValveInstruction {
	chunks := int(math.Ceil(math.Abs(delta) / config.LevelStepM))
	maxMoving := config.MaxRampSteps - 1 // reserve the final step for closing
	if chunks > maxMoving {
		chunks = maxMoving
	}
	device := model.DeviceFillValve
	sign := 1.0
	if delta < 0 {
		device = model.DeviceDrainValve
		sign = -1.0
	}
	instructions := make([]ValveInstruction, 0, chunks+1)
	for i := 0; i < chunks; i++ {
		// Authority rises at the start of the move and is withdrawn towards
		// the target so the chamber does not overshoot the pool level.
		authority := float64(min(i+1, chunks-i)) * config.ValveIncrement
		opening := math.Min(config.MaxValveOpening, authority)
		instructions = append(instructions, ValveInstruction{
			Sequence: i + 1, Device: device, Action: "set_opening",
			Opening: sign * roundTwo(opening),
			Rationale: fmt.Sprintf("ramp segment %d/%d towards %.2f m", i+1, chunks,
				currentTarget(config, delta)),
		})
	}
	instructions = append(instructions, ValveInstruction{
		Sequence: len(instructions) + 1, Device: device, Action: "set_opening",
		Opening:   0,
		Rationale: "close valve once target level window is reached",
	})
	return instructions
}

func currentTarget(config ChamberConfig, delta float64) float64 {
	if delta > 0 {
		return config.UpstreamLevelM
	}
	return config.DownstreamLevelM
}

func roundTwo(value float64) float64 { return math.Round(value*100) / 100 }
