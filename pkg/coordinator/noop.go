package coordinator

import (
	"context"
)

// NoOpCoordinator is a coordinator that always returns true for IsLeader().
// Used for local development where distributed locking is not needed.
type NoOpCoordinator struct {
	logger Logger
}

// NewNoOpCoordinator creates a new no-op coordinator.
func NewNoOpCoordinator(logger Logger) *NoOpCoordinator {
	return &NoOpCoordinator{
		logger: logger,
	}
}

// Start is a no-op.
func (c *NoOpCoordinator) Start(_ context.Context) error {
	c.logger.Println("Using no-op coordinator (always leader)")
	return nil
}

// IsLeader always returns true.
func (c *NoOpCoordinator) IsLeader() bool {
	return true
}

// Stop is a no-op.
func (c *NoOpCoordinator) Stop(_ context.Context) error {
	return nil
}
