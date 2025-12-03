package state

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func setupTestValkey(t *testing.T) (*ValkeyStateStore, *miniredis.Miniredis) {
	t.Helper()

	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("failed to start miniredis: %v", err)
	}

	client := redis.NewClient(&redis.Options{
		Addr: mr.Addr(),
	})

	store := NewValkeyStateStoreWithClient(client, "test:")
	return store, mr
}

func TestValkeyStateStore_SaveAndGetPoolConfig(t *testing.T) {
	store, mr := setupTestValkey(t)
	defer mr.Close()

	ctx := context.Background()

	config := &K8sPoolConfig{
		PoolName:           "default",
		Arm64Replicas:      3,
		Amd64Replicas:      2,
		IdleTimeoutMinutes: 15,
		Resources: K8sResourceRequests{
			CPU:    "2",
			Memory: "4Gi",
		},
	}

	err := store.SaveK8sPoolConfig(ctx, config)
	if err != nil {
		t.Fatalf("SaveK8sPoolConfig failed: %v", err)
	}

	retrieved, err := store.GetK8sPoolConfig(ctx, "default")
	if err != nil {
		t.Fatalf("GetK8sPoolConfig failed: %v", err)
	}

	if retrieved == nil {
		t.Fatal("retrieved config is nil")
	}
	if retrieved.PoolName != "default" {
		t.Errorf("expected pool name 'default', got %q", retrieved.PoolName)
	}
	if retrieved.Arm64Replicas != 3 {
		t.Errorf("expected arm64 replicas 3, got %d", retrieved.Arm64Replicas)
	}
	if retrieved.Amd64Replicas != 2 {
		t.Errorf("expected amd64 replicas 2, got %d", retrieved.Amd64Replicas)
	}
	if retrieved.IdleTimeoutMinutes != 15 {
		t.Errorf("expected idle timeout 15, got %d", retrieved.IdleTimeoutMinutes)
	}
	if retrieved.UpdatedAt.IsZero() {
		t.Error("expected UpdatedAt to be set")
	}
}

func TestValkeyStateStore_GetNonexistentPool(t *testing.T) {
	store, mr := setupTestValkey(t)
	defer mr.Close()

	ctx := context.Background()

	config, err := store.GetK8sPoolConfig(ctx, "nonexistent")
	if err != nil {
		t.Fatalf("GetK8sPoolConfig failed: %v", err)
	}
	if config != nil {
		t.Error("expected nil config for nonexistent pool")
	}
}

func TestValkeyStateStore_ListPools(t *testing.T) {
	store, mr := setupTestValkey(t)
	defer mr.Close()

	ctx := context.Background()

	configs := []*K8sPoolConfig{
		{PoolName: "pool-a", Arm64Replicas: 1},
		{PoolName: "pool-b", Amd64Replicas: 2},
		{PoolName: "pool-c", Arm64Replicas: 1, Amd64Replicas: 1},
	}

	for _, cfg := range configs {
		if err := store.SaveK8sPoolConfig(ctx, cfg); err != nil {
			t.Fatalf("SaveK8sPoolConfig failed: %v", err)
		}
	}

	pools, err := store.ListK8sPools(ctx)
	if err != nil {
		t.Fatalf("ListK8sPools failed: %v", err)
	}

	if len(pools) != 3 {
		t.Errorf("expected 3 pools, got %d", len(pools))
	}

	poolSet := make(map[string]bool)
	for _, p := range pools {
		poolSet[p] = true
	}

	for _, expected := range []string{"pool-a", "pool-b", "pool-c"} {
		if !poolSet[expected] {
			t.Errorf("expected pool %q in list", expected)
		}
	}
}

func TestValkeyStateStore_DeletePool(t *testing.T) {
	store, mr := setupTestValkey(t)
	defer mr.Close()

	ctx := context.Background()

	config := &K8sPoolConfig{
		PoolName:      "to-delete",
		Arm64Replicas: 1,
	}

	if err := store.SaveK8sPoolConfig(ctx, config); err != nil {
		t.Fatalf("SaveK8sPoolConfig failed: %v", err)
	}

	retrieved, _ := store.GetK8sPoolConfig(ctx, "to-delete")
	if retrieved == nil {
		t.Fatal("expected config to exist before delete")
	}

	if err := store.DeleteK8sPoolConfig(ctx, "to-delete"); err != nil {
		t.Fatalf("DeleteK8sPoolConfig failed: %v", err)
	}

	retrieved, _ = store.GetK8sPoolConfig(ctx, "to-delete")
	if retrieved != nil {
		t.Error("expected config to be deleted")
	}

	pools, _ := store.ListK8sPools(ctx)
	for _, p := range pools {
		if p == "to-delete" {
			t.Error("expected pool to be removed from index")
		}
	}
}

func TestValkeyStateStore_PoolWithSchedules(t *testing.T) {
	store, mr := setupTestValkey(t)
	defer mr.Close()

	ctx := context.Background()

	config := &K8sPoolConfig{
		PoolName:      "scheduled",
		Arm64Replicas: 2,
		Amd64Replicas: 1,
		Schedules: []K8sPoolSchedule{
			{
				Name:          "business-hours",
				StartHour:     9,
				EndHour:       17,
				DaysOfWeek:    []int{1, 2, 3, 4, 5},
				Arm64Replicas: 5,
				Amd64Replicas: 3,
			},
			{
				Name:          "weekend",
				StartHour:     0,
				EndHour:       24,
				DaysOfWeek:    []int{0, 6},
				Arm64Replicas: 0,
				Amd64Replicas: 0,
			},
		},
	}

	if err := store.SaveK8sPoolConfig(ctx, config); err != nil {
		t.Fatalf("SaveK8sPoolConfig failed: %v", err)
	}

	retrieved, err := store.GetK8sPoolConfig(ctx, "scheduled")
	if err != nil {
		t.Fatalf("GetK8sPoolConfig failed: %v", err)
	}

	if len(retrieved.Schedules) != 2 {
		t.Errorf("expected 2 schedules, got %d", len(retrieved.Schedules))
	}
	if retrieved.Schedules[0].Name != "business-hours" {
		t.Errorf("expected schedule name 'business-hours', got %q", retrieved.Schedules[0].Name)
	}
	if retrieved.Schedules[0].Arm64Replicas != 5 {
		t.Errorf("expected business hours arm64 replicas 5, got %d", retrieved.Schedules[0].Arm64Replicas)
	}
}

func TestValkeyStateStore_UpdatePoolState(t *testing.T) {
	store, mr := setupTestValkey(t)
	defer mr.Close()

	ctx := context.Background()

	err := store.UpdateK8sPoolState(ctx, "test-pool", 3, 2)
	if err != nil {
		t.Fatalf("UpdateK8sPoolState failed: %v", err)
	}

	stateKey := "test:pool-state:test-pool"
	exists := mr.Exists(stateKey)
	if !exists {
		t.Error("expected pool state key to exist")
	}
}

func TestValkeyStateStore_ValidationErrors(t *testing.T) {
	store, mr := setupTestValkey(t)
	defer mr.Close()

	ctx := context.Background()

	_, err := store.GetK8sPoolConfig(ctx, "")
	if err == nil {
		t.Error("expected error for empty pool name in Get")
	}

	err = store.SaveK8sPoolConfig(ctx, nil)
	if err == nil {
		t.Error("expected error for nil config")
	}

	err = store.SaveK8sPoolConfig(ctx, &K8sPoolConfig{})
	if err == nil {
		t.Error("expected error for empty pool name in Save")
	}

	err = store.DeleteK8sPoolConfig(ctx, "")
	if err == nil {
		t.Error("expected error for empty pool name in Delete")
	}

	err = store.UpdateK8sPoolState(ctx, "", 1, 1)
	if err == nil {
		t.Error("expected error for empty pool name in UpdateState")
	}
}

func TestK8sPoolSchedule_TimeFields(t *testing.T) {
	store, mr := setupTestValkey(t)
	defer mr.Close()

	ctx := context.Background()

	before := time.Now()

	config := &K8sPoolConfig{
		PoolName:      "timing-test",
		Arm64Replicas: 1,
	}

	if err := store.SaveK8sPoolConfig(ctx, config); err != nil {
		t.Fatalf("SaveK8sPoolConfig failed: %v", err)
	}

	after := time.Now()

	retrieved, _ := store.GetK8sPoolConfig(ctx, "timing-test")
	if retrieved.UpdatedAt.Before(before) || retrieved.UpdatedAt.After(after) {
		t.Errorf("UpdatedAt %v should be between %v and %v",
			retrieved.UpdatedAt, before, after)
	}
}
