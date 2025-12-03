package pools

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/Shavakan/runs-fleet/pkg/state"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// DefaultPoolName is the single pool name for K8s warm pools.
// K8s warm pools operate on fixed Helm-created deployments (one arm64, one amd64),
// so multi-pool support is not available. Use "default" for all operations.
const DefaultPoolName = "default"

// K8sCoordinator defines distributed coordination operations.
type K8sCoordinator interface {
	IsLeader() bool
}

// K8sStateStore defines state operations for K8s pool configuration.
type K8sStateStore interface {
	GetK8sPoolConfig(ctx context.Context, poolName string) (*state.K8sPoolConfig, error)
	SaveK8sPoolConfig(ctx context.Context, config *state.K8sPoolConfig) error
	DeleteK8sPoolConfig(ctx context.Context, poolName string) error
	ListK8sPools(ctx context.Context) ([]string, error)
	UpdateK8sPoolState(ctx context.Context, poolName string, arm64Running, amd64Running int) error
}

// K8sManager orchestrates K8s warm pool operations by managing placeholder Deployments.
// Note: K8s warm pools support only a single pool ("default") because Helm creates
// fixed deployments (one per architecture). For multi-pool support, use EC2 backend.
type K8sManager struct {
	mu          sync.RWMutex
	clientset   kubernetes.Interface
	stateStore  K8sStateStore
	coordinator K8sCoordinator
	config      *config.Config

	// Deployment naming
	releaseName string // Helm release name for deployment naming
}

// NewK8sManager creates a K8s pool manager.
func NewK8sManager(clientset kubernetes.Interface, stateStore K8sStateStore, cfg *config.Config) *K8sManager {
	releaseName := cfg.KubeReleaseName
	if releaseName == "" {
		releaseName = "runs-fleet"
	}
	return &K8sManager{
		clientset:   clientset,
		stateStore:  stateStore,
		config:      cfg,
		releaseName: releaseName,
	}
}

// SetCoordinator sets the coordinator for leader election.
func (m *K8sManager) SetCoordinator(coordinator K8sCoordinator) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.coordinator = coordinator
}

// ReconcileLoop runs periodically to maintain pool size.
func (m *K8sManager) ReconcileLoop(ctx context.Context) {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()

	m.reconcile(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.reconcile(ctx)
		}
	}
}

// reconcileTimeout bounds K8s API call duration to prevent indefinite hangs.
const reconcileTimeout = 30 * time.Second

func (m *K8sManager) reconcile(ctx context.Context) {
	// Hold RLock for entire operation to prevent TOCTOU race between leader check and K8s API calls.
	// RLock blocks SetCoordinator but allows concurrent reads - acceptable tradeoff.
	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.coordinator != nil && !m.coordinator.IsLeader() {
		return
	}

	// Timeout context prevents indefinite hangs on unresponsive K8s API.
	timeoutCtx, cancel := context.WithTimeout(ctx, reconcileTimeout)
	defer cancel()

	// Only reconcile the default pool (K8s has fixed deployments from Helm)
	if err := m.reconcilePool(timeoutCtx, DefaultPoolName); err != nil {
		log.Printf("K8s pool manager: failed to reconcile pool %s: %v", DefaultPoolName, err)
	}
}

func (m *K8sManager) reconcilePool(ctx context.Context, poolName string) error {
	poolConfig, err := m.stateStore.GetK8sPoolConfig(ctx, poolName)
	if err != nil {
		return fmt.Errorf("failed to get pool config: %w", err)
	}
	if poolConfig == nil {
		// No config yet, nothing to do
		return nil
	}

	desiredArm64, desiredAmd64 := m.getScheduledDesiredCounts(poolConfig)

	arm64DeployName := m.placeholderDeploymentName("arm64")
	amd64DeployName := m.placeholderDeploymentName("amd64")

	// Track actual counts, querying current state on error
	actualArm64 := m.scaleOrGetCurrent(ctx, arm64DeployName, int32(desiredArm64))
	actualAmd64 := m.scaleOrGetCurrent(ctx, amd64DeployName, int32(desiredAmd64))

	log.Printf("K8s pool %s: desired arm64=%d amd64=%d, actual arm64=%d amd64=%d",
		poolName, desiredArm64, desiredAmd64, actualArm64, actualAmd64)

	if err := m.stateStore.UpdateK8sPoolState(ctx, poolName, actualArm64, actualAmd64); err != nil {
		log.Printf("K8s pool manager: failed to update pool state: %v", err)
	}

	return nil
}

// scaleOrGetCurrent scales a deployment and returns actual replicas. On error, queries current state.
func (m *K8sManager) scaleOrGetCurrent(ctx context.Context, deployName string, replicas int32) int {
	actual, err := m.scaleDeployment(ctx, deployName, replicas)
	if err != nil {
		log.Printf("K8s pool manager: failed to scale %s: %v", deployName, err)
		// Query current state to report accurate metrics
		actual = m.getCurrentReplicas(ctx, deployName)
	}
	return actual
}

// getCurrentReplicas queries the current ready replica count for a deployment.
func (m *K8sManager) getCurrentReplicas(ctx context.Context, deployName string) int {
	namespace := m.config.KubeNamespace
	deploy, err := m.clientset.AppsV1().Deployments(namespace).Get(ctx, deployName, metav1.GetOptions{})
	if err != nil {
		return 0
	}
	return int(deploy.Status.ReadyReplicas)
}

// getScheduledDesiredCounts returns the desired replica counts based on schedule.
func (m *K8sManager) getScheduledDesiredCounts(poolConfig *state.K8sPoolConfig) (arm64, amd64 int) {
	if len(poolConfig.Schedules) == 0 {
		return poolConfig.Arm64Replicas, poolConfig.Amd64Replicas
	}

	now := time.Now()
	currentHour := now.Hour()
	currentDay := now.Weekday()

	for _, schedule := range poolConfig.Schedules {
		if m.scheduleMatches(schedule, currentHour, currentDay) {
			return schedule.Arm64Replicas, schedule.Amd64Replicas
		}
	}

	return poolConfig.Arm64Replicas, poolConfig.Amd64Replicas
}

// scheduleMatches checks if a schedule applies to the current time.
func (m *K8sManager) scheduleMatches(schedule state.K8sPoolSchedule, hour int, day time.Weekday) bool {
	if len(schedule.DaysOfWeek) > 0 {
		dayMatch := false
		for _, d := range schedule.DaysOfWeek {
			if d == int(day) {
				dayMatch = true
				break
			}
		}
		if !dayMatch {
			return false
		}
	}

	if schedule.StartHour <= schedule.EndHour {
		return hour >= schedule.StartHour && hour < schedule.EndHour
	}
	return hour >= schedule.StartHour || hour < schedule.EndHour
}

func (m *K8sManager) placeholderDeploymentName(arch string) string {
	return m.releaseName + "-placeholder-" + arch
}

// scaleDeployment scales a placeholder deployment and returns actual ready replicas.
// No explicit retry logic: the 60s reconcile loop provides automatic retry for transient failures.
func (m *K8sManager) scaleDeployment(ctx context.Context, deployName string, replicas int32) (int, error) {
	namespace := m.config.KubeNamespace

	deploy, err := m.clientset.AppsV1().Deployments(namespace).Get(ctx, deployName, metav1.GetOptions{})
	if err != nil {
		return 0, fmt.Errorf("failed to get deployment %s: %w", deployName, err)
	}

	if deploy.Spec.Replicas != nil && *deploy.Spec.Replicas == replicas {
		return int(deploy.Status.ReadyReplicas), nil
	}

	deploy.Spec.Replicas = &replicas
	updated, err := m.clientset.AppsV1().Deployments(namespace).Update(ctx, deploy, metav1.UpdateOptions{})
	if err != nil {
		return 0, fmt.Errorf("failed to update deployment %s: %w", deployName, err)
	}

	log.Printf("K8s pool manager: scaled deployment %s to %d replicas", deployName, replicas)
	return int(updated.Status.ReadyReplicas), nil
}

// GetPlaceholderStatus returns current status of placeholder deployments.
// Returns (nil, nil, nil) if deployments don't exist (warm pool not configured).
// Returns error if K8s API call fails for reasons other than NotFound.
func (m *K8sManager) GetPlaceholderStatus(ctx context.Context) (arm64, amd64 *appsv1.Deployment, err error) {
	namespace := m.config.KubeNamespace

	arm64Deploy, arm64Err := m.clientset.AppsV1().Deployments(namespace).Get(
		ctx, m.placeholderDeploymentName("arm64"), metav1.GetOptions{})
	if arm64Err != nil {
		if !errors.IsNotFound(arm64Err) {
			return nil, nil, fmt.Errorf("failed to get arm64 deployment: %w", arm64Err)
		}
		arm64Deploy = nil
	}

	amd64Deploy, amd64Err := m.clientset.AppsV1().Deployments(namespace).Get(
		ctx, m.placeholderDeploymentName("amd64"), metav1.GetOptions{})
	if amd64Err != nil {
		if !errors.IsNotFound(amd64Err) {
			return nil, nil, fmt.Errorf("failed to get amd64 deployment: %w", amd64Err)
		}
		amd64Deploy = nil
	}

	return arm64Deploy, amd64Deploy, nil
}

// ScalePool immediately scales the pool to specified replica counts.
// Note: poolName must be DefaultPoolName ("default") for K8s warm pools.
func (m *K8sManager) ScalePool(ctx context.Context, poolName string, arm64Replicas, amd64Replicas int) error {
	if err := m.validatePoolName(poolName); err != nil {
		return err
	}
	if err := validateReplicaCounts(arm64Replicas, amd64Replicas); err != nil {
		return err
	}

	poolConfig, err := m.stateStore.GetK8sPoolConfig(ctx, poolName)
	if err != nil {
		return fmt.Errorf("failed to get pool config: %w", err)
	}
	if poolConfig == nil {
		poolConfig = &state.K8sPoolConfig{PoolName: poolName}
	}

	poolConfig.Arm64Replicas = arm64Replicas
	poolConfig.Amd64Replicas = amd64Replicas

	if err := m.stateStore.SaveK8sPoolConfig(ctx, poolConfig); err != nil {
		return fmt.Errorf("failed to save pool config: %w", err)
	}

	return m.reconcilePool(ctx, poolName)
}

// SetSchedule sets time-based scaling schedule for the pool.
// Note: poolName must be DefaultPoolName ("default") for K8s warm pools.
func (m *K8sManager) SetSchedule(ctx context.Context, poolName string, schedules []state.K8sPoolSchedule) error {
	if err := m.validatePoolName(poolName); err != nil {
		return err
	}
	if err := validateSchedules(schedules); err != nil {
		return err
	}

	poolConfig, err := m.stateStore.GetK8sPoolConfig(ctx, poolName)
	if err != nil {
		return fmt.Errorf("failed to get pool config: %w", err)
	}
	if poolConfig == nil {
		return fmt.Errorf("pool %s not found", poolName)
	}

	poolConfig.Schedules = schedules

	if err := m.stateStore.SaveK8sPoolConfig(ctx, poolConfig); err != nil {
		return fmt.Errorf("failed to save pool config: %w", err)
	}

	return nil
}

// CreatePool creates a new pool configuration.
// Note: poolName must be DefaultPoolName ("default") for K8s warm pools.
func (m *K8sManager) CreatePool(ctx context.Context, poolName string, arm64Replicas, amd64Replicas int) error {
	if err := m.validatePoolName(poolName); err != nil {
		return err
	}
	if err := validateReplicaCounts(arm64Replicas, amd64Replicas); err != nil {
		return err
	}

	existing, err := m.stateStore.GetK8sPoolConfig(ctx, poolName)
	if err != nil {
		return fmt.Errorf("failed to check existing pool: %w", err)
	}
	if existing != nil {
		return fmt.Errorf("pool %s already exists", poolName)
	}

	poolConfig := &state.K8sPoolConfig{
		PoolName:           poolName,
		Arm64Replicas:      arm64Replicas,
		Amd64Replicas:      amd64Replicas,
		IdleTimeoutMinutes: 10,
	}

	if err := m.stateStore.SaveK8sPoolConfig(ctx, poolConfig); err != nil {
		return fmt.Errorf("failed to create pool config: %w", err)
	}

	return nil
}

// DeletePool removes the pool configuration and scales deployments to zero.
// Note: poolName must be DefaultPoolName ("default") for K8s warm pools.
func (m *K8sManager) DeletePool(ctx context.Context, poolName string) error {
	if err := m.validatePoolName(poolName); err != nil {
		return err
	}

	// Scale down before deleting config
	if _, err := m.scaleDeployment(ctx, m.placeholderDeploymentName("arm64"), 0); err != nil {
		log.Printf("K8s pool manager: failed to scale down arm64 on delete: %v", err)
	}
	if _, err := m.scaleDeployment(ctx, m.placeholderDeploymentName("amd64"), 0); err != nil {
		log.Printf("K8s pool manager: failed to scale down amd64 on delete: %v", err)
	}

	return m.stateStore.DeleteK8sPoolConfig(ctx, poolName)
}

// validatePoolName ensures only the default pool is used.
func (m *K8sManager) validatePoolName(poolName string) error {
	if poolName != DefaultPoolName {
		return fmt.Errorf("K8s warm pools only support pool name %q, got %q", DefaultPoolName, poolName)
	}
	return nil
}

// MaxReplicasPerArch limits placeholder replicas to prevent cluster resource exhaustion.
const MaxReplicasPerArch = 100

// validateReplicaCounts ensures replica counts are within valid range.
func validateReplicaCounts(arm64, amd64 int) error {
	if arm64 < 0 {
		return fmt.Errorf("arm64 replicas must be non-negative, got %d", arm64)
	}
	if amd64 < 0 {
		return fmt.Errorf("amd64 replicas must be non-negative, got %d", amd64)
	}
	if arm64 > MaxReplicasPerArch {
		return fmt.Errorf("arm64 replicas exceeds max %d, got %d", MaxReplicasPerArch, arm64)
	}
	if amd64 > MaxReplicasPerArch {
		return fmt.Errorf("amd64 replicas exceeds max %d, got %d", MaxReplicasPerArch, amd64)
	}
	return nil
}

// validateSchedules ensures schedule parameters are valid.
func validateSchedules(schedules []state.K8sPoolSchedule) error {
	for i, s := range schedules {
		if s.StartHour < 0 || s.StartHour > 23 {
			return fmt.Errorf("schedule[%d]: start_hour must be 0-23, got %d", i, s.StartHour)
		}
		if s.EndHour < 0 || s.EndHour > 23 {
			return fmt.Errorf("schedule[%d]: end_hour must be 0-23, got %d", i, s.EndHour)
		}
		if s.StartHour == s.EndHour {
			return fmt.Errorf("schedule[%d]: start_hour cannot equal end_hour (would never match)", i)
		}
		for j, day := range s.DaysOfWeek {
			if day < 0 || day > 6 {
				return fmt.Errorf("schedule[%d].days_of_week[%d]: must be 0-6, got %d", i, j, day)
			}
		}
		if s.Arm64Replicas < 0 {
			return fmt.Errorf("schedule[%d]: arm64_replicas must be non-negative, got %d", i, s.Arm64Replicas)
		}
		if s.Amd64Replicas < 0 {
			return fmt.Errorf("schedule[%d]: amd64_replicas must be non-negative, got %d", i, s.Amd64Replicas)
		}
		if s.Arm64Replicas > MaxReplicasPerArch {
			return fmt.Errorf("schedule[%d]: arm64_replicas exceeds max %d, got %d", i, MaxReplicasPerArch, s.Arm64Replicas)
		}
		if s.Amd64Replicas > MaxReplicasPerArch {
			return fmt.Errorf("schedule[%d]: amd64_replicas exceeds max %d, got %d", i, MaxReplicasPerArch, s.Amd64Replicas)
		}
	}
	return nil
}
