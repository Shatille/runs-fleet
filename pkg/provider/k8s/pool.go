package k8s

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/Shavakan/runs-fleet/pkg/provider"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// defaultIdleTimeoutMinutes is used when config value is zero.
const defaultIdleTimeoutMinutes = 10

// Coordinator defines distributed coordination operations.
type Coordinator interface {
	IsLeader() bool
}

// PoolProvider implements provider.PoolProvider for Kubernetes.
// K8s pools use labeled pods; "stopped" state is not supported (pods are running or terminated).
type PoolProvider struct {
	mu          sync.RWMutex
	clientset   kubernetes.Interface
	config      *config.Config
	coordinator Coordinator
	podIdle     map[string]time.Time // tracks when pods became idle
}

// NewPoolProvider creates a K8s pool provider.
func NewPoolProvider(clientset kubernetes.Interface, appConfig *config.Config) *PoolProvider {
	return &PoolProvider{
		clientset: clientset,
		config:    appConfig,
		podIdle:   make(map[string]time.Time),
	}
}

// ListPoolRunners returns all runner pods in a pool.
func (p *PoolProvider) ListPoolRunners(ctx context.Context, poolName string) ([]provider.PoolRunner, error) {
	namespace := p.config.KubeNamespace

	// List pods with pool label
	labelSelector := fmt.Sprintf("runs-fleet.io/pool=%s,app=runs-fleet-runner", poolName)
	pods, err := p.clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list pool pods: %w", err)
	}

	var runners []provider.PoolRunner
	for _, pod := range pods.Items {
		// Skip terminated pods
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}

		runner := provider.PoolRunner{
			RunnerID:     pod.Name,
			State:        mapPodPhase(pod.Status.Phase),
			InstanceType: pod.Labels["runs-fleet.io/instance-type"],
		}

		if pod.Status.StartTime != nil {
			runner.LaunchTime = pod.Status.StartTime.Time
		} else {
			runner.LaunchTime = pod.CreationTimestamp.Time
		}

		runners = append(runners, runner)
	}

	// Apply idle tracking (read-only, reconcile handles initialization)
	p.mu.RLock()
	defer p.mu.RUnlock()

	for i := range runners {
		if idleSince, ok := p.podIdle[runners[i].RunnerID]; ok {
			runners[i].IdleSince = idleSince
		}
	}

	return runners, nil
}

// StartRunners is a no-op for K8s - pods cannot be "started" like EC2 instances.
// In K8s, to add capacity, create new pods instead.
func (p *PoolProvider) StartRunners(_ context.Context, _ []string) error {
	// K8s pods don't have a stopped state that can be resumed.
	// Pool scaling is handled by creating new pods, not starting existing ones.
	return nil
}

// StopRunners deletes pods. K8s doesn't support stopping pods like EC2.
// For warm pool management, use Deployment scaling instead.
func (p *PoolProvider) StopRunners(ctx context.Context, runnerIDs []string) error {
	if len(runnerIDs) == 0 {
		return nil
	}

	namespace := p.config.KubeNamespace
	var failCount int
	var lastErr error

	for _, podName := range runnerIDs {
		err := p.clientset.CoreV1().Pods(namespace).Delete(ctx, podName, metav1.DeleteOptions{})
		if err != nil && !errors.IsNotFound(err) {
			failCount++
			lastErr = err
		} else {
			p.mu.Lock()
			delete(p.podIdle, podName)
			p.mu.Unlock()
		}
	}

	if lastErr != nil {
		return fmt.Errorf("failed to delete %d pod(s): %w", failCount, lastErr)
	}
	return nil
}

// TerminateRunners deletes pods from the cluster.
func (p *PoolProvider) TerminateRunners(ctx context.Context, runnerIDs []string) error {
	if len(runnerIDs) == 0 {
		return nil
	}

	namespace := p.config.KubeNamespace
	var failCount int
	var lastErr error

	for _, podName := range runnerIDs {
		err := p.clientset.CoreV1().Pods(namespace).Delete(ctx, podName, metav1.DeleteOptions{})
		if err != nil && !errors.IsNotFound(err) {
			failCount++
			lastErr = err
		} else {
			p.mu.Lock()
			delete(p.podIdle, podName)
			p.mu.Unlock()
		}
	}

	if lastErr != nil {
		return fmt.Errorf("failed to terminate %d pod(s): %w", failCount, lastErr)
	}
	return nil
}

// MarkRunnerBusy marks a runner as busy (has an assigned job).
func (p *PoolProvider) MarkRunnerBusy(runnerID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.podIdle, runnerID)
}

// MarkRunnerIdle marks a runner as idle (no assigned job).
func (p *PoolProvider) MarkRunnerIdle(runnerID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.podIdle[runnerID] = time.Now()
}

// SetCoordinator sets the coordinator for leader election.
func (p *PoolProvider) SetCoordinator(coordinator Coordinator) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.coordinator = coordinator
}

// ReconcileLoop runs periodically to terminate idle pods.
func (p *PoolProvider) ReconcileLoop(ctx context.Context) {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()

	// Initial reconciliation
	p.reconcile(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.reconcile(ctx)
		}
	}
}

func (p *PoolProvider) reconcile(ctx context.Context) {
	// Hold lock for entire reconcile to prevent race where pod becomes busy
	// after termination decision but before delete. Blocks MarkRunnerBusy,
	// MarkRunnerIdle, ListPoolRunners during pod termination (50-500ms typical).
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.coordinator != nil && !p.coordinator.IsLeader() {
		return
	}

	namespace := p.config.KubeNamespace

	// List all runner pods (not filtered by pool to catch all pools)
	pods, err := p.clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app=runs-fleet-runner",
	})
	if err != nil {
		log.Printf("K8s reconcile: failed to list pods: %v", err)
		return
	}

	// Build set of existing pod names for cleanup
	existingPods := make(map[string]bool)
	for _, pod := range pods.Items {
		existingPods[pod.Name] = true
	}

	// Clean up stale entries from podIdle map (memory leak fix)
	for podName := range p.podIdle {
		if !existingPods[podName] {
			delete(p.podIdle, podName)
		}
	}

	idleTimeout := p.config.KubeIdleTimeoutMinutes
	if idleTimeout <= 0 {
		idleTimeout = defaultIdleTimeoutMinutes
	}
	threshold := time.Now().Add(-time.Duration(idleTimeout) * time.Minute)
	var toTerminate []string

	for _, pod := range pods.Items {
		// Skip non-running pods
		if pod.Status.Phase != corev1.PodRunning {
			continue
		}

		// Initialize idle tracking for untracked running pods
		if _, ok := p.podIdle[pod.Name]; !ok {
			p.podIdle[pod.Name] = time.Now()
			continue // Don't terminate newly tracked pods
		}

		// Check if pod is idle and past threshold
		if idleSince := p.podIdle[pod.Name]; idleSince.Before(threshold) {
			toTerminate = append(toTerminate, pod.Name)
		}
	}

	if len(toTerminate) == 0 {
		return
	}

	log.Printf("K8s reconcile: terminating %d idle pod(s)", len(toTerminate))

	// Terminate pods and clean up tracking (under same lock to prevent race)
	for _, podName := range toTerminate {
		err := p.clientset.CoreV1().Pods(namespace).Delete(ctx, podName, metav1.DeleteOptions{})
		if err != nil && !errors.IsNotFound(err) {
			log.Printf("K8s reconcile: failed to delete pod %s: %v", podName, err)
		} else {
			delete(p.podIdle, podName)
		}
	}
}

// Ensure PoolProvider implements provider.PoolProvider.
var _ provider.PoolProvider = (*PoolProvider)(nil)
