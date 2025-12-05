package agent

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

const testNamespace = "test-ns"

func init() {
	// Use minimal delays in tests to avoid slow test execution
	k8sTerminationDelay = 1 * time.Millisecond
}

// mockTelemetryClient implements TelemetryClient for testing K8sTerminator.
type mockTelemetryClient struct {
	mu                  sync.Mutex
	sendStartedCalls    int
	sendCompletedCalls  int
	sendStartedErr      error
	sendCompletedErr    error
	lastCompletedStatus JobStatus
	sendCompletedFunc   func(ctx context.Context, status JobStatus) error
}

func (m *mockTelemetryClient) SendJobStarted(_ context.Context, _ JobStatus) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sendStartedCalls++
	return m.sendStartedErr
}

func (m *mockTelemetryClient) SendJobCompleted(ctx context.Context, status JobStatus) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sendCompletedCalls++
	m.lastCompletedStatus = status
	if m.sendCompletedFunc != nil {
		return m.sendCompletedFunc(ctx, status)
	}
	return m.sendCompletedErr
}

func (m *mockTelemetryClient) getCompletedCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sendCompletedCalls
}

func (m *mockTelemetryClient) getLastCompletedStatus() JobStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastCompletedStatus
}

func TestK8sTerminator_ImplementsInstanceTerminator(_ *testing.T) {
	// Verify at compile time that K8sTerminator implements InstanceTerminator
	var _ InstanceTerminator = (*K8sTerminator)(nil)
}

func TestNewK8sTerminator(t *testing.T) {
	logger := &mockLogger{}
	telemetry := &mockTelemetryClient{}
	clientset := fake.NewSimpleClientset()

	terminator := NewK8sTerminator(clientset, testNamespace, telemetry, logger)

	if terminator == nil {
		t.Fatal("NewK8sTerminator() returned nil")
	}
	if terminator.telemetry == nil {
		t.Error("NewK8sTerminator() did not set telemetry")
	}
	if terminator.logger == nil {
		t.Error("NewK8sTerminator() did not set logger")
	}
	if terminator.clientset == nil {
		t.Error("NewK8sTerminator() did not set clientset")
	}
	if terminator.namespace != testNamespace {
		t.Errorf("NewK8sTerminator() namespace = %q, want %q", terminator.namespace, testNamespace)
	}
}

func TestNewK8sTerminator_NilTelemetry(t *testing.T) {
	logger := &mockLogger{}

	terminator := NewK8sTerminator(nil, "default", nil, logger)

	if terminator == nil {
		t.Fatal("NewK8sTerminator() returned nil")
	}
	if terminator.telemetry != nil {
		t.Error("NewK8sTerminator() should allow nil telemetry")
	}
	if terminator.clientset != nil {
		t.Error("NewK8sTerminator() should allow nil clientset")
	}
}

func TestK8sTerminator_TerminateInstance(t *testing.T) {
	t.Run("with telemetry", func(t *testing.T) {
		logger := &mockLogger{}
		telemetry := &mockTelemetryClient{}
		terminator := NewK8sTerminator(nil, "default", telemetry, logger)

		status := JobStatus{
			InstanceID:      "pod-123",
			JobID:           "job-456",
			ExitCode:        0,
			DurationSeconds: 60,
			CompletedAt:     time.Now(),
		}

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		err := terminator.TerminateInstance(ctx, "pod-123", status)
		if err != nil {
			t.Fatalf("TerminateInstance() error = %v", err)
		}

		if telemetry.getCompletedCalls() != 1 {
			t.Errorf("expected 1 SendJobCompleted call, got %d", telemetry.getCompletedCalls())
		}
	})

	t.Run("without telemetry", func(t *testing.T) {
		logger := &mockLogger{}
		terminator := NewK8sTerminator(nil, "default", nil, logger)

		status := JobStatus{
			InstanceID: "pod-123",
		}

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		err := terminator.TerminateInstance(ctx, "pod-123", status)
		if err != nil {
			t.Fatalf("TerminateInstance() error = %v", err)
		}
		// Should complete without telemetry
	})

	t.Run("telemetry error", func(t *testing.T) {
		logger := &mockLogger{}
		telemetry := &mockTelemetryClient{
			sendCompletedErr: errors.New("telemetry failed"),
		}
		terminator := NewK8sTerminator(nil, "default", telemetry, logger)

		status := JobStatus{
			InstanceID: "pod-123",
		}

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		// Should not return error even if telemetry fails
		err := terminator.TerminateInstance(ctx, "pod-123", status)
		if err != nil {
			t.Fatalf("TerminateInstance() should not return error for telemetry failure, got: %v", err)
		}
	})
}

func TestK8sTerminator_TerminateWithStatus(t *testing.T) {
	t.Run("success status", func(t *testing.T) {
		logger := &mockLogger{}
		telemetry := &mockTelemetryClient{}
		terminator := NewK8sTerminator(nil, "default", telemetry, logger)

		err := terminator.TerminateWithStatus("pod-123", "success", 0, 60*time.Second, "")
		if err != nil {
			t.Fatalf("TerminateWithStatus() error = %v", err)
		}

		status := telemetry.getLastCompletedStatus()
		if status.InstanceID != "pod-123" {
			t.Errorf("expected InstanceID 'pod-123', got '%s'", status.InstanceID)
		}
		if status.Status != "success" {
			t.Errorf("expected Status 'success', got '%s'", status.Status)
		}
		if status.ExitCode != 0 {
			t.Errorf("expected ExitCode 0, got %d", status.ExitCode)
		}
		if status.DurationSeconds != 60 {
			t.Errorf("expected DurationSeconds 60, got %d", status.DurationSeconds)
		}
	})

	t.Run("failure status", func(t *testing.T) {
		logger := &mockLogger{}
		telemetry := &mockTelemetryClient{}
		terminator := NewK8sTerminator(nil, "default", telemetry, logger)

		err := terminator.TerminateWithStatus("pod-456", "failure", 1, 30*time.Second, "job failed")
		if err != nil {
			t.Fatalf("TerminateWithStatus() error = %v", err)
		}

		status := telemetry.getLastCompletedStatus()
		if status.Status != "failure" {
			t.Errorf("expected Status 'failure', got '%s'", status.Status)
		}
		if status.ExitCode != 1 {
			t.Errorf("expected ExitCode 1, got %d", status.ExitCode)
		}
		if status.Error != "job failed" {
			t.Errorf("expected Error 'job failed', got '%s'", status.Error)
		}
	})
}

func TestK8sTerminator_TerminateOnPanic(t *testing.T) {
	t.Run("string panic", func(t *testing.T) {
		logger := &mockLogger{}
		telemetry := &mockTelemetryClient{}
		terminator := NewK8sTerminator(nil, "default", telemetry, logger)

		terminator.TerminateOnPanic("pod-789", "job-999", "test panic message")

		status := telemetry.getLastCompletedStatus()
		if status.InstanceID != "pod-789" {
			t.Errorf("expected InstanceID 'pod-789', got '%s'", status.InstanceID)
		}
		if status.JobID != "job-999" {
			t.Errorf("expected JobID 'job-999', got '%s'", status.JobID)
		}
		if status.Status != "failure" {
			t.Errorf("expected Status 'failure', got '%s'", status.Status)
		}
		if status.ExitCode != -1 {
			t.Errorf("expected ExitCode -1, got %d", status.ExitCode)
		}
		if status.Error != "agent panic: test panic message" {
			t.Errorf("expected Error 'agent panic: test panic message', got '%s'", status.Error)
		}
	})

	t.Run("error panic", func(t *testing.T) {
		logger := &mockLogger{}
		telemetry := &mockTelemetryClient{}
		terminator := NewK8sTerminator(nil, "default", telemetry, logger)

		terminator.TerminateOnPanic("pod-123", "job-456", errors.New("error panic"))

		status := telemetry.getLastCompletedStatus()
		if status.Error != "agent panic: error panic" {
			t.Errorf("expected Error 'agent panic: error panic', got '%s'", status.Error)
		}
	})

	t.Run("other panic type", func(t *testing.T) {
		logger := &mockLogger{}
		telemetry := &mockTelemetryClient{}
		terminator := NewK8sTerminator(nil, "default", telemetry, logger)

		terminator.TerminateOnPanic("pod-123", "job-456", 12345)

		status := telemetry.getLastCompletedStatus()
		if status.Error != "agent panic: unknown panic" {
			t.Errorf("expected Error 'agent panic: unknown panic', got '%s'", status.Error)
		}
	})

	t.Run("telemetry failure", func(_ *testing.T) {
		logger := &mockLogger{}
		telemetry := &mockTelemetryClient{
			sendCompletedErr: errors.New("failed"),
		}
		terminator := NewK8sTerminator(nil, "default", telemetry, logger)

		// Should not panic even if telemetry fails
		terminator.TerminateOnPanic("pod-123", "job-456", "panic")
	})
}

func TestFormatPanicValue(t *testing.T) {
	tests := []struct {
		name     string
		input    interface{}
		expected string
	}{
		{
			name:     "string value",
			input:    "panic message",
			expected: "panic message",
		},
		{
			name:     "error value",
			input:    errors.New("error message"),
			expected: "error message",
		},
		{
			name:     "int value",
			input:    42,
			expected: "unknown panic",
		},
		{
			name:     "nil value",
			input:    nil,
			expected: "unknown panic",
		},
		{
			name:     "struct value",
			input:    struct{ X int }{X: 1},
			expected: "unknown panic",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := formatPanicValue(tt.input)
			if result != tt.expected {
				t.Errorf("formatPanicValue(%v) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestK8sTerminator_CleanupResources(t *testing.T) {
	t.Run("deletes secret and configmap", func(t *testing.T) {
		namespace := testNamespace
		podName := "runner-12345"
		secretName := podName + "-secrets"
		configMapName := podName + "-config"

		// Create fake clientset with pre-existing resources
		clientset := fake.NewSimpleClientset(
			&corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      secretName,
					Namespace: namespace,
				},
			},
			&corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      configMapName,
					Namespace: namespace,
				},
			},
		)

		logger := &mockLogger{}
		terminator := NewK8sTerminator(clientset, namespace, nil, logger)

		ctx := context.Background()
		status := JobStatus{InstanceID: podName}

		err := terminator.TerminateInstance(ctx, podName, status)
		if err != nil {
			t.Fatalf("TerminateInstance() error = %v", err)
		}

		// Verify Secret was deleted
		_, err = clientset.CoreV1().Secrets(namespace).Get(ctx, secretName, metav1.GetOptions{})
		if err == nil {
			t.Error("Secret should have been deleted")
		}

		// Verify ConfigMap was deleted
		_, err = clientset.CoreV1().ConfigMaps(namespace).Get(ctx, configMapName, metav1.GetOptions{})
		if err == nil {
			t.Error("ConfigMap should have been deleted")
		}
	})

	t.Run("handles missing resources gracefully", func(t *testing.T) {
		namespace := testNamespace
		podName := "runner-nonexistent"

		// Create fake clientset with no resources
		clientset := fake.NewSimpleClientset()

		logger := &mockLogger{}
		terminator := NewK8sTerminator(clientset, namespace, nil, logger)

		ctx := context.Background()
		status := JobStatus{InstanceID: podName}

		// Should not error when resources don't exist
		err := terminator.TerminateInstance(ctx, podName, status)
		if err != nil {
			t.Fatalf("TerminateInstance() should not error for missing resources, got: %v", err)
		}
	})

	t.Run("skips cleanup when clientset is nil", func(t *testing.T) {
		logger := &mockLogger{}
		terminator := NewK8sTerminator(nil, "default", nil, logger)

		ctx := context.Background()
		status := JobStatus{InstanceID: "pod-123"}

		// Should not panic or error
		err := terminator.TerminateInstance(ctx, "pod-123", status)
		if err != nil {
			t.Fatalf("TerminateInstance() should not error with nil clientset, got: %v", err)
		}
	})

	t.Run("cleanup with telemetry", func(t *testing.T) {
		namespace := testNamespace
		podName := "runner-with-telemetry"
		secretName := podName + "-secrets"
		configMapName := podName + "-config"

		clientset := fake.NewSimpleClientset(
			&corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      secretName,
					Namespace: namespace,
				},
			},
			&corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      configMapName,
					Namespace: namespace,
				},
			},
		)

		logger := &mockLogger{}
		telemetry := &mockTelemetryClient{}
		terminator := NewK8sTerminator(clientset, namespace, telemetry, logger)

		ctx := context.Background()
		status := JobStatus{InstanceID: podName, ExitCode: 0}

		err := terminator.TerminateInstance(ctx, podName, status)
		if err != nil {
			t.Fatalf("TerminateInstance() error = %v", err)
		}

		// Verify resources were cleaned up
		_, err = clientset.CoreV1().Secrets(namespace).Get(ctx, secretName, metav1.GetOptions{})
		if err == nil {
			t.Error("Secret should have been deleted")
		}

		// Verify telemetry was sent
		if telemetry.getCompletedCalls() != 1 {
			t.Errorf("expected 1 SendJobCompleted call, got %d", telemetry.getCompletedCalls())
		}
	})
}
