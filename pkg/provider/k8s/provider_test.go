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
				RunID:      123,
				JobID:      456,
				Repo:       "org/repo",
				Arch:       "arm64",
				OS:         "linux",
				Pool:       "default",
				CPUCores:   4,
				MemoryGiB:  8,
				StorageGiB: 50,
				JITToken:   "test-jit-token",
			},
			wantErr: false,
		},
		{
			name: "amd64 architecture",
			spec: &provider.RunnerSpec{
				RunID:      789,
				JobID:      12,
				Arch:       "amd64",
				OS:         "linux",
				CPUCores:   4,
				MemoryGiB:  8,
				StorageGiB: 50,
				JITToken:   "test-jit-token",
			},
			wantErr: false,
		},
		{
			name: "default resources when not specified",
			spec: &provider.RunnerSpec{
				RunID:    1001,
				JobID:    1002,
				Arch:     "arm64",
				JITToken: "test-jit-token",
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

				if pod.Labels["runs-fleet.io/run-id"] != fmt.Sprintf("%d", tt.spec.RunID) {
					t.Errorf("Pod run-id label = %v, want %d", pod.Labels["runs-fleet.io/run-id"], tt.spec.RunID)
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
		name    string
		spec    *provider.RunnerSpec
		wantCPU string
		wantMem string
	}{
		{
			name:    "explicit resources",
			spec:    &provider.RunnerSpec{CPUCores: 4, MemoryGiB: 8, StorageGiB: 50},
			wantCPU: "4",
			wantMem: "8Gi",
		},
		{
			name:    "large resources",
			spec:    &provider.RunnerSpec{CPUCores: 8, MemoryGiB: 32, StorageGiB: 100},
			wantCPU: "8",
			wantMem: "32Gi",
		},
		{
			name:    "fractional memory",
			spec:    &provider.RunnerSpec{CPUCores: 4, MemoryGiB: 8.5, StorageGiB: 50},
			wantCPU: "4",
			wantMem: "8704Mi", // 8.5 * 1024 = 8704Mi
		},
		{
			name:    "defaults when zero",
			spec:    &provider.RunnerSpec{},
			wantCPU: "2",
			wantMem: "4Gi",
		},
		{
			name:    "partial spec uses defaults",
			spec:    &provider.RunnerSpec{CPUCores: 4},
			wantCPU: "4",
			wantMem: "4Gi",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resources := p.getResourceRequirements(tt.spec)

			// Check requests (storage handled via PVC, not ephemeral)
			cpu := resources.Requests[corev1.ResourceCPU]
			if cpu.String() != tt.wantCPU {
				t.Errorf("CPU request = %v, want %v", cpu.String(), tt.wantCPU)
			}

			mem := resources.Requests[corev1.ResourceMemory]
			if mem.String() != tt.wantMem {
				t.Errorf("Memory request = %v, want %v", mem.String(), tt.wantMem)
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
		RunID:    99999,
		JITToken: "test-jit-token",
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
		name  string
		jobID string
		want  string
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
			got := sanitizePodName(tt.jobID)
			if len(got) > 63 {
				t.Errorf("sanitizePodName(%q) length = %d, exceeds 63", tt.jobID, len(got))
			}
			if got != tt.want && tt.name != "very long" {
				t.Errorf("sanitizePodName(%q) = %q, want %q", tt.jobID, got, tt.want)
			}
			// Verify DNS-1123 compliance
			if got != "" && (got[0] == '-' || got[len(got)-1] == '-') {
				t.Errorf("sanitizePodName(%q) = %q, has leading/trailing dash", tt.jobID, got)
			}
		})
	}
}

func TestCreateRunner_MultipleJobsSameWorkflow(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	cfg := &config.Config{
		KubeNamespace:      "runs-fleet",
		KubeServiceAccount: "runner-sa",
		KubeRunnerImage:    "runner:latest",
	}
	p := NewProviderWithClient(clientset, cfg)

	// Same RunID (workflow run), different JobIDs (individual jobs)
	var sharedRunID int64 = 12345

	spec1 := &provider.RunnerSpec{
		RunID:    sharedRunID,
		JobID:    111,
		Repo:     "org/repo",
		Arch:     "arm64",
		JITToken: "test-jit-token-1",
	}

	spec2 := &provider.RunnerSpec{
		RunID:    sharedRunID,
		JobID:    222,
		Repo:     "org/repo",
		Arch:     "arm64",
		JITToken: "test-jit-token-2",
	}

	// Create first job's runner
	result1, err := p.CreateRunner(context.Background(), spec1)
	if err != nil {
		t.Fatalf("CreateRunner(job-111) error = %v", err)
	}

	// Create second job's runner
	result2, err := p.CreateRunner(context.Background(), spec2)
	if err != nil {
		t.Fatalf("CreateRunner(job-222) error = %v", err)
	}

	// Verify different pod names
	if result1.RunnerIDs[0] == result2.RunnerIDs[0] {
		t.Errorf("Pods should have different names, both got %s", result1.RunnerIDs[0])
	}

	// Verify both pods exist
	pods, err := clientset.CoreV1().Pods("runs-fleet").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("Failed to list pods: %v", err)
	}
	if len(pods.Items) != 2 {
		t.Errorf("Expected 2 pods for 2 jobs, got %d", len(pods.Items))
	}

	// Verify both pods share the same run-id label
	expectedRunID := fmt.Sprintf("%d", sharedRunID)
	for _, pod := range pods.Items {
		if pod.Labels["runs-fleet.io/run-id"] != expectedRunID {
			t.Errorf("Pod %s has run-id label %s, want %s", pod.Name, pod.Labels["runs-fleet.io/run-id"], expectedRunID)
		}
	}
}

func TestBuildTolerations(t *testing.T) {
	cfg := &config.Config{
		KubeNamespace:   "runs-fleet",
		KubeRunnerImage: "runner:latest",
	}
	p := &Provider{config: cfg}

	tests := []struct {
		name         string
		spec         *provider.RunnerSpec
		wantCount    int
		wantTaintKey string
	}{
		{
			name: "spot runner has tolerations",
			spec: &provider.RunnerSpec{
				RunID: 123,
				Spot:  true,
			},
			wantCount:    7, // GKE preemptible, GKE spot, GKE provisioning, EKS, AKS, runs-fleet, Karpenter
			wantTaintKey: "eks.amazonaws.com/capacityType",
		},
		{
			name: "non-spot runner has no tolerations",
			spec: &provider.RunnerSpec{
				RunID: 456,
				Spot:  false,
			},
			wantCount: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tolerations := p.buildTolerations(tt.spec)
			if len(tolerations) != tt.wantCount {
				t.Errorf("buildTolerations() count = %d, want %d", len(tolerations), tt.wantCount)
			}
			if tt.wantTaintKey != "" {
				found := false
				for _, tol := range tolerations {
					if tol.Key == tt.wantTaintKey {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("buildTolerations() missing toleration for key %s", tt.wantTaintKey)
				}
			}
		})
	}
}

func TestCreateRunner_CreatesPVC(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	cfg := &config.Config{
		KubeNamespace:      "runs-fleet",
		KubeServiceAccount: "runner-sa",
		KubeRunnerImage:    "runner:latest",
	}
	p := NewProviderWithClient(clientset, cfg)

	spec := &provider.RunnerSpec{
		RunID:      33333,
		JobID:      44444,
		Repo:       "org/repo",
		Arch:       "arm64",
		OS:         "linux",
		StorageGiB: 50,
		JITToken:   "test-jit-token",
	}

	result, err := p.CreateRunner(context.Background(), spec)
	if err != nil {
		t.Fatalf("CreateRunner() error = %v", err)
	}

	// Verify PVC was created
	pvc, err := clientset.CoreV1().PersistentVolumeClaims("runs-fleet").Get(context.Background(), pvcName(result.RunnerIDs[0]), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("PVC should exist: %v", err)
	}

	// Verify PVC size
	storage := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	if storage.String() != "50Gi" {
		t.Errorf("PVC storage = %s, want 50Gi", storage.String())
	}

	// Verify PVC labels
	if pvc.Labels["runs-fleet.io/run-id"] != "33333" {
		t.Errorf("PVC run-id label = %s, want 33333", pvc.Labels["runs-fleet.io/run-id"])
	}

	// Verify pod has workspace volume mount
	pod, err := clientset.CoreV1().Pods("runs-fleet").Get(context.Background(), result.RunnerIDs[0], metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Pod should exist: %v", err)
	}

	workspaceFound := false
	for _, mount := range pod.Spec.Containers[0].VolumeMounts {
		if mount.Name == "workspace" && mount.MountPath == "/runner/_work" {
			workspaceFound = true
			break
		}
	}
	if !workspaceFound {
		t.Error("Pod should have workspace volume mounted at /runner/_work")
	}
}

func TestTerminateRunner_DeletesPVC(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	cfg := &config.Config{
		KubeNamespace:      "runs-fleet",
		KubeServiceAccount: "runner-sa",
		KubeRunnerImage:    "runner:latest",
	}
	p := NewProviderWithClient(clientset, cfg)

	spec := &provider.RunnerSpec{
		RunID:      55555,
		JobID:      66666,
		Repo:       "org/repo",
		Arch:       "arm64",
		StorageGiB: 30,
		JITToken:   "test-jit-token",
	}

	result, err := p.CreateRunner(context.Background(), spec)
	if err != nil {
		t.Fatalf("CreateRunner() error = %v", err)
	}

	runnerID := result.RunnerIDs[0]

	// Verify PVC exists before termination
	_, err = clientset.CoreV1().PersistentVolumeClaims("runs-fleet").Get(context.Background(), pvcName(runnerID), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("PVC should exist before termination: %v", err)
	}

	// Terminate runner
	err = p.TerminateRunner(context.Background(), runnerID)
	if err != nil {
		t.Fatalf("TerminateRunner() error = %v", err)
	}

	// Verify PVC was deleted
	_, err = clientset.CoreV1().PersistentVolumeClaims("runs-fleet").Get(context.Background(), pvcName(runnerID), metav1.GetOptions{})
	if err == nil {
		t.Error("PVC should be deleted after termination")
	}
}

func TestCreateRunner_PVCDefaultStorage(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	cfg := &config.Config{
		KubeNamespace:      "runs-fleet",
		KubeServiceAccount: "runner-sa",
		KubeRunnerImage:    "runner:latest",
	}
	p := NewProviderWithClient(clientset, cfg)

	spec := &provider.RunnerSpec{
		RunID:    77777,
		JobID:    88888,
		Repo:     "org/repo",
		Arch:     "arm64",
		JITToken: "test-jit-token",
		// StorageGiB not set - should use default
	}

	result, err := p.CreateRunner(context.Background(), spec)
	if err != nil {
		t.Fatalf("CreateRunner() error = %v", err)
	}

	// Verify PVC uses default storage (30Gi)
	pvc, err := clientset.CoreV1().PersistentVolumeClaims("runs-fleet").Get(context.Background(), pvcName(result.RunnerIDs[0]), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("PVC should exist: %v", err)
	}

	storage := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	if storage.String() != "30Gi" {
		t.Errorf("PVC storage = %s, want 30Gi (default)", storage.String())
	}
}

func TestCreateRunner_PVCWithStorageClass(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	cfg := &config.Config{
		KubeNamespace:      "runs-fleet",
		KubeServiceAccount: "runner-sa",
		KubeRunnerImage:    "runner:latest",
		KubeStorageClass:   "gp3-csi",
	}
	p := NewProviderWithClient(clientset, cfg)

	spec := &provider.RunnerSpec{
		RunID:      99999,
		JobID:      10101,
		Repo:       "org/repo",
		Arch:       "arm64",
		StorageGiB: 50,
		JITToken:   "test-jit-token",
	}

	result, err := p.CreateRunner(context.Background(), spec)
	if err != nil {
		t.Fatalf("CreateRunner() error = %v", err)
	}

	pvc, err := clientset.CoreV1().PersistentVolumeClaims("runs-fleet").Get(context.Background(), pvcName(result.RunnerIDs[0]), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("PVC should exist: %v", err)
	}

	// Verify storage class is set
	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName != "gp3-csi" {
		t.Errorf("PVC StorageClassName = %v, want gp3-csi", pvc.Spec.StorageClassName)
	}
}

func TestCreateRunner_ValidatesDaemonJSONConfigMap(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	cfg := &config.Config{
		KubeNamespace:           "runs-fleet",
		KubeRunnerImage:         "runner:latest",
		KubeDindImage:           "docker:dind",
		KubeDockerWaitSeconds:   120,
		KubeDockerGroupGID:      123,
		KubeDaemonJSONConfigMap: "missing-daemon-json",
	}
	p := NewProviderWithClient(clientset, cfg)

	spec := &provider.RunnerSpec{
		RunID:    20001,
		JobID:    20002,
		Repo:     "org/repo",
		Arch:     "arm64",
		OS:       "linux",
		Pool:     "default",
		JITToken: "test-jit-token",
	}

	_, err := p.CreateRunner(context.Background(), spec)
	if err == nil {
		t.Fatal("CreateRunner() should fail when daemon.json ConfigMap doesn't exist")
	}

	if !strings.Contains(err.Error(), "daemon.json ConfigMap") {
		t.Errorf("Error should mention daemon.json ConfigMap, got: %v", err)
	}
}

func TestCreateRunner_IdempotentOnAlreadyExists(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	cfg := &config.Config{
		KubeNamespace:      "runs-fleet",
		KubeServiceAccount: "runner-sa",
		KubeRunnerImage:    "runner:latest",
	}
	p := NewProviderWithClient(clientset, cfg)

	spec := &provider.RunnerSpec{
		RunID:    30001,
		JobID:    30002,
		Repo:     "org/repo",
		Arch:     "arm64",
		OS:       "linux",
		JITToken: "test-jit-token",
	}

	// First call creates the runner
	result1, err := p.CreateRunner(context.Background(), spec)
	if err != nil {
		t.Fatalf("First CreateRunner() error = %v", err)
	}

	// Second call should succeed (idempotent) even though resources exist
	result2, err := p.CreateRunner(context.Background(), spec)
	if err != nil {
		t.Fatalf("Second CreateRunner() error = %v, want success (idempotent)", err)
	}

	// Both should return the same runner ID
	if result1.RunnerIDs[0] != result2.RunnerIDs[0] {
		t.Errorf("Runner IDs differ: %s vs %s", result1.RunnerIDs[0], result2.RunnerIDs[0])
	}
}

func TestCreateRunner_IdempotentOnPodAlreadyExists(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	cfg := &config.Config{
		KubeNamespace:      "runs-fleet",
		KubeServiceAccount: "runner-sa",
		KubeRunnerImage:    "runner:latest",
	}
	p := NewProviderWithClient(clientset, cfg)

	spec := &provider.RunnerSpec{
		RunID:    40001,
		JobID:    40002,
		Repo:     "org/repo",
		Arch:     "arm64",
		OS:       "linux",
		JITToken: "test-jit-token",
	}

	// Pre-create pod to simulate another instance already creating it (name derived from JobID)
	_, err := clientset.CoreV1().Pods("runs-fleet").Create(context.Background(), &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "runner-40002",
			Namespace: "runs-fleet",
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Failed to pre-create pod: %v", err)
	}

	// CreateRunner should succeed (idempotent)
	result, err := p.CreateRunner(context.Background(), spec)
	if err != nil {
		t.Fatalf("CreateRunner() error = %v, want success (idempotent)", err)
	}

	if result.RunnerIDs[0] != "runner-40002" {
		t.Errorf("Runner ID = %s, want runner-40002", result.RunnerIDs[0])
	}
}

func TestBuildPodSpec_DockerGIDConsistency(t *testing.T) {
	cfg := &config.Config{
		KubeNamespace:       "runs-fleet",
		KubeRunnerImage:     "runner:latest",
		KubeDindImage:       "docker:dind",
		KubeDockerGroupGID:  999,
		KubeDockerWaitSeconds: 120,
	}
	p := NewProviderWithClient(fake.NewSimpleClientset(), cfg)
	spec := &provider.RunnerSpec{
		RunID:    50001,
		JobID:    50002,
		Repo:     "org/repo",
		Arch:     "arm64",
		JITToken: "test-token",
	}

	pod := p.buildPodSpec("test-pod", spec)

	// Verify SupplementalGroups contains the docker GID
	if pod.Spec.SecurityContext == nil {
		t.Fatal("Pod SecurityContext is nil")
	}
	gidFound := false
	for _, gid := range pod.Spec.SecurityContext.SupplementalGroups {
		if gid == int64(cfg.KubeDockerGroupGID) {
			gidFound = true
			break
		}
	}
	if !gidFound {
		t.Errorf("Docker GID %d not found in SupplementalGroups %v", cfg.KubeDockerGroupGID, pod.Spec.SecurityContext.SupplementalGroups)
	}

	// Find DinD container and verify env var matches
	var dindContainer *corev1.Container
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == "dind" {
			dindContainer = &pod.Spec.Containers[i]
			break
		}
	}
	if dindContainer == nil {
		t.Fatal("DinD container not found")
	}

	envFound := false
	expectedGID := fmt.Sprintf("%d", cfg.KubeDockerGroupGID)
	for _, env := range dindContainer.Env {
		if env.Name == "DOCKER_GROUP_GID" {
			if env.Value != expectedGID {
				t.Errorf("DOCKER_GROUP_GID = %s, want %s", env.Value, expectedGID)
			}
			envFound = true
			break
		}
	}
	if !envFound {
		t.Error("DOCKER_GROUP_GID env var not found in DinD container")
	}
}

// Ensure Provider implements provider.Provider.
var _ provider.Provider = (*Provider)(nil)
