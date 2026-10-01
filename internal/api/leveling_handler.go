package api

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/wyw14/cry-96/internal/leveling"
)

type levelSampleInput struct {
	Meters float64 `json:"meters"`
}

func levelSampleHandler(runtime *Runtime) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var input levelSampleInput
		if err := decodeJSON(r, &input); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if input.Meters <= 0 || input.Meters > 100 {
			writeError(w, http.StatusBadRequest, errors.New("meters must be between 0 and 100"))
			return
		}
		chamberID := chi.URLParam(r, "chamberID")
		sample, stable, err := runtime.Leveling.Observe(r.Context(), chamberID, input.Meters)
		if err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"sample": sample, "stable": stable})
	}
}

func levelingViewHandler(runtime *Runtime) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, runtime.Leveling.View(chi.URLParam(r, "chamberID")))
	}
}

func levelingPlanHandler(runtime *Runtime) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		view, err := runtime.Leveling.BuildPlan(r.Context(), chi.URLParam(r, "chamberID"))
		if err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
		writeJSON(w, http.StatusCreated, view)
	}
}

type levelingExecuteInput struct {
	CycleID    string `json:"cycle_id"`
	Generation uint64 `json:"generation"`
}

func levelingExecuteHandler(runtime *Runtime) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var input levelingExecuteInput
		if err := decodeJSON(r, &input); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		cycleID, err := parseUUID(input.CycleID)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if input.Generation == 0 {
			writeError(w, http.StatusBadRequest, errors.New("generation is required"))
			return
		}
		plan, result, err := runtime.Leveling.ExecutePlan(
			r.Context(), chi.URLParam(r, "chamberID"), cycleID, input.Generation)
		if err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"plan": plan, "ramp": result})
	}
}

func levelingConfigsHandler(runtime *Runtime) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, runtime.LevelConfigs.List())
	}
}

func levelingConfigHandler(runtime *Runtime) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		chamberID := chi.URLParam(r, "chamberID")
		if r.Method == http.MethodGet {
			config, ok := runtime.LevelConfigs.Get(chamberID)
			if !ok {
				writeError(w, http.StatusNotFound, errors.New("no configuration for chamber"))
				return
			}
			writeJSON(w, http.StatusOK, config)
			return
		}
		var config leveling.ChamberConfig
		if err := decodeJSON(r, &config); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if err := runtime.Leveling.UpdateConfig(chamberID, config); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		saved, _ := runtime.LevelConfigs.Get(chamberID)
		writeJSON(w, http.StatusOK, saved)
	}
}
