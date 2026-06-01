package worker

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/Shavakan/runs-fleet/pkg/logging"
	"github.com/Shavakan/runs-fleet/pkg/queue"
)

var workerLog = logging.WithComponent(logging.LogTypeQueue, "worker")

const (
	// maxConcurrency bounds the number of messages processed simultaneously.
	maxConcurrency = 5
	// maxMessagesPerReceive is the SQS ReceiveMessage per-call ceiling.
	maxMessagesPerReceive = 10
	// receiveWaitSeconds is the SQS long-poll wait; it doubles as the idle
	// backoff so an empty queue does not busy-loop.
	receiveWaitSeconds = 20
	// idlePollInterval is how often the worker re-polls once the queue has been
	// drained. It is independent of the per-receive timeout below.
	idlePollInterval = 25 * time.Second
)

// MessageProcessor processes a single queue message.
type MessageProcessor func(ctx context.Context, msg queue.Message)

// ReceiveObserver reports the outcome of each queue receive: "messages",
// "empty", or "error". It is invoked once per ReceiveMessages call.
type ReceiveObserver func(ctx context.Context, result string)

// ProcessObserver reports the per-message processing duration and result
// ("ok" or "error", where "error" means the processor panicked). It is invoked
// once per processed message after the processor returns.
type ProcessObserver func(ctx context.Context, result string, seconds float64)

// RunWorkerLoop runs a generic worker loop that polls a queue and processes messages concurrently.
func RunWorkerLoop(ctx context.Context, name string, q queue.Queue, processor MessageProcessor) {
	RunWorkerLoopWithObserver(ctx, name, q, processor, nil)
}

// RunWorkerLoopWithObserver runs the worker loop and reports each receive
// outcome to the observer (nil disables reporting).
func RunWorkerLoopWithObserver(ctx context.Context, name string, q queue.Queue, processor MessageProcessor, observe ReceiveObserver) {
	RunWorkerLoopWithObservers(ctx, name, q, processor, observe, nil)
}

// RunWorkerLoopWithObservers runs the worker loop and reports each receive
// outcome and per-message processing duration to the respective observers (nil
// disables either).
func RunWorkerLoopWithObservers(ctx context.Context, name string, q queue.Queue, processor MessageProcessor, observe ReceiveObserver, observeProcess ProcessObserver) {
	ticker := time.NewTicker(idlePollInterval)
	defer ticker.Stop()
	RunWorkerLoopWithTicker(ctx, name, q, processor, observe, observeProcess, ticker.C)
}

// RunWorkerLoopWithTicker runs the worker loop with an injectable ticker for testing.
//
// Each tick triggers a drain that keeps receiving while the queue returns full
// batches, so intake is bounded by processing throughput rather than capped at
// one batch per tick. The ticker only paces re-polling once the queue is drained.
func RunWorkerLoopWithTicker(ctx context.Context, name string, q queue.Queue, processor MessageProcessor, observe ReceiveObserver, observeProcess ProcessObserver, tick <-chan time.Time) {
	workerLog.Info(ctx, "worker starting", slog.String("worker", name))

	sem := make(chan struct{}, maxConcurrency)
	var activeWork sync.WaitGroup

	defer func() {
		activeWork.Wait()
		workerLog.Info(ctx, "worker shutdown complete", slog.String("worker", name))
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick:
			drainQueue(ctx, name, q, processor, observe, observeProcess, sem, &activeWork)
		}
	}
}

// drainQueue receives and dispatches messages until the queue returns a
// partial/empty batch or an error, then returns so the caller waits for the
// next tick. Draining continuously while batches come back full prevents the
// per-tick batch size from throttling intake when a backlog is present.
func drainQueue(ctx context.Context, name string, q queue.Queue, processor MessageProcessor, observe ReceiveObserver, observeProcess ProcessObserver, sem chan struct{}, activeWork *sync.WaitGroup) {
	for {
		if ctx.Err() != nil {
			return
		}

		timeout := config.MessageReceiveTimeout
		if deadline, ok := ctx.Deadline(); ok {
			if remaining := time.Until(deadline); remaining < timeout {
				timeout = remaining
			}
		}
		recvCtx, cancel := context.WithTimeout(ctx, timeout)
		messages, err := q.ReceiveMessages(recvCtx, maxMessagesPerReceive, receiveWaitSeconds)
		cancel()
		if err != nil {
			reportReceive(ctx, observe, "error")
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				workerLog.Warn(ctx, "receive messages failed",
					slog.String("worker", name),
					slog.String("error", err.Error()))
			} else {
				workerLog.Error(ctx, "receive messages failed",
					slog.String("worker", name),
					slog.String("error", err.Error()))
			}
			return
		}

		if len(messages) == 0 {
			reportReceive(ctx, observe, "empty")
		} else {
			reportReceive(ctx, observe, "messages")
		}

		for i := range messages {
			msg := messages[i]
			// Acquire a slot before spawning so the number of in-flight
			// goroutines (and the SQS visibility leases they hold) stays bounded
			// by maxConcurrency even while draining a deep backlog.
			// TODO(metrics): track in-flight workers via PublishWorkerInflight(ctx,
			// queue, len(sem)) once timing instrumentation lands.
			select {
			case <-ctx.Done():
				return
			case sem <- struct{}{}:
			}
			activeWork.Add(1)
			go func() {
				// Default to "error"; the processor sets it to "ok" only on a
				// clean return, so a panic (recovered below) reports "error".
				start := time.Now()
				result := "error"
				defer func() {
					if observeProcess != nil {
						observeProcess(ctx, result, time.Since(start).Seconds())
					}
					if r := recover(); r != nil {
						workerLog.Error(ctx, "panic in message processor",
							slog.String("worker", name),
							slog.Any("panic", r))
					}
				}()
				defer activeWork.Done()
				defer func() { <-sem }()

				processCtx, processCancel := context.WithTimeout(ctx, config.MessageProcessTimeout)
				defer processCancel()
				processor(processCtx, msg)
				result = "ok"
			}()
		}

		if len(messages) == 0 {
			return
		}
	}
}

// reportReceive invokes the observer with the receive outcome when one is set.
func reportReceive(ctx context.Context, observe ReceiveObserver, result string) {
	if observe != nil {
		observe(ctx, result)
	}
}
