// Package state provides state storage backends for pool configuration and job tracking.
package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// K8sPoolConfig represents pool configuration for Kubernetes deployments.
// Unlike EC2 pools with hot/stopped states, K8s pools manage placeholder Deployments
// with architecture-specific replica counts.
type K8sPoolConfig struct {
	PoolName           string              `json:"pool_name"`
	Arm64Replicas      int                 `json:"arm64_replicas"`
	Amd64Replicas      int                 `json:"amd64_replicas"`
	IdleTimeoutMinutes int                 `json:"idle_timeout_minutes,omitempty"`
	Schedules          []K8sPoolSchedule   `json:"schedules,omitempty"`
	Resources          K8sResourceRequests `json:"resources,omitempty"`
	UpdatedAt          time.Time           `json:"updated_at"`
}

// K8sPoolSchedule defines time-based pool sizing for K8s.
type K8sPoolSchedule struct {
	Name          string `json:"name"`
	StartHour     int    `json:"start_hour"`
	EndHour       int    `json:"end_hour"`
	DaysOfWeek    []int  `json:"days_of_week,omitempty"`
	Arm64Replicas int    `json:"arm64_replicas"`
	Amd64Replicas int    `json:"amd64_replicas"`
}

// K8sResourceRequests defines resource requests for placeholder pods.
type K8sResourceRequests struct {
	CPU    string `json:"cpu,omitempty"`
	Memory string `json:"memory,omitempty"`
}

// ValkeyStateStore implements state storage using Valkey/Redis.
type ValkeyStateStore struct {
	client    *redis.Client
	keyPrefix string
}

// NewValkeyStateStoreWithClient creates a state store with an existing client (for testing).
func NewValkeyStateStoreWithClient(client *redis.Client, keyPrefix string) *ValkeyStateStore {
	if keyPrefix == "" {
		keyPrefix = "runs-fleet:"
	}
	return &ValkeyStateStore{
		client:    client,
		keyPrefix: keyPrefix,
	}
}

func (s *ValkeyStateStore) poolKey(poolName string) string {
	return s.keyPrefix + "pool:" + poolName
}

func (s *ValkeyStateStore) poolsIndexKey() string {
	return s.keyPrefix + "pools:index"
}

// GetK8sPoolConfig retrieves pool configuration from Valkey.
func (s *ValkeyStateStore) GetK8sPoolConfig(ctx context.Context, poolName string) (*K8sPoolConfig, error) {
	if poolName == "" {
		return nil, fmt.Errorf("pool name cannot be empty")
	}

	data, err := s.client.Get(ctx, s.poolKey(poolName)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get pool config: %w", err)
	}

	var config K8sPoolConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("failed to unmarshal pool config: %w", err)
	}

	return &config, nil
}

// SaveK8sPoolConfig saves pool configuration to Valkey atomically.
func (s *ValkeyStateStore) SaveK8sPoolConfig(ctx context.Context, config *K8sPoolConfig) error {
	if config == nil {
		return fmt.Errorf("pool config cannot be nil")
	}
	if config.PoolName == "" {
		return fmt.Errorf("pool name cannot be empty")
	}

	config.UpdatedAt = time.Now()
	data, err := json.Marshal(config)
	if err != nil {
		return fmt.Errorf("failed to marshal pool config: %w", err)
	}

	// TxPipeline wraps in MULTI/EXEC for atomic execution
	pipe := s.client.TxPipeline()
	pipe.SAdd(ctx, s.poolsIndexKey(), config.PoolName)
	pipe.Set(ctx, s.poolKey(config.PoolName), data, 0)

	_, err = pipe.Exec(ctx)
	if err != nil {
		return fmt.Errorf("failed to save pool config: %w", err)
	}

	return nil
}

// DeleteK8sPoolConfig removes pool configuration from Valkey atomically.
func (s *ValkeyStateStore) DeleteK8sPoolConfig(ctx context.Context, poolName string) error {
	if poolName == "" {
		return fmt.Errorf("pool name cannot be empty")
	}

	// TxPipeline wraps in MULTI/EXEC for atomic execution
	pipe := s.client.TxPipeline()
	pipe.SRem(ctx, s.poolsIndexKey(), poolName)
	pipe.Del(ctx, s.poolKey(poolName))

	_, err := pipe.Exec(ctx)
	if err != nil {
		return fmt.Errorf("failed to delete pool config: %w", err)
	}

	return nil
}

// ListK8sPools returns all pool names from Valkey.
func (s *ValkeyStateStore) ListK8sPools(ctx context.Context) ([]string, error) {
	pools, err := s.client.SMembers(ctx, s.poolsIndexKey()).Result()
	if err != nil {
		return nil, fmt.Errorf("failed to list pools: %w", err)
	}
	return pools, nil
}

// UpdateK8sPoolState updates the current state of the pool (for metrics/monitoring).
func (s *ValkeyStateStore) UpdateK8sPoolState(ctx context.Context, poolName string, arm64Running, amd64Running int) error {
	if poolName == "" {
		return fmt.Errorf("pool name cannot be empty")
	}

	stateKey := s.keyPrefix + "pool-state:" + poolName
	state := map[string]interface{}{
		"arm64_running": arm64Running,
		"amd64_running": amd64Running,
		"updated_at":    time.Now().Unix(),
	}

	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("failed to marshal pool state: %w", err)
	}

	if err := s.client.Set(ctx, stateKey, data, 24*time.Hour).Err(); err != nil {
		return fmt.Errorf("failed to update pool state: %w", err)
	}

	return nil
}

// Close closes the Valkey client connection.
func (s *ValkeyStateStore) Close() error {
	return s.client.Close()
}
