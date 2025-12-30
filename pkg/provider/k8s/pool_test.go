package k8s

import (
	"context"
	"testing"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/config"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestPoolProvider_ListPoolRunners(t *testing.T) {
	now := time.Now()

	tests := []struct {
		name      string
		poolName  string
		setup     func(*fake.Clientset)
		wantCount int
		wantErr   bool
	}{
		{
			name:     "list running pods",
			poolName: "default",
			setup: func(cs *fake.Clientset) {
				_, _ = cs.CoreV1().Pods("runs-fleet").Create(context.Background(), &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "runner-1",
						Namespace: "runs-fleet",
						Labels: map[string]string{
							"app":                         "runs-fleet-runner",
							"runs-fleet.io/pool":          "default",
							"runs-fleet.io/instance-type": "c7g.xlarge",
						},
					},
					Status: corev1.PodStatus{
						Phase:     corev1.PodRunning,
						StartTime: &metav1.Time{Time: now},
					},
				}, metav1.CreateOptions{})
				_, _ = cs.CoreV1().Pods("runs-fleet").Create(context.Background(), &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "runner-2",
						Namespace: "runs-fleet",
						Labels: map[string]string{
							"app":                "runs-fleet-runner",
							"runs-fleet.io/pool": "default",
						},
					},
					Status: corev1.PodStatus{
						Phase: corev1.PodPending,
					},
				}, metav1.CreateOptions{})
			},
			wantCount: 2,
		},
		{
			name:     "exclude terminated pods",
			poolName: "default",
			setup: func(cs *fake.Clientset) {
				_, _ = cs.CoreV1().Pods("runs-fleet").Create(context.Background(), &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "runner-running",
						Namespace: "runs-fleet",
						Labels: map[string]string{
							"app":                "runs-fleet-runner",
							"runs-fleet.io/pool": "default",
						},
					},
					Status: corev1.PodStatus{
						Phase: corev1.PodRunning,
					},
				}, metav1.CreateOptions{})
				_, _ = cs.CoreV1().Pods("runs-fleet").Create(context.Background(), &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "runner-succeeded",
						Namespace: "runs-fleet",
						Labels: map[string]string{
							"app":                "runs-fleet-runner",
							"runs-fleet.io/pool": "default",
						},
					},
					Status: corev1.PodStatus{
						Phase: corev1.PodSucceeded,
					},
				}, metav1.CreateOptions{})
			},
			wantCount: 1,
		},
		{
			name:      "empty pool",
			poolName:  "nonexistent",
			setup:     func(_ *fake.Clientset) {},
			wantCount: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clientset := fake.NewClientset()
			tt.setup(clientset)

			cfg := &config.Config{
				KubeNamespace: "runs-fleet",
			}

			p := NewPoolProvider(clientset, cfg)
			runners, err := p.ListPoolRunners(context.Background(), tt.poolName)

			if (err != nil) != tt.wantErr {
				t.Errorf("ListPoolRunners() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if len(runners) != tt.wantCount {
				t.Errorf("ListPoolRunners() returned %d runners, want %d", len(runners), tt.wantCount)
			}
		})
	}
}

func TestPoolProvider_TerminateRunners(t *testing.T) {
	tests := []struct {
		name      string
		runnerIDs []string
		setup     func(*fake.Clientset)
		wantErr   bool
	}{
		{
			name:      "terminate existing pods",
			runnerIDs: []string{"runner-1", "runner-2"},
			setup: func(cs *fake.Clientset) {
				_, _ = cs.CoreV1().Pods("runs-fleet").Create(context.Background(), &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "runner-1",
						Namespace: "runs-fleet",
					},
				}, metav1.CreateOptions{})
				_, _ = cs.CoreV1().Pods("runs-fleet").Create(context.Background(), &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "runner-2",
						Namespace: "runs-fleet",
					},
				}, metav1.CreateOptions{})
			},
			wantErr: false,
		},
		{
			name:      "terminate nonexistent pods is ok",
			runnerIDs: []string{"nonexistent"},
			setup:     func(_ *fake.Clientset) {},
			wantErr:   false,
		},
		{
			name:      "empty list",
			runnerIDs: []string{},
			setup:     func(_ *fake.Clientset) {},
			wantErr:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clientset := fake.NewClientset()
			tt.setup(clientset)

			cfg := &config.Config{
				KubeNamespace: "runs-fleet",
			}

			p := NewPoolProvider(clientset, cfg)
			err := p.TerminateRunners(context.Background(), tt.runnerIDs)

			if (err != nil) != tt.wantErr {
				t.Errorf("TerminateRunners() error = %v, wantErr %v", err, tt.wantErr)
			}

			// Verify pods are deleted
			for _, id := range tt.runnerIDs {
				_, err := clientset.CoreV1().Pods("runs-fleet").Get(context.Background(), id, metav1.GetOptions{})
				if err == nil {
					t.Errorf("Pod %s should have been deleted", id)
				}
			}
		})
	}
}

func TestPoolProvider_StopRunners(t *testing.T) {
	clientset := fake.NewClientset()
	_, _ = clientset.CoreV1().Pods("runs-fleet").Create(context.Background(), &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "runner-1",
			Namespace: "runs-fleet",
		},
	}, metav1.CreateOptions{})

	cfg := &config.Config{
		KubeNamespace: "runs-fleet",
	}

	p := NewPoolProvider(clientset, cfg)

	// StopRunners should delete pods (K8s doesn't have stop)
	err := p.StopRunners(context.Background(), []string{"runner-1"})
	if err != nil {
		t.Errorf("StopRunners() error = %v", err)
	}

	// Verify pod is deleted
	_, err = clientset.CoreV1().Pods("runs-fleet").Get(context.Background(), "runner-1", metav1.GetOptions{})
	if err == nil {
		t.Error("Pod should have been deleted by StopRunners")
	}
}

func TestPoolProvider_StartRunners(t *testing.T) {
	clientset := fake.NewClientset()
	cfg := &config.Config{
		KubeNamespace: "runs-fleet",
	}

	p := NewPoolProvider(clientset, cfg)

	// StartRunners is a no-op for K8s
	err := p.StartRunners(context.Background(), []string{"runner-1"})
	if err != nil {
		t.Errorf("StartRunners() should be no-op, got error = %v", err)
	}
}

func TestPoolProvider_MarkRunnerBusyIdle(t *testing.T) {
	clientset := fake.NewClientset()
	cfg := &config.Config{
		KubeNamespace: "runs-fleet",
	}

	p := NewPoolProvider(clientset, cfg)

	// Mark runner idle
	p.MarkRunnerIdle("runner-1")

	p.mu.RLock()
	if _, ok := p.podIdle["runner-1"]; !ok {
		t.Error("MarkRunnerIdle should track runner in podIdle map")
	}
	p.mu.RUnlock()

	// Mark runner busy
	p.MarkRunnerBusy("runner-1")

	p.mu.RLock()
	if _, ok := p.podIdle["runner-1"]; ok {
		t.Error("MarkRunnerBusy should remove runner from podIdle map")
	}
	p.mu.RUnlock()
}

func TestPoolProvider_IdleTracking(t *testing.T) {
	clientset := fake.NewClientset()
	_, _ = clientset.CoreV1().Pods("runs-fleet").Create(context.Background(), &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "runner-1",
			Namespace: "runs-fleet",
			Labels: map[string]string{
				"app":                "runs-fleet-runner",
				"runs-fleet.io/pool": "default",
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
		},
	}, metav1.CreateOptions{})

	cfg := &config.Config{
		KubeNamespace:          "runs-fleet",
		KubeIdleTimeoutMinutes: 10,
	}

	p := NewPoolProvider(clientset, cfg)

	// Reconcile initializes idle tracking for running pods
	p.reconcile(context.Background())

	// ListPoolRunners should now show IdleSince
	runners, err := p.ListPoolRunners(context.Background(), "default")
	if err != nil {
		t.Fatalf("ListPoolRunners() error = %v", err)
	}

	if len(runners) != 1 {
		t.Fatalf("Expected 1 runner, got %d", len(runners))
	}

	if runners[0].IdleSince.IsZero() {
		t.Error("IdleSince should be set after reconcile")
	}

	firstIdleSince := runners[0].IdleSince

	// Mark busy removes from idle tracking
	p.MarkRunnerBusy("runner-1")

	p.mu.RLock()
	_, inMap := p.podIdle["runner-1"]
	p.mu.RUnlock()

	if inMap {
		t.Error("MarkRunnerBusy should remove runner from podIdle map")
	}

	// Subsequent list should not show IdleSince (pod is busy, not tracked)
	runners, err = p.ListPoolRunners(context.Background(), "default")
	if err != nil {
		t.Fatalf("ListPoolRunners() error = %v", err)
	}
	if !runners[0].IdleSince.IsZero() {
		t.Error("IdleSince should be zero for busy runner not in podIdle")
	}

	// Mark idle again
	p.MarkRunnerIdle("runner-1")

	runners, err = p.ListPoolRunners(context.Background(), "default")
	if err != nil {
		t.Fatalf("ListPoolRunners() error = %v", err)
	}
	if runners[0].IdleSince.Before(firstIdleSince) {
		t.Error("IdleSince should not go backwards after marking idle")
	}
}

func TestPoolProvider_Reconcile(t *testing.T) {
	tests := []struct {
		name            string
		podName         string
		idleDuration    time.Duration
		expectTerminate bool
	}{
		{
			name:            "terminates idle pods past threshold",
			podName:         "idle-runner",
			idleDuration:    15 * time.Minute, // threshold is 10 min
			expectTerminate: true,
		},
		{
			name:            "keeps recently idle pods",
			podName:         "recent-idle-runner",
			idleDuration:    5 * time.Minute, // within 10 min threshold
			expectTerminate: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clientset := fake.NewClientset()
			_, _ = clientset.CoreV1().Pods("runs-fleet").Create(context.Background(), &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      tt.podName,
					Namespace: "runs-fleet",
					Labels: map[string]string{
						"app":                "runs-fleet-runner",
						"runs-fleet.io/pool": "default",
					},
				},
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
				},
			}, metav1.CreateOptions{})

			cfg := &config.Config{KubeNamespace: "runs-fleet"}
			p := NewPoolProvider(clientset, cfg)

			p.mu.Lock()
			p.podIdle[tt.podName] = time.Now().Add(-tt.idleDuration)
			p.mu.Unlock()

			p.reconcile(context.Background())

			_, err := clientset.CoreV1().Pods("runs-fleet").Get(context.Background(), tt.podName, metav1.GetOptions{})
			terminated := err != nil

			if terminated != tt.expectTerminate {
				t.Errorf("pod terminated = %v, want %v", terminated, tt.expectTerminate)
			}
		})
	}
}

func TestPoolProvider_ReconcileCleansUpStalePodIdle(t *testing.T) {
	clientset := fake.NewClientset()

	cfg := &config.Config{
		KubeNamespace:          "runs-fleet",
		KubeIdleTimeoutMinutes: 10,
	}
	p := NewPoolProvider(clientset, cfg)

	// Add stale entries for pods that no longer exist
	p.mu.Lock()
	p.podIdle["deleted-pod-1"] = time.Now().Add(-20 * time.Minute)
	p.podIdle["deleted-pod-2"] = time.Now().Add(-30 * time.Minute)
	p.mu.Unlock()

	// Run reconcile - should clean up stale entries
	p.reconcile(context.Background())

	p.mu.RLock()
	defer p.mu.RUnlock()
	if len(p.podIdle) != 0 {
		t.Errorf("expected podIdle to be empty after cleanup, got %d entries", len(p.podIdle))
	}
}

func TestPoolProvider_ReconcileInitializesUntrackedPods(t *testing.T) {
	clientset := fake.NewClientset()
	// Create a running pod that is not tracked in podIdle
	_, _ = clientset.CoreV1().Pods("runs-fleet").Create(context.Background(), &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "untracked-runner",
			Namespace: "runs-fleet",
			Labels: map[string]string{
				"app":                "runs-fleet-runner",
				"runs-fleet.io/pool": "default",
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
		},
	}, metav1.CreateOptions{})

	cfg := &config.Config{
		KubeNamespace:          "runs-fleet",
		KubeIdleTimeoutMinutes: 10,
	}
	p := NewPoolProvider(clientset, cfg)

	// Verify pod is not tracked
	p.mu.RLock()
	_, tracked := p.podIdle["untracked-runner"]
	p.mu.RUnlock()
	if tracked {
		t.Fatal("pod should not be tracked before reconcile")
	}

	// Run reconcile - should initialize tracking for untracked pod
	p.reconcile(context.Background())

	// Verify pod is now tracked
	p.mu.RLock()
	idleSince, tracked := p.podIdle["untracked-runner"]
	p.mu.RUnlock()
	if !tracked {
		t.Error("pod should be tracked after reconcile")
	}
	if time.Since(idleSince) > time.Second {
		t.Error("idle time should be very recent")
	}

	// Verify pod is not terminated (just initialized)
	_, err := clientset.CoreV1().Pods("runs-fleet").Get(context.Background(), "untracked-runner", metav1.GetOptions{})
	if err != nil {
		t.Errorf("pod should not be terminated on first reconcile: %v", err)
	}
}

// Ensure PoolProvider implements provider.PoolProvider.
var _ = (*PoolProvider)(nil)
