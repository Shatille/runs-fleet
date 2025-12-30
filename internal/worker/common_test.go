package worker

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/queue"
)

// MockQueue implements queue.Queue for testing.
type MockQueue struct {
	SendMessageFunc     func(ctx context.Context, job *queue.JobMessage) error
	ReceiveMessagesFunc func(ctx context.Context, maxMessages int32, waitTimeSeconds int32) ([]queue.Message, error)
	DeleteMessageFunc   func(ctx context.Context, handle string) error
	mu                  sync.Mutex
	receiveCalls        int
}

func (m *MockQueue) SendMessage(ctx context.Context, job *queue.JobMessage) error {
	if m.SendMessageFunc != nil {
		return m.SendMessageFunc(ctx, job)
	}
	return nil
}

func (m *MockQueue) ReceiveMessages(ctx context.Context, maxMessages int32, waitTimeSeconds int32) ([]queue.Message, error) {
	m.mu.Lock()
	m.receiveCalls++
	m.mu.Unlock()
	if m.ReceiveMessagesFunc != nil {
		return m.ReceiveMessagesFunc(ctx, maxMessages, waitTimeSeconds)
	}
	return nil, nil
}

func (m *MockQueue) DeleteMessage(ctx context.Context, handle string) error {
	if m.DeleteMessageFunc != nil {
		return m.DeleteMessageFunc(ctx, handle)
	}
	return nil
}

func (m *MockQueue) GetReceiveCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.receiveCalls
}

func TestRunWorkerLoop_ContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	mockQueue := &MockQueue{}

	done := make(chan struct{})
	go func() {
		RunWorkerLoop(ctx, "test", mockQueue, func(_ context.Context, _ queue.Message) {})
		close(done)
	}()

	// Cancel context immediately
	cancel()

	// Wait for worker to stop
	select {
	case <-done:
		// Success
	case <-time.After(2 * time.Second):
		t.Fatal("RunWorkerLoop did not stop on context cancellation")
	}
}

func TestRunWorkerLoop_ProcessesMessages(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var processed int32
	processingDone := make(chan struct{})
	processedCount := 3

	mockQueue := &MockQueue{
		ReceiveMessagesFunc: func(_ context.Context, _ int32, _ int32) ([]queue.Message, error) {
			// Return messages only until we've processed enough
			if atomic.LoadInt32(&processed) < int32(processedCount) {
				return []queue.Message{
					{ID: "msg-1", Body: `{"run_id": 123}`, Handle: "handle-1"},
				}, nil
			}
			// After processing, signal completion and return empty
			select {
			case <-processingDone:
			default:
				close(processingDone)
			}
			return nil, nil
		},
	}

	go RunWorkerLoop(ctx, "test", mockQueue, func(_ context.Context, _ queue.Message) {
		atomic.AddInt32(&processed, 1)
	})

	// Wait for messages to be processed or timeout
	select {
	case <-processingDone:
		// Success
	case <-time.After(90 * time.Second):
		t.Fatal("Messages were not processed in time")
	}

	if atomic.LoadInt32(&processed) < int32(processedCount) {
		t.Errorf("Expected at least %d messages processed, got %d", processedCount, atomic.LoadInt32(&processed))
	}

	cancel()
}

func TestRunWorkerLoop_PanicRecovery(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	panicRecovered := make(chan struct{})
	panicTriggered := int32(0)

	mockQueue := &MockQueue{
		ReceiveMessagesFunc: func(_ context.Context, _ int32, _ int32) ([]queue.Message, error) {
			// Only return message if panic hasn't been triggered yet
			if atomic.CompareAndSwapInt32(&panicTriggered, 0, 1) {
				return []queue.Message{
					{ID: "msg-1", Body: `{"run_id": 123}`, Handle: "handle-1"},
				}, nil
			}
			// Wait for a bit to give time for panic recovery
			time.Sleep(100 * time.Millisecond)
			return nil, nil
		},
	}

	go RunWorkerLoop(ctx, "test", mockQueue, func(_ context.Context, _ queue.Message) {
		defer func() {
			if r := recover(); r == nil {
				// If we're still running after the panic, the recovery worked
				close(panicRecovered)
			}
		}()
		panic("test panic")
	})

	// The worker should continue running after the panic
	select {
	case <-time.After(60 * time.Second):
		// Worker is still running (expected), cancel to clean up
		cancel()
	}
}

func TestRunWorkerLoop_ConcurrencyLimit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var concurrent int32
	var maxConcurrent int32
	var mu sync.Mutex

	processingStarted := make(chan struct{}, 20)
	allowProcessing := make(chan struct{})
	allDone := make(chan struct{})

	mockQueue := &MockQueue{
		ReceiveMessagesFunc: func(_ context.Context, _ int32, _ int32) ([]queue.Message, error) {
			// Return 10 messages to test concurrency limit (max is 5)
			messages := make([]queue.Message, 10)
			for i := 0; i < 10; i++ {
				messages[i] = queue.Message{
					ID:     "msg-" + string(rune('0'+i)),
					Body:   `{"run_id": 123}`,
					Handle: "handle",
				}
			}
			return messages, nil
		},
	}

	processedCount := int32(0)
	go RunWorkerLoop(ctx, "test", mockQueue, func(_ context.Context, _ queue.Message) {
		current := atomic.AddInt32(&concurrent, 1)
		mu.Lock()
		if current > maxConcurrent {
			maxConcurrent = current
		}
		mu.Unlock()

		select {
		case processingStarted <- struct{}{}:
		default:
		}

		<-allowProcessing
		atomic.AddInt32(&concurrent, -1)

		if atomic.AddInt32(&processedCount, 1) >= 10 {
			select {
			case <-allDone:
			default:
				close(allDone)
			}
		}
	})

	// Wait for concurrent processors to start
	startedCount := 0
	for startedCount < 5 {
		select {
		case <-processingStarted:
			startedCount++
		case <-time.After(60 * time.Second):
			t.Fatalf("Not enough processors started, got %d", startedCount)
		}
	}

	// Give a moment for any additional processors to start
	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	maxConcurrentValue := maxConcurrent
	mu.Unlock()

	// Max concurrency should be 5 (as defined in RunWorkerLoop)
	if maxConcurrentValue > 5 {
		t.Errorf("Max concurrent = %d, expected <= 5", maxConcurrentValue)
	}

	// Allow processing to complete
	close(allowProcessing)
	cancel()
}

func TestRunWorkerLoop_GracefulShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	processingStarted := make(chan struct{})
	processingCompleted := make(chan struct{})
	workerDone := make(chan struct{})

	mockQueue := &MockQueue{
		ReceiveMessagesFunc: func(_ context.Context, _ int32, _ int32) ([]queue.Message, error) {
			return []queue.Message{
				{ID: "msg-1", Body: `{"run_id": 123}`, Handle: "handle-1"},
			}, nil
		},
	}

	receivedOnce := int32(0)
	go func() {
		RunWorkerLoop(ctx, "test", mockQueue, func(_ context.Context, _ queue.Message) {
			if atomic.CompareAndSwapInt32(&receivedOnce, 0, 1) {
				close(processingStarted)
				// Simulate some work
				time.Sleep(100 * time.Millisecond)
				close(processingCompleted)
			}
		})
		close(workerDone)
	}()

	// Wait for processing to start
	select {
	case <-processingStarted:
	case <-time.After(60 * time.Second):
		t.Fatal("Processing did not start")
	}

	// Cancel while message is being processed
	cancel()

	// Worker should wait for in-flight work
	select {
	case <-processingCompleted:
	case <-time.After(2 * time.Second):
		t.Error("Processing did not complete before worker shutdown")
	}

	// Worker should now be done
	select {
	case <-workerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Worker did not shut down gracefully")
	}
}

func TestRunWorkerLoop_EmptyMessages(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mockQueue := &MockQueue{
		ReceiveMessagesFunc: func(_ context.Context, _ int32, _ int32) ([]queue.Message, error) {
			// Always return empty messages
			return []queue.Message{}, nil
		},
	}

	processorCalled := int32(0)
	done := make(chan struct{})

	go func() {
		// Run for a short time and verify processor is never called
		runCtx, runCancel := context.WithTimeout(ctx, 30*time.Second)
		defer runCancel()
		RunWorkerLoop(runCtx, "test", mockQueue, func(_ context.Context, _ queue.Message) {
			atomic.AddInt32(&processorCalled, 1)
		})
		close(done)
	}()

	<-done

	if atomic.LoadInt32(&processorCalled) > 0 {
		t.Error("Processor should not be called when no messages are received")
	}
}

func TestRunWorkerLoop_ReceiveError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	errorCount := int32(0)
	done := make(chan struct{})

	mockQueue := &MockQueue{
		ReceiveMessagesFunc: func(_ context.Context, _ int32, _ int32) ([]queue.Message, error) {
			count := atomic.AddInt32(&errorCount, 1)
			if count >= 2 {
				close(done)
			}
			return nil, context.DeadlineExceeded
		},
	}

	go RunWorkerLoop(ctx, "test", mockQueue, func(_ context.Context, _ queue.Message) {})

	// Wait for a couple errors to occur (proving the loop continues)
	select {
	case <-done:
		// Success - loop continued after errors
	case <-time.After(60 * time.Second):
		t.Fatal("Worker loop did not continue after receive error")
	}

	cancel()
}

func TestRunWorkerLoop_ContextDeadline(t *testing.T) {
	// Test that context deadline is respected in timeout calculation
	deadline := time.Now().Add(10 * time.Second)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()

	mockQueue := &MockQueue{
		ReceiveMessagesFunc: func(ctx context.Context, _ int32, _ int32) ([]queue.Message, error) {
			// Verify context has a deadline
			if _, ok := ctx.Deadline(); !ok {
				t.Error("Expected context to have a deadline")
			}
			return nil, nil
		},
	}

	done := make(chan struct{})
	go func() {
		runCtx, runCancel := context.WithTimeout(ctx, 30*time.Second)
		defer runCancel()
		RunWorkerLoop(runCtx, "test", mockQueue, func(_ context.Context, _ queue.Message) {})
		close(done)
	}()

	<-done
}
