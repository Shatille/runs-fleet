// Package coordinator implements distributed leader election using DynamoDB.
package coordinator

import (
	"context"
	"time"
)

// Coordinator manages distributed leader election across multiple instances.
type Coordinator interface {
	// Start begins the leader election process.
	Start(ctx context.Context) error

	// IsLeader returns true if the current instance is the leader.
	IsLeader() bool

	// Stop gracefully shuts down the coordinator.
	Stop(ctx context.Context) error
}

// Config holds configuration for the coordinator.
type Config struct {
	// InstanceID uniquely identifies this instance.
	InstanceID string

	// LockTableName is the DynamoDB table for leader locks.
	LockTableName string

	// LockName is the name of the lock to acquire.
	LockName string

	// LeaseDuration is how long a leader holds the lock without renewal.
	LeaseDuration time.Duration

	// HeartbeatInterval is how often the leader renews the lock.
	HeartbeatInterval time.Duration

	// RetryInterval is how often followers attempt to acquire the lock.
	RetryInterval time.Duration
}

// DefaultConfig returns recommended configuration values.
func DefaultConfig(instanceID string) Config {
	return Config{
		InstanceID:        instanceID,
		LockTableName:     "runs-fleet-locks",
		LockName:          "runs-fleet-leader",
		LeaseDuration:     60 * time.Second,
		HeartbeatInterval: 20 * time.Second,
		RetryInterval:     30 * time.Second,
	}
}
