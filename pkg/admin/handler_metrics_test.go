package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/db"
)

type mockMetricsDB struct {
	stats   *db.AdminJobStats
	entries []db.AdminJobEntry
	err     error
}

func (m *mockMetricsDB) GetJobStatsForAdmin(_ context.Context, _ time.Time) (*db.AdminJobStats, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.stats, nil
}

func (m *mockMetricsDB) ListJobsForAdmin(_ context.Context, _ db.AdminJobFilter) ([]db.AdminJobEntry, int, error) {
	if m.err != nil {
		return nil, 0, m.err
	}
	return m.entries, len(m.entries), nil
}

type stubCostMTD struct {
	value float64
	err   error
}

func (s *stubCostMTD) CostMTD(_ context.Context) (float64, error) {
	return s.value, s.err
}

func TestMetricsHandler_GetSummary(t *testing.T) {
	t.Parallel()

	now := time.Now()
	entries := []db.AdminJobEntry{
		{CreatedAt: now.Add(-time.Hour), StartedAt: now.Add(-time.Hour).Add(30 * time.Second)},
		{CreatedAt: now.Add(-2 * time.Hour), StartedAt: now.Add(-2 * time.Hour).Add(50 * time.Second)},
		{CreatedAt: now, StartedAt: time.Time{}}, // no start -> skipped from startup avg
		{Spot: true, RetryCount: 0, Status: string(db.JobStatusCompleted)},
		{Spot: true, RetryCount: 2, Status: string(db.JobStatusCompleted)}, // interrupted (retry)
		{Spot: true, RetryCount: 0, Status: string(db.JobStatusRequeued)},  // interrupted (requeued)
		{Spot: true, RetryCount: 0, Status: string(db.JobStatusCompleted)},
	}
	mockDB := &mockMetricsDB{
		stats:   &db.AdminJobStats{Total: 10, Completed: 8, Failed: 1, Running: 1, WarmPoolHit: 6},
		entries: entries,
	}
	handler := NewMetricsHandler(mockDB, &stubCostMTD{value: 52.30}, NewAuthMiddleware(""))

	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)
	req := httptest.NewRequest("GET", "/api/metrics/summary", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var resp MetricsSummaryResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if resp.Jobs24h != (JobsWindow{Total: 10, Completed: 8, Failed: 1, InProgress: 1}) {
		t.Errorf("jobs_24h = %+v", resp.Jobs24h)
	}
	if !approx(resp.WarmPoolHitRate, 0.75) {
		t.Errorf("warm_pool_hit_rate = %f, want 0.75", resp.WarmPoolHitRate)
	}
	if !approx(resp.AvgStartupSeconds, 40) {
		t.Errorf("avg_startup_time_seconds = %f, want 40", resp.AvgStartupSeconds)
	}
	if !approx(resp.SpotInterruptionRate, 0.5) {
		t.Errorf("spot_interruption_rate = %f, want 0.5", resp.SpotInterruptionRate)
	}
	if !resp.SpotInterruptionRateEstimated {
		t.Error("spot_interruption_rate_estimated = false, want true")
	}
	if !approx(resp.CostMTDUSD, 52.30) {
		t.Errorf("cost_mtd_usd = %f, want 52.30", resp.CostMTDUSD)
	}
}

func TestMetricsHandler_EmptyWindow(t *testing.T) {
	t.Parallel()

	mockDB := &mockMetricsDB{stats: &db.AdminJobStats{}, entries: nil}
	handler := NewMetricsHandler(mockDB, &stubCostMTD{value: 0}, NewAuthMiddleware(""))

	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)
	req := httptest.NewRequest("GET", "/api/metrics/summary", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var resp MetricsSummaryResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.WarmPoolHitRate != 0 || resp.AvgStartupSeconds != 0 || resp.SpotInterruptionRate != 0 {
		t.Errorf("empty window should yield zero rates, got %+v", resp)
	}
}

func TestMetricsHandler_CostErrorTolerated(t *testing.T) {
	t.Parallel()

	mockDB := &mockMetricsDB{stats: &db.AdminJobStats{Total: 1, Completed: 1}, entries: nil}
	handler := NewMetricsHandler(mockDB, &stubCostMTD{err: errors.New("pricing down")}, NewAuthMiddleware(""))

	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)
	req := httptest.NewRequest("GET", "/api/metrics/summary", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (cost error must not fail the summary)", rec.Code)
	}
	var resp MetricsSummaryResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.CostMTDUSD != 0 {
		t.Errorf("cost_mtd_usd = %f, want 0 on cost error", resp.CostMTDUSD)
	}
}

func TestMetricsHandler_StatsError(t *testing.T) {
	t.Parallel()

	mockDB := &mockMetricsDB{err: errors.New("db down")}
	handler := NewMetricsHandler(mockDB, &stubCostMTD{}, NewAuthMiddleware(""))

	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)
	req := httptest.NewRequest("GET", "/api/metrics/summary", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}
