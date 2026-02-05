package metrics

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/logging"
)

const publishTimeout = 5 * time.Second

var metricsLog = logging.WithComponent(logging.LogTypeMetrics, "multi")

// MultiPublisher publishes metrics to multiple backends simultaneously.
// All Publisher interface methods are documented on the Publisher interface.
type MultiPublisher struct {
	publishers []Publisher
}

// Ensure MultiPublisher implements Publisher.
var _ Publisher = (*MultiPublisher)(nil)

// NewMultiPublisher creates a publisher that fans out to multiple backends.
func NewMultiPublisher(publishers ...Publisher) *MultiPublisher {
	return &MultiPublisher{publishers: publishers}
}

// Add adds a publisher to the fan-out list.
func (m *MultiPublisher) Add(p Publisher) {
	m.publishers = append(m.publishers, p)
}

// Publishers returns the list of configured publishers.
func (m *MultiPublisher) Publishers() []Publisher {
	return m.publishers
}

// Close closes all child publishers.
func (m *MultiPublisher) Close() error {
	var errs []error
	for _, p := range m.publishers {
		if err := p.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (m *MultiPublisher) publishAll(fn func(p Publisher) error) error {
	var wg sync.WaitGroup
	var mu sync.Mutex
	var errs []error

	for _, p := range m.publishers {
		wg.Add(1)
		go func(pub Publisher) {
			defer wg.Done()
			done := make(chan error, 1)
			go func() {
				done <- fn(pub)
			}()
			select {
			case err := <-done:
				if err != nil {
					metricsLog.Warn("metrics publish error", slog.String("error", err.Error()))
					mu.Lock()
					errs = append(errs, err)
					mu.Unlock()
				}
			case <-time.After(publishTimeout):
				metricsLog.Warn("metrics publish timeout", slog.Duration("timeout", publishTimeout))
				mu.Lock()
				errs = append(errs, fmt.Errorf("publish timeout after %v", publishTimeout))
				mu.Unlock()
			}
		}(p)
	}
	wg.Wait()
	return errors.Join(errs...)
}

// Publisher interface implementation below.
// All methods are documented on the Publisher interface.

func (m *MultiPublisher) PublishQueueDepth(ctx context.Context, depth float64) error { //nolint:revive
	return m.publishAll(func(p Publisher) error {
		return p.PublishQueueDepth(ctx, depth)
	})
}

func (m *MultiPublisher) PublishFleetSizeIncrement(ctx context.Context) error { //nolint:revive
	return m.publishAll(func(p Publisher) error {
		return p.PublishFleetSizeIncrement(ctx)
	})
}

func (m *MultiPublisher) PublishFleetSizeDecrement(ctx context.Context) error { //nolint:revive
	return m.publishAll(func(p Publisher) error {
		return p.PublishFleetSizeDecrement(ctx)
	})
}

func (m *MultiPublisher) PublishJobDuration(ctx context.Context, durationSeconds int) error { //nolint:revive
	return m.publishAll(func(p Publisher) error {
		return p.PublishJobDuration(ctx, durationSeconds)
	})
}

func (m *MultiPublisher) PublishJobSuccess(ctx context.Context) error { //nolint:revive
	return m.publishAll(func(p Publisher) error {
		return p.PublishJobSuccess(ctx)
	})
}

func (m *MultiPublisher) PublishJobFailure(ctx context.Context) error { //nolint:revive
	return m.publishAll(func(p Publisher) error {
		return p.PublishJobFailure(ctx)
	})
}

func (m *MultiPublisher) PublishJobQueued(ctx context.Context) error { //nolint:revive
	return m.publishAll(func(p Publisher) error {
		return p.PublishJobQueued(ctx)
	})
}

func (m *MultiPublisher) PublishSpotInterruption(ctx context.Context) error { //nolint:revive
	return m.publishAll(func(p Publisher) error {
		return p.PublishSpotInterruption(ctx)
	})
}

func (m *MultiPublisher) PublishMessageDeletionFailure(ctx context.Context) error { //nolint:revive
	return m.publishAll(func(p Publisher) error {
		return p.PublishMessageDeletionFailure(ctx)
	})
}

func (m *MultiPublisher) PublishCacheHit(ctx context.Context) error { //nolint:revive
	return m.publishAll(func(p Publisher) error {
		return p.PublishCacheHit(ctx)
	})
}

func (m *MultiPublisher) PublishCacheMiss(ctx context.Context) error { //nolint:revive
	return m.publishAll(func(p Publisher) error {
		return p.PublishCacheMiss(ctx)
	})
}

func (m *MultiPublisher) PublishOrphanedInstancesTerminated(ctx context.Context, count int) error { //nolint:revive
	return m.publishAll(func(p Publisher) error {
		return p.PublishOrphanedInstancesTerminated(ctx, count)
	})
}

func (m *MultiPublisher) PublishSSMParametersDeleted(ctx context.Context, count int) error { //nolint:revive
	return m.publishAll(func(p Publisher) error {
		return p.PublishSSMParametersDeleted(ctx, count)
	})
}

func (m *MultiPublisher) PublishJobRecordsArchived(ctx context.Context, count int) error { //nolint:revive
	return m.publishAll(func(p Publisher) error {
		return p.PublishJobRecordsArchived(ctx, count)
	})
}

func (m *MultiPublisher) PublishOrphanedJobsCleanedUp(ctx context.Context, count int) error { //nolint:revive
	return m.publishAll(func(p Publisher) error {
		return p.PublishOrphanedJobsCleanedUp(ctx, count)
	})
}

func (m *MultiPublisher) PublishPoolUtilization(ctx context.Context, poolName string, utilization float64) error { //nolint:revive
	return m.publishAll(func(p Publisher) error {
		return p.PublishPoolUtilization(ctx, poolName, utilization)
	})
}

func (m *MultiPublisher) PublishPoolRunningJobs(ctx context.Context, poolName string, count int) error { //nolint:revive
	return m.publishAll(func(p Publisher) error {
		return p.PublishPoolRunningJobs(ctx, poolName, count)
	})
}

func (m *MultiPublisher) PublishSchedulingFailure(ctx context.Context, taskType string) error { //nolint:revive
	return m.publishAll(func(p Publisher) error {
		return p.PublishSchedulingFailure(ctx, taskType)
	})
}

func (m *MultiPublisher) PublishCircuitBreakerTriggered(ctx context.Context, instanceType string) error { //nolint:revive
	return m.publishAll(func(p Publisher) error {
		return p.PublishCircuitBreakerTriggered(ctx, instanceType)
	})
}

func (m *MultiPublisher) PublishJobClaimFailure(ctx context.Context) error { //nolint:revive
	return m.publishAll(func(p Publisher) error {
		return p.PublishJobClaimFailure(ctx)
	})
}

func (m *MultiPublisher) PublishWarmPoolHit(ctx context.Context) error { //nolint:revive
	return m.publishAll(func(p Publisher) error {
		return p.PublishWarmPoolHit(ctx)
	})
}

func (m *MultiPublisher) PublishFleetSize(ctx context.Context, size int) error { //nolint:revive
	return m.publishAll(func(p Publisher) error {
		return p.PublishFleetSize(ctx, size)
	})
}

func (m *MultiPublisher) PublishServiceCheck(ctx context.Context, name string, status int, message string) error { //nolint:revive
	return m.publishAll(func(p Publisher) error {
		return p.PublishServiceCheck(ctx, name, status, message)
	})
}

func (m *MultiPublisher) PublishEvent(ctx context.Context, title, text, alertType string, tags []string) error { //nolint:revive
	return m.publishAll(func(p Publisher) error {
		return p.PublishEvent(ctx, title, text, alertType, tags)
	})
}
