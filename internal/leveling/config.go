package leveling

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/wyw14/cry-96/internal/model"
)

// chamberConfigDTO exposes the sample freshness window in seconds over JSON.
type chamberConfigDTO struct {
	UpstreamLevelM      float64 `json:"upstream_level_m"`
	DownstreamLevelM    float64 `json:"downstream_level_m"`
	LevelToleranceM     float64 `json:"level_tolerance_m"`
	SampleMaxAgeSeconds float64 `json:"sample_max_age_seconds"`
	MinimumSamples      int     `json:"minimum_samples"`
	ValveIncrement      float64 `json:"valve_increment"`
	MaxValveOpening     float64 `json:"max_valve_opening"`
	LevelStepM          float64 `json:"level_step_m"`
	MaxRampSteps        int     `json:"max_ramp_steps"`
}

func (c ChamberConfig) MarshalJSON() ([]byte, error) {
	return json.Marshal(chamberConfigDTO{
		UpstreamLevelM: c.UpstreamLevelM, DownstreamLevelM: c.DownstreamLevelM,
		LevelToleranceM: c.LevelToleranceM, SampleMaxAgeSeconds: c.SampleMaxAge.Seconds(),
		MinimumSamples: c.MinimumSamples, ValveIncrement: c.ValveIncrement,
		MaxValveOpening: c.MaxValveOpening, LevelStepM: c.LevelStepM, MaxRampSteps: c.MaxRampSteps,
	})
}

func (c *ChamberConfig) UnmarshalJSON(data []byte) error {
	var dto chamberConfigDTO
	if err := json.Unmarshal(data, &dto); err != nil {
		return err
	}
	*c = ChamberConfig{
		UpstreamLevelM: dto.UpstreamLevelM, DownstreamLevelM: dto.DownstreamLevelM,
		LevelToleranceM: dto.LevelToleranceM, SampleMaxAge: durationFromSeconds(dto.SampleMaxAgeSeconds),
		MinimumSamples: dto.MinimumSamples, ValveIncrement: dto.ValveIncrement,
		MaxValveOpening: dto.MaxValveOpening, LevelStepM: dto.LevelStepM, MaxRampSteps: dto.MaxRampSteps,
	}
	return nil
}

func durationFromSeconds(seconds float64) time.Duration {
	return time.Duration(seconds * float64(time.Second))
}

// ChamberConfig holds the per-chamber leveling targets and valve ramping
// parameters. Every value is chamber specific so different reaches can run
// with their own target levels and valve authority.
type ChamberConfig struct {
	UpstreamLevelM   float64       `json:"upstream_level_m"`
	DownstreamLevelM float64       `json:"downstream_level_m"`
	LevelToleranceM  float64       `json:"level_tolerance_m"`
	SampleMaxAge     time.Duration `json:"sample_max_age"`
	MinimumSamples   int           `json:"minimum_samples"`
	ValveIncrement   float64       `json:"valve_increment"`
	MaxValveOpening  float64       `json:"max_valve_opening"`
	LevelStepM       float64       `json:"level_step_m"`
	MaxRampSteps     int           `json:"max_ramp_steps"`
}

// DefaultConfig ships sane values for a chamber with an upstream pool around
// five metres and a downstream pool around one metre.
func DefaultConfig() ChamberConfig {
	return ChamberConfig{
		UpstreamLevelM:   5.0,
		DownstreamLevelM: 1.0,
		LevelToleranceM:  0.05,
		SampleMaxAge:     10 * time.Second,
		MinimumSamples:   2,
		ValveIncrement:   0.25,
		MaxValveOpening:  1.0,
		LevelStepM:       0.30,
		MaxRampSteps:     8,
	}
}

func (c ChamberConfig) Validate() error {
	if c.UpstreamLevelM <= 0 || c.DownstreamLevelM <= 0 {
		return errors.New("reach target levels must be positive")
	}
	if c.UpstreamLevelM <= c.DownstreamLevelM {
		return errors.New("upstream target level must be above downstream target level")
	}
	if c.LevelToleranceM <= 0 {
		return errors.New("level tolerance must be positive")
	}
	if c.SampleMaxAge <= 0 {
		return errors.New("sample max age must be positive")
	}
	if c.MinimumSamples < 1 {
		return errors.New("minimum sample count must be at least one")
	}
	if c.ValveIncrement <= 0 || c.ValveIncrement > c.MaxValveOpening {
		return errors.New("valve increment must be positive and no larger than max opening")
	}
	if c.MaxValveOpening <= 0 || c.MaxValveOpening > 1 {
		return errors.New("max valve opening must be within (0,1]")
	}
	if c.LevelStepM <= 0 {
		return errors.New("level step must be positive")
	}
	if c.MaxRampSteps < 1 || c.MaxRampSteps > 12 {
		return errors.New("max ramp steps must be between one and twelve")
	}
	return nil
}

// TargetLevel returns the reach level the chamber must equalize towards for
// the given transit direction. Direction identifies the reach that sends the
// vessel in: after the entrance gates are sealed the chamber is leveled to
// the opposite reach so the vessel can be released.
func (c ChamberConfig) TargetLevel(direction model.Direction) (float64, error) {
	switch direction {
	case model.DirectionUpstream:
		// Vessel entered from the upstream pool: drain down to the downstream pool.
		return c.DownstreamLevelM, nil
	case model.DirectionDownstream:
		// Vessel entered from the downstream pool: fill up to the upstream pool.
		return c.UpstreamLevelM, nil
	default:
		return 0, fmt.Errorf("unknown transit direction %q", direction)
	}
}

// Registry stores one validated configuration per chamber.
type Registry struct {
	mu      sync.RWMutex
	configs map[string]ChamberConfig
}

func NewRegistry() *Registry {
	return &Registry{configs: make(map[string]ChamberConfig)}
}

// Register stores a chamber configuration, falling back to defaults when the
// supplied config is the zero value.
func (r *Registry) Register(chamberID string, config ChamberConfig) error {
	if chamberID == "" {
		return errors.New("chamber id is required")
	}
	var candidate ChamberConfig
	if config == (ChamberConfig{}) {
		candidate = DefaultConfig()
	} else {
		candidate = config
	}
	if err := candidate.Validate(); err != nil {
		return err
	}
	r.mu.Lock()
	r.configs[chamberID] = candidate
	r.mu.Unlock()
	return nil
}

func (r *Registry) Get(chamberID string) (ChamberConfig, bool) {
	r.mu.RLock()
	config, ok := r.configs[chamberID]
	r.mu.RUnlock()
	return config, ok
}

func (r *Registry) List() map[string]ChamberConfig {
	r.mu.RLock()
	result := make(map[string]ChamberConfig, len(r.configs))
	for chamberID, config := range r.configs {
		result[chamberID] = config
	}
	r.mu.RUnlock()
	return result
}
