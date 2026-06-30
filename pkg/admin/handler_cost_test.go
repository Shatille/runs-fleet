package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/cost"
	"github.com/Shavakan/runs-fleet/pkg/db"
)

const testArchARM64 = "arm64"

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

type fakeOnDemandPricer struct{ prices map[string]float64 }

func (f *fakeOnDemandPricer) GetPrice(_ context.Context, instanceType string) (float64, error) {
	if p, ok := f.prices[instanceType]; ok {
		return p, nil
	}
	return 0, errors.New("no price")
}

type fakeSpotPricer struct{ prices map[string]float64 }

func (f *fakeSpotPricer) SpotPrice(_ context.Context, instanceType string) (float64, bool) {
	p, ok := f.prices[instanceType]
	return p, ok
}

func approx(got, want float64) bool {
	d := got - want
	return d < 1e-9 && d > -1e-9
}

func TestCostHandler_LivePricing(t *testing.T) {
	t.Parallel()

	// Live prices differ from the hard-coded table (c7g.xlarge = $0.145), proving
	// the live values are used.
	od := &fakeOnDemandPricer{prices: map[string]float64{"c7g.xlarge": 0.10}}
	sp := &fakeSpotPricer{prices: map[string]float64{"c7g.xlarge": 0.03}}
	mockDB := &mockCostDB{
		jobs: []db.AdminJobEntry{
			{JobID: 1, InstanceType: "c7g.xlarge", Spot: true, DurationSeconds: 3600, Status: string(db.JobStatusCompleted)},
			{JobID: 2, InstanceType: "c7g.xlarge", Spot: false, DurationSeconds: 3600, Status: string(db.JobStatusCompleted)},
		},
	}

	handler := NewCostHandler(mockDB, NewAuthMiddleware(""), od, sp)
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/cost/summary", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp CostSummaryResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Spot job: 1h × $0.03 = $0.03. On-demand job: 1h × $0.10 = $0.10.
	// Savings = 1h × ($0.10 − $0.03) = $0.07.
	if !approx(resp.SpotCost, 0.03) {
		t.Errorf("spot cost = %v, want 0.03 (live spot rate)", resp.SpotCost)
	}
	if !approx(resp.OnDemandCost, 0.10) {
		t.Errorf("on-demand cost = %v, want 0.10 (live on-demand rate)", resp.OnDemandCost)
	}
	if !approx(resp.SpotSavings, 0.07) {
		t.Errorf("spot savings = %v, want 0.07 (on-demand − spot)", resp.SpotSavings)
	}
	if !approx(resp.TotalCost, 0.13) {
		t.Errorf("total cost = %v, want 0.13", resp.TotalCost)
	}
}

func TestCostHandler_NilPricersUseHardcodedFallback(t *testing.T) {
	t.Parallel()

	// With nil pricers the handler must reproduce the pre-live-pricing math:
	// the hard-coded on-demand table + fixed spot discount.
	mockDB := &mockCostDB{
		jobs: []db.AdminJobEntry{
			{JobID: 1, InstanceType: "c7g.xlarge", Spot: true, DurationSeconds: 3600, Status: string(db.JobStatusCompleted)},
			{JobID: 2, InstanceType: "c7g.xlarge", Spot: false, DurationSeconds: 3600, Status: string(db.JobStatusCompleted)},
		},
	}

	handler := NewCostHandler(mockDB, NewAuthMiddleware(""), nil, nil)
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/cost/summary", nil))

	var resp CostSummaryResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	od := cost.GetInstancePrice("c7g.xlarge")
	if !approx(resp.OnDemandCost, od) {
		t.Errorf("on-demand cost = %v, want hard-coded %v", resp.OnDemandCost, od)
	}
	if !approx(resp.SpotCost, od*(1-cost.SpotDiscount)) {
		t.Errorf("spot cost = %v, want hard-coded discount %v", resp.SpotCost, od*(1-cost.SpotDiscount))
	}
}

func TestCostHandler_SpotFallsBackToDiscountWhenNoLiveSpotPrice(t *testing.T) {
	t.Parallel()

	// On-demand price is live; no spot price available → fall back to the fixed
	// spot discount applied to the live on-demand rate.
	od := &fakeOnDemandPricer{prices: map[string]float64{"c7g.xlarge": 0.20}}
	sp := &fakeSpotPricer{prices: map[string]float64{}}
	mockDB := &mockCostDB{
		jobs: []db.AdminJobEntry{
			{JobID: 1, InstanceType: "c7g.xlarge", Spot: true, DurationSeconds: 3600, Status: string(db.JobStatusCompleted)},
		},
	}

	handler := NewCostHandler(mockDB, NewAuthMiddleware(""), od, sp)
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/cost/summary", nil))

	var resp CostSummaryResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// 1h × $0.20 × (1 − 0.7) = $0.06; savings = 1h × $0.20 × 0.7 = $0.14.
	if !approx(resp.SpotCost, 0.06) {
		t.Errorf("spot cost = %v, want 0.06 (live on-demand × discount)", resp.SpotCost)
	}
	if !approx(resp.SpotSavings, 0.14) {
		t.Errorf("spot savings = %v, want 0.14", resp.SpotSavings)
	}
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
	handler := NewCostHandler(mockDB, auth, nil, nil)

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

	// Runner-minute matrix: all four jobs are arm64 (c7g/t4g/m7g), in two vCPU
	// shapes — (arm64,2): c7g.large+t4g.medium+m7g.large, (arm64,4): c7g.xlarge.
	if len(resp.RunnerMinuteBreakdown) != 2 {
		t.Fatalf("expected 2 runner-minute shapes, got %d: %+v", len(resp.RunnerMinuteBreakdown), resp.RunnerMinuteBreakdown)
	}
	if resp.RunnerMinuteCost <= 0 {
		t.Error("expected positive runner-minute cost")
	}
	if len(resp.RunnerMinuteRates) == 0 {
		t.Error("expected runner-minute rates in response")
	}
	// arm64/4 row = c7g.xlarge, 300s = 5 min, 20 vCPU-min, 20*0.00125 = $0.025.
	var arm4 *RunnerMinuteEntry
	for i := range resp.RunnerMinuteBreakdown {
		if resp.RunnerMinuteBreakdown[i].Arch == testArchARM64 && resp.RunnerMinuteBreakdown[i].Vcpu == 4 {
			arm4 = &resp.RunnerMinuteBreakdown[i]
		}
	}
	if arm4 == nil {
		t.Fatal("expected an arm64/4 runner-minute row")
	}
	if arm4.VcpuMinutes != 20 {
		t.Errorf("arm64/4 vcpu-minutes = %v, want 20", arm4.VcpuMinutes)
	}
	if d := arm4.Cost - 0.025; d > 1e-9 || d < -1e-9 {
		t.Errorf("arm64/4 cost = %v, want 0.025", arm4.Cost)
	}
}

func TestCostHandler_RunnerMinuteBreakdown_UnknownInstanceTypeExcluded(t *testing.T) {
	t.Parallel()

	mockDB := &mockCostDB{
		jobs: []db.AdminJobEntry{
			{JobID: 1, InstanceType: "c7g.xlarge", Spot: true, DurationSeconds: 600, Status: string(db.JobStatusCompleted)},
			{JobID: 2, InstanceType: "made-up.type", Spot: false, DurationSeconds: 600, Status: string(db.JobStatusCompleted)},
		},
	}

	handler := NewCostHandler(mockDB, NewAuthMiddleware(""), nil, nil)
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)
	req := httptest.NewRequest("GET", "/api/cost/summary", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp CostSummaryResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Only the catalogued c7g.xlarge contributes: arm64/4, 600s = 10 min,
	// 40 vCPU-min, 40*0.00125 = $0.05. The made-up type is excluded.
	if len(resp.RunnerMinuteBreakdown) != 1 {
		t.Fatalf("expected 1 runner-minute shape, got %d", len(resp.RunnerMinuteBreakdown))
	}
	row := resp.RunnerMinuteBreakdown[0]
	if row.Arch != testArchARM64 || row.Vcpu != 4 || row.RunnerMinutes != 10 || row.VcpuMinutes != 40 {
		t.Errorf("unexpected row: %+v", row)
	}
	if d := resp.RunnerMinuteCost - 0.05; d > 1e-9 || d < -1e-9 {
		t.Errorf("runner-minute cost = %v, want 0.05", resp.RunnerMinuteCost)
	}
}

func TestCostHandler_GetCostSummary_Empty(t *testing.T) {
	t.Parallel()

	mockDB := &mockCostDB{jobs: []db.AdminJobEntry{}}

	auth := NewAuthMiddleware("")
	handler := NewCostHandler(mockDB, auth, nil, nil)

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
	handler := NewCostHandler(mockDB, auth, nil, nil)

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
	handler := NewCostHandler(mockDB, auth, nil, nil)

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

func TestCostHandler_RunnerMinuteBreakdown_ZeroDurationExcluded(t *testing.T) {
	t.Parallel()

	mockDB := &mockCostDB{
		jobs: []db.AdminJobEntry{
			// Zero duration: contributes to the EC2-cost fallback but must NOT
			// fabricate runner-minutes in the matrix.
			{JobID: 1, InstanceType: "c7g.xlarge", Spot: true, DurationSeconds: 0, Status: string(db.JobStatusCompleted)},
			{JobID: 2, InstanceType: "c7g.xlarge", Spot: true, DurationSeconds: 600, Status: string(db.JobStatusCompleted)},
		},
	}

	handler := NewCostHandler(mockDB, NewAuthMiddleware(""), nil, nil)
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)
	req := httptest.NewRequest("GET", "/api/cost/summary", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp CostSummaryResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Only the 600s job counts: arm64/4, 10 min, 40 vCPU-min.
	if len(resp.RunnerMinuteBreakdown) != 1 {
		t.Fatalf("expected 1 runner-minute shape, got %d", len(resp.RunnerMinuteBreakdown))
	}
	if got := resp.RunnerMinuteBreakdown[0].VcpuMinutes; got != 40 {
		t.Errorf("vcpu-minutes = %v, want 40 (zero-duration job excluded)", got)
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
