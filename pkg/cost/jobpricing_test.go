package cost_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Shavakan/runs-fleet/pkg/cost"
	"github.com/Shavakan/runs-fleet/pkg/db"
)

type fakeOnDemandPricer struct {
	prices map[string]float64
	calls  int
}

func (f *fakeOnDemandPricer) GetPrice(_ context.Context, instanceType string) (float64, error) {
	f.calls++
	if p, ok := f.prices[instanceType]; ok {
		return p, nil
	}
	return 0, errors.New("no price")
}

type fakeSpotPricer struct {
	prices map[string]float64
	calls  int
}

func (f *fakeSpotPricer) SpotPrice(_ context.Context, instanceType string) (float64, bool) {
	f.calls++
	p, ok := f.prices[instanceType]
	return p, ok
}

func approx(got, want float64) bool {
	d := got - want
	return d < 1e-9 && d > -1e-9
}

func TestJobPricer_LiveSpotUsed(t *testing.T) {
	t.Parallel()

	od := &fakeOnDemandPricer{prices: map[string]float64{"c7g.xlarge": 0.10}}
	sp := &fakeSpotPricer{prices: map[string]float64{"c7g.xlarge": 0.03}}
	p := cost.NewJobPricer(od, sp)

	got := p.Price(context.Background(), db.AdminJobEntry{InstanceType: "c7g.xlarge", Spot: true, DurationSeconds: 3600})
	if !approx(got.Total, 0.03) {
		t.Errorf("total = %v, want 0.03 (live spot rate)", got.Total)
	}
	if !approx(got.Spot, 0.03) {
		t.Errorf("spot = %v, want 0.03", got.Spot)
	}
	if !approx(got.OnDemand, 0) {
		t.Errorf("on-demand = %v, want 0 for a spot job", got.OnDemand)
	}
	if !approx(got.Savings, 0.07) {
		t.Errorf("savings = %v, want 0.07 (on-demand - spot)", got.Savings)
	}
	if !approx(got.Hours, 1) {
		t.Errorf("hours = %v, want 1", got.Hours)
	}
}

func TestJobPricer_OnDemandLive(t *testing.T) {
	t.Parallel()

	od := &fakeOnDemandPricer{prices: map[string]float64{"c7g.xlarge": 0.10}}
	sp := &fakeSpotPricer{prices: map[string]float64{}}
	p := cost.NewJobPricer(od, sp)

	got := p.Price(context.Background(), db.AdminJobEntry{InstanceType: "c7g.xlarge", Spot: false, DurationSeconds: 3600})
	if !approx(got.Total, 0.10) {
		t.Errorf("total = %v, want 0.10 (live on-demand rate)", got.Total)
	}
	if !approx(got.OnDemand, 0.10) {
		t.Errorf("on-demand = %v, want 0.10", got.OnDemand)
	}
	if !approx(got.Spot, 0) {
		t.Errorf("spot = %v, want 0 for on-demand job", got.Spot)
	}
}

func TestJobPricer_SpotFallsBackToDiscount(t *testing.T) {
	t.Parallel()

	od := &fakeOnDemandPricer{prices: map[string]float64{"c7g.xlarge": 0.20}}
	sp := &fakeSpotPricer{prices: map[string]float64{}}
	p := cost.NewJobPricer(od, sp)

	got := p.Price(context.Background(), db.AdminJobEntry{InstanceType: "c7g.xlarge", Spot: true, DurationSeconds: 3600})
	// 1h * 0.20 * (1 - 0.7) = 0.06; savings = 1h * 0.20 * 0.7 = 0.14.
	if !approx(got.Total, 0.06) {
		t.Errorf("total = %v, want 0.06 (live on-demand x discount)", got.Total)
	}
	if !approx(got.Savings, 0.14) {
		t.Errorf("savings = %v, want 0.14", got.Savings)
	}
}

func TestJobPricer_NilPricersUseHardcodedFallback(t *testing.T) {
	t.Parallel()

	p := cost.NewJobPricer(nil, nil)
	od := cost.GetInstancePrice("c7g.xlarge")

	spot := p.Price(context.Background(), db.AdminJobEntry{InstanceType: "c7g.xlarge", Spot: true, DurationSeconds: 3600})
	if !approx(spot.Total, od*(1-cost.SpotDiscount)) {
		t.Errorf("spot total = %v, want hard-coded discount %v", spot.Total, od*(1-cost.SpotDiscount))
	}
	if !approx(spot.Savings, od*cost.SpotDiscount) {
		t.Errorf("spot savings = %v, want %v", spot.Savings, od*cost.SpotDiscount)
	}

	onDemand := p.Price(context.Background(), db.AdminJobEntry{InstanceType: "c7g.xlarge", Spot: false, DurationSeconds: 3600})
	if !approx(onDemand.Total, od) {
		t.Errorf("on-demand total = %v, want hard-coded %v", onDemand.Total, od)
	}
}

func TestJobPricer_EmptyInstanceTypeDefaultsToT4gMedium(t *testing.T) {
	t.Parallel()

	p := cost.NewJobPricer(nil, nil)
	got := p.Price(context.Background(), db.AdminJobEntry{InstanceType: "", Spot: false, DurationSeconds: 3600})
	if !approx(got.Total, cost.GetInstancePrice("t4g.medium")) {
		t.Errorf("total = %v, want t4g.medium price %v", got.Total, cost.GetInstancePrice("t4g.medium"))
	}
}

func TestJobPricer_ZeroDurationUsesHalfHourFloor(t *testing.T) {
	t.Parallel()

	p := cost.NewJobPricer(nil, nil)
	got := p.Price(context.Background(), db.AdminJobEntry{InstanceType: "t4g.medium", Spot: false, DurationSeconds: 0})
	if !approx(got.Hours, 0.5) {
		t.Errorf("hours = %v, want 0.5 floor", got.Hours)
	}
	if !approx(got.Total, 0.5*cost.GetInstancePrice("t4g.medium")) {
		t.Errorf("total = %v, want 0.5h * on-demand rate", got.Total)
	}
}

func TestJobPricer_SavingsClampedAtZero(t *testing.T) {
	t.Parallel()

	// Live spot price above on-demand (rare, but possible mid-spike) must not
	// produce negative savings.
	od := &fakeOnDemandPricer{prices: map[string]float64{"c7g.xlarge": 0.05}}
	sp := &fakeSpotPricer{prices: map[string]float64{"c7g.xlarge": 0.08}}
	p := cost.NewJobPricer(od, sp)

	got := p.Price(context.Background(), db.AdminJobEntry{InstanceType: "c7g.xlarge", Spot: true, DurationSeconds: 3600})
	if got.Savings < 0 {
		t.Errorf("savings = %v, want >= 0", got.Savings)
	}
	if !approx(got.Savings, 0) {
		t.Errorf("savings = %v, want 0 (clamped)", got.Savings)
	}
}

func TestJobPricer_MemoizesPerInstanceType(t *testing.T) {
	t.Parallel()

	od := &fakeOnDemandPricer{prices: map[string]float64{"c7g.xlarge": 0.10}}
	sp := &fakeSpotPricer{prices: map[string]float64{"c7g.xlarge": 0.03}}
	p := cost.NewJobPricer(od, sp)

	for range 5 {
		p.Price(context.Background(), db.AdminJobEntry{InstanceType: "c7g.xlarge", Spot: true, DurationSeconds: 3600})
	}
	if od.calls != 1 {
		t.Errorf("on-demand fetcher called %d times, want 1 (memoized)", od.calls)
	}
	if sp.calls != 1 {
		t.Errorf("spot fetcher called %d times, want 1 (memoized)", sp.calls)
	}
}
