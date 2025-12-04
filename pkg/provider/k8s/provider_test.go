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
				JITToken:   "test-jit-token",
			},
			wantErr: false,
		},
		{
			name: "amd64 architecture",
			spec: &provider.RunnerSpec{
				RunID:      "run-789",
				JobID:      "job-012",
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
				RunID:    "run-defaults",
				JobID:    "job-defaults",
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
		RunID:    "run-error",
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
				RunID: "run-123",
				Spot:  true,
			},
			wantCount:    7, // GKE preemptible, GKE spot, GKE provisioning, EKS, AKS, runs-fleet, Karpenter
			wantTaintKey: "eks.amazonaws.com/capacityType",
		},
		{
			name: "non-spot runner has no tolerations",
			spec: &provider.RunnerSpec{
				RunID: "run-456",
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

const labelValueTrue = "true"

func TestBuildNetworkPolicy(t *testing.T) {
	cfg := &config.Config{
		KubeNamespace:   "runs-fleet",
		KubeRunnerImage: "runner:latest",
	}
	p := &Provider{config: cfg}

	spec := &provider.RunnerSpec{
		RunID:   "run-123",
		Private: true,
	}

	netpol := p.buildNetworkPolicy("runner-run-123", spec)

	if netpol.Name != "runner-run-123-netpol" {
		t.Errorf("NetworkPolicy name = %s, want runner-run-123-netpol", netpol.Name)
	}

	if netpol.Namespace != "runs-fleet" {
		t.Errorf("NetworkPolicy namespace = %s, want runs-fleet", netpol.Namespace)
	}

	if len(netpol.Spec.Egress) != 3 {
		t.Errorf("NetworkPolicy egress rules = %d, want 3 (DNS, HTTPS, HTTP)", len(netpol.Spec.Egress))
	}

	// Verify selector targets private pods
	if netpol.Spec.PodSelector.MatchLabels["runs-fleet.io/private"] != labelValueTrue {
		t.Error("NetworkPolicy should select private pods")
	}

	// Verify blockedCIDRs are applied to HTTP/HTTPS egress rules
	metadataServiceCIDR := "169.254.0.0/16"
	for i, rule := range netpol.Spec.Egress {
		if i == 0 {
			continue // Skip DNS rule (index 0)
		}
		if len(rule.To) == 0 {
			t.Errorf("Egress rule %d should have To destinations", i)
			continue
		}
		ipBlock := rule.To[0].IPBlock
		if ipBlock == nil {
			t.Errorf("Egress rule %d should have IPBlock", i)
			continue
		}
		found := false
		for _, except := range ipBlock.Except {
			if except == metadataServiceCIDR {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("Egress rule %d should block metadata service CIDR %s", i, metadataServiceCIDR)
		}
	}
}

func TestCreateRunner_WithSpotAndPrivate(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	cfg := &config.Config{
		KubeNamespace:   "runs-fleet",
		KubeRunnerImage: "runner:latest",
	}
	p := NewProviderWithClient(clientset, cfg)

	spec := &provider.RunnerSpec{
		RunID:    "run-spot-private",
		JobID:    "job-123",
		Repo:     "org/repo",
		Arch:     "arm64",
		OS:       "linux",
		Pool:     "default",
		Spot:     true,
		Private:  true,
		JITToken: "test-jit-token",
	}

	result, err := p.CreateRunner(context.Background(), spec)
	if err != nil {
		t.Fatalf("CreateRunner() error = %v", err)
	}

	if len(result.RunnerIDs) != 1 {
		t.Fatalf("CreateRunner() returned %d runner IDs, want 1", len(result.RunnerIDs))
	}

	// Verify pod was created with spot tolerations
	pod, err := clientset.CoreV1().Pods("runs-fleet").Get(context.Background(), result.RunnerIDs[0], metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Failed to get created pod: %v", err)
	}

	if len(pod.Spec.Tolerations) == 0 {
		t.Error("Pod should have tolerations for spot scheduling")
	}

	// Verify spot label
	if pod.Labels["runs-fleet.io/spot"] != labelValueTrue {
		t.Error("Pod should have runs-fleet.io/spot=true label")
	}

	// Verify private label
	if pod.Labels["runs-fleet.io/private"] != labelValueTrue {
		t.Error("Pod should have runs-fleet.io/private=true label")
	}

	// Verify NetworkPolicy was created
	netpol, err := clientset.NetworkingV1().NetworkPolicies("runs-fleet").Get(context.Background(), networkPolicyName(result.RunnerIDs[0]), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Failed to get NetworkPolicy: %v", err)
	}

	if netpol.Spec.PodSelector.MatchLabels["runs-fleet.io/private"] != labelValueTrue {
		t.Error("NetworkPolicy should target private pods")
	}
}

func TestTerminateRunner_CleansUpNetworkPolicy(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	cfg := &config.Config{
		KubeNamespace:   "runs-fleet",
		KubeRunnerImage: "runner:latest",
	}
	p := NewProviderWithClient(clientset, cfg)

	// Create a private runner (creates pod + NetworkPolicy)
	spec := &provider.RunnerSpec{
		RunID:    "run-cleanup-test",
		JobID:    "job-123",
		Repo:     "org/repo",
		Arch:     "arm64",
		OS:       "linux",
		Pool:     "default",
		Private:  true,
		JITToken: "test-jit-token",
	}

	result, err := p.CreateRunner(context.Background(), spec)
	if err != nil {
		t.Fatalf("CreateRunner() error = %v", err)
	}

	runnerID := result.RunnerIDs[0]

	// Verify NetworkPolicy exists
	_, err = clientset.NetworkingV1().NetworkPolicies("runs-fleet").Get(context.Background(), networkPolicyName(runnerID), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("NetworkPolicy should exist before termination: %v", err)
	}

	// Terminate runner
	err = p.TerminateRunner(context.Background(), runnerID)
	if err != nil {
		t.Fatalf("TerminateRunner() error = %v", err)
	}

	// Verify pod was deleted
	_, err = clientset.CoreV1().Pods("runs-fleet").Get(context.Background(), runnerID, metav1.GetOptions{})
	if err == nil {
		t.Error("Pod should be deleted after termination")
	}

	// Verify NetworkPolicy was deleted
	_, err = clientset.NetworkingV1().NetworkPolicies("runs-fleet").Get(context.Background(), networkPolicyName(runnerID), metav1.GetOptions{})
	if err == nil {
		t.Error("NetworkPolicy should be deleted after termination")
	}
}

func TestCreateRunner_NetworkPolicyFailure_CleansPod(t *testing.T) {
	clientset := fake.NewSimpleClientset()

	// Inject failure on NetworkPolicy creation
	clientset.PrependReactor("create", "networkpolicies", func(_ k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("simulated NetworkPolicy creation failure")
	})

	cfg := &config.Config{
		KubeNamespace:   "runs-fleet",
		KubeRunnerImage: "runner:latest",
	}
	p := NewProviderWithClient(clientset, cfg)

	spec := &provider.RunnerSpec{
		RunID:    "run-netpol-fail",
		JobID:    "job-123",
		Repo:     "org/repo",
		Arch:     "arm64",
		OS:       "linux",
		Private:  true,
		JITToken: "test-jit-token",
	}

	_, err := p.CreateRunner(context.Background(), spec)
	if err == nil {
		t.Fatal("CreateRunner() should fail when NetworkPolicy creation fails")
	}

	if !strings.Contains(err.Error(), "NetworkPolicy") {
		t.Errorf("Error should mention NetworkPolicy, got: %v", err)
	}

	// Verify pod was cleaned up
	pods, listErr := clientset.CoreV1().Pods("runs-fleet").List(context.Background(), metav1.ListOptions{})
	if listErr != nil {
		t.Fatalf("Failed to list pods: %v", listErr)
	}

	if len(pods.Items) != 0 {
		t.Errorf("Pod should be cleaned up after NetworkPolicy failure, found %d pods", len(pods.Items))
	}
}

func TestCreateRunner_NonPrivate_SkipsNetworkPolicy(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	cfg := &config.Config{
		KubeNamespace:   "runs-fleet",
		KubeRunnerImage: "runner:latest",
	}
	p := NewProviderWithClient(clientset, cfg)

	spec := &provider.RunnerSpec{
		RunID:    "run-non-private",
		JobID:    "job-123",
		Repo:     "org/repo",
		Arch:     "arm64",
		OS:       "linux",
		Private:  false, // Explicitly non-private
		JITToken: "test-jit-token",
	}

	result, err := p.CreateRunner(context.Background(), spec)
	if err != nil {
		t.Fatalf("CreateRunner() error = %v", err)
	}

	// Verify pod was created
	_, err = clientset.CoreV1().Pods("runs-fleet").Get(context.Background(), result.RunnerIDs[0], metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Pod should exist: %v", err)
	}

	// Verify NO NetworkPolicy was created
	netpols, err := clientset.NetworkingV1().NetworkPolicies("runs-fleet").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("Failed to list NetworkPolicies: %v", err)
	}

	if len(netpols.Items) != 0 {
		t.Errorf("Non-private runner should not create NetworkPolicy, found %d", len(netpols.Items))
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
		RunID:      "run-pvc-test",
		JobID:      "job-123",
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
	if pvc.Labels["runs-fleet.io/run-id"] != "run-pvc-test" {
		t.Errorf("PVC run-id label = %s, want run-pvc-test", pvc.Labels["runs-fleet.io/run-id"])
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
		RunID:      "run-pvc-cleanup",
		JobID:      "job-123",
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
		RunID:    "run-default-storage",
		JobID:    "job-123",
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
		RunID:      "run-storage-class",
		JobID:      "job-123",
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
		RunID:    "run-configmap-test",
		JobID:    "job-123",
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
		RunID:    "run-idempotent",
		JobID:    "job-123",
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
		RunID:    "run-pod-exists",
		JobID:    "job-123",
		Repo:     "org/repo",
		Arch:     "arm64",
		OS:       "linux",
		Private:  true,
		JITToken: "test-jit-token",
	}

	// Pre-create pod to simulate another instance already creating it
	_, err := clientset.CoreV1().Pods("runs-fleet").Create(context.Background(), &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "runner-run-pod-exists",
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

	if result.RunnerIDs[0] != "runner-run-pod-exists" {
		t.Errorf("Runner ID = %s, want runner-run-pod-exists", result.RunnerIDs[0])
	}

	// Verify NetworkPolicy is still created even when Pod already existed
	netpolName := "runner-run-pod-exists-netpol"
	_, err = clientset.NetworkingV1().NetworkPolicies("runs-fleet").Get(context.Background(), netpolName, metav1.GetOptions{})
	if err != nil {
		t.Errorf("NetworkPolicy not created for private job when Pod already existed: %v", err)
	}
}

// Ensure Provider implements provider.Provider.
var _ provider.Provider = (*Provider)(nil)
