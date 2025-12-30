package worker

import (
	"context"
	"fmt"
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
	tick := make(chan time.Time)

	done := make(chan struct{})
	go func() {
		RunWorkerLoopWithTicker(ctx, "test", mockQueue, func(_ context.Context, _ queue.Message) {}, tick)
		close(done)
	}()

	cancel()

	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("RunWorkerLoop did not stop on context cancellation")
	}
}

func TestRunWorkerLoop_ProcessesMessages(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var processed int32
	processedCount := int32(3)
	tick := make(chan time.Time)

	mockQueue := &MockQueue{
		ReceiveMessagesFunc: func(_ context.Context, _ int32, _ int32) ([]queue.Message, error) {
			return []queue.Message{
				{ID: "msg-1", Body: `{"run_id": 123}`, Handle: "handle-1"},
			}, nil
		},
	}

	go RunWorkerLoopWithTicker(ctx, "test", mockQueue, func(_ context.Context, _ queue.Message) {
		atomic.AddInt32(&processed, 1)
	}, tick)

	for atomic.LoadInt32(&processed) < processedCount {
		tick <- time.Now()
		time.Sleep(10 * time.Millisecond)
	}

	if atomic.LoadInt32(&processed) < processedCount {
		t.Errorf("Expected at least %d messages processed, got %d", processedCount, atomic.LoadInt32(&processed))
	}
}

func TestRunWorkerLoop_PanicRecovery(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var panicCount int32
	tick := make(chan time.Time)

	mockQueue := &MockQueue{
		ReceiveMessagesFunc: func(_ context.Context, _ int32, _ int32) ([]queue.Message, error) {
			return []queue.Message{
				{ID: "msg-1", Body: `{"run_id": 123}`, Handle: "handle-1"},
			}, nil
		},
	}

	go RunWorkerLoopWithTicker(ctx, "test", mockQueue, func(_ context.Context, _ queue.Message) {
		atomic.AddInt32(&panicCount, 1)
		panic("test panic")
	}, tick)

	tick <- time.Now()
	time.Sleep(50 * time.Millisecond)

	tick <- time.Now()
	time.Sleep(50 * time.Millisecond)

	if atomic.LoadInt32(&panicCount) < 2 {
		t.Error("Worker should continue after panic")
	}
}

func TestRunWorkerLoop_ConcurrencyLimit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var concurrent int32
	var maxConcurrent int32
	var mu sync.Mutex
	tick := make(chan time.Time)

	processingStarted := make(chan struct{}, 20)
	allowProcessing := make(chan struct{})

	mockQueue := &MockQueue{
		ReceiveMessagesFunc: func(_ context.Context, _ int32, _ int32) ([]queue.Message, error) {
			messages := make([]queue.Message, 10)
			for i := 0; i < 10; i++ {
				messages[i] = queue.Message{
					ID:     fmt.Sprintf("msg-%d", i),
					Body:   `{"run_id": 123}`,
					Handle: "handle",
				}
			}
			return messages, nil
		},
	}

	go RunWorkerLoopWithTicker(ctx, "test", mockQueue, func(_ context.Context, _ queue.Message) {
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
	}, tick)

	tick <- time.Now()

	startedCount := 0
	for startedCount < 5 {
		select {
		case <-processingStarted:
			startedCount++
		case <-time.After(100 * time.Millisecond):
			t.Fatalf("Not enough processors started, got %d", startedCount)
		}
	}

	time.Sleep(10 * time.Millisecond)

	mu.Lock()
	maxConcurrentValue := maxConcurrent
	mu.Unlock()

	if maxConcurrentValue > 5 {
		t.Errorf("Max concurrent = %d, expected <= 5", maxConcurrentValue)
	}

	close(allowProcessing)
}

func TestRunWorkerLoop_GracefulShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	tick := make(chan time.Time)

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
		RunWorkerLoopWithTicker(ctx, "test", mockQueue, func(_ context.Context, _ queue.Message) {
			if atomic.CompareAndSwapInt32(&receivedOnce, 0, 1) {
				close(processingStarted)
				time.Sleep(50 * time.Millisecond)
				close(processingCompleted)
			}
		}, tick)
		close(workerDone)
	}()

	tick <- time.Now()

	select {
	case <-processingStarted:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Processing did not start")
	}

	cancel()

	select {
	case <-processingCompleted:
	case <-time.After(200 * time.Millisecond):
		t.Error("Processing did not complete before worker shutdown")
	}

	select {
	case <-workerDone:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("Worker did not shut down gracefully")
	}
}

func TestRunWorkerLoop_EmptyMessages(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tick := make(chan time.Time)

	mockQueue := &MockQueue{
		ReceiveMessagesFunc: func(_ context.Context, _ int32, _ int32) ([]queue.Message, error) {
			return []queue.Message{}, nil
		},
	}

	processorCalled := int32(0)

	go RunWorkerLoopWithTicker(ctx, "test", mockQueue, func(_ context.Context, _ queue.Message) {
		atomic.AddInt32(&processorCalled, 1)
	}, tick)

	for i := 0; i < 5; i++ {
		tick <- time.Now()
	}
	time.Sleep(50 * time.Millisecond)

	if atomic.LoadInt32(&processorCalled) > 0 {
		t.Error("Processor should not be called when no messages are received")
	}
}

func TestRunWorkerLoop_ReceiveError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tick := make(chan time.Time)

	errorCount := int32(0)

	mockQueue := &MockQueue{
		ReceiveMessagesFunc: func(_ context.Context, _ int32, _ int32) ([]queue.Message, error) {
			atomic.AddInt32(&errorCount, 1)
			return nil, context.DeadlineExceeded
		},
	}

	go RunWorkerLoopWithTicker(ctx, "test", mockQueue, func(_ context.Context, _ queue.Message) {}, tick)

	tick <- time.Now()
	tick <- time.Now()
	time.Sleep(50 * time.Millisecond)

	if atomic.LoadInt32(&errorCount) < 2 {
		t.Fatal("Worker loop did not continue after receive error")
	}
}

func TestRunWorkerLoop_ContextDeadline(t *testing.T) {
	deadline := time.Now().Add(10 * time.Second)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	tick := make(chan time.Time)

	deadlineVerified := int32(0)
	mockQueue := &MockQueue{
		ReceiveMessagesFunc: func(ctx context.Context, _ int32, _ int32) ([]queue.Message, error) {
			if _, ok := ctx.Deadline(); ok {
				atomic.StoreInt32(&deadlineVerified, 1)
			}
			return nil, nil
		},
	}

	go RunWorkerLoopWithTicker(ctx, "test", mockQueue, func(_ context.Context, _ queue.Message) {}, tick)

	tick <- time.Now()
	time.Sleep(50 * time.Millisecond)

	if atomic.LoadInt32(&deadlineVerified) != 1 {
		t.Error("Expected context to have a deadline")
	}
}
