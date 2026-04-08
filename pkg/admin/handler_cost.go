package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/cost"
	"github.com/Shavakan/runs-fleet/pkg/db"
	"github.com/Shavakan/runs-fleet/pkg/logging"
)

// CostDB defines the database operations required by the cost handler.
type CostDB interface {
	ListJobsForAdmin(ctx context.Context, filter db.AdminJobFilter) ([]db.AdminJobEntry, int, error)
}

// CostSummaryResponse represents the cost summary API response.
type CostSummaryResponse struct {
	PeriodStart    string                `json:"period_start"`
	PeriodEnd      string                `json:"period_end"`
	TotalCost      float64               `json:"total_cost"`
	SpotCost       float64               `json:"spot_cost"`
	OnDemandCost   float64               `json:"on_demand_cost"`
	SpotSavings    float64               `json:"spot_savings"`
	AvgCostPerJob  float64               `json:"avg_cost_per_job"`
	JobCount       int                   `json:"job_count"`
	SpotJobCount   int                   `json:"spot_job_count"`
	OnDemandCount  int                   `json:"on_demand_count"`
	FamilyBreakdown []FamilyBreakdownEntry `json:"family_breakdown"`
}

// FamilyBreakdownEntry represents cost breakdown for one instance family.
type FamilyBreakdownEntry struct {
	Family      string  `json:"family"`
	JobCount    int     `json:"job_count"`
	TotalHours  float64 `json:"total_hours"`
	TotalCost   float64 `json:"total_cost"`
	SpotPercent float64 `json:"spot_percent"`
}

// CostHandler provides HTTP endpoints for cost reporting.
type CostHandler struct {
	db   CostDB
	auth *AuthMiddleware
	log  *logging.Logger
}

// NewCostHandler creates a new cost handler.
func NewCostHandler(db CostDB, auth *AuthMiddleware) *CostHandler {
	return &CostHandler{
		db:   db,
		auth: auth,
		log:  logging.WithComponent(logging.LogTypeAdmin, "cost"),
	}
}

// RegisterRoutes registers cost API routes on the given mux.
func (h *CostHandler) RegisterRoutes(mux *http.ServeMux) {
	mux.Handle("GET /api/cost/summary", h.auth.WrapFunc(h.GetCostSummary))
}

// GetCostSummary handles GET /api/cost/summary.
func (h *CostHandler) GetCostSummary(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	now := time.Now().UTC()
	periodStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	periodEnd := now

	jobs, _, err := h.db.ListJobsForAdmin(ctx, db.AdminJobFilter{
		Status: string(db.JobStatusCompleted),
		Since:  periodStart,
		Limit:  10000,
	})
	if err != nil {
		h.writeError(w, http.StatusInternalServerError, "Failed to fetch jobs", err.Error())
		return
	}

	summary := h.computeCostSummary(jobs, periodStart, periodEnd)
	h.writeJSON(w, http.StatusOK, summary)
}

func (h *CostHandler) computeCostSummary(jobs []db.AdminJobEntry, start, end time.Time) *CostSummaryResponse {
	type familyAccum struct {
		jobCount  int
		hours     float64
		cost      float64
		spotCount int
	}

	families := make(map[string]*familyAccum)
	var totalCost, spotCost, onDemandCost, spotSavings float64
	var spotJobCount, onDemandCount int

	for _, job := range jobs {
		instanceType := job.InstanceType
		if instanceType == "" {
			instanceType = "t4g.medium"
		}

		durationHours := float64(job.DurationSeconds) / 3600
		if durationHours <= 0 {
			durationHours = 0.5
		}

		hourlyPrice := cost.GetInstancePrice(instanceType)

		var jobCost float64
		if job.Spot {
			jobCost = durationHours * hourlyPrice * (1 - cost.SpotDiscount)
			spotCost += jobCost
			spotSavings += durationHours * hourlyPrice * cost.SpotDiscount
			spotJobCount++
		} else {
			jobCost = durationHours * hourlyPrice
			onDemandCost += jobCost
			onDemandCount++
		}
		totalCost += jobCost

		family := extractFamily(instanceType)
		acc, ok := families[family]
		if !ok {
			acc = &familyAccum{}
			families[family] = acc
		}
		acc.jobCount++
		acc.hours += durationHours
		acc.cost += jobCost
		if job.Spot {
			acc.spotCount++
		}
	}

	breakdown := make([]FamilyBreakdownEntry, 0, len(families))
	for fam, acc := range families {
		spotPct := 0.0
		if acc.jobCount > 0 {
			spotPct = float64(acc.spotCount) / float64(acc.jobCount) * 100
		}
		breakdown = append(breakdown, FamilyBreakdownEntry{
			Family:      fam,
			JobCount:    acc.jobCount,
			TotalHours:  acc.hours,
			TotalCost:   acc.cost,
			SpotPercent: spotPct,
		})
	}

	avgCost := 0.0
	if len(jobs) > 0 {
		avgCost = totalCost / float64(len(jobs))
	}

	return &CostSummaryResponse{
		PeriodStart:     start.Format(time.RFC3339),
		PeriodEnd:       end.Format(time.RFC3339),
		TotalCost:       totalCost,
		SpotCost:        spotCost,
		OnDemandCost:    onDemandCost,
		SpotSavings:     spotSavings,
		AvgCostPerJob:   avgCost,
		JobCount:        len(jobs),
		SpotJobCount:    spotJobCount,
		OnDemandCount:   onDemandCount,
		FamilyBreakdown: breakdown,
	}
}

func (h *CostHandler) writeJSON(w http.ResponseWriter, status int, data interface{}) {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(data); err != nil {
		h.log.Error("json encode failed", slog.String(logging.KeyError, err.Error()))
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := buf.WriteTo(w); err != nil {
		h.log.Error("write response failed", slog.String(logging.KeyError, err.Error()))
	}
}

func (h *CostHandler) writeError(w http.ResponseWriter, status int, message, details string) {
	resp := ErrorResponse{Error: message}
	if details != "" {
		resp.Details = details
	}
	h.writeJSON(w, status, resp)
}

func extractFamily(instanceType string) string {
	if instanceType == "" {
		return "unknown"
	}
	parts := strings.SplitN(instanceType, ".", 2)
	if len(parts) >= 1 && parts[0] != "" {
		return parts[0]
	}
	return "unknown"
}
