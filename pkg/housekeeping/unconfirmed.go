package housekeeping

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/db"
	"github.com/Shavakan/runs-fleet/pkg/logging"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
)

// launchedConfirmThreshold is how long a job may sit in the launched state
// (instance up, runner not yet confirmed) before the watchdog treats the runner
// as never having registered. Sized for the EC2 boot + runner-registration
// budget; a healthy cold start confirms well within it. Overridable in tests.
var launchedConfirmThreshold = 5 * time.Minute

// maxLaunchRecoveryRetries bounds how many times the watchdog requeues a job
// whose runner never came up, mirroring the worker's on-demand retry cap so a
// fleet-wide registration failure cannot churn instances indefinitely. Shared with
// the operator-triggered requeue action.
const maxLaunchRecoveryRetries = MaxRequeueRetries

// ExecuteUnconfirmedRunners recovers jobs whose instance launched but whose
// runner never registered (stuck in the launched state past
// launchedConfirmThreshold). For each it terminates the dead instance and, if
// the job has retries left, requeues it on-demand; otherwise it marks the job
// terminal and emits a scheduling-failure alert. This is the case the orphaned-
// and stale-job sweeps miss: the instance is healthy, only the agent failed.
func (t *Tasks) ExecuteUnconfirmedRunners(ctx context.Context) error {
	if t.config.JobsTableName == "" {
		return nil
	}
	if t.jobRequeuer == nil {
		t.logger().Warn(ctx, "unconfirmed-runner watchdog disabled: no job requeuer configured")
		return nil
	}

	candidates, err := FindRequeueableJobs(ctx, t.dynamoClient, t.config.JobsTableName, launchedConfirmThreshold, []db.JobStatus{db.JobStatusLaunched})
	if err != nil {
		return fmt.Errorf("failed to scan launched jobs: %w", err)
	}
	if len(candidates) == 0 {
		return nil
	}

	// The watchdog only recovers launched records that have an instance; a launched
	// record with no instance_id never produced an instance and is left for the
	// orphan/stale reapers (preserving prior behavior).
	withInstance := make([]RequeueableJob, 0, len(candidates))
	for _, c := range candidates {
		if c.InstanceID != "" {
			withInstance = append(withInstance, c)
		}
	}
	if len(withInstance) == 0 {
		return nil
	}

	fallback := func(ctx context.Context, instanceID string) bool {
		exists, _ := t.instanceExists(ctx, instanceID)
		return exists
	}
	checkList := make([]OrphanedJobCandidate, 0, len(withInstance))
	for _, c := range withInstance {
		checkList = append(checkList, OrphanedJobCandidate{JobID: c.JobID, InstanceID: c.InstanceID})
	}
	alive := BatchCheckInstanceExistence(ctx, t.ec2Client, checkList, fallback)

	var recovered int
	for _, c := range withInstance {
		jobCtx := logging.ContextWith(ctx,
			slog.Int64(logging.KeyJobID, c.JobID),
			slog.String(logging.KeyInstanceID, c.InstanceID))

		if alive[c.InstanceID] {
			t.cancelSpotRequestsForInstances(jobCtx, []string{c.InstanceID})
			if _, err := t.ec2Client.TerminateInstances(jobCtx, &ec2.TerminateInstancesInput{
				InstanceIds: []string{c.InstanceID},
			}); err != nil {
				t.logger().Error(jobCtx, "failed to terminate unconfirmed-runner instance", slog.String("error", err.Error()))
				continue
			}
		}

		if c.RetryCount < maxLaunchRecoveryRetries {
			if err := t.requeueUnconfirmed(jobCtx, c); err != nil {
				t.logger().Error(jobCtx, "failed to requeue unconfirmed-runner job", slog.String("error", err.Error()))
				continue
			}
			recovered++
			t.logger().Info(jobCtx, "unconfirmed-runner job requeued", slog.Int("retry_count", c.RetryCount+1))
			continue
		}

		if err := t.markLaunchedJobError(jobCtx, c.JobID); err != nil {
			t.logger().Error(jobCtx, "failed to fail exhausted unconfirmed-runner job", slog.String("error", err.Error()))
			continue
		}
		recovered++
		if t.metrics != nil {
			_ = t.metrics.PublishSchedulingFailure(jobCtx, housekeepingActionUnconfirmedRunner)
		}
		t.logger().Warn(jobCtx, "unconfirmed-runner job exhausted retries; marked failed")
	}

	if t.metrics != nil && recovered > 0 {
		_ = t.metrics.PublishHousekeepingAction(ctx, housekeepingActionUnconfirmedRunner, recovered)
	}
	if recovered > 0 {
		t.logger().Info(ctx, "unconfirmed-runner jobs recovered", slog.Int(logging.KeyCount, recovered))
	}
	return nil
}

// requeueUnconfirmed re-enqueues the job on-demand and flips its record to
// requeued so the watchdog stops seeing it. The message is sent before the
// status flip; if the flip fails, the next cycle re-sends (SQS dedupes on
// job_id + retry_count).
func (t *Tasks) requeueUnconfirmed(ctx context.Context, c RequeueableJob) error {
	if c.RunID == 0 {
		return fmt.Errorf("job %d has no run_id; cannot requeue", c.JobID)
	}

	if err := t.jobRequeuer.SendMessage(ctx, BuildRequeueMessage(c)); err != nil {
		return fmt.Errorf("send requeue message: %w", err)
	}
	if err := t.markLaunchedJobRequeued(ctx, c.JobID); err != nil {
		return fmt.Errorf("mark requeued: %w", err)
	}
	if t.metrics != nil {
		_ = t.metrics.PublishJobRequeued(ctx, housekeepingActionUnconfirmedRunner)
	}
	return nil
}

func (t *Tasks) markLaunchedJobRequeued(ctx context.Context, jobID int64) error {
	return t.updateLaunchedJob(ctx, jobID, "SET #status = :requeued, requeued_at = :now",
		map[string]types.AttributeValue{
			":requeued": &types.AttributeValueMemberS{Value: string(db.JobStatusRequeued)},
			":launched": &types.AttributeValueMemberS{Value: string(db.JobStatusLaunched)},
			":now":      &types.AttributeValueMemberS{Value: time.Now().Format(time.RFC3339)},
		})
}

func (t *Tasks) markLaunchedJobError(ctx context.Context, jobID int64) error {
	return t.updateLaunchedJob(ctx, jobID, "SET #status = :error, completed_at = :now",
		map[string]types.AttributeValue{
			":error":    &types.AttributeValueMemberS{Value: string(db.JobStatusError)},
			":launched": &types.AttributeValueMemberS{Value: string(db.JobStatusLaunched)},
			":now":      &types.AttributeValueMemberS{Value: time.Now().Format(time.RFC3339)},
		})
}

// updateLaunchedJob applies a status transition guarded on the record still
// being launched, so a runner that confirms (or a completion) concurrently is
// not clobbered. A ConditionalCheckFailedException means the job advanced under
// us and is treated as success.
func (t *Tasks) updateLaunchedJob(ctx context.Context, jobID int64, updateExpr string, values map[string]types.AttributeValue) error {
	_, err := t.dynamoClient.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(t.config.JobsTableName),
		Key: map[string]types.AttributeValue{
			"job_id": &types.AttributeValueMemberN{Value: strconv.FormatInt(jobID, 10)},
		},
		UpdateExpression:          aws.String(updateExpr),
		ConditionExpression:       aws.String("#status = :launched"),
		ExpressionAttributeNames:  map[string]string{"#status": "status"},
		ExpressionAttributeValues: values,
	})
	if err != nil {
		var condErr *types.ConditionalCheckFailedException
		if errors.As(err, &condErr) {
			return nil
		}
		return fmt.Errorf("update launched job: %w", err)
	}
	return nil
}

func avString(item map[string]types.AttributeValue, key string) string {
	if v, ok := item[key].(*types.AttributeValueMemberS); ok {
		return v.Value
	}
	return ""
}

func avInt64(item map[string]types.AttributeValue, key string) int64 {
	if v, ok := item[key].(*types.AttributeValueMemberN); ok {
		n, _ := strconv.ParseInt(v.Value, 10, 64)
		return n
	}
	return 0
}
