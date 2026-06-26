package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/cost"
	"github.com/Shavakan/runs-fleet/pkg/db"
	"github.com/Shavakan/runs-fleet/pkg/fleet"
	"github.com/Shavakan/runs-fleet/pkg/logging"
)

// CostDB defines the database operations required by the cost handler.
type CostDB interface {
	ListJobsForAdmin(ctx context.Context, filter db.AdminJobFilter) ([]db.AdminJobEntry, int, error)
}

// onDemandPricer supplies live on-demand hourly prices by instance type
// (satisfied by *cost.PriceFetcher). Nil-safe: the handler falls back to the
// hard-coded table when absent.
type onDemandPricer interface {
	GetPrice(ctx context.Context, instanceType string) (float64, error)
}

// spotPricer supplies the current market spot hourly price by instance type
// (satisfied by *fleet.Manager). The bool is false when no price is available,
// so the handler falls back to the fixed spot-discount estimate.
type spotPricer interface {
	SpotPrice(ctx context.Context, instanceType string) (float64, bool)
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

	// Runner-minute cost expresses the same usage in the standard hosted-runner
	// unit (vCPU-minutes × per-vCPU-minute rate), broken down by (arch, vCPU).
	RunnerMinuteCost      float64             `json:"runner_minute_cost"`
	RunnerMinuteRates     map[string]float64  `json:"runner_minute_rates"`
	RunnerMinuteBreakdown []RunnerMinuteEntry `json:"runner_minute_breakdown"`
}

// RunnerMinuteEntry is one (arch, vCPU) row of the runner-minute cost matrix.
type RunnerMinuteEntry struct {
	Arch          string  `json:"arch"`
	Vcpu          int     `json:"vcpu"`
	RunnerMinutes float64 `json:"runner_minutes"`
	VcpuMinutes   float64 `json:"vcpu_minutes"`
	Cost          float64 `json:"cost"`
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
	db       CostDB
	auth     *AuthMiddleware
	onDemand onDemandPricer
	spot     spotPricer
	rates    map[string]float64
	log      *logging.Logger
}

// NewCostHandler creates a new cost handler. onDemand and spot supply live
// AWS prices; both may be nil, in which case pricing falls back to the
// hard-coded on-demand table and fixed spot discount.
func NewCostHandler(db CostDB, auth *AuthMiddleware, onDemand onDemandPricer, spot spotPricer) *CostHandler {
	return &CostHandler{
		db:       db,
		auth:     auth,
		onDemand: onDemand,
		spot:     spot,
		// A fresh copy — h.rates is exposed in the JSON response and must never
		// become a handle to the package default map.
		rates: cost.DefaultRunnerMinuteRates(),
		log:   logging.WithComponent(logging.LogTypeAdmin, "cost"),
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

	summary := h.computeCostSummary(ctx, jobs, periodStart, periodEnd)
	h.writeJSON(w, http.StatusOK, summary)
}

// archVcpuKey keys the runner-minute matrix by (architecture, vCPU count).
type archVcpuKey struct {
	arch string
	vcpu int
}

func (h *CostHandler) computeCostSummary(ctx context.Context, jobs []db.AdminJobEntry, start, end time.Time) *CostSummaryResponse {
	type familyAccum struct {
		jobCount  int
		hours     float64
		cost      float64
		spotCount int
	}

	type shapeAccum struct {
		arch        string
		vcpu        int
		runnerMins  float64
		vcpuMinutes float64
		cost        float64
	}

	families := make(map[string]*familyAccum)
	shapes := make(map[archVcpuKey]*shapeAccum)
	var totalCost, spotCost, onDemandCost, spotSavings, runnerMinuteCost float64
	var spotJobCount, onDemandCount int

	// Per-request memoization so each distinct instance type is priced once,
	// even though the underlying fetchers also cache across requests.
	odMemo := make(map[string]float64)
	type spotResult struct {
		price float64
		live  bool
	}
	spotMemo := make(map[string]spotResult)

	for _, job := range jobs {
		instanceType := job.InstanceType
		if instanceType == "" {
			instanceType = "t4g.medium"
		}

		durationHours := float64(job.DurationSeconds) / 3600
		if durationHours <= 0 {
			durationHours = 0.5
		}

		onDemandHourly, ok := odMemo[instanceType]
		if !ok {
			onDemandHourly = cost.GetInstancePrice(instanceType)
			if h.onDemand != nil {
				if live, err := h.onDemand.GetPrice(ctx, instanceType); err != nil {
					h.log.Warn(ctx, "live on-demand price unavailable, using fallback",
						slog.String(logging.KeyInstanceType, instanceType),
						slog.String(logging.KeyError, err.Error()))
				} else if live > 0 {
					onDemandHourly = live
				}
			}
			odMemo[instanceType] = onDemandHourly
		}

		var jobCost float64
		if job.Spot {
			sr, seen := spotMemo[instanceType]
			if !seen {
				if h.spot != nil {
					if sp, found := h.spot.SpotPrice(ctx, instanceType); found && sp > 0 {
						sr = spotResult{price: sp, live: true}
					}
				}
				spotMemo[instanceType] = sr
			}
			if sr.live {
				jobCost = durationHours * sr.price
				if saving := durationHours * (onDemandHourly - sr.price); saving > 0 {
					spotSavings += saving
				}
			} else {
				jobCost = durationHours * onDemandHourly * (1 - cost.SpotDiscount)
				spotSavings += durationHours * onDemandHourly * cost.SpotDiscount
			}
			spotCost += jobCost
			spotJobCount++
		} else {
			jobCost = durationHours * onDemandHourly
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

		// Runner-minute matrix, keyed by (arch, vCPU). Uses the actual reported
		// duration (not the EC2-cost 0.5h fallback) so it faithfully reflects
		// billable runner-minutes, matching the RunnerExecutionSeconds metric.
		// Skips zero-duration jobs, instance types not in the catalog (arch/vCPU
		// unknown), and shapes without a configured rate.
		if job.DurationSeconds <= 0 {
			continue
		}
		spec, found := fleet.GetInstanceSpec(instanceType)
		if !found {
			continue
		}
		rate, priced := h.rates[spec.Arch]
		if !priced {
			continue
		}
		key := archVcpuKey{arch: spec.Arch, vcpu: spec.CPU}
		shape, ok := shapes[key]
		if !ok {
			shape = &shapeAccum{arch: spec.Arch, vcpu: spec.CPU}
			shapes[key] = shape
		}
		mins := float64(job.DurationSeconds) / 60
		vcpuMins := mins * float64(spec.CPU)
		shapeCost := vcpuMins * rate
		shape.runnerMins += mins
		shape.vcpuMinutes += vcpuMins
		shape.cost += shapeCost
		runnerMinuteCost += shapeCost
	}

	runnerBreakdown := make([]RunnerMinuteEntry, 0, len(shapes))
	for _, s := range shapes {
		runnerBreakdown = append(runnerBreakdown, RunnerMinuteEntry{
			Arch:          s.arch,
			Vcpu:          s.vcpu,
			RunnerMinutes: s.runnerMins,
			VcpuMinutes:   s.vcpuMinutes,
			Cost:          s.cost,
		})
	}
	sort.Slice(runnerBreakdown, func(i, j int) bool {
		if runnerBreakdown[i].Arch != runnerBreakdown[j].Arch {
			return runnerBreakdown[i].Arch < runnerBreakdown[j].Arch
		}
		return runnerBreakdown[i].Vcpu < runnerBreakdown[j].Vcpu
	})

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
		PeriodStart:           start.Format(time.RFC3339),
		PeriodEnd:             end.Format(time.RFC3339),
		TotalCost:             totalCost,
		SpotCost:              spotCost,
		OnDemandCost:          onDemandCost,
		SpotSavings:           spotSavings,
		AvgCostPerJob:         avgCost,
		JobCount:              len(jobs),
		SpotJobCount:          spotJobCount,
		OnDemandCount:         onDemandCount,
		FamilyBreakdown:       breakdown,
		RunnerMinuteCost:      runnerMinuteCost,
		RunnerMinuteRates:     h.rates,
		RunnerMinuteBreakdown: runnerBreakdown,
	}
}

func (h *CostHandler) writeJSON(w http.ResponseWriter, status int, data interface{}) {
	// Response-writer helper with no request/context in scope.
	ctx := context.Background()
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(data); err != nil {
		h.log.Error(ctx, "json encode failed", slog.String(logging.KeyError, err.Error()))
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := buf.WriteTo(w); err != nil {
		h.log.Error(ctx, "write response failed", slog.String(logging.KeyError, err.Error()))
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
