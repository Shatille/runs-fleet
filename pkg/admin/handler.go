// Package admin provides HTTP handlers for pool configuration management.
package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"github.com/Shavakan/runs-fleet/pkg/db"
	"github.com/Shavakan/runs-fleet/pkg/logging"
)

var adminLog = logging.WithComponent(logging.LogTypeAdmin, "handler")
var auditLog = logging.WithComponent(logging.LogTypeAdmin, "audit")

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
	db   PoolDB
	auth *AuthMiddleware
}

// NewHandler creates a new admin handler with authentication.
// If adminSecret is empty, authentication is disabled.
func NewHandler(db PoolDB, adminSecret string) *Handler {
	return &Handler{
		db:   db,
		auth: NewAuthMiddleware(adminSecret),
	}
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
// All endpoints require authentication when RUNS_FLEET_ADMIN_SECRET is set.
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.Handle("GET /api/pools", h.auth.WrapFunc(h.ListPools))
	mux.Handle("GET /api/pools/{name}", h.auth.WrapFunc(h.GetPool))
	mux.Handle("POST /api/pools", h.auth.WrapFunc(h.CreatePool))
	mux.Handle("PUT /api/pools/{name}", h.auth.WrapFunc(h.UpdatePool))
	mux.Handle("DELETE /api/pools/{name}", h.auth.WrapFunc(h.DeletePool))
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
			adminLog.Error("pool config fetch failed",
				slog.String(logging.KeyPoolName, name),
				slog.String("error", err.Error()))
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
		h.auditLog(r, "pool.create", req.PoolName, "denied", slog.String(logging.KeyReason, err.Error()))
		h.writeError(w, http.StatusBadRequest, "Validation failed", err.Error())
		return
	}

	existing, err := h.db.GetPoolConfig(r.Context(), req.PoolName)
	if err != nil {
		h.auditLog(r, "pool.create", req.PoolName, "error", slog.String(logging.KeyError, err.Error()))
		h.writeError(w, http.StatusInternalServerError, "Failed to check existing pool", err.Error())
		return
	}
	if existing != nil {
		h.auditLog(r, "pool.create", req.PoolName, "denied", slog.String(logging.KeyReason, "already exists"))
		h.writeError(w, http.StatusConflict, "Pool already exists", "")
		return
	}

	config := h.requestToConfig(&req)
	if err := h.db.SavePoolConfig(r.Context(), config); err != nil {
		h.auditLog(r, "pool.create", req.PoolName, "error", slog.String(logging.KeyError, err.Error()))
		h.writeError(w, http.StatusInternalServerError, "Failed to create pool", err.Error())
		return
	}

	h.auditLog(r, "pool.create", req.PoolName, "success")
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
		h.auditLog(r, "pool.update", name, "denied", slog.String(logging.KeyReason, err.Error()))
		h.writeError(w, http.StatusBadRequest, "Validation failed", err.Error())
		return
	}

	existing, err := h.db.GetPoolConfig(r.Context(), name)
	if err != nil {
		h.auditLog(r, "pool.update", name, "error", slog.String(logging.KeyError, err.Error()))
		h.writeError(w, http.StatusInternalServerError, "Failed to get pool", err.Error())
		return
	}
	if existing == nil {
		h.auditLog(r, "pool.update", name, "denied", slog.String(logging.KeyReason, "not found"))
		h.writeError(w, http.StatusNotFound, "Pool not found", "")
		return
	}

	config := h.requestToConfig(&req)
	config.Ephemeral = existing.Ephemeral
	config.LastJobTime = existing.LastJobTime

	if err := h.db.SavePoolConfig(r.Context(), config); err != nil {
		h.auditLog(r, "pool.update", name, "error", slog.String(logging.KeyError, err.Error()))
		h.writeError(w, http.StatusInternalServerError, "Failed to update pool", err.Error())
		return
	}

	changes := poolDiff(existing, config)
	h.auditLog(r, "pool.update", name, "success", slog.String("changes", changes))
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
		h.auditLog(r, "pool.delete", name, "error", slog.String(logging.KeyError, err.Error()))
		h.writeError(w, http.StatusInternalServerError, "Failed to get pool", err.Error())
		return
	}
	if existing == nil {
		h.auditLog(r, "pool.delete", name, "denied", slog.String(logging.KeyReason, "not found"))
		h.writeError(w, http.StatusNotFound, "Pool not found", "")
		return
	}

	if !existing.Ephemeral {
		h.auditLog(r, "pool.delete", name, "denied", slog.String(logging.KeyReason, "non-ephemeral"))
		h.writeError(w, http.StatusForbidden, "Cannot delete non-ephemeral pool", "Only ephemeral pools can be deleted via API")
		return
	}

	if err := h.db.DeletePoolConfig(r.Context(), name); err != nil {
		h.auditLog(r, "pool.delete", name, "error", slog.String(logging.KeyError, err.Error()))
		h.writeError(w, http.StatusInternalServerError, "Failed to delete pool", err.Error())
		return
	}

	h.auditLog(r, "pool.delete", name, "success")
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
		adminLog.Error("json encode failed", slog.String("error", err.Error()))
	}
}

func (h *Handler) writeError(w http.ResponseWriter, status int, message, details string) {
	resp := ErrorResponse{Error: message}
	if details != "" {
		resp.Details = details
	}
	h.writeJSON(w, status, resp)
}

func (h *Handler) auditLog(r *http.Request, action, poolName, result string, extra ...any) {
	remoteAddr := r.Header.Get("X-Forwarded-For")
	if remoteAddr == "" {
		remoteAddr = r.RemoteAddr
	}
	attrs := []any{
		slog.Bool(logging.KeyAudit, true),
		slog.String(logging.KeyAction, action),
		slog.String(logging.KeyPoolName, poolName),
		slog.String(logging.KeyResult, result),
		slog.String(logging.KeyRemoteAddr, remoteAddr),
	}
	attrs = append(attrs, extra...)

	switch result {
	case "denied":
		auditLog.Warn("admin action denied", attrs...)
	case "error":
		auditLog.Error("admin action failed", attrs...)
	default:
		auditLog.Info("admin action", attrs...)
	}
}

func poolDiff(old, updated *db.PoolConfig) string {
	var diffs []string

	if old.InstanceType != updated.InstanceType {
		diffs = append(diffs, fmt.Sprintf("instance_type: %q -> %q", old.InstanceType, updated.InstanceType))
	}
	if old.DesiredRunning != updated.DesiredRunning {
		diffs = append(diffs, fmt.Sprintf("desired_running: %d -> %d", old.DesiredRunning, updated.DesiredRunning))
	}
	if old.DesiredStopped != updated.DesiredStopped {
		diffs = append(diffs, fmt.Sprintf("desired_stopped: %d -> %d", old.DesiredStopped, updated.DesiredStopped))
	}
	if old.IdleTimeoutMinutes != updated.IdleTimeoutMinutes {
		diffs = append(diffs, fmt.Sprintf("idle_timeout_minutes: %d -> %d", old.IdleTimeoutMinutes, updated.IdleTimeoutMinutes))
	}
	if old.Arch != updated.Arch {
		diffs = append(diffs, fmt.Sprintf("arch: %q -> %q", old.Arch, updated.Arch))
	}
	if old.CPUMin != updated.CPUMin {
		diffs = append(diffs, fmt.Sprintf("cpu_min: %d -> %d", old.CPUMin, updated.CPUMin))
	}
	if old.CPUMax != updated.CPUMax {
		diffs = append(diffs, fmt.Sprintf("cpu_max: %d -> %d", old.CPUMax, updated.CPUMax))
	}
	if old.RAMMin != updated.RAMMin {
		diffs = append(diffs, fmt.Sprintf("ram_min: %g -> %g", old.RAMMin, updated.RAMMin))
	}
	if old.RAMMax != updated.RAMMax {
		diffs = append(diffs, fmt.Sprintf("ram_max: %g -> %g", old.RAMMax, updated.RAMMax))
	}
	if old.Environment != updated.Environment {
		diffs = append(diffs, fmt.Sprintf("environment: %q -> %q", old.Environment, updated.Environment))
	}
	if old.Region != updated.Region {
		diffs = append(diffs, fmt.Sprintf("region: %q -> %q", old.Region, updated.Region))
	}
	oldFamilies := slices.Sorted(slices.Values(old.Families))
	updatedFamilies := slices.Sorted(slices.Values(updated.Families))
	if fmt.Sprint(oldFamilies) != fmt.Sprint(updatedFamilies) {
		diffs = append(diffs, fmt.Sprintf("families: %v -> %v", oldFamilies, updatedFamilies))
	}
	if !reflect.DeepEqual(old.Schedules, updated.Schedules) {
		diffs = append(diffs, fmt.Sprintf("schedules: %d -> %d entries", len(old.Schedules), len(updated.Schedules)))
	}

	if len(diffs) == 0 {
		return "none"
	}
	return strings.Join(diffs, "; ")
}
