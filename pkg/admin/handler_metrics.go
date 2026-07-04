package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/db"
	"github.com/Shavakan/runs-fleet/pkg/logging"
)

// metricsWindow is the trailing window for the jobs_24h / startup / interruption
// figures in the metrics summary.
const metricsWindow = 24 * time.Hour

// metricsDB is the subset of job queries the metrics summary needs.
type metricsDB interface {
	GetJobStatsForAdmin(ctx context.Context, since time.Time) (*db.AdminJobStats, error)
	ListJobsForAdmin(ctx context.Context, filter db.AdminJobFilter) ([]db.AdminJobEntry, int, error)
}

// costMTDProvider supplies the month-to-date total cost (satisfied by *CostHandler).
type costMTDProvider interface {
	CostMTD(ctx context.Context) (float64, error)
}

// JobsWindow is the job count breakdown over the trailing 24h window.
type JobsWindow struct {
	Total      int `json:"total"`
	Completed  int `json:"completed"`
	Failed     int `json:"failed"`
	InProgress int `json:"in_progress"`
}

// MetricsSummaryResponse is the aggregate operational snapshot: it stitches
// together job counts, warm-pool hit rate, average startup latency, a
// best-effort spot-interruption estimate, and month-to-date cost.
type MetricsSummaryResponse struct {
	Jobs24h           JobsWindow `json:"jobs_24h"`
	WarmPoolHitRate   float64    `json:"warm_pool_hit_rate"`
	AvgStartupSeconds float64    `json:"avg_startup_time_seconds"`
	// SpotInterruptionRate is a best-effort estimate; SpotInterruptionRateEstimated
	// is always true until Phase 3 stores real interruption history.
	SpotInterruptionRate          float64 `json:"spot_interruption_rate"`
	SpotInterruptionRateEstimated bool    `json:"spot_interruption_rate_estimated"`
	// CostMTDUSD is nil (omitted) when the cost lookup failed, so the UI can
	// distinguish "unavailable" from a real $0.00.
	CostMTDUSD *float64 `json:"cost_mtd_usd,omitempty"`
}

// MetricsHandler provides the aggregate metrics summary endpoint.
type MetricsHandler struct {
	db   metricsDB
	cost costMTDProvider
	auth *AuthMiddleware
	log  *logging.Logger
}

// NewMetricsHandler creates a new metrics handler.
func NewMetricsHandler(db metricsDB, cost costMTDProvider, auth *AuthMiddleware) *MetricsHandler {
	return &MetricsHandler{
		db:   db,
		cost: cost,
		auth: auth,
		log:  logging.WithComponent(logging.LogTypeAdmin, "metrics"),
	}
}

// RegisterRoutes registers metrics API routes on the given mux.
func (h *MetricsHandler) RegisterRoutes(mux *http.ServeMux) {
	mux.Handle("GET /api/metrics/summary", h.auth.WrapFunc(h.GetSummary))
}

// GetSummary handles GET /api/metrics/summary.
func (h *MetricsHandler) GetSummary(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	since := time.Now().Add(-metricsWindow)

	stats, err := h.db.GetJobStatsForAdmin(ctx, since)
	if err != nil {
		h.writeError(w, http.StatusInternalServerError, "Failed to get job stats", err.Error())
		return
	}

	resp := MetricsSummaryResponse{
		Jobs24h: JobsWindow{
			Total:      stats.Total,
			Completed:  stats.Completed,
			Failed:     stats.Failed,
			InProgress: stats.Running,
		},
		SpotInterruptionRateEstimated: true,
	}
	if stats.Completed > 0 {
		resp.WarmPoolHitRate = float64(stats.WarmPoolHit) / float64(stats.Completed)
	}

	// Average startup latency and the interruption estimate need the raw job
	// entries, not just the aggregate counts.
	entries, _, err := h.db.ListJobsForAdmin(ctx, db.AdminJobFilter{Since: since, Limit: 10000})
	if err != nil {
		h.writeError(w, http.StatusInternalServerError, "Failed to list jobs", err.Error())
		return
	}
	resp.AvgStartupSeconds = avgStartupSeconds(entries)
	resp.SpotInterruptionRate = spotInterruptionRate(entries)

	if mtd, err := h.cost.CostMTD(ctx); err != nil {
		h.log.Warn(ctx, "cost mtd unavailable", slog.String(logging.KeyError, err.Error()))
	} else {
		resp.CostMTDUSD = &mtd
	}

	h.writeJSON(w, http.StatusOK, resp)
}

// avgStartupSeconds averages StartedAt-CreatedAt over jobs where both are set
// and ordered sanely.
func avgStartupSeconds(entries []db.AdminJobEntry) float64 {
	var sum float64
	var n int
	for _, e := range entries {
		if e.CreatedAt.IsZero() || e.StartedAt.IsZero() || e.StartedAt.Before(e.CreatedAt) {
			continue
		}
		sum += e.StartedAt.Sub(e.CreatedAt).Seconds()
		n++
	}
	if n == 0 {
		return 0
	}
	return sum / float64(n)
}

// spotInterruptionRate is a best-effort estimate: the share of spot jobs that
// were retried or requeued. It conflates spot interruptions with bootstrap
// failures and stale-claim re-claims (all bump retry_count), so it over-counts;
// an exact figure awaits the Phase 3 interruption history.
func spotInterruptionRate(entries []db.AdminJobEntry) float64 {
	var spot, interrupted int
	for _, e := range entries {
		if !e.Spot {
			continue
		}
		spot++
		if e.RetryCount > 0 || e.Status == string(db.JobStatusRequeued) {
			interrupted++
		}
	}
	if spot == 0 {
		return 0
	}
	return float64(interrupted) / float64(spot)
}

func (h *MetricsHandler) writeJSON(w http.ResponseWriter, status int, data interface{}) {
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

func (h *MetricsHandler) writeError(w http.ResponseWriter, status int, message, details string) {
	resp := ErrorResponse{Error: message}
	if details != "" {
		resp.Details = details
	}
	h.writeJSON(w, status, resp)
}
