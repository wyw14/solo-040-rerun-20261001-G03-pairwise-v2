package leveling

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-96/internal/hydraulics"
	"github.com/wyw14/cry-96/internal/journal"
	"github.com/wyw14/cry-96/internal/model"
)

// SampleSource provides the live chamber level window.
type SampleSource interface {
	Observe(sample model.LevelSample) (bool, error)
	WindowSamples(chamberID string) []model.LevelSample
	LatestSample(chamberID string) (model.LevelSample, bool)
	ActiveGeneration(chamberID string) (uuid.UUID, uint64, bool)
}

// CycleLookup identifies the transit generation currently authoritative for
// a chamber.
type CycleLookup interface {
	Current(chamberID string) (model.TransitCycle, bool)
}

// RampExecutor issues the valve ramp against an explicit device, so both the
// fill and drain valves can be driven to a fully closed opening.
type RampExecutor interface {
	RampDevice(ctx context.Context, cycle model.TransitCycle, device model.DeviceKind, openings []float64) (hydraulics.RampResult, error)
}

// Service is the gatekeeper around the planner: no plan is executable without
// fresh samples, and every plan is bound to one transit generation.
type Service struct {
	mu       sync.RWMutex
	configs  *Registry
	planner  *Planner
	samples  SampleSource
	cycles   CycleLookup
	ramp     RampExecutor
	recorder *journal.Recorder
	plans    map[string]Plan
	now      func() time.Time
}

func NewService(configs *Registry, planner *Planner, samples SampleSource, cycles CycleLookup, ramp RampExecutor, recorder *journal.Recorder, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{
		configs: configs, planner: planner, samples: samples, cycles: cycles,
		ramp: ramp, recorder: recorder, plans: make(map[string]Plan), now: now,
	}
}

// View assembles everything an operator needs to judge the leveling target:
// the live cycle, the freshest sample, freshness reasons and the bound plan.
func (s *Service) View(chamberID string) View {
	view := View{ChamberID: chamberID, UpdatedAt: s.now().UTC(), Reasons: []string{}}
	config, ok := s.configs.Get(chamberID)
	if !ok {
		config = DefaultConfig()
		view.Reasons = append(view.Reasons, "no chamber configuration registered: using built-in defaults")
	}
	view.Config = config

	if cycleValue, ok := s.cycles.Current(chamberID); ok {
		cycleCopy := cycleValue.Clone()
		view.Cycle = &cycleCopy
	}
	if latest, ok := s.samples.LatestSample(chamberID); ok {
		sampleCopy := latest
		view.LatestSample = &sampleCopy
	}

	s.mu.RLock()
	plan, hasPlan := s.plans[chamberID]
	s.mu.RUnlock()
	if hasPlan {
		planCopy := plan
		view.Plan = &planCopy
	}

	fresh, freshReasons := s.freshness(chamberID, config)
	view.Reasons = append(view.Reasons, freshReasons...)
	view.PlanCurrent = hasPlan && view.Cycle != nil &&
		plan.CycleID == view.Cycle.ID && plan.Generation == view.Cycle.Generation
	if !hasPlan {
		view.Reasons = append(view.Reasons, "no leveling plan has been built for this chamber yet")
	} else if !view.PlanCurrent {
		view.Reasons = append(view.Reasons, fmt.Sprintf(
			"stored plan belongs to transit %s generation %d and must not be executed again",
			plan.CycleID.String()[:8], plan.Generation))
	}
	view.Executable = view.PlanCurrent && !plan.Executed && fresh &&
		view.Cycle != nil && view.Cycle.Stage == model.StageLeveling
	if view.PlanCurrent && view.Cycle != nil && view.Cycle.Stage != model.StageLeveling {
		view.Reasons = append(view.Reasons, fmt.Sprintf(
			"transit is in stage %q: leveling plans only execute in the leveling stage", view.Cycle.Stage))
	}
	return view
}

// BuildPlan creates the explainable target for the active generation. Stale
// telemetry or a mismatched transit generation blocks planning outright.
func (s *Service) BuildPlan(ctx context.Context, chamberID string) (View, error) {
	cycleValue, ok := s.cycles.Current(chamberID)
	if !ok {
		return View{}, errors.New("active transit not found")
	}
	if cycleValue.Stage != model.StageLeveling {
		return View{}, fmt.Errorf("transit %s is in stage %q, not leveling", cycleValue.ID, cycleValue.Stage)
	}
	config, ok := s.configs.Get(chamberID)
	if !ok {
		config = DefaultConfig()
	}
	if fresh, reasons := s.freshness(chamberID, config); !fresh {
		return View{}, fmt.Errorf("refusing to plan without fresh level samples: %v", reasons)
	}
	samples := s.samples.WindowSamples(chamberID)
	plan, err := s.planner.Build(config, cycleValue, samples)
	if err != nil {
		return View{}, err
	}
	s.mu.Lock()
	s.plans[chamberID] = plan
	s.mu.Unlock()
	if _, err := s.recorder.Record(ctx, chamberID, model.EventLevelingPlanned, plan.CycleID, plan.Generation, plan); err != nil {
		return View{}, err
	}
	return s.View(chamberID), nil
}

// ExecutePlan runs the stored ramp only when it still belongs to the live
// transit generation and has never run. A plan from an earlier transit is
// rejected.
func (s *Service) ExecutePlan(ctx context.Context, chamberID string, cycleID uuid.UUID, generation uint64) (Plan, hydraulics.RampResult, error) {
	cycleValue, ok := s.cycles.Current(chamberID)
	if !ok {
		return Plan{}, hydraulics.RampResult{}, errors.New("active transit not found")
	}
	if !cycleValue.IdentityMatches(cycleID, generation) {
		return Plan{}, hydraulics.RampResult{}, errors.New("request references a transit generation that is no longer current")
	}
	if cycleValue.Stage != model.StageLeveling {
		return Plan{}, hydraulics.RampResult{}, fmt.Errorf("transit is in stage %q, not leveling", cycleValue.Stage)
	}
	s.mu.Lock()
	plan, ok := s.plans[chamberID]
	if !ok {
		s.mu.Unlock()
		return Plan{}, hydraulics.RampResult{}, errors.New("no leveling plan stored for this chamber")
	}
	if plan.CycleID != cycleID || plan.Generation != generation {
		s.mu.Unlock()
		return Plan{}, hydraulics.RampResult{}, fmt.Errorf(
			"stored plan belongs to transit %s generation %d, not the active generation %d",
			plan.CycleID, plan.Generation, generation)
	}
	if plan.Executed {
		s.mu.Unlock()
		return Plan{}, hydraulics.RampResult{}, errors.New("leveling plan has already been executed for this transit")
	}
	s.mu.Unlock()

	config, ok := s.configs.Get(chamberID)
	if !ok {
		config = DefaultConfig()
	}
	if fresh, reasons := s.freshness(chamberID, config); !fresh {
		return Plan{}, hydraulics.RampResult{}, fmt.Errorf("level samples are no longer fresh: %v", reasons)
	}

	// Already within tolerance: the planner produced no valve movement, so
	// there is nothing to ramp and the plan is simply marked complete.
	if len(plan.Instructions) == 0 {
		result := hydraulics.RampResult{}
		s.markExecuted(chamberID, &plan)
		return plan, result, nil
	}

	device := deviceFor(plan.DeltaM)
	openings := make([]float64, 0, len(plan.Instructions))
	for _, instruction := range plan.Instructions {
		opening := instruction.Opening
		if opening < 0 {
			opening = -opening
		}
		openings = append(openings, opening)
	}
	result, err := s.ramp.RampDevice(ctx, cycleValue, device, openings)
	if err != nil {
		return plan, result, err
	}

	s.markExecuted(chamberID, &plan)
	return plan, result, nil
}

func (s *Service) markExecuted(chamberID string, plan *Plan) {
	s.mu.Lock()
	defer s.mu.Unlock()
	executedAt := s.now().UTC()
	plan.Executed = true
	plan.ExecutedAt = &executedAt
	s.plans[chamberID] = *plan
}

// Observe ingests one chamber level reading and binds it to the active transit
// generation. Readings that do not belong to the live generation are rejected.
func (s *Service) Observe(ctx context.Context, chamberID string, meters float64) (model.LevelSample, bool, error) {
	cycleValue, ok := s.cycles.Current(chamberID)
	if !ok || cycleValue.Stage != model.StageLeveling {
		return model.LevelSample{}, false, errors.New("level observations are only accepted during the leveling stage")
	}
	sample := model.LevelSample{
		CycleID: cycleValue.ID, Generation: cycleValue.Generation,
		Meters: meters, ObservedAt: s.now().UTC(),
	}
	stable, err := s.samples.Observe(sample)
	if err != nil {
		return model.LevelSample{}, false, err
	}
	if _, err := s.recorder.Record(ctx, chamberID, model.EventLevelObserved, sample.CycleID, sample.Generation, sample); err != nil {
		return model.LevelSample{}, false, err
	}
	return sample, stable, nil
}

// UpdateConfig replaces the per-chamber target and ramp parameters.
func (s *Service) UpdateConfig(chamberID string, config ChamberConfig) error {
	return s.configs.Register(chamberID, config)
}

// freshness is the single rule deciding whether telemetry is trustworthy
// enough to act on: right generation, enough readings, recent readings.
func (s *Service) freshness(chamberID string, config ChamberConfig) (bool, []string) {
	var reasons []string
	cycleValue, cycleOK := s.cycles.Current(chamberID)
	activeID, activeGeneration, windowOK := s.samples.ActiveGeneration(chamberID)
	samples := s.samples.WindowSamples(chamberID)
	latest, latestOK := s.samples.LatestSample(chamberID)

	switch {
	case !cycleOK:
		reasons = append(reasons, "no active transit on this chamber")
	case cycleValue.Stage != model.StageLeveling:
		// Not an error: there is simply nothing to level yet.
	case !windowOK:
		reasons = append(reasons, "leveling window has not been opened for this transit")
	case activeID != cycleValue.ID || activeGeneration != cycleValue.Generation:
		reasons = append(reasons, "level window belongs to a different transit generation")
	case !latestOK:
		reasons = append(reasons, "no level samples have been received")
	case len(samples) < config.MinimumSamples:
		reasons = append(reasons, fmt.Sprintf(
			"only %d of %d required samples available", len(samples), config.MinimumSamples))
	default:
		age := s.now().UTC().Sub(latest.ObservedAt.UTC())
		if age > config.SampleMaxAge {
			reasons = append(reasons, fmt.Sprintf(
				"latest sample is %s old; freshness limit is %s", age.Round(time.Second), config.SampleMaxAge))
		}
		if latest.CycleID != cycleValue.ID || latest.Generation != cycleValue.Generation {
			reasons = append(reasons, "latest sample does not match the active transit generation")
		}
	}
	return len(reasons) == 0, reasons
}

func deviceFor(delta float64) model.DeviceKind {
	if delta < 0 {
		return model.DeviceDrainValve
	}
	return model.DeviceFillValve
}

// View is the operator-facing projection of the leveling state.
type View struct {
	ChamberID    string              `json:"chamber_id"`
	Config       ChamberConfig       `json:"config"`
	Cycle        *model.TransitCycle `json:"cycle,omitempty"`
	LatestSample *model.LevelSample  `json:"latest_sample,omitempty"`
	Plan         *Plan               `json:"plan,omitempty"`
	PlanCurrent  bool                `json:"plan_current"`
	Executable   bool                `json:"executable"`
	Reasons      []string            `json:"reasons"`
	UpdatedAt    time.Time           `json:"updated_at"`
}
