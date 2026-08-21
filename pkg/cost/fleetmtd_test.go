package cost_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/cost"
	"github.com/Shavakan/runs-fleet/pkg/db"
)

type fakeFleetStore struct {
	days     []db.FleetCostDay
	err      error
	gotFrom  string
	gotUntil string
}

func (f *fakeFleetStore) GetFleetCostDays(_ context.Context, fromDay, toDay string) ([]db.FleetCostDay, error) {
	f.gotFrom, f.gotUntil = fromDay, toDay
	return f.days, f.err
}

func day(d string, cost, instSecs, attrSecs float64) db.FleetCostDay {
	return db.FleetCostDay{
		Day:               d,
		TotalCost:         cost,
		ComputeCost:       cost * 0.9,
		EBSCost:           cost * 0.1,
		InstanceSeconds:   instSecs,
		AttributedSeconds: attrSecs,
	}
}

func TestComputeFleetMTDSumsEveryRecordedDay(t *testing.T) {
	store := &fakeFleetStore{days: []db.FleetCostDay{
		day("2026-08-18", 1.0, 600, 300),
		day("2026-08-19", 2.0, 600, 300),
		day("2026-08-20", 3.0, 600, 300),
	}}
	start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)

	got, err := cost.ComputeFleetMTD(context.Background(), store, start, now)
	if err != nil {
		t.Fatalf("ComputeFleetMTD() error = %v", err)
	}
	if got == nil {
		t.Fatal("ComputeFleetMTD() = nil, want a result")
	}
	if !approx(got.TotalCost, 6.0) {
		t.Errorf("TotalCost = %v, want 6.0", got.TotalCost)
	}
	if got.DaysCovered != 3 {
		t.Errorf("DaysCovered = %d, want 3", got.DaysCovered)
	}
}

// No rollups is not zero cost — it means the sampler has not run. Returning a
// zero-valued result would render as "$0.00 fleet cost" beside a non-zero
// attributed cost, which reads as "there is no overhead". Absent means absent.
func TestComputeFleetMTDReturnsNilWhenNothingHasBeenSampled(t *testing.T) {
	store := &fakeFleetStore{}
	start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)

	got, err := cost.ComputeFleetMTD(context.Background(), store, start, now)
	if err != nil {
		t.Fatalf("ComputeFleetMTD() error = %v", err)
	}
	if got != nil {
		t.Errorf("ComputeFleetMTD() = %+v, want nil so the caller omits the figure", got)
	}
}

func TestComputeFleetMTDPropagatesStoreErrors(t *testing.T) {
	store := &fakeFleetStore{err: errors.New("dynamo down")}
	start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

	if _, err := cost.ComputeFleetMTD(context.Background(), store, start, start.AddDate(0, 0, 5)); err == nil {
		t.Fatal("ComputeFleetMTD() error = nil, want the store failure surfaced")
	}
}

// A day the sampler missed entirely must lower coverage and mark the result
// partial, so the total is never presented as complete when it understates.
func TestComputeFleetMTDMarksAGapPartial(t *testing.T) {
	store := &fakeFleetStore{days: []db.FleetCostDay{
		day("2026-08-18", 1.0, 600, 300),
		day("2026-08-20", 3.0, 600, 300),
	}}
	start := time.Date(2026, 8, 18, 0, 0, 0, 0, time.UTC)
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)

	got, err := cost.ComputeFleetMTD(context.Background(), store, start, now)
	if err != nil {
		t.Fatalf("ComputeFleetMTD() error = %v", err)
	}
	if !got.Partial {
		t.Error("Partial = false, want true when a day in the window has no rollup")
	}
	if got.DaysCovered != 2 || got.DaysInPeriod != 3 {
		t.Errorf("DaysCovered/DaysInPeriod = %d/%d, want 2/3", got.DaysCovered, got.DaysInPeriod)
	}
}

// A day whose sampler had to clamp propagates its partial flag upward.
func TestComputeFleetMTDInheritsAPartialDay(t *testing.T) {
	partialDay := day("2026-08-20", 3.0, 600, 300)
	partialDay.Partial = true
	store := &fakeFleetStore{days: []db.FleetCostDay{partialDay}}
	start := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)

	got, err := cost.ComputeFleetMTD(context.Background(), store, start, now)
	if err != nil {
		t.Fatalf("ComputeFleetMTD() error = %v", err)
	}
	if !got.Partial {
		t.Error("Partial = false, want a clamped day to propagate")
	}
}

// Coverage from the sampler's own pass: attributed instance-seconds over total
// instance-seconds. Both come from one observation, so the ratio never compares
// two independently sourced numbers.
func TestComputeFleetMTDDerivesCoverageFromSampledSeconds(t *testing.T) {
	store := &fakeFleetStore{days: []db.FleetCostDay{
		day("2026-08-20", 4.0, 1000, 250),
	}}
	start := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)

	got, err := cost.ComputeFleetMTD(context.Background(), store, start, now)
	if err != nil {
		t.Fatalf("ComputeFleetMTD() error = %v", err)
	}
	if !approx(got.AttributedPercent, 25) {
		t.Errorf("AttributedPercent = %v, want 25", got.AttributedPercent)
	}
	if !approx(got.AttributedCost, 1.0) {
		t.Errorf("AttributedCost = %v, want 1.0 (25%% of 4.0)", got.AttributedCost)
	}
	if !approx(got.UnattributedCost, 3.0) {
		t.Errorf("UnattributedCost = %v, want 3.0", got.UnattributedCost)
	}
}

// A window where nothing ran at all must not divide by zero.
func TestComputeFleetMTDHandlesAZeroSecondWindow(t *testing.T) {
	store := &fakeFleetStore{days: []db.FleetCostDay{day("2026-08-20", 0, 0, 0)}}
	start := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)

	got, err := cost.ComputeFleetMTD(context.Background(), store, start, now)
	if err != nil {
		t.Fatalf("ComputeFleetMTD() error = %v", err)
	}
	if got.AttributedPercent != 0 {
		t.Errorf("AttributedPercent = %v, want 0 for an idle window", got.AttributedPercent)
	}
}

// The query must span the whole requested period, inclusive of today, or the
// current day's accumulating rollup is dropped from the total.
func TestComputeFleetMTDQueriesTheWholePeriodInclusiveOfToday(t *testing.T) {
	store := &fakeFleetStore{days: []db.FleetCostDay{day("2026-08-20", 1, 60, 60)}}
	start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)

	if _, err := cost.ComputeFleetMTD(context.Background(), store, start, now); err != nil {
		t.Fatalf("ComputeFleetMTD() error = %v", err)
	}
	if store.gotFrom != "2026-08-01" {
		t.Errorf("queried from %q, want 2026-08-01", store.gotFrom)
	}
	if store.gotUntil != "2026-08-20" {
		t.Errorf("queried until %q, want today inclusive", store.gotUntil)
	}
}

// A day can carry cost with no recorded instance-seconds only if a future
// change accumulates one without the other. Coverage must degrade to zero
// rather than dividing by it and rendering NaN in the UI.
func TestComputeFleetMTDReportsZeroCoverageWhenSecondsAreMissing(t *testing.T) {
	store := &fakeFleetStore{days: []db.FleetCostDay{
		{Day: "2026-08-20", TotalCost: 5, InstanceSeconds: 0, AttributedSeconds: 0},
	}}
	start := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)

	got, err := cost.ComputeFleetMTD(context.Background(), store, start, now)
	if err != nil {
		t.Fatalf("ComputeFleetMTD() error = %v", err)
	}
	if got.AttributedPercent != 0 || got.AttributedCost != 0 {
		t.Errorf("AttributedPercent/Cost = %v/%v, want 0 when no seconds were recorded",
			got.AttributedPercent, got.AttributedCost)
	}
	// The whole cost is unattributed when nothing is known to have run a job:
	// claiming otherwise would overstate how much of the fleet did useful work.
	if !approx(got.UnattributedCost, 0) {
		t.Errorf("UnattributedCost = %v, want 0 rather than a guess", got.UnattributedCost)
	}
}

// Day keys are written by the sampler in the reporting zone, so the reader must
// format its range in the same zone or an off-by-one day drops the current
// day's accumulating rollup out of the queried window.
func TestComputeFleetMTDQueriesInTheReportingZone(t *testing.T) {
	seoul, err := time.LoadLocation("Asia/Seoul")
	if err != nil {
		t.Fatalf("load zone: %v", err)
	}
	store := &fakeFleetStore{days: []db.FleetCostDay{day("2026-08-21", 1, 60, 60)}}

	// 23:30 UTC on the 20th is already 08:30 on the 21st in Seoul.
	now := time.Date(2026, 8, 20, 23, 30, 0, 0, time.UTC)
	start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

	if _, err := cost.ComputeFleetMTDIn(context.Background(), store, start, now, seoul); err != nil {
		t.Fatalf("ComputeFleetMTDIn() error = %v", err)
	}
	if store.gotUntil != "2026-08-21" {
		t.Errorf("queried until %q, want 2026-08-21 (the local date)", store.gotUntil)
	}
}
