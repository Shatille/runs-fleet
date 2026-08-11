package housekeeping

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/logging"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// AMIReference reports the image each architecture would launch today.
type AMIReference interface {
	CurrentImageIDs(ctx context.Context) (map[string]string, error)
}

// staleAMIPerPoolPerCycle bounds the drip to one instance per pool per cycle.
// A pool with ten stale spares converges over ten cycles and never dips more
// than one below target, which a batch replacement could not promise.
const staleAMIPerPoolPerCycle = 1

// SetAMIReference wires the reference-AMI source for the stale-AMI sweep. When
// unset the sweep is a no-op: it never guesses which image is current, because
// the consequence of guessing is terminating instances.
func (t *Tasks) SetAMIReference(ref AMIReference) {
	t.amiReference = ref
}

// ExecuteStaleAMIInstances retires stopped pool instances that are not running
// the image their architecture's launch template would boot today.
//
// Only stopped pool members are eligible, and that is the whole point: EC2 does
// not re-image on StartInstances, so a stopped instance keeps its creation-time
// AMI for as long as it exists. Running instances are left alone — they pick up
// the new image when they cycle after their next job, and one that is not
// cycling is a hung instance, a different problem with a different fix.
func (t *Tasks) ExecuteStaleAMIInstances(ctx context.Context) error {
	if t.amiReference == nil {
		return nil
	}

	current, err := t.amiReference.CurrentImageIDs(ctx)
	if err != nil {
		// Not a task failure: without a reference there is simply nothing to do,
		// and treating an unknown reference as "everything is stale" would roll
		// the fleet on a transient EC2 error.
		t.logger().Warn(ctx, "stale-ami sweep skipped: reference AMI unknown",
			slog.String(logging.KeyError, err.Error()))
		return nil
	}
	if len(current) == 0 {
		return nil
	}

	candidates, err := t.findStaleStoppedInstances(ctx, current)
	if err != nil {
		return err
	}
	if len(candidates) == 0 {
		return nil
	}

	var terminated []string
	var errs []error
	for _, c := range candidates {
		// The scan is seconds old; an instance that has started since is serving
		// a job. Confirm against EC2 before the terminate, which cannot be undone.
		state, _, stateErr := t.instanceRuntimeState(ctx, c.instanceID)
		if stateErr != nil {
			t.logger().Warn(ctx, "stale-ami candidate skipped: state re-read failed",
				slog.String(logging.KeyInstanceID, c.instanceID),
				slog.String(logging.KeyError, stateErr.Error()))
			continue
		}
		if state != string(ec2types.InstanceStateNameStopped) {
			t.logger().Info(ctx, "stale-ami candidate skipped: no longer stopped",
				slog.String(logging.KeyInstanceID, c.instanceID),
				slog.String("state", state))
			continue
		}
		// Terminate immediately after confirming this instance rather than
		// batching at the end: batching would stretch the gap between "confirmed
		// stopped" and "destroyed" across every other candidate's re-read.
		if _, err := t.ec2Client.TerminateInstances(ctx, &ec2.TerminateInstancesInput{
			InstanceIds: []string{c.instanceID},
		}); err != nil {
			// One pool's failure must not abandon the others.
			errs = append(errs, fmt.Errorf("terminate %s: %w", c.instanceID, err))
			continue
		}
		terminated = append(terminated, c.instanceID)
		t.logger().Info(ctx, "retired stopped instance on a stale ami",
			slog.String(logging.KeyInstanceID, c.instanceID),
			slog.String(logging.KeyPoolName, c.pool),
			slog.String("image_id", c.imageID),
			slog.String("current_image_id", current[c.architecture]))
	}

	if len(terminated) > 0 && t.metrics != nil {
		_ = t.metrics.PublishHousekeepingAction(ctx, housekeepingActionStaleAMI, len(terminated))
	}
	return errors.Join(errs...)
}

// staleAMICandidate is a stopped pool member on an outdated image.
type staleAMICandidate struct {
	instanceID   string
	pool         string
	imageID      string
	architecture string
	launchedAt   time.Time
}

// findStaleStoppedInstances returns at most staleAMIPerPoolPerCycle candidates
// per pool, oldest launch first so the drip works through the longest-standing
// holdouts before the rest.
func (t *Tasks) findStaleStoppedInstances(ctx context.Context, current map[string]string) ([]staleAMICandidate, error) {
	input := &ec2.DescribeInstancesInput{
		Filters: []ec2types.Filter{
			{Name: aws.String("tag:runs-fleet:managed"), Values: []string{"true"}},
			{Name: aws.String("instance-state-name"), Values: []string{string(ec2types.InstanceStateNameStopped)}},
		},
	}

	byPool := map[string][]staleAMICandidate{}
	for {
		output, err := t.ec2Client.DescribeInstances(ctx, input)
		if err != nil {
			return nil, fmt.Errorf("failed to describe stopped instances: %w", err)
		}
		for _, res := range output.Reservations {
			for _, inst := range res.Instances {
				c, ok := staleCandidate(inst, current)
				if !ok {
					continue
				}
				byPool[c.pool] = append(byPool[c.pool], c)
			}
		}
		if output.NextToken == nil {
			break
		}
		input.NextToken = output.NextToken
	}

	pools := make([]string, 0, len(byPool))
	for pool := range byPool {
		pools = append(pools, pool)
	}
	slices.Sort(pools)

	var candidates []staleAMICandidate
	for _, pool := range pools {
		members := byPool[pool]
		// Oldest first: the longest-standing holdout is the one most likely to be
		// several AMIs behind, and the drip should reach it first.
		slices.SortFunc(members, func(a, b staleAMICandidate) int {
			return a.launchedAt.Compare(b.launchedAt)
		})
		if len(members) > staleAMIPerPoolPerCycle {
			members = members[:staleAMIPerPoolPerCycle]
		}
		candidates = append(candidates, members...)
	}
	return candidates, nil
}

// instancePoolTag returns an instance's pool tag, or empty when it has none.
func instancePoolTag(inst ec2types.Instance) string {
	for _, tag := range inst.Tags {
		if aws.ToString(tag.Key) == "runs-fleet:pool" {
			return aws.ToString(tag.Value)
		}
	}
	return ""
}

// staleCandidate reports whether one instance is a stale stopped pool member.
// Anything it cannot judge — no pool, no image, an architecture with no known
// reference — is not a candidate, so incomplete information never costs an
// instance.
func staleCandidate(inst ec2types.Instance, current map[string]string) (staleAMICandidate, bool) {
	pool := instancePoolTag(inst)
	if pool == "" {
		return staleAMICandidate{}, false
	}
	imageID := aws.ToString(inst.ImageId)
	if imageID == "" {
		return staleAMICandidate{}, false
	}
	arch := string(inst.Architecture)
	reference, ok := current[arch]
	if !ok || reference == "" || imageID == reference {
		return staleAMICandidate{}, false
	}
	c := staleAMICandidate{
		instanceID:   aws.ToString(inst.InstanceId),
		pool:         pool,
		imageID:      imageID,
		architecture: arch,
	}
	if inst.LaunchTime != nil {
		c.launchedAt = *inst.LaunchTime
	}
	return c, true
}
