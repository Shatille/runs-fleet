package housekeeping

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/cost"
	"github.com/Shavakan/runs-fleet/pkg/db"
	"github.com/Shavakan/runs-fleet/pkg/logging"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// fleetCostSampleInterval is the nominal spacing between fleet cost samples,
// and what a tick attributes when it has no previous checkpoint to measure from.
//
// A sampler that observes every T and credits T per instance is unbiased in
// aggregate for any T; T controls variance, not bias. At 60s the daily
// aggregate error is ~3% for a fleet of ~100 instances a day, for 1440
// DescribeInstances calls — halving it to 30s would buy under a percentage
// point. The per-instance figure is statistical either way, which is why this
// feeds a fleet total and never a per-job number.
const fleetCostSampleInterval = 60 * time.Second

// fleetCostMaxElapsed caps how much time one tick may attribute.
//
// Ticks credit the elapsed time since the last checkpoint rather than a fixed
// interval, so a missed tick is covered by the next one instead of vanishing.
// Without a cap, though, a multi-hour outage would come back and attribute one
// enormous phantom block. A clamped tick marks its day partial so the number is
// reported as understating rather than as complete.
const fleetCostMaxElapsed = 15 * time.Minute

// fleetCostEBSGiB is the assumed root volume size per instance.
//
// DescribeInstances reports each attached volume's ID but not its size
// (ec2types.EbsInstanceBlockDevice carries no VolumeSize), so resolving real
// sizes would need a DescribeVolumes fan-out every tick. This is an ESTIMATE,
// and the API labels the EBS component as such.
const fleetCostEBSGiB = 100

// billableStates are the instance states AWS charges compute for. Only stopped
// is excluded — a stopped instance keeps paying for its EBS volume alone, which
// is exactly the cost a warm pool incurs and the job-based number never sees.
var billableStates = []ec2types.InstanceStateName{
	ec2types.InstanceStateNamePending,
	ec2types.InstanceStateNameRunning,
	ec2types.InstanceStateNameStopping,
}

// FleetCostStore persists sampled fleet cost and reads back what coverage needs.
type FleetCostStore interface {
	AddFleetCostSample(ctx context.Context, day string, d db.FleetCostDelta) error
	GetFleetCostDays(ctx context.Context, fromDay, toDay string) ([]db.FleetCostDay, error)
	ListBusyInstanceIDs(ctx context.Context) ([]string, error)
}

// SetFleetCostStore wires the fleet-cost sampler's storage. When unset the
// sampler is inert, and the cost API omits its fleet figures entirely rather
// than reporting a zero that would read as "no overhead".
func (t *Tasks) SetFleetCostStore(s FleetCostStore) {
	t.fleetCost = s
}

// ExecuteFleetCostSample records what the whole managed fleet cost since the
// last sample.
//
// This is the only enumeration that sees both pool members and cold-start
// instances: pool reconciliation filters on runs-fleet:pool, so it is blind to
// cold-start capacity, and the jobs table never records an instance that ran no
// job. Both gaps are why the Cost tab's job-derived total understates spend.
func (t *Tasks) ExecuteFleetCostSample(ctx context.Context) error {
	if t.fleetCost == nil {
		return nil
	}

	now := time.Now().UTC()
	day := now.Format(db.FleetDayFormat)
	elapsed, partial := t.fleetCostElapsed(ctx, day, now)

	instances, err := t.describeManagedFleet(ctx)
	if err != nil {
		return err
	}

	busy := t.busyInstanceSet(ctx)
	pricer := cost.NewFleetPricer(nil, nil, fleetCostEBSGiB)
	delta := db.FleetCostDelta{SampledAt: now, Partial: partial}

	for _, inst := range instances {
		id := aws.ToString(inst.InstanceId)
		running := isBillableState(inst.State)
		sample := pricer.PriceInterval(ctx, string(inst.InstanceType),
			inst.InstanceLifecycle == ec2types.InstanceLifecycleTypeSpot, running, elapsed)

		delta.TotalCost += sample.Total
		delta.ComputeCost += sample.Compute
		delta.EBSCost += sample.EBS
		delta.InstanceSeconds += elapsed.Seconds()
		if busy != nil && running {
			if _, ok := busy[id]; ok {
				delta.AttributedSeconds += elapsed.Seconds()
			}
		}
	}

	// An empty fleet still checkpoints: skipping the write would leave the next
	// tick measuring from a stale timestamp and over-attributing the gap.
	if err := t.fleetCost.AddFleetCostSample(ctx, day, delta); err != nil {
		return fmt.Errorf("failed to record fleet cost sample: %w", err)
	}

	t.logger().Info(ctx, "fleet cost sampled",
		slog.Int(logging.KeyCount, len(instances)),
		slog.Float64("cost_usd", delta.TotalCost),
		slog.Float64("elapsed_seconds", elapsed.Seconds()),
		slog.Bool("partial", partial))
	return nil
}

// fleetCostElapsed returns how much time this tick should attribute, and
// whether it had to be clamped. A tick measures from the previous checkpoint so
// a missed tick is absorbed rather than lost; the first tick of a day, and any
// checkpoint in the future (a backwards clock), fall back to the nominal
// interval.
func (t *Tasks) fleetCostElapsed(ctx context.Context, day string, now time.Time) (time.Duration, bool) {
	days, err := t.fleetCost.GetFleetCostDays(ctx, day, day)
	if err != nil {
		t.logger().Warn(ctx, "fleet cost checkpoint unavailable, using the nominal interval",
			slog.String(logging.KeyError, err.Error()))
		return fleetCostSampleInterval, false
	}

	var last time.Time
	for _, d := range days {
		if d.Day == day {
			last = d.LastSampleAt
		}
	}
	if last.IsZero() || !last.Before(now) {
		return fleetCostSampleInterval, false
	}

	elapsed := now.Sub(last)
	if elapsed > fleetCostMaxElapsed {
		return fleetCostMaxElapsed, true
	}
	return elapsed, false
}

// busyInstanceSet returns the instances currently running a job, or nil when
// that cannot be determined. Nil is deliberate: attributing nothing is honest,
// while attributing everything would erase the gap this feature measures.
func (t *Tasks) busyInstanceSet(ctx context.Context) map[string]struct{} {
	ids, err := t.fleetCost.ListBusyInstanceIDs(ctx)
	if err != nil {
		t.logger().Warn(ctx, "busy instance lookup failed, coverage omitted for this tick",
			slog.String(logging.KeyError, err.Error()))
		return nil
	}
	busy := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		busy[id] = struct{}{}
	}
	return busy
}

// describeManagedFleet returns every runs-fleet instance that currently costs
// money, pool member and cold-start alike.
func (t *Tasks) describeManagedFleet(ctx context.Context) ([]ec2types.Instance, error) {
	states := make([]string, 0, len(billableStates)+1)
	for _, s := range billableStates {
		states = append(states, string(s))
	}
	states = append(states, string(ec2types.InstanceStateNameStopped))

	input := &ec2.DescribeInstancesInput{
		Filters: []ec2types.Filter{
			{Name: aws.String("tag:runs-fleet:managed"), Values: []string{"true"}},
			{Name: aws.String("instance-state-name"), Values: states},
		},
	}

	var instances []ec2types.Instance
	for {
		output, err := t.ec2Client.DescribeInstances(ctx, input)
		if err != nil {
			return nil, fmt.Errorf("failed to describe managed fleet: %w", err)
		}
		for _, reservation := range output.Reservations {
			instances = append(instances, reservation.Instances...)
		}
		if output.NextToken == nil {
			return instances, nil
		}
		input.NextToken = output.NextToken
	}
}

func isBillableState(state *ec2types.InstanceState) bool {
	if state == nil {
		return false
	}
	for _, s := range billableStates {
		if state.Name == s {
			return true
		}
	}
	return false
}
