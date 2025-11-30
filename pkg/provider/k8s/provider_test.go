package k8s

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/Shavakan/runs-fleet/pkg/provider"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestProvider_Name(t *testing.T) {
	p := &Provider{}
	if got := p.Name(); got != "k8s" {
		t.Errorf("Name() = %v, want k8s", got)
	}
}

func TestProvider_CreateRunner(t *testing.T) {
	tests := []struct {
		name    string
		spec    *provider.RunnerSpec
		wantErr bool
	}{
		{
			name: "successful creation with resources",
			spec: &provider.RunnerSpec{
				RunID:      "run-123",
				JobID:      "job-456",
				Repo:       "org/repo",
				Arch:       "arm64",
				OS:         "linux",
				Pool:       "default",
				CPUCores:   4,
				MemoryGiB:  8,
				StorageGiB: 50,
			},
			wantErr: false,
		},
		{
			name: "x64 architecture",
			spec: &provider.RunnerSpec{
				RunID:      "run-789",
				JobID:      "job-012",
				Arch:       "x64",
				OS:         "linux",
				CPUCores:   4,
				MemoryGiB:  8,
				StorageGiB: 50,
			},
			wantErr: false,
		},
		{
			name: "default resources when not specified",
			spec: &provider.RunnerSpec{
				RunID: "run-defaults",
				JobID: "job-defaults",
				Arch:  "arm64",
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clientset := fake.NewSimpleClientset()
			cfg := &config.Config{
				KubeNamespace:      "runs-fleet",
				KubeServiceAccount: "runner-sa",
				KubeRunnerImage:    "runner:latest",
			}

			p := NewProviderWithClient(clientset, cfg)
			result, err := p.CreateRunner(context.Background(), tt.spec)

			if (err != nil) != tt.wantErr {
				t.Errorf("CreateRunner() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if !tt.wantErr {
				if len(result.RunnerIDs) != 1 {
					t.Errorf("CreateRunner() returned %d runner IDs, want 1", len(result.RunnerIDs))
				}

				// Verify pod was created
				pod, err := clientset.CoreV1().Pods("runs-fleet").Get(context.Background(), result.RunnerIDs[0], metav1.GetOptions{})
				if err != nil {
					t.Errorf("Failed to get created pod: %v", err)
					return
				}

				if pod.Labels["runs-fleet.io/run-id"] != tt.spec.RunID {
					t.Errorf("Pod run-id label = %v, want %v", pod.Labels["runs-fleet.io/run-id"], tt.spec.RunID)
				}

				if pod.Spec.ServiceAccountName != "runner-sa" {
					t.Errorf("Pod ServiceAccountName = %v, want runner-sa", pod.Spec.ServiceAccountName)
				}
			}
		})
	}
}

func TestProvider_TerminateRunner(t *testing.T) {
	tests := []struct {
		name     string
		runnerID string
		setup    func(*fake.Clientset)
		wantErr  bool
	}{
		{
			name:     "successful termination",
			runnerID: "runner-123",
			setup: func(cs *fake.Clientset) {
				_, _ = cs.CoreV1().Pods("runs-fleet").Create(context.Background(), &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "runner-123",
						Namespace: "runs-fleet",
					},
				}, metav1.CreateOptions{})
			},
			wantErr: false,
		},
		{
			name:     "pod not found",
			runnerID: "runner-nonexistent",
			setup:    func(_ *fake.Clientset) {},
			wantErr:  false, // NotFound is not an error for termination
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clientset := fake.NewSimpleClientset()
			tt.setup(clientset)

			cfg := &config.Config{
				KubeNamespace: "runs-fleet",
			}

			p := NewProviderWithClient(clientset, cfg)
			err := p.TerminateRunner(context.Background(), tt.runnerID)

			if (err != nil) != tt.wantErr {
				t.Errorf("TerminateRunner() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestProvider_DescribeRunner(t *testing.T) {
	now := time.Now()

	tests := []struct {
		name      string
		runnerID  string
		setup     func(*fake.Clientset)
		wantState string
		wantErr   bool
	}{
		{
			name:     "running pod",
			runnerID: "runner-running",
			setup: func(cs *fake.Clientset) {
				_, _ = cs.CoreV1().Pods("runs-fleet").Create(context.Background(), &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "runner-running",
						Namespace: "runs-fleet",
						Labels: map[string]string{
							"runs-fleet.io/instance-type": "c7g.xlarge",
						},
					},
					Status: corev1.PodStatus{
						Phase:     corev1.PodRunning,
						StartTime: &metav1.Time{Time: now},
					},
				}, metav1.CreateOptions{})
			},
			wantState: StateRunning,
		},
		{
			name:     "pending pod",
			runnerID: "runner-pending",
			setup: func(cs *fake.Clientset) {
				_, _ = cs.CoreV1().Pods("runs-fleet").Create(context.Background(), &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "runner-pending",
						Namespace: "runs-fleet",
					},
					Status: corev1.PodStatus{
						Phase: corev1.PodPending,
					},
				}, metav1.CreateOptions{})
			},
			wantState: StatePending,
		},
		{
			name:     "succeeded pod",
			runnerID: "runner-succeeded",
			setup: func(cs *fake.Clientset) {
				_, _ = cs.CoreV1().Pods("runs-fleet").Create(context.Background(), &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "runner-succeeded",
						Namespace: "runs-fleet",
					},
					Status: corev1.PodStatus{
						Phase: corev1.PodSucceeded,
					},
				}, metav1.CreateOptions{})
			},
			wantState: StateTerminated,
		},
		{
			name:     "pod not found",
			runnerID: "runner-nonexistent",
			setup:    func(_ *fake.Clientset) {},
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clientset := fake.NewSimpleClientset()
			tt.setup(clientset)

			cfg := &config.Config{
				KubeNamespace: "runs-fleet",
			}

			p := NewProviderWithClient(clientset, cfg)
			state, err := p.DescribeRunner(context.Background(), tt.runnerID)

			if (err != nil) != tt.wantErr {
				t.Errorf("DescribeRunner() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if !tt.wantErr && state.State != tt.wantState {
				t.Errorf("DescribeRunner() state = %v, want %v", state.State, tt.wantState)
			}
		})
	}
}

func TestMapPodPhase(t *testing.T) {
	tests := []struct {
		phase corev1.PodPhase
		want  string
	}{
		{corev1.PodPending, StatePending},
		{corev1.PodRunning, StateRunning},
		{corev1.PodSucceeded, StateTerminated},
		{corev1.PodFailed, StateTerminated},
		{corev1.PodUnknown, StateUnknown},
	}

	for _, tt := range tests {
		t.Run(string(tt.phase), func(t *testing.T) {
			if got := mapPodPhase(tt.phase); got != tt.want {
				t.Errorf("mapPodPhase(%v) = %v, want %v", tt.phase, got, tt.want)
			}
		})
	}
}

func TestGetResourceRequirements(t *testing.T) {
	cfg := &config.Config{}
	p := NewProviderWithClient(nil, cfg)

	tests := []struct {
		name        string
		spec        *provider.RunnerSpec
		wantCPU     string
		wantMem     string
		wantStorage string
	}{
		{
			name:        "explicit resources",
			spec:        &provider.RunnerSpec{CPUCores: 4, MemoryGiB: 8, StorageGiB: 50},
			wantCPU:     "4",
			wantMem:     "8Gi",
			wantStorage: "50Gi",
		},
		{
			name:        "large resources",
			spec:        &provider.RunnerSpec{CPUCores: 8, MemoryGiB: 32, StorageGiB: 100},
			wantCPU:     "8",
			wantMem:     "32Gi",
			wantStorage: "100Gi",
		},
		{
			name:        "fractional memory",
			spec:        &provider.RunnerSpec{CPUCores: 4, MemoryGiB: 8.5, StorageGiB: 50},
			wantCPU:     "4",
			wantMem:     "8704Mi", // 8.5 * 1024 = 8704Mi
			wantStorage: "50Gi",
		},
		{
			name:        "defaults when zero",
			spec:        &provider.RunnerSpec{},
			wantCPU:     "2",
			wantMem:     "4Gi",
			wantStorage: "30Gi",
		},
		{
			name:        "partial spec uses defaults",
			spec:        &provider.RunnerSpec{CPUCores: 4},
			wantCPU:     "4",
			wantMem:     "4Gi",
			wantStorage: "30Gi",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resources := p.getResourceRequirements(tt.spec)

			// Check requests
			cpu := resources.Requests[corev1.ResourceCPU]
			if cpu.String() != tt.wantCPU {
				t.Errorf("CPU request = %v, want %v", cpu.String(), tt.wantCPU)
			}

			mem := resources.Requests[corev1.ResourceMemory]
			if mem.String() != tt.wantMem {
				t.Errorf("Memory request = %v, want %v", mem.String(), tt.wantMem)
			}

			storage := resources.Requests[corev1.ResourceEphemeralStorage]
			if storage.String() != tt.wantStorage {
				t.Errorf("Storage request = %v, want %v", storage.String(), tt.wantStorage)
			}

			// Check limits match requests
			cpuLimit := resources.Limits[corev1.ResourceCPU]
			if cpuLimit.String() != tt.wantCPU {
				t.Errorf("CPU limit = %v, want %v", cpuLimit.String(), tt.wantCPU)
			}

			memLimit := resources.Limits[corev1.ResourceMemory]
			if memLimit.String() != tt.wantMem {
				t.Errorf("Memory limit = %v, want %v", memLimit.String(), tt.wantMem)
			}

			storageLimit := resources.Limits[corev1.ResourceEphemeralStorage]
			if storageLimit.String() != tt.wantStorage {
				t.Errorf("Storage limit = %v, want %v", storageLimit.String(), tt.wantStorage)
			}
		})
	}
}

func TestProvider_CreateRunner_Error(t *testing.T) {
	clientset := fake.NewSimpleClientset()

	// Add reactor to simulate creation error
	clientset.PrependReactor("create", "pods", func(_ k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("quota exceeded")
	})

	cfg := &config.Config{
		KubeNamespace:      "runs-fleet",
		KubeServiceAccount: "runner-sa",
		KubeRunnerImage:    "runner:latest",
	}

	p := NewProviderWithClient(clientset, cfg)
	_, err := p.CreateRunner(context.Background(), &provider.RunnerSpec{
		RunID: "run-error",
	})

	if err == nil {
		t.Error("CreateRunner() expected error, got nil")
	}
}

func TestProvider_WaitForPodRunning(t *testing.T) {
	tests := []struct {
		name      string
		podName   string
		setup     func(*fake.Clientset)
		timeout   time.Duration
		wantErr   bool
		errSubstr string
	}{
		{
			name:    "pod becomes running",
			podName: "runner-123",
			setup: func(cs *fake.Clientset) {
				_, _ = cs.CoreV1().Pods("runs-fleet").Create(context.Background(), &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "runner-123",
						Namespace: "runs-fleet",
					},
					Status: corev1.PodStatus{
						Phase: corev1.PodRunning,
					},
				}, metav1.CreateOptions{})
			},
			timeout: 5 * time.Second,
			wantErr: false,
		},
		{
			name:    "pod failed",
			podName: "runner-failed",
			setup: func(cs *fake.Clientset) {
				_, _ = cs.CoreV1().Pods("runs-fleet").Create(context.Background(), &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "runner-failed",
						Namespace: "runs-fleet",
					},
					Status: corev1.PodStatus{
						Phase: corev1.PodFailed,
					},
				}, metav1.CreateOptions{})
			},
			timeout:   5 * time.Second,
			wantErr:   true,
			errSubstr: "terminated",
		},
		{
			name:    "pod succeeded",
			podName: "runner-succeeded",
			setup: func(cs *fake.Clientset) {
				_, _ = cs.CoreV1().Pods("runs-fleet").Create(context.Background(), &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "runner-succeeded",
						Namespace: "runs-fleet",
					},
					Status: corev1.PodStatus{
						Phase: corev1.PodSucceeded,
					},
				}, metav1.CreateOptions{})
			},
			timeout:   5 * time.Second,
			wantErr:   true,
			errSubstr: "terminated",
		},
		{
			name:      "pod not found",
			podName:   "nonexistent",
			setup:     func(_ *fake.Clientset) {},
			timeout:   5 * time.Second,
			wantErr:   true,
			errSubstr: "not found",
		},
		{
			name:    "context cancelled",
			podName: "runner-pending",
			setup: func(cs *fake.Clientset) {
				_, _ = cs.CoreV1().Pods("runs-fleet").Create(context.Background(), &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "runner-pending",
						Namespace: "runs-fleet",
					},
					Status: corev1.PodStatus{
						Phase: corev1.PodPending,
					},
				}, metav1.CreateOptions{})
			},
			timeout:   100 * time.Millisecond,
			wantErr:   true,
			errSubstr: "timeout",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clientset := fake.NewSimpleClientset()
			tt.setup(clientset)

			cfg := &config.Config{
				KubeNamespace: "runs-fleet",
			}

			p := NewProviderWithClient(clientset, cfg)
			err := p.WaitForPodRunning(context.Background(), tt.podName, tt.timeout)

			if (err != nil) != tt.wantErr {
				t.Errorf("WaitForPodRunning() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr && tt.errSubstr != "" && err != nil {
				if !strings.Contains(err.Error(), tt.errSubstr) {
					t.Errorf("WaitForPodRunning() error = %q, want substring %q", err.Error(), tt.errSubstr)
				}
			}
		})
	}
}

func TestSanitizePodName(t *testing.T) {
	tests := []struct {
		name   string
		runID  string
		want   string
	}{
		{"simple numeric", "12345", "runner-12345"},
		{"with uppercase", "ABC123", "runner-abc123"},
		{"with underscores", "run_123_456", "runner-run-123-456"},
		{"with special chars", "run@123#456", "runner-run-123-456"},
		{"leading special", "@@@123", "runner-123"},
		{"trailing special", "123@@@", "runner-123"},
		{"all special", "@@@", "runner"},
		{"empty string", "", "runner"},
		{"very long", "a" + string(make([]byte, 100)), "runner-a" + string(make([]byte, 54))},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitizePodName(tt.runID)
			if len(got) > 63 {
				t.Errorf("sanitizePodName(%q) length = %d, exceeds 63", tt.runID, len(got))
			}
			if got != tt.want && tt.name != "very long" {
				t.Errorf("sanitizePodName(%q) = %q, want %q", tt.runID, got, tt.want)
			}
			// Verify DNS-1123 compliance
			if got != "" && (got[0] == '-' || got[len(got)-1] == '-') {
				t.Errorf("sanitizePodName(%q) = %q, has leading/trailing dash", tt.runID, got)
			}
		})
	}
}

// Ensure Provider implements provider.Provider.
var _ provider.Provider = (*Provider)(nil)
