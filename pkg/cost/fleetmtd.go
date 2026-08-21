package cost

import (
	"context"
	"fmt"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/db"
)

// FleetCostStore reads the persisted per-day fleet cost rollups.
type FleetCostStore interface {
	GetFleetCostDays(ctx context.Context, fromDay, toDay string) ([]db.FleetCostDay, error)
}

// FleetMTD is fleet-wide EC2 cost for a period, measured by sampling every
// managed instance rather than by pricing job records.
//
// It answers the question the job-derived total cannot: what the fleet cost
// including the time no job was running — boot and teardown, idle pool
// capacity, and instances that never received a job at all.
type FleetMTD struct {
	TotalCost   float64
	ComputeCost float64
	EBSCost     float64

	// AttributedCost is the share of TotalCost incurred while an instance was
	// running a job; UnattributedCost is the remainder. Both are derived from
	// AttributedPercent, which comes from the sampler's own observation of
	// busy versus total instance-seconds.
	//
	// Deliberately NOT computed by dividing the job-priced total by this one:
	// job records are deleted after 7 days, so that ratio would decay across a
	// month for calendar reasons rather than attribution ones.
	AttributedCost    float64
	UnattributedCost  float64
	AttributedPercent float64

	// DaysCovered is how many days in the period actually have a rollup;
	// DaysInPeriod is how many have elapsed. They differ before the sampler has
	// been running a full period, which is why the figure must not be presented
	// as a complete month until they match.
	DaysCovered  int
	DaysInPeriod int

	// Partial marks a total known to understate: a day with no rollup at all,
	// or a day whose sampler had to clamp a long gap.
	Partial bool
}

// ComputeFleetMTD sums the fleet cost rollups covering [start, now].
//
// Returns (nil, nil) when no day in the period has been sampled. That is not
// the same as zero: a zero would render beside a non-zero attributed cost and
// read as "the fleet has no overhead", when the truth is that nothing has been
// measured yet. Callers omit the figure entirely instead.
func ComputeFleetMTD(ctx context.Context, store FleetCostStore, start, now time.Time) (*FleetMTD, error) {
	return ComputeFleetMTDIn(ctx, store, start, now, time.UTC)
}

// ComputeFleetMTDIn is ComputeFleetMTD bucketed in loc.
//
// The zone must match the one the sampler wrote its day keys in, or the range
// is off by a day at the boundary and the current day's accumulating rollup
// falls outside the queried window.
func ComputeFleetMTDIn(ctx context.Context, store FleetCostStore, start, now time.Time, loc *time.Location) (*FleetMTD, error) {
	if store == nil {
		return nil, nil
	}
	if loc == nil {
		loc = time.UTC
	}

	fromDay := start.In(loc).Format(db.FleetDayFormat)
	toDay := now.In(loc).Format(db.FleetDayFormat)

	days, err := store.GetFleetCostDays(ctx, fromDay, toDay)
	if err != nil {
		return nil, fmt.Errorf("failed to read fleet cost days: %w", err)
	}
	if len(days) == 0 {
		return nil, nil
	}

	mtd := &FleetMTD{
		DaysCovered:  len(days),
		DaysInPeriod: daysInPeriod(start, now, loc),
	}

	var instanceSeconds, attributedSeconds float64
	for _, d := range days {
		mtd.TotalCost += d.TotalCost
		mtd.ComputeCost += d.ComputeCost
		mtd.EBSCost += d.EBSCost
		instanceSeconds += d.InstanceSeconds
		attributedSeconds += d.AttributedSeconds
		if d.Partial {
			mtd.Partial = true
		}
	}

	// A day the sampler never wrote is a hole in the total, not an empty day.
	if mtd.DaysCovered < mtd.DaysInPeriod {
		mtd.Partial = true
	}

	if instanceSeconds > 0 {
		mtd.AttributedPercent = attributedSeconds / instanceSeconds * 100
		mtd.AttributedCost = mtd.TotalCost * attributedSeconds / instanceSeconds
		mtd.UnattributedCost = mtd.TotalCost - mtd.AttributedCost
	}
	return mtd, nil
}

// daysInPeriod counts the calendar days in loc that [start, now] spans,
// inclusive of both.
//
// Built from the calendar date rather than time.Truncate, which rounds against
// absolute time and so lands on UTC midnight regardless of the zone.
func daysInPeriod(start, now time.Time, loc *time.Location) int {
	s := startOfDay(start.In(loc))
	n := startOfDay(now.In(loc))
	if n.Before(s) {
		return 0
	}
	// Hours rather than Sub/24h: a DST transition makes a local day 23 or 25
	// hours long, and integer division would drop or double a day.
	return int(n.Sub(s).Hours()/24+0.5) + 1
}

func startOfDay(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}
