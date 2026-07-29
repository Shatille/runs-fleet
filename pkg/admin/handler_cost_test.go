package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/cost"
	"github.com/Shavakan/runs-fleet/pkg/db"
)

const testArchARM64 = "arm64"

// testCompletedAt stamps fixture jobs as finished so they survive the
// CompletedOnly predicate the cost endpoints send.
var testCompletedAt = time.Now().UTC()

type mockCostDB struct {
	jobs      []db.AdminJobEntry
	err       error
	gotFilter db.AdminJobFilter
}

// ListJobsForAdmin mirrors the store's CompletedOnly semantics: only rows
// carrying a completed_at timestamp are returned.
func (m *mockCostDB) ListJobsForAdmin(_ context.Context, filter db.AdminJobFilter) ([]db.AdminJobEntry, int, error) {
	m.gotFilter = filter
	if m.err != nil {
		return nil, 0, m.err
	}
	if !filter.CompletedOnly {
		return m.jobs, len(m.jobs), nil
	}
	matched := make([]db.AdminJobEntry, 0, len(m.jobs))
	for _, job := range m.jobs {
		if !job.CompletedAt.IsZero() {
			matched = append(matched, job)
		}
	}
	return matched, len(matched), nil
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
			{JobID: 1, InstanceType: "c7g.xlarge", Spot: true, DurationSeconds: 3600, Status: string(db.JobStatusCompleted), CompletedAt: testCompletedAt},
			{JobID: 2, InstanceType: "c7g.xlarge", Spot: false, DurationSeconds: 3600, Status: string(db.JobStatusCompleted), CompletedAt: testCompletedAt},
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
			{JobID: 1, InstanceType: "c7g.xlarge", Spot: true, DurationSeconds: 3600, Status: string(db.JobStatusCompleted), CompletedAt: testCompletedAt},
			{JobID: 2, InstanceType: "c7g.xlarge", Spot: false, DurationSeconds: 3600, Status: string(db.JobStatusCompleted), CompletedAt: testCompletedAt},
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
			{JobID: 1, InstanceType: "c7g.xlarge", Spot: true, DurationSeconds: 3600, Status: string(db.JobStatusCompleted), CompletedAt: testCompletedAt},
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
			{JobID: 1, InstanceType: "c7g.xlarge", Spot: true, DurationSeconds: 600, Status: string(db.JobStatusCompleted), CompletedAt: testCompletedAt},
			{JobID: 2, InstanceType: "made-up.type", Spot: false, DurationSeconds: 600, Status: string(db.JobStatusCompleted), CompletedAt: testCompletedAt},
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

func TestCostHandler_PricesEveryFinishedJobConclusion(t *testing.T) {
	t.Parallel()

	// Terminal rows carry GitHub's raw conclusion, not "completed"; all of them
	// burned billable EC2 time and must be priced. Unfinished rows have no
	// duration, so pricing them would invent cost via the 0.5h fallback.
	mockDB := &mockCostDB{jobs: []db.AdminJobEntry{
		{JobID: 1, InstanceType: "t4g.medium", DurationSeconds: 3600, Status: "success", CompletedAt: testCompletedAt},
		{JobID: 2, InstanceType: "t4g.medium", DurationSeconds: 3600, Status: "failure", CompletedAt: testCompletedAt},
		{JobID: 3, InstanceType: "t4g.medium", DurationSeconds: 3600, Status: "interrupted", CompletedAt: testCompletedAt},
		{JobID: 4, InstanceType: "t4g.medium", DurationSeconds: 3600, Status: string(db.JobStatusOrphaned), CompletedAt: testCompletedAt},
		{JobID: 5, InstanceType: "t4g.medium", Status: string(db.JobStatusRunning)},
		{JobID: 6, InstanceType: "t4g.medium", Status: string(db.JobStatusRequeued)},
	}}

	handler := NewCostHandler(mockDB, NewAuthMiddleware(""), nil, nil)
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

	if resp.JobCount != 4 {
		t.Errorf("job_count = %d, want 4 (every finished conclusion, no unfinished job)", resp.JobCount)
	}
	want := 4 * cost.GetInstancePrice("t4g.medium")
	if !approx(resp.OnDemandCost, want) {
		t.Errorf("on-demand cost = %v, want %v (running/requeued must not be priced)", resp.OnDemandCost, want)
	}
	if !approx(resp.TotalCost, want) {
		t.Errorf("total cost = %v, want %v", resp.TotalCost, want)
	}
}

func TestCostHandler_FilterNeverUsesAStatusString(t *testing.T) {
	t.Parallel()

	for _, path := range []string{"/api/cost/summary", "/api/cost/daily", "/api/cost/by-pool", "/api/cost/by-repository"} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()

			mockDB := &mockCostDB{}
			handler := NewCostHandler(mockDB, NewAuthMiddleware(""), nil, nil)
			mux := http.NewServeMux()
			handler.RegisterRoutes(mux)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))

			if rec.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
			}
			f := mockDB.gotFilter
			if !f.CompletedOnly {
				t.Error("filter must select finished jobs via CompletedOnly")
			}
			if f.Status != "" {
				t.Errorf("filter status = %q, want empty (status strings drift)", f.Status)
			}
			if f.Limit != 0 {
				t.Errorf("filter limit = %d, want 0 (a cap would silently undercount)", f.Limit)
			}
			if f.Since.IsZero() {
				t.Error("filter must bound the window with Since")
			}
		})
	}
}

func TestCostHandler_CostMTD_UsesTheFinishedJobPredicate(t *testing.T) {
	t.Parallel()

	mockDB := &mockCostDB{jobs: []db.AdminJobEntry{
		{JobID: 1, InstanceType: "t4g.medium", DurationSeconds: 3600, Status: "success", CompletedAt: testCompletedAt},
		{JobID: 2, InstanceType: "t4g.medium", Status: string(db.JobStatusRunning)},
	}}
	handler := NewCostHandler(mockDB, NewAuthMiddleware(""), nil, nil)

	total, err := handler.CostMTD(context.Background())
	if err != nil {
		t.Fatalf("CostMTD: %v", err)
	}
	if !mockDB.gotFilter.CompletedOnly || mockDB.gotFilter.Status != "" {
		t.Errorf("filter = %+v, want CompletedOnly with no status string", mockDB.gotFilter)
	}
	if want := cost.GetInstancePrice("t4g.medium"); !approx(total, want) {
		t.Errorf("CostMTD = %v, want %v (only the finished job priced)", total, want)
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
				CompletedAt:     testCompletedAt,
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
			{JobID: 1, InstanceType: "c7g.xlarge", Spot: true, DurationSeconds: 0, Status: string(db.JobStatusCompleted), CompletedAt: testCompletedAt},
			{JobID: 2, InstanceType: "c7g.xlarge", Spot: true, DurationSeconds: 600, Status: string(db.JobStatusCompleted), CompletedAt: testCompletedAt},
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

func TestCostHandler_Daily(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 30, 0, 0, time.UTC)
	mockDB := &mockCostDB{jobs: []db.AdminJobEntry{
		{JobID: 1, InstanceType: "t4g.medium", DurationSeconds: 3600, Status: string(db.JobStatusCompleted), CreatedAt: monthStart, CompletedAt: testCompletedAt},
		{JobID: 2, InstanceType: "t4g.medium", DurationSeconds: 3600, Status: string(db.JobStatusCompleted), CreatedAt: now, CompletedAt: testCompletedAt},
	}}

	// nil pricers -> hard-coded fallback (t4g.medium = $0.0336/hr on-demand).
	handler := NewCostHandler(mockDB, NewAuthMiddleware(""), nil, nil)
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest("GET", "/api/cost/daily", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var resp CostDailyResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	wantDays := int(now.Sub(time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)).Hours()/24) + 1
	if len(resp.Days) != wantDays {
		t.Errorf("zero-filled days = %d, want %d", len(resp.Days), wantDays)
	}

	var totalJobs int
	var totalCost float64
	byDate := map[string]CostDayEntry{}
	for _, d := range resp.Days {
		totalJobs += d.JobCount
		totalCost += d.TotalCost
		byDate[d.Date] = d
	}
	if totalJobs != 2 {
		t.Errorf("summed job_count = %d, want 2", totalJobs)
	}
	if !approx(totalCost, 2*0.0336) {
		t.Errorf("summed total_cost = %f, want %f", totalCost, 2*0.0336)
	}
	if _, ok := byDate[monthStart.Format("2006-01-02")]; !ok {
		t.Errorf("missing bucket for month start %s", monthStart.Format("2006-01-02"))
	}
	if e := byDate[now.Format("2006-01-02")]; e.JobCount < 1 {
		t.Errorf("today's bucket job_count = %d, want >= 1", e.JobCount)
	}
}

func TestCostHandler_ComputeDailyStartAfterEnd(t *testing.T) {
	t.Parallel()

	handler := NewCostHandler(&mockCostDB{}, NewAuthMiddleware(""), nil, nil)
	start := time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)

	resp := handler.computeDaily(context.Background(), nil, start, end)
	if resp == nil {
		t.Fatal("computeDaily returned nil")
	}
	if len(resp.Days) != 0 {
		t.Errorf("Days = %d entries, want 0 when start is after end", len(resp.Days))
	}
}

func TestCostHandler_ByPool(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	mockDB := &mockCostDB{jobs: []db.AdminJobEntry{
		{JobID: 1, Pool: "default", InstanceType: "t4g.medium", DurationSeconds: 3600, Spot: true, Status: string(db.JobStatusCompleted), CreatedAt: now, CompletedAt: testCompletedAt},
		{JobID: 2, Pool: "default", InstanceType: "t4g.medium", DurationSeconds: 3600, Status: string(db.JobStatusCompleted), CreatedAt: now, CompletedAt: testCompletedAt},
		{JobID: 3, Pool: "", InstanceType: "t4g.medium", DurationSeconds: 3600, Status: string(db.JobStatusCompleted), CreatedAt: now, CompletedAt: testCompletedAt},
	}}

	handler := NewCostHandler(mockDB, NewAuthMiddleware(""), nil, nil)
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest("GET", "/api/cost/by-pool", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var resp CostByPoolResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(resp.Pools) != 2 {
		t.Fatalf("pools = %d, want 2", len(resp.Pools))
	}
	// Sorted by total_cost desc: default (2 jobs) before cold-start (1 job).
	if resp.Pools[0].Pool != "default" {
		t.Errorf("pools[0] = %q, want default", resp.Pools[0].Pool)
	}
	if resp.Pools[1].Pool != "cold-start" {
		t.Errorf("pools[1] = %q, want cold-start", resp.Pools[1].Pool)
	}

	byName := map[string]CostPoolEntry{}
	for _, p := range resp.Pools {
		byName[p.Pool] = p
	}
	if byName["default"].JobCount != 2 {
		t.Errorf("default job_count = %d, want 2", byName["default"].JobCount)
	}
	if !approx(byName["default"].SpotPercent, 50) {
		t.Errorf("default spot_percent = %f, want 50", byName["default"].SpotPercent)
	}
	// default = one on-demand ($0.0336) + one spot ($0.0336 * 0.3 = $0.01008).
	if !approx(byName["default"].TotalCost, 0.0336+0.0336*(1-cost.SpotDiscount)) {
		t.Errorf("default total_cost = %f", byName["default"].TotalCost)
	}
	if !approx(byName["cold-start"].TotalCost, 0.0336) {
		t.Errorf("cold-start total_cost = %f, want 0.0336", byName["cold-start"].TotalCost)
	}
}

func TestCostHandler_ByRepository(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	mockDB := &mockCostDB{jobs: []db.AdminJobEntry{
		{JobID: 1, Repo: "org/api", InstanceType: "t4g.medium", DurationSeconds: 3600, Spot: true, Status: string(db.JobStatusCompleted), CreatedAt: now, CompletedAt: testCompletedAt},
		{JobID: 2, Repo: "org/api", InstanceType: "t4g.medium", DurationSeconds: 3600, Status: string(db.JobStatusCompleted), CreatedAt: now, CompletedAt: testCompletedAt},
		{JobID: 3, Repo: "org/web", InstanceType: "t4g.medium", DurationSeconds: 3600, Status: string(db.JobStatusCompleted), CreatedAt: now, CompletedAt: testCompletedAt},
		{JobID: 4, Repo: "", InstanceType: "t4g.medium", DurationSeconds: 3600, Status: string(db.JobStatusCompleted), CreatedAt: now, CompletedAt: testCompletedAt},
	}}

	handler := NewCostHandler(mockDB, NewAuthMiddleware(""), nil, nil)
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest("GET", "/api/cost/by-repository", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var resp CostByRepositoryResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(resp.Repositories) != 3 {
		t.Fatalf("repositories = %d, want 3", len(resp.Repositories))
	}
	// Sorted by total_cost desc: org/api (2 jobs) leads.
	if resp.Repositories[0].Repository != "org/api" {
		t.Errorf("repositories[0] = %q, want org/api", resp.Repositories[0].Repository)
	}

	byName := map[string]CostRepositoryEntry{}
	for _, r := range resp.Repositories {
		byName[r.Repository] = r
	}
	if byName["org/api"].JobCount != 2 {
		t.Errorf("org/api job_count = %d, want 2", byName["org/api"].JobCount)
	}
	if !approx(byName["org/api"].SpotPercent, 50) {
		t.Errorf("org/api spot_percent = %f, want 50", byName["org/api"].SpotPercent)
	}
	// org/api = one on-demand ($0.0336) + one spot ($0.0336 * 0.3 = $0.01008).
	wantAPI := 0.0336 + 0.0336*(1-cost.SpotDiscount)
	if !approx(byName["org/api"].TotalCost, wantAPI) {
		t.Errorf("org/api total_cost = %f, want %f", byName["org/api"].TotalCost, wantAPI)
	}
	if !approx(byName["org/api"].AvgCostPerJob, wantAPI/2) {
		t.Errorf("org/api avg_cost_per_job = %f, want %f", byName["org/api"].AvgCostPerJob, wantAPI/2)
	}
	// Jobs with no recorded repo are grouped under the "unknown" pseudo-repo
	// rather than dropped, so the rows still sum to the summary's TotalCost.
	if !approx(byName["unknown"].TotalCost, 0.0336) {
		t.Errorf("unknown total_cost = %f, want 0.0336", byName["unknown"].TotalCost)
	}
}

// The repository breakdown must account for every priced job so the table
// reconciles with the Total Cost tile -- no silent top-N truncation.
func TestCostHandler_ByRepositoryReconcilesWithSummary(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	jobs := make([]db.AdminJobEntry, 0, 50)
	for i := 0; i < 50; i++ {
		jobs = append(jobs, db.AdminJobEntry{
			JobID:           int64(i),
			Repo:            fmt.Sprintf("org/repo-%d", i),
			InstanceType:    "t4g.medium",
			DurationSeconds: 3600,
			Spot:            i%2 == 0,
			Status:          string(db.JobStatusCompleted),
			CreatedAt:       now,
			CompletedAt:     testCompletedAt,
		})
	}
	mockDB := &mockCostDB{jobs: jobs}

	handler := NewCostHandler(mockDB, NewAuthMiddleware(""), nil, nil)
	ctx := context.Background()
	byRepo := handler.computeByRepository(ctx, jobs, now, now)
	summary := handler.computeCostSummary(ctx, jobs, now, now)

	if len(byRepo.Repositories) != 50 {
		t.Fatalf("repositories = %d, want 50 (no truncation)", len(byRepo.Repositories))
	}
	var sum float64
	for _, r := range byRepo.Repositories {
		sum += r.TotalCost
	}
	if !approx(sum, summary.TotalCost) {
		t.Errorf("repository costs sum to %f, want %f (summary total)", sum, summary.TotalCost)
	}
}
