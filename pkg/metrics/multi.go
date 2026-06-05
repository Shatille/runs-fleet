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

func (m *MultiPublisher) publishAll(ctx context.Context, fn func(p Publisher) error) error {
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
					metricsLog.Warn(ctx, "metrics publish error", slog.String("error", err.Error()))
					mu.Lock()
					errs = append(errs, err)
					mu.Unlock()
				}
			case <-time.After(publishTimeout):
				metricsLog.Warn(ctx, "metrics publish timeout", slog.Duration("timeout", publishTimeout))
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

func (m *MultiPublisher) PublishJobEnqueued(ctx context.Context, pool, arch, capacity, repo string) error { //nolint:revive
	return m.publishAll(ctx, func(p Publisher) error {
		return p.PublishJobEnqueued(ctx, pool, arch, capacity, repo)
	})
}

func (m *MultiPublisher) PublishJobAssigned(ctx context.Context, pool, source, repo string) error { //nolint:revive
	return m.publishAll(ctx, func(p Publisher) error {
		return p.PublishJobAssigned(ctx, pool, source, repo)
	})
}

func (m *MultiPublisher) PublishRunnerConfirmed(ctx context.Context, pool string) error { //nolint:revive
	return m.publishAll(ctx, func(p Publisher) error {
		return p.PublishRunnerConfirmed(ctx, pool)
	})
}

func (m *MultiPublisher) PublishJobCompleted(ctx context.Context, pool, result, repo string) error { //nolint:revive
	return m.publishAll(ctx, func(p Publisher) error {
		return p.PublishJobCompleted(ctx, pool, result, repo)
	})
}

func (m *MultiPublisher) PublishJobRequeued(ctx context.Context, reason string) error { //nolint:revive
	return m.publishAll(ctx, func(p Publisher) error {
		return p.PublishJobRequeued(ctx, reason)
	})
}

func (m *MultiPublisher) PublishJobWaitSeconds(ctx context.Context, pool, source string, seconds float64) error { //nolint:revive
	return m.publishAll(ctx, func(p Publisher) error {
		return p.PublishJobWaitSeconds(ctx, pool, source, seconds)
	})
}

func (m *MultiPublisher) PublishJobExecutionSeconds(ctx context.Context, pool, result string, seconds float64) error { //nolint:revive
	return m.publishAll(ctx, func(p Publisher) error {
		return p.PublishJobExecutionSeconds(ctx, pool, result, seconds)
	})
}

func (m *MultiPublisher) PublishInstanceProvisionSeconds(ctx context.Context, source, family string, seconds float64) error { //nolint:revive
	return m.publishAll(ctx, func(p Publisher) error {
		return p.PublishInstanceProvisionSeconds(ctx, source, family, seconds)
	})
}

func (m *MultiPublisher) PublishFleetCreate(ctx context.Context, capacity, result string) error { //nolint:revive
	return m.publishAll(ctx, func(p Publisher) error {
		return p.PublishFleetCreate(ctx, capacity, result)
	})
}

func (m *MultiPublisher) PublishFleetCreateSeconds(ctx context.Context, capacity string, seconds float64) error { //nolint:revive
	return m.publishAll(ctx, func(p Publisher) error {
		return p.PublishFleetCreateSeconds(ctx, capacity, seconds)
	})
}

func (m *MultiPublisher) PublishInstances(ctx context.Context, state, capacity, pool string, n int) error { //nolint:revive
	return m.publishAll(ctx, func(p Publisher) error {
		return p.PublishInstances(ctx, state, capacity, pool, n)
	})
}

func (m *MultiPublisher) PublishSpotInterruption(ctx context.Context, family string) error { //nolint:revive
	return m.publishAll(ctx, func(p Publisher) error {
		return p.PublishSpotInterruption(ctx, family)
	})
}

func (m *MultiPublisher) PublishCircuitBreakerTrip(ctx context.Context, instanceType string) error { //nolint:revive
	return m.publishAll(ctx, func(p Publisher) error {
		return p.PublishCircuitBreakerTrip(ctx, instanceType)
	})
}

func (m *MultiPublisher) PublishCircuitBreakerOpen(ctx context.Context, instanceType string, open bool) error { //nolint:revive
	return m.publishAll(ctx, func(p Publisher) error {
		return p.PublishCircuitBreakerOpen(ctx, instanceType, open)
	})
}

func (m *MultiPublisher) PublishPoolInstances(ctx context.Context, pool, state string, n int) error { //nolint:revive
	return m.publishAll(ctx, func(p Publisher) error {
		return p.PublishPoolInstances(ctx, pool, state, n)
	})
}

func (m *MultiPublisher) PublishPoolDesired(ctx context.Context, pool, kind string, n int) error { //nolint:revive
	return m.publishAll(ctx, func(p Publisher) error {
		return p.PublishPoolDesired(ctx, pool, kind, n)
	})
}

func (m *MultiPublisher) PublishPoolAction(ctx context.Context, pool, action, reason string) error { //nolint:revive
	return m.publishAll(ctx, func(p Publisher) error {
		return p.PublishPoolAction(ctx, pool, action, reason)
	})
}

func (m *MultiPublisher) PublishPoolReconcileSeconds(ctx context.Context, seconds float64) error { //nolint:revive
	return m.publishAll(ctx, func(p Publisher) error {
		return p.PublishPoolReconcileSeconds(ctx, seconds)
	})
}

func (m *MultiPublisher) PublishMessageProcessingSeconds(ctx context.Context, queue, result string, seconds float64) error { //nolint:revive
	return m.publishAll(ctx, func(p Publisher) error {
		return p.PublishMessageProcessingSeconds(ctx, queue, result, seconds)
	})
}

func (m *MultiPublisher) PublishLockWaitSeconds(ctx context.Context, lock string, seconds float64) error { //nolint:revive
	return m.publishAll(ctx, func(p Publisher) error {
		return p.PublishLockWaitSeconds(ctx, lock, seconds)
	})
}

func (m *MultiPublisher) PublishWorkerInflight(ctx context.Context, queue string, n int) error { //nolint:revive
	return m.publishAll(ctx, func(p Publisher) error {
		return p.PublishWorkerInflight(ctx, queue, n)
	})
}

func (m *MultiPublisher) PublishQueueDepth(ctx context.Context, queue string, depth float64) error { //nolint:revive
	return m.publishAll(ctx, func(p Publisher) error {
		return p.PublishQueueDepth(ctx, queue, depth)
	})
}

func (m *MultiPublisher) PublishQueueReceive(ctx context.Context, queue, result string) error { //nolint:revive
	return m.publishAll(ctx, func(p Publisher) error {
		return p.PublishQueueReceive(ctx, queue, result)
	})
}

func (m *MultiPublisher) PublishAWSCallDuration(ctx context.Context, service, operation string, durationSeconds float64) error { //nolint:revive
	return m.publishAll(ctx, func(p Publisher) error {
		return p.PublishAWSCallDuration(ctx, service, operation, durationSeconds)
	})
}

func (m *MultiPublisher) PublishAWSCallFailure(ctx context.Context, service, operation, result string) error { //nolint:revive
	return m.publishAll(ctx, func(p Publisher) error {
		return p.PublishAWSCallFailure(ctx, service, operation, result)
	})
}

func (m *MultiPublisher) PublishCacheRequest(ctx context.Context, result string) error { //nolint:revive
	return m.publishAll(ctx, func(p Publisher) error {
		return p.PublishCacheRequest(ctx, result)
	})
}

func (m *MultiPublisher) PublishCacheOperation(ctx context.Context, operation string) error { //nolint:revive
	return m.publishAll(ctx, func(p Publisher) error {
		return p.PublishCacheOperation(ctx, operation)
	})
}

func (m *MultiPublisher) PublishCacheBytesStored(ctx context.Context, bytes int64) error { //nolint:revive
	return m.publishAll(ctx, func(p Publisher) error {
		return p.PublishCacheBytesStored(ctx, bytes)
	})
}

func (m *MultiPublisher) PublishCacheError(ctx context.Context, operation string) error { //nolint:revive
	return m.publishAll(ctx, func(p Publisher) error {
		return p.PublishCacheError(ctx, operation)
	})
}

func (m *MultiPublisher) PublishCacheAuthRejected(ctx context.Context, reason string) error { //nolint:revive
	return m.publishAll(ctx, func(p Publisher) error {
		return p.PublishCacheAuthRejected(ctx, reason)
	})
}

func (m *MultiPublisher) PublishHousekeepingAction(ctx context.Context, action string, count int) error { //nolint:revive
	return m.publishAll(ctx, func(p Publisher) error {
		return p.PublishHousekeepingAction(ctx, action, count)
	})
}

func (m *MultiPublisher) PublishSchedulingFailure(ctx context.Context, taskType string) error { //nolint:revive
	return m.publishAll(ctx, func(p Publisher) error {
		return p.PublishSchedulingFailure(ctx, taskType)
	})
}

func (m *MultiPublisher) PublishMessageDeletionFailure(ctx context.Context, queue string) error { //nolint:revive
	return m.publishAll(ctx, func(p Publisher) error {
		return p.PublishMessageDeletionFailure(ctx, queue)
	})
}

func (m *MultiPublisher) PublishServiceCheck(ctx context.Context, name string, status int, message string) error { //nolint:revive
	return m.publishAll(ctx, func(p Publisher) error {
		return p.PublishServiceCheck(ctx, name, status, message)
	})
}

func (m *MultiPublisher) PublishEvent(ctx context.Context, title, text, alertType string, tags []string) error { //nolint:revive
	return m.publishAll(ctx, func(p Publisher) error {
		return p.PublishEvent(ctx, title, text, alertType, tags)
	})
}

func (m *MultiPublisher) PublishInstanceHours(ctx context.Context, capacity, family string, hours float64) error { //nolint:revive
	return m.publishAll(ctx, func(p Publisher) error {
		return p.PublishInstanceHours(ctx, capacity, family, hours)
	})
}

func (m *MultiPublisher) PublishEstimatedCost(ctx context.Context, usd float64) error { //nolint:revive
	return m.publishAll(ctx, func(p Publisher) error {
		return p.PublishEstimatedCost(ctx, usd)
	})
}
