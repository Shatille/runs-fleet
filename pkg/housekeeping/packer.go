package housekeeping

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/logging"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// packerOrphanThreshold is the minimum age for a packer-tagged instance
// to be considered orphaned. Packer builds in this repo finish in well
// under an hour; anything older is almost certainly a leak from a killed
// or cancelled workflow whose packer process never reached its cleanup phase.
const packerOrphanThreshold = 1 * time.Hour

// ExecuteOrphanedPackerInstances detects and terminates EC2 instances tagged
// `created-by=runs-fleet-packer` that have outlived a reasonable build duration.
//
// Packer normally terminates its own builder when a build ends (success or
// failure), but workflow cancellation (e.g. concurrency-group eviction)
// SIGKILLs packer mid-build before its cleanup runs. ExecuteOrphanedInstances
// keys its cleanup on the `runs-fleet:managed` tag, which packer builders never
// carry, so without this task they leak indefinitely.
func (t *Tasks) ExecuteOrphanedPackerInstances(ctx context.Context) error {
	cutoffTime := time.Now().Add(-packerOrphanThreshold)

	input := &ec2.DescribeInstancesInput{
		Filters: []ec2types.Filter{
			{Name: aws.String("tag:created-by"), Values: []string{"runs-fleet-packer"}},
			// "stopping"/"stopped" are included because packer's amazon-ebs builder
			// stops the instance to snapshot the AMI; a build killed during that
			// window leaves the builder stopped, where it would otherwise leak
			// indefinitely.
			{Name: aws.String("instance-state-name"), Values: []string{"pending", "running", "stopping", "stopped"}},
		},
	}

	var orphanedIDs []string
	for {
		output, err := t.ec2Client.DescribeInstances(ctx, input)
		if err != nil {
			return fmt.Errorf("failed to describe packer instances: %w", err)
		}
		orphanedIDs = append(orphanedIDs, extractOrphanIDs(output, cutoffTime)...)
		if output.NextToken == nil {
			break
		}
		input.NextToken = output.NextToken
	}

	if len(orphanedIDs) == 0 {
		t.logger().Debug("no orphaned packer instances found")
		return nil
	}

	t.logger().Info("terminating orphaned packer instances",
		slog.Int(logging.KeyCount, len(orphanedIDs)),
		slog.Any("instance_ids", orphanedIDs))

	_, err := t.ec2Client.TerminateInstances(ctx, &ec2.TerminateInstancesInput{
		InstanceIds: orphanedIDs,
	})
	if err != nil {
		return fmt.Errorf("failed to terminate orphaned packer instances: %w", err)
	}

	if t.metrics != nil {
		_ = t.metrics.PublishOrphanedInstancesTerminated(ctx, len(orphanedIDs))
	}

	return nil
}
