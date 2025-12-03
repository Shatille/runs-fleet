package pools

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/Shavakan/runs-fleet/pkg/state"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"
)

const testNamespace = "runs-fleet"

type mockStateStore struct {
	pools map[string]*state.K8sPoolConfig
}

func newMockStateStore() *mockStateStore {
	return &mockStateStore{
		pools: make(map[string]*state.K8sPoolConfig),
	}
}

func (m *mockStateStore) GetK8sPoolConfig(_ context.Context, poolName string) (*state.K8sPoolConfig, error) {
	return m.pools[poolName], nil
}

func (m *mockStateStore) SaveK8sPoolConfig(_ context.Context, config *state.K8sPoolConfig) error {
	m.pools[config.PoolName] = config
	return nil
}

func (m *mockStateStore) ListK8sPools(_ context.Context) ([]string, error) {
	var names []string
	for name := range m.pools {
		names = append(names, name)
	}
	return names, nil
}

func (m *mockStateStore) UpdateK8sPoolState(_ context.Context, _ string, _, _ int) error {
	return nil
}

func (m *mockStateStore) DeleteK8sPoolConfig(_ context.Context, poolName string) error {
	delete(m.pools, poolName)
	return nil
}

type mockCoordinator struct {
	isLeader bool
}

func (m *mockCoordinator) IsLeader() bool {
	return m.isLeader
}

func createTestDeployment(name, namespace string, replicas int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(replicas),
		},
		Status: appsv1.DeploymentStatus{
			ReadyReplicas: replicas,
		},
	}
}

func TestK8sManager_ScalePool(t *testing.T) {
	ctx := context.Background()

	clientset := fake.NewSimpleClientset(
		createTestDeployment("runs-fleet-placeholder-arm64", testNamespace, 0),
		createTestDeployment("runs-fleet-placeholder-amd64", testNamespace, 0),
	)

	stateStore := newMockStateStore()
	cfg := &config.Config{
		KubeNamespace:   testNamespace,
		KubeReleaseName: "runs-fleet",
	}

	manager := NewK8sManager(clientset, stateStore, cfg)

	err := manager.CreatePool(ctx, DefaultPoolName, 0, 0)
	if err != nil {
		t.Fatalf("CreatePool failed: %v", err)
	}

	err = manager.ScalePool(ctx, DefaultPoolName, 3, 2)
	if err != nil {
		t.Fatalf("ScalePool failed: %v", err)
	}

	poolConfig, _ := stateStore.GetK8sPoolConfig(ctx, DefaultPoolName)
	if poolConfig.Arm64Replicas != 3 {
		t.Errorf("expected arm64 replicas 3, got %d", poolConfig.Arm64Replicas)
	}
	if poolConfig.Amd64Replicas != 2 {
		t.Errorf("expected amd64 replicas 2, got %d", poolConfig.Amd64Replicas)
	}

	arm64Deploy, err := clientset.AppsV1().Deployments(testNamespace).Get(ctx, "runs-fleet-placeholder-arm64", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get arm64 deployment: %v", err)
	}
	if *arm64Deploy.Spec.Replicas != 3 {
		t.Errorf("expected arm64 deployment replicas 3, got %d", *arm64Deploy.Spec.Replicas)
	}

	amd64Deploy, err := clientset.AppsV1().Deployments(testNamespace).Get(ctx, "runs-fleet-placeholder-amd64", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get amd64 deployment: %v", err)
	}
	if *amd64Deploy.Spec.Replicas != 2 {
		t.Errorf("expected amd64 deployment replicas 2, got %d", *amd64Deploy.Spec.Replicas)
	}
}

func TestK8sManager_CreatePool(t *testing.T) {
	ctx := context.Background()
	stateStore := newMockStateStore()
	cfg := &config.Config{
		KubeNamespace: testNamespace,
	}

	clientset := fake.NewSimpleClientset()
	manager := NewK8sManager(clientset, stateStore, cfg)

	err := manager.CreatePool(ctx, DefaultPoolName, 5, 3)
	if err != nil {
		t.Fatalf("CreatePool failed: %v", err)
	}

	poolConfig, _ := stateStore.GetK8sPoolConfig(ctx, DefaultPoolName)
	if poolConfig == nil {
		t.Fatal("pool config should exist")
	}
	if poolConfig.Arm64Replicas != 5 {
		t.Errorf("expected arm64 replicas 5, got %d", poolConfig.Arm64Replicas)
	}
	if poolConfig.Amd64Replicas != 3 {
		t.Errorf("expected amd64 replicas 3, got %d", poolConfig.Amd64Replicas)
	}
	if poolConfig.IdleTimeoutMinutes != 10 {
		t.Errorf("expected default idle timeout 10, got %d", poolConfig.IdleTimeoutMinutes)
	}

	err = manager.CreatePool(ctx, DefaultPoolName, 1, 1)
	if err == nil {
		t.Error("expected error when creating duplicate pool")
	}
}

func TestK8sManager_CreatePoolInvalidName(t *testing.T) {
	ctx := context.Background()
	stateStore := newMockStateStore()
	cfg := &config.Config{KubeNamespace: testNamespace}
	clientset := fake.NewSimpleClientset()
	manager := NewK8sManager(clientset, stateStore, cfg)

	err := manager.CreatePool(ctx, "custom-pool", 1, 1)
	if err == nil {
		t.Error("expected error for non-default pool name")
	}
	if err.Error() != `K8s warm pools only support pool name "default", got "custom-pool"` {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestK8sManager_CreatePoolNegativeReplicas(t *testing.T) {
	ctx := context.Background()
	stateStore := newMockStateStore()
	cfg := &config.Config{KubeNamespace: testNamespace}
	clientset := fake.NewSimpleClientset()
	manager := NewK8sManager(clientset, stateStore, cfg)

	err := manager.CreatePool(ctx, DefaultPoolName, -1, 1)
	if err == nil {
		t.Error("expected error for negative arm64 replicas")
	}

	err = manager.CreatePool(ctx, DefaultPoolName, 1, -1)
	if err == nil {
		t.Error("expected error for negative amd64 replicas")
	}
}

func TestK8sManager_DeletePool(t *testing.T) {
	ctx := context.Background()

	clientset := fake.NewSimpleClientset(
		createTestDeployment("runs-fleet-placeholder-arm64", testNamespace, 3),
		createTestDeployment("runs-fleet-placeholder-amd64", testNamespace, 2),
	)

	stateStore := newMockStateStore()
	stateStore.pools[DefaultPoolName] = &state.K8sPoolConfig{
		PoolName:      DefaultPoolName,
		Arm64Replicas: 3,
		Amd64Replicas: 2,
	}

	cfg := &config.Config{
		KubeNamespace:   testNamespace,
		KubeReleaseName: "runs-fleet",
	}

	manager := NewK8sManager(clientset, stateStore, cfg)

	err := manager.DeletePool(ctx, DefaultPoolName)
	if err != nil {
		t.Fatalf("DeletePool failed: %v", err)
	}

	poolConfig, _ := stateStore.GetK8sPoolConfig(ctx, DefaultPoolName)
	if poolConfig != nil {
		t.Error("expected pool config to be deleted")
	}

	arm64Deploy, _ := clientset.AppsV1().Deployments(testNamespace).Get(ctx, "runs-fleet-placeholder-arm64", metav1.GetOptions{})
	if *arm64Deploy.Spec.Replicas != 0 {
		t.Errorf("expected arm64 deployment scaled to 0, got %d", *arm64Deploy.Spec.Replicas)
	}
}

func TestK8sManager_DeletePoolInvalidName(t *testing.T) {
	ctx := context.Background()
	clientset := fake.NewSimpleClientset()
	stateStore := newMockStateStore()
	cfg := &config.Config{KubeNamespace: testNamespace}
	manager := NewK8sManager(clientset, stateStore, cfg)

	err := manager.DeletePool(ctx, "custom-pool")
	if err == nil {
		t.Error("expected error for non-default pool name")
	}
}

func TestK8sManager_SetSchedule(t *testing.T) {
	ctx := context.Background()
	stateStore := newMockStateStore()
	stateStore.pools[DefaultPoolName] = &state.K8sPoolConfig{
		PoolName:      DefaultPoolName,
		Arm64Replicas: 2,
		Amd64Replicas: 1,
	}

	cfg := &config.Config{
		KubeNamespace: testNamespace,
	}

	clientset := fake.NewSimpleClientset()
	manager := NewK8sManager(clientset, stateStore, cfg)

	schedules := []state.K8sPoolSchedule{
		{
			Name:          "business-hours",
			StartHour:     9,
			EndHour:       17,
			DaysOfWeek:    []int{1, 2, 3, 4, 5},
			Arm64Replicas: 5,
			Amd64Replicas: 3,
		},
	}

	err := manager.SetSchedule(ctx, DefaultPoolName, schedules)
	if err != nil {
		t.Fatalf("SetSchedule failed: %v", err)
	}

	poolConfig, _ := stateStore.GetK8sPoolConfig(ctx, DefaultPoolName)
	if len(poolConfig.Schedules) != 1 {
		t.Errorf("expected 1 schedule, got %d", len(poolConfig.Schedules))
	}
	if poolConfig.Schedules[0].Name != "business-hours" {
		t.Errorf("expected schedule name 'business-hours', got %q", poolConfig.Schedules[0].Name)
	}
}

func TestK8sManager_SetScheduleNonexistent(t *testing.T) {
	ctx := context.Background()
	stateStore := newMockStateStore()
	cfg := &config.Config{KubeNamespace: testNamespace}

	clientset := fake.NewSimpleClientset()
	manager := NewK8sManager(clientset, stateStore, cfg)

	err := manager.SetSchedule(ctx, DefaultPoolName, nil)
	if err == nil {
		t.Error("expected error when setting schedule on nonexistent pool")
	}
}

func TestK8sManager_SetScheduleInvalidName(t *testing.T) {
	ctx := context.Background()
	stateStore := newMockStateStore()
	cfg := &config.Config{KubeNamespace: testNamespace}
	clientset := fake.NewSimpleClientset()
	manager := NewK8sManager(clientset, stateStore, cfg)

	err := manager.SetSchedule(ctx, "custom-pool", nil)
	if err == nil {
		t.Error("expected error for non-default pool name")
	}
}

func TestK8sManager_SetScheduleInvalidHours(t *testing.T) {
	ctx := context.Background()
	stateStore := newMockStateStore()
	stateStore.pools[DefaultPoolName] = &state.K8sPoolConfig{
		PoolName: DefaultPoolName,
	}
	cfg := &config.Config{KubeNamespace: testNamespace}
	clientset := fake.NewSimpleClientset()
	manager := NewK8sManager(clientset, stateStore, cfg)

	tests := []struct {
		name      string
		schedules []state.K8sPoolSchedule
	}{
		{
			name: "invalid start hour",
			schedules: []state.K8sPoolSchedule{
				{StartHour: 25, EndHour: 17},
			},
		},
		{
			name: "negative start hour",
			schedules: []state.K8sPoolSchedule{
				{StartHour: -1, EndHour: 17},
			},
		},
		{
			name: "invalid end hour",
			schedules: []state.K8sPoolSchedule{
				{StartHour: 9, EndHour: 24},
			},
		},
		{
			name: "invalid day of week",
			schedules: []state.K8sPoolSchedule{
				{StartHour: 9, EndHour: 17, DaysOfWeek: []int{7}},
			},
		},
		{
			name: "negative day of week",
			schedules: []state.K8sPoolSchedule{
				{StartHour: 9, EndHour: 17, DaysOfWeek: []int{-1}},
			},
		},
		{
			name: "negative arm64 replicas",
			schedules: []state.K8sPoolSchedule{
				{StartHour: 9, EndHour: 17, Arm64Replicas: -1},
			},
		},
		{
			name: "negative amd64 replicas",
			schedules: []state.K8sPoolSchedule{
				{StartHour: 9, EndHour: 17, Amd64Replicas: -1},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := manager.SetSchedule(ctx, DefaultPoolName, tt.schedules)
			if err == nil {
				t.Error("expected validation error")
			}
		})
	}
}

func TestK8sManager_ScheduleMatching(t *testing.T) {
	manager := &K8sManager{}

	tests := []struct {
		name     string
		schedule state.K8sPoolSchedule
		hour     int
		day      time.Weekday
		expected bool
	}{
		{
			name: "within business hours",
			schedule: state.K8sPoolSchedule{
				StartHour:  9,
				EndHour:    17,
				DaysOfWeek: []int{1, 2, 3, 4, 5},
			},
			hour:     12,
			day:      time.Wednesday,
			expected: true,
		},
		{
			name: "outside business hours",
			schedule: state.K8sPoolSchedule{
				StartHour:  9,
				EndHour:    17,
				DaysOfWeek: []int{1, 2, 3, 4, 5},
			},
			hour:     20,
			day:      time.Wednesday,
			expected: false,
		},
		{
			name: "weekend day",
			schedule: state.K8sPoolSchedule{
				StartHour:  9,
				EndHour:    17,
				DaysOfWeek: []int{1, 2, 3, 4, 5},
			},
			hour:     12,
			day:      time.Saturday,
			expected: false,
		},
		{
			name: "overnight schedule - late night",
			schedule: state.K8sPoolSchedule{
				StartHour: 22,
				EndHour:   6,
			},
			hour:     23,
			day:      time.Monday,
			expected: true,
		},
		{
			name: "overnight schedule - early morning",
			schedule: state.K8sPoolSchedule{
				StartHour: 22,
				EndHour:   6,
			},
			hour:     3,
			day:      time.Monday,
			expected: true,
		},
		{
			name: "overnight schedule - outside",
			schedule: state.K8sPoolSchedule{
				StartHour: 22,
				EndHour:   6,
			},
			hour:     12,
			day:      time.Monday,
			expected: false,
		},
		{
			name: "no day restriction",
			schedule: state.K8sPoolSchedule{
				StartHour:  9,
				EndHour:    17,
				DaysOfWeek: nil,
			},
			hour:     12,
			day:      time.Sunday,
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := manager.scheduleMatches(tt.schedule, tt.hour, tt.day)
			if result != tt.expected {
				t.Errorf("scheduleMatches() = %v, expected %v", result, tt.expected)
			}
		})
	}
}

func TestK8sManager_GetScheduledDesiredCounts(t *testing.T) {
	manager := &K8sManager{}

	tests := []struct {
		name          string
		config        *state.K8sPoolConfig
		expectedArm64 int
		expectedAmd64 int
	}{
		{
			name: "no schedules - use defaults",
			config: &state.K8sPoolConfig{
				Arm64Replicas: 3,
				Amd64Replicas: 2,
			},
			expectedArm64: 3,
			expectedAmd64: 2,
		},
		{
			name: "with schedules - no match - use defaults",
			config: &state.K8sPoolConfig{
				Arm64Replicas: 3,
				Amd64Replicas: 2,
				Schedules: []state.K8sPoolSchedule{
					{
						StartHour:     25,
						EndHour:       26,
						Arm64Replicas: 10,
						Amd64Replicas: 10,
					},
				},
			},
			expectedArm64: 3,
			expectedAmd64: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			arm64, amd64 := manager.getScheduledDesiredCounts(tt.config)
			if arm64 != tt.expectedArm64 {
				t.Errorf("arm64 = %d, expected %d", arm64, tt.expectedArm64)
			}
			if amd64 != tt.expectedAmd64 {
				t.Errorf("amd64 = %d, expected %d", amd64, tt.expectedAmd64)
			}
		})
	}
}

func TestK8sManager_CoordinatorRespected(t *testing.T) {
	ctx := context.Background()

	clientset := fake.NewSimpleClientset(
		createTestDeployment("runs-fleet-placeholder-arm64", testNamespace, 1),
		createTestDeployment("runs-fleet-placeholder-amd64", testNamespace, 1),
	)

	stateStore := newMockStateStore()
	stateStore.pools[DefaultPoolName] = &state.K8sPoolConfig{
		PoolName:      DefaultPoolName,
		Arm64Replicas: 5,
		Amd64Replicas: 5,
	}

	cfg := &config.Config{
		KubeNamespace:   testNamespace,
		KubeReleaseName: "runs-fleet",
	}

	manager := NewK8sManager(clientset, stateStore, cfg)
	manager.SetCoordinator(&mockCoordinator{isLeader: false})

	manager.reconcile(ctx)

	arm64Deploy, _ := clientset.AppsV1().Deployments(testNamespace).Get(ctx, "runs-fleet-placeholder-arm64", metav1.GetOptions{})
	if *arm64Deploy.Spec.Replicas != 1 {
		t.Errorf("expected no change when not leader, got replicas %d", *arm64Deploy.Spec.Replicas)
	}
}

func TestK8sManager_PlaceholderDeploymentName(t *testing.T) {
	tests := []struct {
		releaseName string
		arch        string
		expected    string
	}{
		{
			releaseName: "my-release",
			arch:        "arm64",
			expected:    "my-release-placeholder-arm64",
		},
		{
			releaseName: "",
			arch:        "amd64",
			expected:    "-placeholder-amd64",
		},
	}

	for _, tt := range tests {
		t.Run(tt.expected, func(t *testing.T) {
			manager := &K8sManager{releaseName: tt.releaseName}
			result := manager.placeholderDeploymentName(tt.arch)
			if result != tt.expected {
				t.Errorf("got %q, expected %q", result, tt.expected)
			}
		})
	}
}

func TestK8sManager_GetPlaceholderStatus(t *testing.T) {
	ctx := context.Background()

	arm64Deploy := createTestDeployment("runs-fleet-placeholder-arm64", testNamespace, 3)
	arm64Deploy.Status.ReadyReplicas = 2

	clientset := fake.NewSimpleClientset(arm64Deploy)

	cfg := &config.Config{
		KubeNamespace:   testNamespace,
		KubeReleaseName: "runs-fleet",
	}

	manager := NewK8sManager(clientset, newMockStateStore(), cfg)

	arm64, amd64, err := manager.GetPlaceholderStatus(ctx)
	if err != nil {
		t.Fatalf("GetPlaceholderStatus failed: %v", err)
	}

	if arm64 == nil {
		t.Fatal("expected arm64 deployment")
	}
	if *arm64.Spec.Replicas != 3 {
		t.Errorf("expected arm64 replicas 3, got %d", *arm64.Spec.Replicas)
	}
	if arm64.Status.ReadyReplicas != 2 {
		t.Errorf("expected arm64 ready replicas 2, got %d", arm64.Status.ReadyReplicas)
	}

	if amd64 != nil {
		t.Error("expected amd64 deployment to be nil (not created)")
	}
}

func TestK8sManager_ReconcileWithLeader(t *testing.T) {
	ctx := context.Background()

	clientset := fake.NewSimpleClientset(
		createTestDeployment("runs-fleet-placeholder-arm64", testNamespace, 1),
		createTestDeployment("runs-fleet-placeholder-amd64", testNamespace, 1),
	)

	stateStore := newMockStateStore()
	stateStore.pools[DefaultPoolName] = &state.K8sPoolConfig{
		PoolName:      DefaultPoolName,
		Arm64Replicas: 3,
		Amd64Replicas: 2,
	}

	cfg := &config.Config{
		KubeNamespace:   testNamespace,
		KubeReleaseName: "runs-fleet",
	}

	manager := NewK8sManager(clientset, stateStore, cfg)
	manager.SetCoordinator(&mockCoordinator{isLeader: true})

	manager.reconcile(ctx)

	arm64Deploy, _ := clientset.AppsV1().Deployments(testNamespace).Get(ctx, "runs-fleet-placeholder-arm64", metav1.GetOptions{})
	if *arm64Deploy.Spec.Replicas != 3 {
		t.Errorf("expected arm64 replicas 3 when leader, got %d", *arm64Deploy.Spec.Replicas)
	}

	amd64Deploy, _ := clientset.AppsV1().Deployments(testNamespace).Get(ctx, "runs-fleet-placeholder-amd64", metav1.GetOptions{})
	if *amd64Deploy.Spec.Replicas != 2 {
		t.Errorf("expected amd64 replicas 2 when leader, got %d", *amd64Deploy.Spec.Replicas)
	}
}

func TestK8sManager_ReconcileNoCoordinator(t *testing.T) {
	ctx := context.Background()

	clientset := fake.NewSimpleClientset(
		createTestDeployment("runs-fleet-placeholder-arm64", testNamespace, 1),
		createTestDeployment("runs-fleet-placeholder-amd64", testNamespace, 1),
	)

	stateStore := newMockStateStore()
	stateStore.pools[DefaultPoolName] = &state.K8sPoolConfig{
		PoolName:      DefaultPoolName,
		Arm64Replicas: 5,
		Amd64Replicas: 5,
	}

	cfg := &config.Config{
		KubeNamespace:   testNamespace,
		KubeReleaseName: "runs-fleet",
	}

	manager := NewK8sManager(clientset, stateStore, cfg)
	// No coordinator set - should act as leader

	manager.reconcile(ctx)

	arm64Deploy, _ := clientset.AppsV1().Deployments(testNamespace).Get(ctx, "runs-fleet-placeholder-arm64", metav1.GetOptions{})
	if *arm64Deploy.Spec.Replicas != 5 {
		t.Errorf("expected arm64 replicas 5 with no coordinator, got %d", *arm64Deploy.Spec.Replicas)
	}
}

func TestK8sManager_ReconcilePoolNotConfigured(t *testing.T) {
	ctx := context.Background()

	clientset := fake.NewSimpleClientset(
		createTestDeployment("runs-fleet-placeholder-arm64", testNamespace, 1),
	)
	stateStore := newMockStateStore()
	// No pool config - reconcile should be a no-op

	cfg := &config.Config{
		KubeNamespace:   testNamespace,
		KubeReleaseName: "runs-fleet",
	}

	manager := NewK8sManager(clientset, stateStore, cfg)

	// Should not panic
	manager.reconcile(ctx)

	// Deployment should remain unchanged
	deploy, _ := clientset.AppsV1().Deployments(testNamespace).Get(ctx, "runs-fleet-placeholder-arm64", metav1.GetOptions{})
	if *deploy.Spec.Replicas != 1 {
		t.Errorf("expected deployment unchanged at 1 replica, got %d", *deploy.Spec.Replicas)
	}
}

func TestK8sManager_ScaleToZero(t *testing.T) {
	ctx := context.Background()

	clientset := fake.NewSimpleClientset(
		createTestDeployment("runs-fleet-placeholder-arm64", testNamespace, 5),
		createTestDeployment("runs-fleet-placeholder-amd64", testNamespace, 3),
	)

	stateStore := newMockStateStore()
	stateStore.pools[DefaultPoolName] = &state.K8sPoolConfig{
		PoolName:      DefaultPoolName,
		Arm64Replicas: 5,
		Amd64Replicas: 3,
	}

	cfg := &config.Config{
		KubeNamespace:   testNamespace,
		KubeReleaseName: "runs-fleet",
	}

	manager := NewK8sManager(clientset, stateStore, cfg)

	err := manager.ScalePool(ctx, DefaultPoolName, 0, 0)
	if err != nil {
		t.Fatalf("ScalePool to zero failed: %v", err)
	}

	arm64Deploy, _ := clientset.AppsV1().Deployments(testNamespace).Get(ctx, "runs-fleet-placeholder-arm64", metav1.GetOptions{})
	if *arm64Deploy.Spec.Replicas != 0 {
		t.Errorf("expected arm64 replicas 0, got %d", *arm64Deploy.Spec.Replicas)
	}

	amd64Deploy, _ := clientset.AppsV1().Deployments(testNamespace).Get(ctx, "runs-fleet-placeholder-amd64", metav1.GetOptions{})
	if *amd64Deploy.Spec.Replicas != 0 {
		t.Errorf("expected amd64 replicas 0, got %d", *amd64Deploy.Spec.Replicas)
	}
}

func TestK8sManager_ScalePoolInvalidName(t *testing.T) {
	ctx := context.Background()
	clientset := fake.NewSimpleClientset()
	stateStore := newMockStateStore()
	cfg := &config.Config{KubeNamespace: testNamespace}
	manager := NewK8sManager(clientset, stateStore, cfg)

	err := manager.ScalePool(ctx, "custom-pool", 1, 1)
	if err == nil {
		t.Error("expected error for non-default pool name")
	}
}

func TestK8sManager_ScalePoolNegativeReplicas(t *testing.T) {
	ctx := context.Background()
	clientset := fake.NewSimpleClientset()
	stateStore := newMockStateStore()
	cfg := &config.Config{KubeNamespace: testNamespace}
	manager := NewK8sManager(clientset, stateStore, cfg)

	err := manager.ScalePool(ctx, DefaultPoolName, -1, 1)
	if err == nil {
		t.Error("expected error for negative arm64 replicas")
	}

	err = manager.ScalePool(ctx, DefaultPoolName, 1, -1)
	if err == nil {
		t.Error("expected error for negative amd64 replicas")
	}
}

func TestK8sManager_ScalePoolCreateNew(t *testing.T) {
	ctx := context.Background()

	clientset := fake.NewSimpleClientset(
		createTestDeployment("runs-fleet-placeholder-arm64", testNamespace, 0),
		createTestDeployment("runs-fleet-placeholder-amd64", testNamespace, 0),
	)

	stateStore := newMockStateStore()
	// Pool doesn't exist yet

	cfg := &config.Config{
		KubeNamespace:   testNamespace,
		KubeReleaseName: "runs-fleet",
	}

	manager := NewK8sManager(clientset, stateStore, cfg)

	// ScalePool on non-existent pool should create it
	err := manager.ScalePool(ctx, DefaultPoolName, 2, 1)
	if err != nil {
		t.Fatalf("ScalePool on new pool failed: %v", err)
	}

	poolConfig, _ := stateStore.GetK8sPoolConfig(ctx, DefaultPoolName)
	if poolConfig == nil {
		t.Fatal("expected pool config to be created")
	}
	if poolConfig.Arm64Replicas != 2 {
		t.Errorf("expected arm64 replicas 2, got %d", poolConfig.Arm64Replicas)
	}
}

type errorStateStore struct {
	mockStateStore
	getError bool
}

func (e *errorStateStore) GetK8sPoolConfig(_ context.Context, poolName string) (*state.K8sPoolConfig, error) {
	if e.getError {
		return nil, fmt.Errorf("get error")
	}
	return e.mockStateStore.GetK8sPoolConfig(context.Background(), poolName)
}

func TestK8sManager_ReconcileGetConfigError(t *testing.T) {
	ctx := context.Background()

	clientset := fake.NewSimpleClientset(
		createTestDeployment("runs-fleet-placeholder-arm64", testNamespace, 1),
	)
	store := &errorStateStore{
		mockStateStore: mockStateStore{pools: map[string]*state.K8sPoolConfig{
			DefaultPoolName: {PoolName: DefaultPoolName},
		}},
		getError: true,
	}

	cfg := &config.Config{
		KubeNamespace:   testNamespace,
		KubeReleaseName: "runs-fleet",
	}

	manager := NewK8sManager(clientset, store, cfg)

	// Should not panic and deployment should remain unchanged
	manager.reconcile(ctx)

	deploy, _ := clientset.AppsV1().Deployments(testNamespace).Get(ctx, "runs-fleet-placeholder-arm64", metav1.GetOptions{})
	if *deploy.Spec.Replicas != 1 {
		t.Errorf("expected deployment unchanged at 1 replica, got %d", *deploy.Spec.Replicas)
	}
}

func TestK8sManager_GetCurrentReplicas(t *testing.T) {
	ctx := context.Background()

	clientset := fake.NewSimpleClientset(
		createTestDeployment("runs-fleet-placeholder-arm64", testNamespace, 3),
	)

	cfg := &config.Config{
		KubeNamespace:   testNamespace,
		KubeReleaseName: "runs-fleet",
	}

	manager := NewK8sManager(clientset, newMockStateStore(), cfg)

	replicas := manager.getCurrentReplicas(ctx, "runs-fleet-placeholder-arm64")
	if replicas != 3 {
		t.Errorf("expected 3 replicas, got %d", replicas)
	}

	// Non-existent deployment should return 0
	replicas = manager.getCurrentReplicas(ctx, "nonexistent")
	if replicas != 0 {
		t.Errorf("expected 0 replicas for nonexistent deployment, got %d", replicas)
	}
}

func TestK8sManager_ScaleDeploymentNoChange(t *testing.T) {
	ctx := context.Background()

	clientset := fake.NewSimpleClientset(
		createTestDeployment("runs-fleet-placeholder-arm64", testNamespace, 3),
	)

	cfg := &config.Config{
		KubeNamespace:   testNamespace,
		KubeReleaseName: "runs-fleet",
	}

	manager := NewK8sManager(clientset, newMockStateStore(), cfg)

	// Scaling to same value should be a no-op
	replicas, err := manager.scaleDeployment(ctx, "runs-fleet-placeholder-arm64", 3)
	if err != nil {
		t.Fatalf("scaleDeployment failed: %v", err)
	}
	if replicas != 3 {
		t.Errorf("expected 3 replicas, got %d", replicas)
	}
}

func TestK8sManager_ScaleDeploymentNotFound(t *testing.T) {
	ctx := context.Background()

	clientset := fake.NewSimpleClientset()

	cfg := &config.Config{
		KubeNamespace:   testNamespace,
		KubeReleaseName: "runs-fleet",
	}

	manager := NewK8sManager(clientset, newMockStateStore(), cfg)

	_, err := manager.scaleDeployment(ctx, "nonexistent", 3)
	if err == nil {
		t.Error("expected error for nonexistent deployment")
	}
}

func TestValidateReplicaCounts(t *testing.T) {
	tests := []struct {
		name    string
		arm64   int
		amd64   int
		wantErr bool
	}{
		{"valid zero", 0, 0, false},
		{"valid positive", 5, 3, false},
		{"valid at max", MaxReplicasPerArch, MaxReplicasPerArch, false},
		{"negative arm64", -1, 0, true},
		{"negative amd64", 0, -1, true},
		{"both negative", -1, -1, true},
		{"arm64 exceeds max", MaxReplicasPerArch + 1, 0, true},
		{"amd64 exceeds max", 0, MaxReplicasPerArch + 1, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateReplicaCounts(tt.arm64, tt.amd64)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateReplicaCounts() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestValidateSchedules(t *testing.T) {
	tests := []struct {
		name      string
		schedules []state.K8sPoolSchedule
		wantErr   bool
	}{
		{"nil schedules", nil, false},
		{"empty schedules", []state.K8sPoolSchedule{}, false},
		{"valid schedule", []state.K8sPoolSchedule{{StartHour: 9, EndHour: 17}}, false},
		{"valid at max replicas", []state.K8sPoolSchedule{{StartHour: 9, EndHour: 17, Arm64Replicas: MaxReplicasPerArch, Amd64Replicas: MaxReplicasPerArch}}, false},
		{"valid overnight schedule", []state.K8sPoolSchedule{{StartHour: 22, EndHour: 6}}, false},
		{"invalid start hour high", []state.K8sPoolSchedule{{StartHour: 24}}, true},
		{"invalid start hour negative", []state.K8sPoolSchedule{{StartHour: -1}}, true},
		{"invalid end hour", []state.K8sPoolSchedule{{EndHour: 24}}, true},
		{"start equals end", []state.K8sPoolSchedule{{StartHour: 9, EndHour: 9}}, true},
		{"invalid day of week high", []state.K8sPoolSchedule{{DaysOfWeek: []int{7}}}, true},
		{"invalid day of week negative", []state.K8sPoolSchedule{{DaysOfWeek: []int{-1}}}, true},
		{"negative arm64", []state.K8sPoolSchedule{{Arm64Replicas: -1}}, true},
		{"negative amd64", []state.K8sPoolSchedule{{Amd64Replicas: -1}}, true},
		{"arm64 exceeds max", []state.K8sPoolSchedule{{StartHour: 9, EndHour: 17, Arm64Replicas: MaxReplicasPerArch + 1}}, true},
		{"amd64 exceeds max", []state.K8sPoolSchedule{{StartHour: 9, EndHour: 17, Amd64Replicas: MaxReplicasPerArch + 1}}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateSchedules(tt.schedules)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateSchedules() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
