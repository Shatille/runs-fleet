package cost

import (
	"context"

	"github.com/Shavakan/runs-fleet/pkg/db"
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
	rates rateMemo
}

// NewJobPricer creates a JobPricer. onDemand and spot supply live AWS prices;
// both may be nil, in which case pricing falls back to the hard-coded on-demand
// table and fixed spot discount.
func NewJobPricer(onDemand PriceFetcherAPI, spot SpotPricer) *JobPricer {
	return &JobPricer{rates: newRateMemo(onDemand, spot)}
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

	onDemandHourly := p.rates.onDemandHourly(ctx, instanceType)

	result := JobPricing{Hours: durationHours}
	if job.Spot {
		spotHourly, live := p.rates.spotHourly(ctx, instanceType)
		if live {
			result.Total = durationHours * spotHourly
			if saving := durationHours * (onDemandHourly - spotHourly); saving > 0 {
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
