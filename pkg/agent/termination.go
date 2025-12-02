package agent

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
)

const (
	// TelemetryTimeout is how long to wait for telemetry to be sent.
	TelemetryTimeout = 30 * time.Second
)

// InstanceTerminator defines the interface for instance termination.
// Implementations handle backend-specific termination (EC2 API vs K8s pod exit).
type InstanceTerminator interface {
	TerminateInstance(ctx context.Context, instanceID string, status JobStatus) error
}

// EC2API defines EC2 operations for instance termination.
type EC2API interface {
	TerminateInstances(ctx context.Context, params *ec2.TerminateInstancesInput, optFns ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error)
}

// EC2Terminator handles EC2 instance self-termination.
type EC2Terminator struct {
	ec2Client EC2API
	telemetry TelemetryClient
	logger    Logger
}

// Verify EC2Terminator implements InstanceTerminator.
var _ InstanceTerminator = (*EC2Terminator)(nil)

// NewEC2Terminator creates a new EC2 terminator.
func NewEC2Terminator(cfg aws.Config, telemetry TelemetryClient, logger Logger) *EC2Terminator {
	return &EC2Terminator{
		ec2Client: ec2.NewFromConfig(cfg),
		telemetry: telemetry,
		logger:    logger,
	}
}

// NewTerminator is an alias for NewEC2Terminator for backward compatibility.
// Deprecated: Use NewEC2Terminator instead.
func NewTerminator(cfg aws.Config, telemetry *Telemetry, logger Logger) *EC2Terminator {
	return &EC2Terminator{
		ec2Client: ec2.NewFromConfig(cfg),
		telemetry: telemetry,
		logger:    logger,
	}
}

// TerminateInstance sends telemetry and terminates the EC2 instance.
func (t *EC2Terminator) TerminateInstance(ctx context.Context, instanceID string, status JobStatus) error {
	t.logger.Printf("Preparing to terminate instance %s", instanceID)

	// Send termination notification first
	if t.telemetry != nil {
		t.logger.Println("Sending job completion telemetry...")

		telemetryCtx, cancel := context.WithTimeout(ctx, TelemetryTimeout)
		defer cancel()

		if err := t.telemetry.SendJobCompleted(telemetryCtx, status); err != nil {
			t.logger.Printf("Warning: failed to send telemetry: %v", err)
			// Continue with termination even if telemetry fails
		} else {
			t.logger.Println("Telemetry sent successfully")
		}
	}

	// Small delay to ensure telemetry is queued
	time.Sleep(2 * time.Second)

	// Terminate the instance
	t.logger.Printf("Terminating instance %s...", instanceID)

	_, err := t.ec2Client.TerminateInstances(ctx, &ec2.TerminateInstancesInput{
		InstanceIds: []string{instanceID},
	})
	if err != nil {
		return fmt.Errorf("failed to terminate instance: %w", err)
	}

	t.logger.Printf("Instance %s termination initiated", instanceID)
	return nil
}

// TerminateWithStatus terminates with a specific status.
func (t *EC2Terminator) TerminateWithStatus(instanceID string, status string, exitCode int, duration time.Duration, errorMsg string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	jobStatus := JobStatus{
		InstanceID:      instanceID,
		Status:          status,
		ExitCode:        exitCode,
		DurationSeconds: int(duration.Seconds()),
		CompletedAt:     time.Now(),
		Error:           errorMsg,
	}

	return t.TerminateInstance(ctx, instanceID, jobStatus)
}

// TerminateOnPanic handles termination after a panic.
func (t *EC2Terminator) TerminateOnPanic(instanceID, jobID string, panicValue interface{}) {
	t.logger.Printf("PANIC RECOVERY: Terminating instance %s due to panic: %v", instanceID, panicValue)

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Minute)
	defer cancel()

	status := JobStatus{
		InstanceID:  instanceID,
		JobID:       jobID,
		Status:      "failure",
		ExitCode:    -1,
		CompletedAt: time.Now(),
		Error:       fmt.Sprintf("agent panic: %v", panicValue),
	}

	if err := t.TerminateInstance(ctx, instanceID, status); err != nil {
		t.logger.Printf("Failed to terminate instance after panic: %v", err)
	}
}

// Terminator is an alias for EC2Terminator for backward compatibility.
// Deprecated: Use EC2Terminator instead.
type Terminator = EC2Terminator
