package hydraulics

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-96/internal/gate"
	"github.com/wyw14/cry-96/internal/journal"
	"github.com/wyw14/cry-96/internal/model"
)

// Controller coordinates valve commands, level windows and ramp operations.
type Controller struct {
	mu       sync.RWMutex
	commands *gate.Commands
	recorder *journal.Recorder
	windows  map[string]*LevelWindow
	ramp     *Ramp
	now      func() time.Time
}

func NewController(commands *gate.Commands, recorder *journal.Recorder, ramp *Ramp, now func() time.Time) *Controller {
	if now == nil {
		now = time.Now
	}
	return &Controller{
		commands: commands, recorder: recorder, windows: make(map[string]*LevelWindow),
		ramp: ramp, now: now,
	}
}

func (c *Controller) window(chamberID string) (*LevelWindow, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	window := c.windows[chamberID]
	if window != nil {
		return window, nil
	}
	created, err := NewLevelWindow(4)
	if err != nil {
		return nil, err
	}
	c.windows[chamberID] = created
	return created, nil
}

func (c *Controller) BeginLeveling(cycle model.TransitCycle) error {
	window, err := c.window(cycle.ChamberID)
	if err != nil {
		return err
	}
	window.Begin(cycle.ID, cycle.Generation)
	return nil
}

func (c *Controller) Observe(sample model.LevelSample) (bool, error) {
	window, err := c.windowForCycle(sample.CycleID, sample.Generation)
	if err != nil {
		return false, err
	}
	if err := window.Add(sample); err != nil {
		return false, err
	}
	return window.Stable(0.05), nil
}

func (c *Controller) windowForCycle(cycleID interface{ String() string }, generation uint64) (*LevelWindow, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, window := range c.windows {
		id, currentGeneration := window.Identity()
		if id.String() == cycleID.String() && currentGeneration == generation {
			return window, nil
		}
	}
	return nil, errors.New("no active level window for transit")
}

func (c *Controller) IssueValve(ctx context.Context, cycle model.TransitCycle, target float64) error {
	device := model.DeviceFillValve
	if target < 0 {
		device = model.DeviceDrainValve
		target = -target
	}
	command, err := c.commands.Issue(cycle, device, "set_opening", target, c.now())
	if err != nil {
		return err
	}
	_, err = c.recorder.Record(ctx, cycle.ChamberID, model.EventCommandQueued, cycle.ID, cycle.Generation, command)
	return err
}

func (c *Controller) RampTo(ctx context.Context, cycle model.TransitCycle, targets []float64) (RampResult, error) {
	return c.ramp.Run(ctx, cycle, targets, c.IssueValve)
}

// RampDevice ramps one explicitly selected valve to a series of unsigned
// openings in [0,1]; zero closes it. It lets the leveling module drive the
// fill and drain valves independently.
func (c *Controller) RampDevice(ctx context.Context, cycle model.TransitCycle, device model.DeviceKind, openings []float64) (RampResult, error) {
	if device != model.DeviceFillValve && device != model.DeviceDrainValve {
		return RampResult{}, errors.New("ramp target must be a fill or drain valve")
	}
	issue := func(ctx context.Context, cycleValue model.TransitCycle, opening float64) error {
		if opening < 0 || opening > 1 {
			return errors.New("valve opening must be within [0,1]")
		}
		command, err := c.commands.Issue(cycleValue, device, "set_opening", opening, c.now())
		if err != nil {
			return err
		}
		_, err = c.recorder.Record(ctx, cycleValue.ChamberID, model.EventCommandQueued, cycleValue.ID, cycleValue.Generation, command)
		return err
	}
	return c.ramp.Run(ctx, cycle, openings, issue)
}

func (c *Controller) WindowSamples(chamberID string) []model.LevelSample {
	c.mu.RLock()
	window := c.windows[chamberID]
	c.mu.RUnlock()
	if window == nil {
		return nil
	}
	return window.Samples()
}

// LatestSample returns the most recent chamber level observation together with
// the cycle generation it belongs to.
func (c *Controller) LatestSample(chamberID string) (model.LevelSample, bool) {
	c.mu.RLock()
	window := c.windows[chamberID]
	c.mu.RUnlock()
	if window == nil {
		return model.LevelSample{}, false
	}
	samples := window.Samples()
	if len(samples) == 0 {
		return model.LevelSample{}, false
	}
	return samples[len(samples)-1], true
}

// ActiveGeneration reports which transit generation currently owns the
// chamber's level window, so callers can reject stale leveling plans.
func (c *Controller) ActiveGeneration(chamberID string) (uuid.UUID, uint64, bool) {
	c.mu.RLock()
	window := c.windows[chamberID]
	c.mu.RUnlock()
	if window == nil {
		return uuid.Nil, 0, false
	}
	cycleID, generation := window.Identity()
	if cycleID == uuid.Nil {
		return uuid.Nil, 0, false
	}
	return cycleID, generation, true
}
