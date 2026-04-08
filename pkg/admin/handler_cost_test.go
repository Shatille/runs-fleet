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

type mockCostDB struct {
	jobs []db.AdminJobEntry
	err  error
}

func (m *mockCostDB) ListJobsForAdmin(_ context.Context, _ db.AdminJobFilter) ([]db.AdminJobEntry, int, error) {
	if m.err != nil {
		return nil, 0, m.err
	}
	return m.jobs, len(m.jobs), nil
}

func TestCostHandler_GetCostSummary_MixedInstances(t *testing.T) {
	t.Parallel()

	now := time.Now()
	mockDB := &mockCostDB{
		jobs: []db.AdminJobEntry{
			{
				JobID:           1,
				InstanceType:    "c7g.large",
				Spot:            true,
				DurationSeconds: 600,
				Status:          string(db.JobStatusCompleted),
				CompletedAt:     now,
			},
			{
				JobID:           2,
				InstanceType:    "t4g.medium",
				Spot:            false,
				DurationSeconds: 1200,
				Status:          string(db.JobStatusCompleted),
				CompletedAt:     now,
			},
			{
				JobID:           3,
				InstanceType:    "c7g.xlarge",
				Spot:            true,
				DurationSeconds: 300,
				Status:          string(db.JobStatusCompleted),
				CompletedAt:     now,
			},
			{
				JobID:           4,
				InstanceType:    "m7g.large",
				Spot:            false,
				DurationSeconds: 900,
				Status:          string(db.JobStatusCompleted),
				CompletedAt:     now,
			},
		},
	}

	auth := NewAuthMiddleware("")
	handler := NewCostHandler(mockDB, auth)

	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest("GET", "/api/cost/summary", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp CostSummaryResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.JobCount != 4 {
		t.Errorf("expected 4 jobs, got %d", resp.JobCount)
	}
	if resp.SpotJobCount != 2 {
		t.Errorf("expected 2 spot jobs, got %d", resp.SpotJobCount)
	}
	if resp.OnDemandCount != 2 {
		t.Errorf("expected 2 on-demand jobs, got %d", resp.OnDemandCount)
	}
	if resp.TotalCost <= 0 {
		t.Error("expected positive total cost")
	}
	if resp.SpotCost <= 0 {
		t.Error("expected positive spot cost")
	}
	if resp.OnDemandCost <= 0 {
		t.Error("expected positive on-demand cost")
	}
	if resp.SpotSavings <= 0 {
		t.Error("expected positive spot savings")
	}
	if resp.AvgCostPerJob <= 0 {
		t.Error("expected positive avg cost per job")
	}
	if len(resp.FamilyBreakdown) != 3 {
		t.Errorf("expected 3 families (c7g, t4g, m7g), got %d", len(resp.FamilyBreakdown))
	}
}

func TestCostHandler_GetCostSummary_Empty(t *testing.T) {
	t.Parallel()

	mockDB := &mockCostDB{jobs: []db.AdminJobEntry{}}

	auth := NewAuthMiddleware("")
	handler := NewCostHandler(mockDB, auth)

	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest("GET", "/api/cost/summary", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}

	var resp CostSummaryResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.JobCount != 0 {
		t.Errorf("expected 0 jobs, got %d", resp.JobCount)
	}
	if resp.TotalCost != 0 {
		t.Errorf("expected 0 total cost, got %f", resp.TotalCost)
	}
	if resp.AvgCostPerJob != 0 {
		t.Errorf("expected 0 avg cost, got %f", resp.AvgCostPerJob)
	}
}

func TestCostHandler_GetCostSummary_DBError(t *testing.T) {
	t.Parallel()

	mockDB := &mockCostDB{err: errors.New("database unavailable")}

	auth := NewAuthMiddleware("")
	handler := NewCostHandler(mockDB, auth)

	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest("GET", "/api/cost/summary", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("expected status 500, got %d", rec.Code)
	}

	var errResp ErrorResponse
	if err := json.NewDecoder(rec.Body).Decode(&errResp); err != nil {
		t.Fatalf("failed to decode error response: %v", err)
	}

	if errResp.Error == "" {
		t.Error("expected non-empty error message")
	}
}

func TestCostHandler_GetCostSummary_MissingInstanceType(t *testing.T) {
	t.Parallel()

	mockDB := &mockCostDB{
		jobs: []db.AdminJobEntry{
			{
				JobID:           1,
				InstanceType:    "",
				Spot:            true,
				DurationSeconds: 600,
				Status:          string(db.JobStatusCompleted),
			},
		},
	}

	auth := NewAuthMiddleware("")
	handler := NewCostHandler(mockDB, auth)

	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest("GET", "/api/cost/summary", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}

	var resp CostSummaryResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.JobCount != 1 {
		t.Errorf("expected 1 job, got %d", resp.JobCount)
	}
	if resp.TotalCost <= 0 {
		t.Error("expected positive total cost even with missing instance type")
	}
}

func TestExtractFamily(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input    string
		expected string
	}{
		{"c7g.large", "c7g"},
		{"t4g.medium", "t4g"},
		{"m7g.2xlarge", "m7g"},
		{"unknown", "unknown"},
		{"", "unknown"},
	}

	for _, tt := range tests {
		got := extractFamily(tt.input)
		if got != tt.expected {
			t.Errorf("extractFamily(%q) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}
