// Package admin provides HTTP handlers for pool configuration management.
package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"

	"github.com/Shavakan/runs-fleet/pkg/db"
)

const maxRequestBodySize = 1 << 20 // 1 MB

// PoolDB defines the database operations required by the admin handler.
type PoolDB interface {
	ListPools(ctx context.Context) ([]string, error)
	GetPoolConfig(ctx context.Context, poolName string) (*db.PoolConfig, error)
	SavePoolConfig(ctx context.Context, config *db.PoolConfig) error
	DeletePoolConfig(ctx context.Context, poolName string) error
}

// Handler provides HTTP endpoints for pool configuration management.
type Handler struct {
	db PoolDB
}

// NewHandler creates a new admin handler.
func NewHandler(db PoolDB) *Handler {
	return &Handler{db: db}
}

// poolNamePattern validates pool names: alphanumeric, hyphens, underscores.
var poolNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`)

// PoolResponse represents a pool configuration in API responses.
type PoolResponse struct {
	PoolName           string             `json:"pool_name"`
	InstanceType       string             `json:"instance_type,omitempty"`
	DesiredRunning     int                `json:"desired_running"`
	DesiredStopped     int                `json:"desired_stopped"`
	IdleTimeoutMinutes int                `json:"idle_timeout_minutes,omitempty"`
	Ephemeral          bool               `json:"ephemeral"`
	Environment        string             `json:"environment,omitempty"`
	Region             string             `json:"region,omitempty"`
	Arch               string             `json:"arch,omitempty"`
	CPUMin             int                `json:"cpu_min,omitempty"`
	CPUMax             int                `json:"cpu_max,omitempty"`
	RAMMin             float64            `json:"ram_min,omitempty"`
	RAMMax             float64            `json:"ram_max,omitempty"`
	Families           []string           `json:"families,omitempty"`
	Schedules          []ScheduleResponse `json:"schedules,omitempty"`
}

// ScheduleResponse represents a pool schedule in API responses.
type ScheduleResponse struct {
	Name           string `json:"name"`
	StartHour      int    `json:"start_hour"`
	EndHour        int    `json:"end_hour"`
	DaysOfWeek     []int  `json:"days_of_week,omitempty"`
	DesiredRunning int    `json:"desired_running"`
	DesiredStopped int    `json:"desired_stopped"`
}

// PoolRequest represents a pool configuration in API requests.
type PoolRequest struct {
	PoolName           string            `json:"pool_name"`
	InstanceType       string            `json:"instance_type,omitempty"`
	DesiredRunning     int               `json:"desired_running"`
	DesiredStopped     int               `json:"desired_stopped"`
	IdleTimeoutMinutes int               `json:"idle_timeout_minutes,omitempty"`
	Environment        string            `json:"environment,omitempty"`
	Region             string            `json:"region,omitempty"`
	Arch               string            `json:"arch,omitempty"`
	CPUMin             int               `json:"cpu_min,omitempty"`
	CPUMax             int               `json:"cpu_max,omitempty"`
	RAMMin             float64           `json:"ram_min,omitempty"`
	RAMMax             float64           `json:"ram_max,omitempty"`
	Families           []string          `json:"families,omitempty"`
	Schedules          []ScheduleRequest `json:"schedules,omitempty"`
}

// ScheduleRequest represents a pool schedule in API requests.
type ScheduleRequest struct {
	Name           string `json:"name"`
	StartHour      int    `json:"start_hour"`
	EndHour        int    `json:"end_hour"`
	DaysOfWeek     []int  `json:"days_of_week,omitempty"`
	DesiredRunning int    `json:"desired_running"`
	DesiredStopped int    `json:"desired_stopped"`
}

// ErrorResponse represents an API error.
type ErrorResponse struct {
	Error   string `json:"error"`
	Details string `json:"details,omitempty"`
}

// RegisterRoutes registers admin API routes on the given mux.
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/pools", h.ListPools)
	mux.HandleFunc("GET /api/pools/{name}", h.GetPool)
	mux.HandleFunc("POST /api/pools", h.CreatePool)
	mux.HandleFunc("PUT /api/pools/{name}", h.UpdatePool)
	mux.HandleFunc("DELETE /api/pools/{name}", h.DeletePool)
}

// ListPools handles GET /api/pools.
func (h *Handler) ListPools(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	poolNames, err := h.db.ListPools(ctx)
	if err != nil {
		h.writeError(w, http.StatusInternalServerError, "Failed to list pools", err.Error())
		return
	}

	pools := make([]PoolResponse, 0, len(poolNames))
	for _, name := range poolNames {
		config, err := h.db.GetPoolConfig(ctx, name)
		if err != nil {
			log.Printf("Failed to get pool config for %s: %v", name, err)
			continue
		}
		if config == nil {
			continue
		}
		pools = append(pools, h.configToResponse(config))
	}

	h.writeJSON(w, http.StatusOK, map[string]interface{}{
		"pools": pools,
	})
}

// GetPool handles GET /api/pools/{name}.
func (h *Handler) GetPool(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		h.writeError(w, http.StatusBadRequest, "Pool name is required", "")
		return
	}

	config, err := h.db.GetPoolConfig(r.Context(), name)
	if err != nil {
		h.writeError(w, http.StatusInternalServerError, "Failed to get pool", err.Error())
		return
	}
	if config == nil {
		h.writeError(w, http.StatusNotFound, "Pool not found", "")
		return
	}

	h.writeJSON(w, http.StatusOK, h.configToResponse(config))
}

// CreatePool handles POST /api/pools.
func (h *Handler) CreatePool(w http.ResponseWriter, r *http.Request) {
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		h.writeError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json", "")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodySize)
	var req PoolRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeError(w, http.StatusBadRequest, "Invalid request body", err.Error())
		return
	}

	if err := h.validatePoolRequest(&req, true); err != nil {
		h.writeError(w, http.StatusBadRequest, "Validation failed", err.Error())
		return
	}

	existing, err := h.db.GetPoolConfig(r.Context(), req.PoolName)
	if err != nil {
		h.writeError(w, http.StatusInternalServerError, "Failed to check existing pool", err.Error())
		return
	}
	if existing != nil {
		h.writeError(w, http.StatusConflict, "Pool already exists", "")
		return
	}

	config := h.requestToConfig(&req)
	if err := h.db.SavePoolConfig(r.Context(), config); err != nil {
		h.writeError(w, http.StatusInternalServerError, "Failed to create pool", err.Error())
		return
	}

	h.writeJSON(w, http.StatusCreated, h.configToResponse(config))
}

// UpdatePool handles PUT /api/pools/{name}.
func (h *Handler) UpdatePool(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		h.writeError(w, http.StatusBadRequest, "Pool name is required", "")
		return
	}

	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		h.writeError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json", "")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodySize)
	var req PoolRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeError(w, http.StatusBadRequest, "Invalid request body", err.Error())
		return
	}

	req.PoolName = name

	if err := h.validatePoolRequest(&req, false); err != nil {
		h.writeError(w, http.StatusBadRequest, "Validation failed", err.Error())
		return
	}

	existing, err := h.db.GetPoolConfig(r.Context(), name)
	if err != nil {
		h.writeError(w, http.StatusInternalServerError, "Failed to get pool", err.Error())
		return
	}
	if existing == nil {
		h.writeError(w, http.StatusNotFound, "Pool not found", "")
		return
	}

	config := h.requestToConfig(&req)
	config.Ephemeral = existing.Ephemeral
	config.LastJobTime = existing.LastJobTime

	if err := h.db.SavePoolConfig(r.Context(), config); err != nil {
		h.writeError(w, http.StatusInternalServerError, "Failed to update pool", err.Error())
		return
	}

	h.writeJSON(w, http.StatusOK, h.configToResponse(config))
}

// DeletePool handles DELETE /api/pools/{name}.
func (h *Handler) DeletePool(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		h.writeError(w, http.StatusBadRequest, "Pool name is required", "")
		return
	}

	existing, err := h.db.GetPoolConfig(r.Context(), name)
	if err != nil {
		h.writeError(w, http.StatusInternalServerError, "Failed to get pool", err.Error())
		return
	}
	if existing == nil {
		h.writeError(w, http.StatusNotFound, "Pool not found", "")
		return
	}

	if !existing.Ephemeral {
		h.writeError(w, http.StatusForbidden, "Cannot delete non-ephemeral pool", "Only ephemeral pools can be deleted via API")
		return
	}

	if err := h.db.DeletePoolConfig(r.Context(), name); err != nil {
		h.writeError(w, http.StatusInternalServerError, "Failed to delete pool", err.Error())
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) validatePoolRequest(req *PoolRequest, isCreate bool) error {
	if isCreate {
		if req.PoolName == "" {
			return errors.New("pool_name is required")
		}
		if !poolNamePattern.MatchString(req.PoolName) {
			return errors.New("pool_name must be alphanumeric with hyphens or underscores, starting with alphanumeric")
		}
		if len(req.PoolName) > 63 {
			return errors.New("pool_name must not exceed 63 characters")
		}
	}

	if req.DesiredRunning < 0 {
		return errors.New("desired_running must be non-negative")
	}
	if req.DesiredStopped < 0 {
		return errors.New("desired_stopped must be non-negative")
	}
	if req.IdleTimeoutMinutes < 0 {
		return errors.New("idle_timeout_minutes must be non-negative")
	}

	if req.Arch != "" && req.Arch != "arm64" && req.Arch != "amd64" {
		return fmt.Errorf("arch must be 'arm64' or 'amd64', got %q", req.Arch)
	}

	if req.CPUMin < 0 || req.CPUMax < 0 {
		return errors.New("cpu_min and cpu_max must be non-negative")
	}
	if req.CPUMin > 0 && req.CPUMax > 0 && req.CPUMin > req.CPUMax {
		return errors.New("cpu_min must not exceed cpu_max")
	}

	if req.RAMMin < 0 || req.RAMMax < 0 {
		return errors.New("ram_min and ram_max must be non-negative")
	}
	if req.RAMMin > 0 && req.RAMMax > 0 && req.RAMMin > req.RAMMax {
		return errors.New("ram_min must not exceed ram_max")
	}

	for i, sched := range req.Schedules {
		if sched.Name == "" {
			return fmt.Errorf("schedule[%d].name is required", i)
		}
		if sched.StartHour < 0 || sched.StartHour > 23 {
			return fmt.Errorf("schedule[%d].start_hour must be 0-23", i)
		}
		if sched.EndHour < 0 || sched.EndHour > 23 {
			return fmt.Errorf("schedule[%d].end_hour must be 0-23", i)
		}
		for j, day := range sched.DaysOfWeek {
			if day < 0 || day > 6 {
				return fmt.Errorf("schedule[%d].days_of_week[%d] must be 0-6", i, j)
			}
		}
	}

	return nil
}

func (h *Handler) configToResponse(config *db.PoolConfig) PoolResponse {
	resp := PoolResponse{
		PoolName:           config.PoolName,
		InstanceType:       config.InstanceType,
		DesiredRunning:     config.DesiredRunning,
		DesiredStopped:     config.DesiredStopped,
		IdleTimeoutMinutes: config.IdleTimeoutMinutes,
		Ephemeral:          config.Ephemeral,
		Environment:        config.Environment,
		Region:             config.Region,
		Arch:               config.Arch,
		CPUMin:             config.CPUMin,
		CPUMax:             config.CPUMax,
		RAMMin:             config.RAMMin,
		RAMMax:             config.RAMMax,
		Families:           config.Families,
	}

	if len(config.Schedules) > 0 {
		resp.Schedules = make([]ScheduleResponse, len(config.Schedules))
		for i, s := range config.Schedules {
			resp.Schedules[i] = ScheduleResponse{
				Name:           s.Name,
				StartHour:      s.StartHour,
				EndHour:        s.EndHour,
				DaysOfWeek:     s.DaysOfWeek,
				DesiredRunning: s.DesiredRunning,
				DesiredStopped: s.DesiredStopped,
			}
		}
	}

	return resp
}

func (h *Handler) requestToConfig(req *PoolRequest) *db.PoolConfig {
	config := &db.PoolConfig{
		PoolName:           req.PoolName,
		InstanceType:       req.InstanceType,
		DesiredRunning:     req.DesiredRunning,
		DesiredStopped:     req.DesiredStopped,
		IdleTimeoutMinutes: req.IdleTimeoutMinutes,
		Environment:        req.Environment,
		Region:             req.Region,
		Arch:               req.Arch,
		CPUMin:             req.CPUMin,
		CPUMax:             req.CPUMax,
		RAMMin:             req.RAMMin,
		RAMMax:             req.RAMMax,
		Families:           req.Families,
	}

	if len(req.Schedules) > 0 {
		config.Schedules = make([]db.PoolSchedule, len(req.Schedules))
		for i, s := range req.Schedules {
			config.Schedules[i] = db.PoolSchedule{
				Name:           s.Name,
				StartHour:      s.StartHour,
				EndHour:        s.EndHour,
				DaysOfWeek:     s.DaysOfWeek,
				DesiredRunning: s.DesiredRunning,
				DesiredStopped: s.DesiredStopped,
			}
		}
	}

	return config
}

func (h *Handler) writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(data); err != nil {
		log.Printf("Failed to encode JSON response: %v", err)
	}
}

func (h *Handler) writeError(w http.ResponseWriter, status int, message, details string) {
	resp := ErrorResponse{Error: message}
	if details != "" {
		resp.Details = details
	}
	h.writeJSON(w, status, resp)
}
