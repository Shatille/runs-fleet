package cost

import (
	"context"
	"log/slog"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/logging"
)

// EBSGiBMonthRate is the gp3 storage rate per GiB-month (approximate, us-east-1).
//
// WARNING: ESTIMATE ONLY, like instancePricing. Regional variation, provisioned
// IOPS/throughput above the gp3 baseline, and snapshot storage are not modelled.
//
// EBS is priced at all because a stopped warm-pool instance costs nothing else:
// omitting it would leave the stopped-pool half of the fleet reporting zero,
// which is the blind spot the fleet number exists to expose.
const EBSGiBMonthRate = 0.08

// hoursPerMonth converts the GiB-month storage rate to an hourly one, using the
// 730-hour month AWS bills against.
const hoursPerMonth = 730

// EBSHourlyCost returns the hourly cost of a gp3 volume of gib gigabytes.
// A non-positive size costs nothing.
func EBSHourlyCost(gib int) float64 {
	if gib <= 0 {
		return 0
	}
	return float64(gib) * EBSGiBMonthRate / hoursPerMonth
}

// spotResult caches a per-instance-type spot price lookup.
type spotResult struct {
	price float64
	live  bool
}

// rateMemo resolves and caches hourly instance rates. It is the single rate
// ladder shared by JobPricer and FleetPricer: live AWS price when available,
// then the hard-coded table, then a flat spot discount. Sharing it keeps the
// job-attributed and fleet-sampled figures derived from one source, so the
// coverage ratio between them compares like with like.
//
// Not safe for concurrent use; create one per pricer.
type rateMemo struct {
	onDemand PriceFetcherAPI
	spot     SpotPricer
	odMemo   map[string]float64
	spotMemo map[string]spotResult
}

func newRateMemo(onDemand PriceFetcherAPI, spot SpotPricer) rateMemo {
	return rateMemo{
		onDemand: onDemand,
		spot:     spot,
		odMemo:   make(map[string]float64),
		spotMemo: make(map[string]spotResult),
	}
}

// onDemandHourly returns the on-demand rate for instanceType, memoized.
func (m *rateMemo) onDemandHourly(ctx context.Context, instanceType string) float64 {
	if rate, ok := m.odMemo[instanceType]; ok {
		return rate
	}
	rate := GetInstancePrice(instanceType)
	if m.onDemand != nil {
		if live, err := m.onDemand.GetPrice(ctx, instanceType); err != nil {
			pricingLog.Warn(ctx, "live on-demand price unavailable, using fallback",
				slog.String(logging.KeyInstanceType, instanceType),
				slog.String(logging.KeyError, err.Error()))
		} else if live > 0 {
			rate = live
		}
	}
	m.odMemo[instanceType] = rate
	return rate
}

// spotHourly returns the market spot rate for instanceType, memoized. The bool
// reports whether it came from a live lookup; callers fall back to the fixed
// SpotDiscount when it is false.
func (m *rateMemo) spotHourly(ctx context.Context, instanceType string) (float64, bool) {
	if sr, seen := m.spotMemo[instanceType]; seen {
		return sr.price, sr.live
	}
	var sr spotResult
	if m.spot != nil {
		if sp, found := m.spot.SpotPrice(ctx, instanceType); found && sp > 0 {
			sr = spotResult{price: sp, live: true}
		}
	}
	m.spotMemo[instanceType] = sr
	return sr.price, sr.live
}

// FleetSample is one observed instance's cost for one sampling interval.
type FleetSample struct {
	Compute  float64
	EBS      float64
	Total    float64
	Spot     float64 // Total if the instance is spot, else 0
	OnDemand float64 // Total if the instance is on-demand, else 0
	Hours    float64 // billable compute hours; 0 for a stopped instance
}

// FleetPricer prices instances observed by the fleet-cost sampler, across the
// whole managed fleet rather than per job. It answers a different question from
// JobPricer: what an instance costs for the wall-clock time it existed,
// including the storage a stopped instance keeps paying for.
//
// Rates are memoized per instance type, so one tick prices a whole fleet with
// at most one lookup per distinct type. Not safe for concurrent use; create one
// per sampling tick.
type FleetPricer struct {
	rates  rateMemo
	ebsGiB int
}

// NewFleetPricer creates a FleetPricer. onDemand and spot supply live AWS
// prices and may both be nil, in which case pricing falls back to the
// hard-coded table and the fixed spot discount. ebsGiB is the assumed root
// volume size: DescribeInstances reports volume IDs but not their sizes, so
// this is an estimate rather than a measurement.
func NewFleetPricer(onDemand PriceFetcherAPI, spot SpotPricer, ebsGiB int) *FleetPricer {
	return &FleetPricer{rates: newRateMemo(onDemand, spot), ebsGiB: ebsGiB}
}

// PriceInterval prices one instance observed for d.
//
// A running instance is charged compute plus storage; a stopped one is charged
// storage alone, since EC2 stops billing compute but the EBS volume persists.
// Hours counts billable compute only, so a stopped instance contributes no
// runner-minutes and cannot dilute a per-minute rate.
func (p *FleetPricer) PriceInterval(ctx context.Context, instanceType string, spot, running bool, d time.Duration) FleetSample {
	if d <= 0 {
		return FleetSample{}
	}
	hours := d.Hours()

	sample := FleetSample{EBS: EBSHourlyCost(p.ebsGiB) * hours}
	if !running {
		sample.Total = sample.EBS
		return sample
	}

	sample.Hours = hours
	onDemandHourly := p.rates.onDemandHourly(ctx, instanceType)
	if spot {
		if rate, live := p.rates.spotHourly(ctx, instanceType); live {
			sample.Compute = hours * rate
		} else {
			sample.Compute = hours * onDemandHourly * (1 - SpotDiscount)
		}
	} else {
		sample.Compute = hours * onDemandHourly
	}

	sample.Total = sample.Compute + sample.EBS
	if spot {
		sample.Spot = sample.Total
	} else {
		sample.OnDemand = sample.Total
	}
	return sample
}
