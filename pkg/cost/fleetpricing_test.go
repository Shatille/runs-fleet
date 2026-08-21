package cost_test

import (
	"context"
	"testing"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/cost"
	"github.com/Shavakan/runs-fleet/pkg/db"
)

// A running instance bills for compute AND its attached storage. This is the
// half the job-based pricer already approximates; the EBS term is new.
func TestFleetPricerPricesRunningInstanceAsComputePlusStorage(t *testing.T) {
	od := &fakeOnDemandPricer{prices: map[string]float64{"c7g.xlarge": 0.12}}
	p := cost.NewFleetPricer(od, nil, 100)

	got := p.PriceInterval(context.Background(), "c7g.xlarge", false, true, time.Hour)

	if !approx(got.Compute, 0.12) {
		t.Errorf("Compute = %v, want 0.12", got.Compute)
	}
	wantEBS := cost.EBSHourlyCost(100)
	if !approx(got.EBS, wantEBS) {
		t.Errorf("EBS = %v, want %v", got.EBS, wantEBS)
	}
	if !approx(got.Total, 0.12+wantEBS) {
		t.Errorf("Total = %v, want %v", got.Total, 0.12+wantEBS)
	}
	if !approx(got.Hours, 1) {
		t.Errorf("Hours = %v, want 1", got.Hours)
	}
}

// The whole point of the fleet number: a stopped warm-pool instance still costs
// money (its EBS volume persists) but reports zero everywhere today. Compute
// and Hours must be zero so it never inflates a runner-minute rate.
func TestFleetPricerChargesStoppedInstanceForStorageOnly(t *testing.T) {
	od := &fakeOnDemandPricer{prices: map[string]float64{"c7g.xlarge": 0.12}}
	p := cost.NewFleetPricer(od, nil, 100)

	got := p.PriceInterval(context.Background(), "c7g.xlarge", false, false, time.Hour)

	if got.Compute != 0 {
		t.Errorf("Compute = %v, want 0 for a stopped instance", got.Compute)
	}
	if got.Hours != 0 {
		t.Errorf("Hours = %v, want 0 for a stopped instance", got.Hours)
	}
	if !approx(got.EBS, cost.EBSHourlyCost(100)) {
		t.Errorf("EBS = %v, want %v", got.EBS, cost.EBSHourlyCost(100))
	}
	if !approx(got.Total, got.EBS) {
		t.Errorf("Total = %v, want it to equal EBS %v", got.Total, got.EBS)
	}
}

func TestFleetPricerUsesLiveSpotPriceWhenAvailable(t *testing.T) {
	od := &fakeOnDemandPricer{prices: map[string]float64{"c7g.xlarge": 0.12}}
	sp := &fakeSpotPricer{prices: map[string]float64{"c7g.xlarge": 0.04}}
	p := cost.NewFleetPricer(od, sp, 0)

	got := p.PriceInterval(context.Background(), "c7g.xlarge", true, true, time.Hour)

	if !approx(got.Compute, 0.04) {
		t.Errorf("Compute = %v, want the live spot price 0.04", got.Compute)
	}
	if !approx(got.Spot, got.Total) {
		t.Errorf("Spot = %v, want it to carry the whole total %v", got.Spot, got.Total)
	}
	if got.OnDemand != 0 {
		t.Errorf("OnDemand = %v, want 0 for a spot instance", got.OnDemand)
	}
}

// Without a live spot price the pricer must land on the same flat discount the
// job pricer uses, so the two numbers stay derived from one table.
func TestFleetPricerFallsBackToTheFixedSpotDiscount(t *testing.T) {
	od := &fakeOnDemandPricer{prices: map[string]float64{"c7g.xlarge": 0.12}}
	p := cost.NewFleetPricer(od, nil, 0)

	got := p.PriceInterval(context.Background(), "c7g.xlarge", true, true, time.Hour)

	want := 0.12 * (1 - cost.SpotDiscount)
	if !approx(got.Compute, want) {
		t.Errorf("Compute = %v, want %v", got.Compute, want)
	}
}

// One lookup per distinct instance type per pricer, matching JobPricer: a tick
// prices a whole fleet and must not issue an API call per instance.
func TestFleetPricerMemoizesPerInstanceType(t *testing.T) {
	od := &fakeOnDemandPricer{prices: map[string]float64{"c7g.xlarge": 0.12}}
	p := cost.NewFleetPricer(od, nil, 0)

	for i := 0; i < 5; i++ {
		p.PriceInterval(context.Background(), "c7g.xlarge", false, true, time.Minute)
	}

	if od.calls != 1 {
		t.Errorf("on-demand lookups = %d, want 1 across 5 instances of one type", od.calls)
	}
}

func TestFleetPricerScalesWithTheObservedInterval(t *testing.T) {
	od := &fakeOnDemandPricer{prices: map[string]float64{"c7g.xlarge": 0.12}}
	p := cost.NewFleetPricer(od, nil, 0)

	got := p.PriceInterval(context.Background(), "c7g.xlarge", false, true, 5*time.Minute)

	if !approx(got.Compute, 0.12/12) {
		t.Errorf("Compute = %v, want a twelfth of the hourly rate", got.Compute)
	}
}

// A zero or negative observation window must contribute nothing rather than a
// negative cost — the sampler clamps elapsed time and this is the backstop.
func TestFleetPricerIgnoresNonPositiveIntervals(t *testing.T) {
	od := &fakeOnDemandPricer{prices: map[string]float64{"c7g.xlarge": 0.12}}
	p := cost.NewFleetPricer(od, nil, 100)

	for _, d := range []time.Duration{0, -time.Minute} {
		got := p.PriceInterval(context.Background(), "c7g.xlarge", false, true, d)
		if got.Total != 0 || got.Compute != 0 || got.EBS != 0 {
			t.Errorf("PriceInterval(%v) = %+v, want an all-zero sample", d, got)
		}
	}
}

// An instance type absent from the catalog still bills; it must fall back
// rather than price at zero and silently shrink the fleet total.
func TestFleetPricerFallsBackForUnknownInstanceTypes(t *testing.T) {
	p := cost.NewFleetPricer(nil, nil, 0)

	got := p.PriceInterval(context.Background(), "x9z.mega", false, true, time.Hour)

	if !approx(got.Compute, cost.GetInstancePrice("x9z.mega")) {
		t.Errorf("Compute = %v, want the fallback price %v", got.Compute, cost.GetInstancePrice("x9z.mega"))
	}
	if got.Compute <= 0 {
		t.Error("an unknown instance type must not price at zero")
	}
}

// The fleet and job pricers must agree on the compute half for identical
// inputs; if they drift, the coverage ratio compares two different rate tables.
func TestFleetPricerComputeMatchesJobPricerForTheSameInstanceHour(t *testing.T) {
	rates := map[string]float64{"c7g.xlarge": 0.12}

	fleet := cost.NewFleetPricer(&fakeOnDemandPricer{prices: rates}, nil, 0)
	fleetSample := fleet.PriceInterval(context.Background(), "c7g.xlarge", false, true, time.Hour)

	job := cost.NewJobPricer(&fakeOnDemandPricer{prices: rates}, nil)
	jobPricing := job.Price(context.Background(), db.AdminJobEntry{
		InstanceType: "c7g.xlarge", Spot: false, DurationSeconds: 3600,
	})

	if !approx(fleetSample.Compute, jobPricing.Total) {
		t.Errorf("fleet compute %v != job total %v for the same instance-hour",
			fleetSample.Compute, jobPricing.Total)
	}
}

func TestEBSHourlyCostIsProportionalToVolumeSize(t *testing.T) {
	if got := cost.EBSHourlyCost(0); got != 0 {
		t.Errorf("EBSHourlyCost(0) = %v, want 0", got)
	}
	if got := cost.EBSHourlyCost(-10); got != 0 {
		t.Errorf("EBSHourlyCost(-10) = %v, want 0", got)
	}
	single := cost.EBSHourlyCost(50)
	if !approx(cost.EBSHourlyCost(100), 2*single) {
		t.Errorf("EBSHourlyCost(100) = %v, want twice EBSHourlyCost(50) = %v",
			cost.EBSHourlyCost(100), 2*single)
	}
}
