package agent

import (
	"context"
	"time"
)

// k8sTerminationDelay is the delay before pod termination to allow telemetry to be sent.
// Exposed as a variable to allow testing with shorter durations.
var k8sTerminationDelay = 1 * time.Second

// K8sTerminator handles termination for Kubernetes pods.
// In K8s, pods terminate naturally when the process exits, so this implementation
// only sends telemetry and returns - the caller should exit cleanly after.
type K8sTerminator struct {
	telemetry TelemetryClient
	logger    Logger
}

// Verify K8sTerminator implements InstanceTerminator.
var _ InstanceTerminator = (*K8sTerminator)(nil)

// NewK8sTerminator creates a new K8s terminator.
func NewK8sTerminator(telemetry TelemetryClient, logger Logger) *K8sTerminator {
	return &K8sTerminator{
		telemetry: telemetry,
		logger:    logger,
	}
}

// TerminateInstance sends telemetry and returns.
// In K8s, the pod terminates when the agent process exits - no API call needed.
func (t *K8sTerminator) TerminateInstance(ctx context.Context, podName string, status JobStatus) error {
	t.logger.Printf("Preparing to terminate pod %s", podName)

	// Send termination notification
	if t.telemetry != nil {
		t.logger.Println("Sending job completion telemetry...")

		telemetryCtx, cancel := context.WithTimeout(ctx, TelemetryTimeout)
		defer cancel()

		if err := t.telemetry.SendJobCompleted(telemetryCtx, status); err != nil {
			t.logger.Printf("Warning: failed to send telemetry: %v", err)
		} else {
			t.logger.Println("Telemetry sent successfully")
		}
	}

	// Small delay to ensure telemetry is sent
	time.Sleep(k8sTerminationDelay)

	t.logger.Printf("Pod %s will terminate when process exits", podName)
	return nil
}

// TerminateWithStatus terminates with a specific status.
func (t *K8sTerminator) TerminateWithStatus(podName string, status string, exitCode int, duration time.Duration, errorMsg string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	jobStatus := JobStatus{
		InstanceID:      podName,
		Status:          status,
		ExitCode:        exitCode,
		DurationSeconds: int(duration.Seconds()),
		CompletedAt:     time.Now(),
		Error:           errorMsg,
	}

	return t.TerminateInstance(ctx, podName, jobStatus)
}

// TerminateOnPanic handles termination after a panic.
func (t *K8sTerminator) TerminateOnPanic(podName, jobID string, panicValue interface{}) {
	t.logger.Printf("PANIC RECOVERY: Pod %s terminating due to panic: %v", podName, panicValue)

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Minute)
	defer cancel()

	status := JobStatus{
		InstanceID:  podName,
		JobID:       jobID,
		Status:      "failure",
		ExitCode:    -1,
		CompletedAt: time.Now(),
		Error:       "agent panic: " + formatPanicValue(panicValue),
	}

	if err := t.TerminateInstance(ctx, podName, status); err != nil {
		t.logger.Printf("Failed to send termination telemetry after panic: %v", err)
	}
}

// formatPanicValue converts panic value to string.
func formatPanicValue(v interface{}) string {
	switch val := v.(type) {
	case string:
		return val
	case error:
		return val.Error()
	default:
		return "unknown panic"
	}
}
