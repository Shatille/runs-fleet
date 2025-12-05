package agent

import (
	"context"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// K8sTerminator handles termination for Kubernetes pods.
// In K8s, pods terminate naturally when the process exits, so this implementation
// cleans up associated secrets and configmaps, sends telemetry, and returns -
// the caller should exit cleanly after.
type K8sTerminator struct {
	clientset kubernetes.Interface
	namespace string
	telemetry TelemetryClient
	logger    Logger
}

// Verify K8sTerminator implements InstanceTerminator.
var _ InstanceTerminator = (*K8sTerminator)(nil)

// NewK8sTerminator creates a new K8s terminator.
// clientset and namespace are required for cleaning up secrets and configmaps.
// If clientset is nil, cleanup will be skipped (useful for testing).
func NewK8sTerminator(clientset kubernetes.Interface, namespace string, telemetry TelemetryClient, logger Logger) *K8sTerminator {
	return &K8sTerminator{
		clientset: clientset,
		namespace: namespace,
		telemetry: telemetry,
		logger:    logger,
	}
}

// TerminateInstance cleans up resources, sends telemetry, and returns.
// In K8s, the pod terminates when the agent process exits - no API call needed.
// Cleans up the Secret and ConfigMap associated with the pod before exiting.
func (t *K8sTerminator) TerminateInstance(ctx context.Context, podName string, status JobStatus) error {
	t.logger.Printf("Preparing to terminate pod %s", podName)

	// Clean up secrets and configmaps before sending telemetry
	t.cleanupResources(ctx, podName)

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
	time.Sleep(1 * time.Second)

	t.logger.Printf("Pod %s will terminate when process exits", podName)
	return nil
}

// cleanupResources deletes the Secret and ConfigMap associated with the pod.
// Uses the same naming convention as the provider: {podName}-secrets and {podName}-config.
// Errors are logged but don't fail the termination - the pod is exiting anyway.
func (t *K8sTerminator) cleanupResources(ctx context.Context, podName string) {
	if t.clientset == nil {
		t.logger.Println("Skipping resource cleanup: no Kubernetes client configured")
		return
	}

	secretName := podName + "-secrets"
	configMapName := podName + "-config"

	t.logger.Printf("Cleaning up resources: Secret=%s, ConfigMap=%s", secretName, configMapName)

	// Delete Secret
	if err := t.clientset.CoreV1().Secrets(t.namespace).Delete(ctx, secretName, metav1.DeleteOptions{}); err != nil {
		if !errors.IsNotFound(err) {
			t.logger.Printf("Warning: failed to delete Secret %s: %v", secretName, err)
		}
	} else {
		t.logger.Printf("Deleted Secret %s", secretName)
	}

	// Delete ConfigMap
	if err := t.clientset.CoreV1().ConfigMaps(t.namespace).Delete(ctx, configMapName, metav1.DeleteOptions{}); err != nil {
		if !errors.IsNotFound(err) {
			t.logger.Printf("Warning: failed to delete ConfigMap %s: %v", configMapName, err)
		}
	} else {
		t.logger.Printf("Deleted ConfigMap %s", configMapName)
	}
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
