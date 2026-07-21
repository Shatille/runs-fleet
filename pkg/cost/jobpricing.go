package cost

import (
	"context"
	"log/slog"

	"github.com/Shavakan/runs-fleet/pkg/db"
	"github.com/Shavakan/runs-fleet/pkg/logging"
)

// SpotPricer supplies the current market spot hourly price by instance type
// (satisfied by *fleet.Manager). The bool is false when no price is available,
// so the pricer falls back to the fixed spot-discount estimate.
type SpotPricer interface {
	SpotPrice(ctx context.Context, instanceType string) (float64, bool)
}

// JobPricing is one job's EC2 cost, split for aggregation.
type JobPricing struct {
	Total    float64
	Spot     float64 // Total if the job ran on spot, else 0
	OnDemand float64 // Total if the job ran on-demand, else 0
	Savings  float64
	Hours    float64 // billable hours (with the 0.5h minimum applied)
}

// JobPricer computes per-job EC2 cost using live on-demand/spot prices when
// available (falling back to the hard-coded table and fixed spot discount). It
// memoizes each distinct instance type's on-demand and spot lookups for the life
// of the pricer, so a single run prices each type once. Not safe for concurrent
// use; create one per run.
type JobPricer struct {
	onDemand PriceFetcherAPI
	spot     SpotPricer
	odMemo   map[string]float64
	spotMemo map[string]spotResult
}

// spotResult caches a per-instance-type spot price lookup.
type spotResult struct {
	price float64
	live  bool
}

// NewJobPricer creates a JobPricer. onDemand and spot supply live AWS prices;
// both may be nil, in which case pricing falls back to the hard-coded on-demand
// table and fixed spot discount.
func NewJobPricer(onDemand PriceFetcherAPI, spot SpotPricer) *JobPricer {
	return &JobPricer{
		onDemand: onDemand,
		spot:     spot,
		odMemo:   make(map[string]float64),
		spotMemo: make(map[string]spotResult),
	}
}

// Price computes one job's EC2 cost. The result is split so callers can
// aggregate spot, on-demand, savings, and billable hours independently.
func (p *JobPricer) Price(ctx context.Context, job db.AdminJobEntry) JobPricing {
	instanceType := job.InstanceType
	if instanceType == "" {
		instanceType = "t4g.medium"
	}

	durationHours := float64(job.DurationSeconds) / 3600
	if durationHours <= 0 {
		durationHours = 0.5
	}

	onDemandHourly, ok := p.odMemo[instanceType]
	if !ok {
		onDemandHourly = GetInstancePrice(instanceType)
		if p.onDemand != nil {
			if live, err := p.onDemand.GetPrice(ctx, instanceType); err != nil {
				pricingLog.Warn(ctx, "live on-demand price unavailable, using fallback",
					slog.String(logging.KeyInstanceType, instanceType),
					slog.String(logging.KeyError, err.Error()))
			} else if live > 0 {
				onDemandHourly = live
			}
		}
		p.odMemo[instanceType] = onDemandHourly
	}

	result := JobPricing{Hours: durationHours}
	if job.Spot {
		sr, seen := p.spotMemo[instanceType]
		if !seen {
			if p.spot != nil {
				if sp, found := p.spot.SpotPrice(ctx, instanceType); found && sp > 0 {
					sr = spotResult{price: sp, live: true}
				}
			}
			p.spotMemo[instanceType] = sr
		}
		if sr.live {
			result.Total = durationHours * sr.price
			if saving := durationHours * (onDemandHourly - sr.price); saving > 0 {
				result.Savings = saving
			}
		} else {
			result.Total = durationHours * onDemandHourly * (1 - SpotDiscount)
			result.Savings = durationHours * onDemandHourly * SpotDiscount
		}
		result.Spot = result.Total
	} else {
		result.Total = durationHours * onDemandHourly
		result.OnDemand = result.Total
	}
	return result
}
