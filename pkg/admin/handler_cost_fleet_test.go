package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/db"
)

type fakeFleetCostStore struct {
	days []db.FleetCostDay
	err  error
}

func (f *fakeFleetCostStore) GetFleetCostDays(_ context.Context, _, _ string) ([]db.FleetCostDay, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.days, nil
}

func fleetSummary(t *testing.T, h *CostHandler) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	h.GetCostSummary(rec, httptest.NewRequest(http.MethodGet, "/api/cost/summary", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return body
}

func fleetCostJobs() *mockCostDB {
	return &mockCostDB{jobs: []db.AdminJobEntry{{
		JobID: 1, InstanceType: "c7g.xlarge", DurationSeconds: 3600,
		Status: string(db.JobStatusCompleted), CompletedAt: testCompletedAt,
	}}}
}

// Without a fleet store the response must be exactly what it is today. A
// deployment that has not enabled sampling, or one running an older
// orchestrator, must see no behaviour change at all.
func TestCostSummaryOmitsFleetWhenNoStoreIsWired(t *testing.T) {
	t.Parallel()

	h := NewCostHandler(fleetCostJobs(), NewAuthMiddleware(""), nil, nil)
	body := fleetSummary(t, h)

	if _, present := body["fleet"]; present {
		t.Error("response carries a fleet block with no store wired; it must be absent")
	}
	if _, present := body["total_cost"]; !present {
		t.Error("total_cost missing; the existing figures must keep working")
	}
}

// Sampling enabled but nothing recorded yet (fresh deploy) is not zero cost.
// A $0.00 fleet figure beside a non-zero attributed cost would read as
// "the fleet has no overhead", which is exactly backwards.
func TestCostSummaryOmitsFleetBeforeAnythingIsSampled(t *testing.T) {
	t.Parallel()

	h := NewCostHandler(fleetCostJobs(), NewAuthMiddleware(""), nil, nil)
	h.SetFleetCostStore(&fakeFleetCostStore{})

	if _, present := fleetSummary(t, h)["fleet"]; present {
		t.Error("fleet block present with no sampled days; it must be absent, not zero")
	}
}

// The fleet number is secondary. A failure reading it must never take down the
// page that already works.
func TestCostSummarySurvivesAFleetStoreFailure(t *testing.T) {
	t.Parallel()

	h := NewCostHandler(fleetCostJobs(), NewAuthMiddleware(""), nil, nil)
	h.SetFleetCostStore(&fakeFleetCostStore{err: errors.New("dynamo down")})

	body := fleetSummary(t, h)
	if _, present := body["fleet"]; present {
		t.Error("fleet block present despite a store failure")
	}
	if _, present := body["total_cost"]; !present {
		t.Error("total_cost missing; a fleet failure must not break the job figures")
	}
}

func TestCostSummaryReportsSampledFleetCost(t *testing.T) {
	t.Parallel()

	today := time.Now().UTC().Format(db.FleetDayFormat)
	h := NewCostHandler(fleetCostJobs(), NewAuthMiddleware(""), nil, nil)
	h.SetFleetCostStore(&fakeFleetCostStore{days: []db.FleetCostDay{{
		Day:               today,
		TotalCost:         10,
		ComputeCost:       9,
		EBSCost:           1,
		InstanceSeconds:   1000,
		AttributedSeconds: 400,
	}}})

	fleet, ok := fleetSummary(t, h)["fleet"].(map[string]any)
	if !ok {
		t.Fatal("fleet block missing")
	}
	if got := fleet["total_cost"].(float64); !approx(got, 10) {
		t.Errorf("fleet total_cost = %v, want 10", got)
	}
	if got := fleet["attributed_percent"].(float64); !approx(got, 40) {
		t.Errorf("attributed_percent = %v, want 40", got)
	}
	if got := fleet["unattributed_cost"].(float64); !approx(got, 6) {
		t.Errorf("unattributed_cost = %v, want 6", got)
	}
	// EBS is priced from an assumed volume size, so the response must say so
	// rather than let it pass as measured.
	if estimated, _ := fleet["ebs_estimated"].(bool); !estimated {
		t.Error("ebs_estimated = false, want the estimate labelled")
	}
}

// A total known to understate must say so, and say why, rather than being
// presented as a complete month.
func TestCostSummaryWarnsWhenTheFleetTotalIsPartial(t *testing.T) {
	t.Parallel()

	today := time.Now().UTC().Format(db.FleetDayFormat)
	h := NewCostHandler(fleetCostJobs(), NewAuthMiddleware(""), nil, nil)
	h.SetFleetCostStore(&fakeFleetCostStore{days: []db.FleetCostDay{{
		Day: today, TotalCost: 10, InstanceSeconds: 100, AttributedSeconds: 50, Partial: true,
	}}})

	fleet := fleetSummary(t, h)["fleet"].(map[string]any)
	if partial, _ := fleet["partial"].(bool); !partial {
		t.Error("partial = false, want the clamped day surfaced")
	}
	if warning, _ := fleet["warning"].(string); warning == "" {
		t.Error("warning is empty, want a caveat explaining the understatement")
	}
}

// Before the sampler has run a full month, days_covered lags days_in_period.
// The UI needs both so it can say "N days of data" instead of implying a month.
func TestCostSummaryReportsHowManyDaysWereSampled(t *testing.T) {
	t.Parallel()

	today := time.Now().UTC().Format(db.FleetDayFormat)
	h := NewCostHandler(fleetCostJobs(), NewAuthMiddleware(""), nil, nil)
	h.SetFleetCostStore(&fakeFleetCostStore{days: []db.FleetCostDay{{
		Day: today, TotalCost: 1, InstanceSeconds: 60, AttributedSeconds: 60,
	}}})

	fleet := fleetSummary(t, h)["fleet"].(map[string]any)
	if got := fleet["days_covered"].(float64); got != 1 {
		t.Errorf("days_covered = %v, want 1", got)
	}
	if _, present := fleet["days_in_period"]; !present {
		t.Error("days_in_period missing; the UI cannot qualify the window without it")
	}
}

// The fleet figure must not silently change what the Metrics tab reports:
// CostMTD stays the job-attributed number it has always been.
func TestCostMTDIsUnaffectedByFleetSampling(t *testing.T) {
	t.Parallel()

	today := time.Now().UTC().Format(db.FleetDayFormat)
	h := NewCostHandler(fleetCostJobs(), NewAuthMiddleware(""), nil, nil)
	before, err := h.CostMTD(context.Background())
	if err != nil {
		t.Fatalf("CostMTD() error = %v", err)
	}

	h.SetFleetCostStore(&fakeFleetCostStore{days: []db.FleetCostDay{{
		Day: today, TotalCost: 999, InstanceSeconds: 100, AttributedSeconds: 10,
	}}})
	after, err := h.CostMTD(context.Background())
	if err != nil {
		t.Fatalf("CostMTD() error = %v", err)
	}

	if !approx(before, after) {
		t.Errorf("CostMTD changed from %v to %v; it must stay the job-attributed figure", before, after)
	}
}

// Guards the help text the UI shows: the existing per-job total must not start
// claiming to be fleet-wide.
func TestCostSummaryKeepsTotalCostAsTheJobAttributedFigure(t *testing.T) {
	t.Parallel()

	today := time.Now().UTC().Format(db.FleetDayFormat)
	h := NewCostHandler(fleetCostJobs(), NewAuthMiddleware(""), nil, nil)
	h.SetFleetCostStore(&fakeFleetCostStore{days: []db.FleetCostDay{{
		Day: today, TotalCost: 500, InstanceSeconds: 100, AttributedSeconds: 10,
	}}})

	body := fleetSummary(t, h)
	total := body["total_cost"].(float64)
	fleet := body["fleet"].(map[string]any)["total_cost"].(float64)
	if approx(total, fleet) {
		t.Error("total_cost equals fleet cost; the headline must stay job-attributed")
	}
	if total <= 0 {
		t.Error("total_cost = 0, want the job figure unchanged")
	}
}

func TestFleetWarningNamesTheCause(t *testing.T) {
	t.Parallel()

	if w := fleetWarning(true, 2, 5); !strings.Contains(w, "5") || !strings.Contains(w, "2") {
		t.Errorf("warning %q should name how many days of the period were sampled", w)
	}
	if w := fleetWarning(false, 5, 5); w != "" {
		t.Errorf("warning = %q, want empty when the period is fully sampled", w)
	}
}
