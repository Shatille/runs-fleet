package agent

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
)

// mockCloudWatchLogsAPI is a mock implementation of CloudWatchLogsAPI.
type mockCloudWatchLogsAPI struct {
	createLogStreamFunc    func(ctx context.Context, params *cloudwatchlogs.CreateLogStreamInput, optFns ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.CreateLogStreamOutput, error)
	putLogEventsFunc       func(ctx context.Context, params *cloudwatchlogs.PutLogEventsInput, optFns ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.PutLogEventsOutput, error)
	describeLogStreamsFunc func(ctx context.Context, params *cloudwatchlogs.DescribeLogStreamsInput, optFns ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.DescribeLogStreamsOutput, error)

	mu              sync.Mutex
	logEvents       []types.InputLogEvent
	createStreamErr error
}

func (m *mockCloudWatchLogsAPI) CreateLogStream(ctx context.Context, params *cloudwatchlogs.CreateLogStreamInput, optFns ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.CreateLogStreamOutput, error) {
	if m.createLogStreamFunc != nil {
		return m.createLogStreamFunc(ctx, params, optFns...)
	}
	if m.createStreamErr != nil {
		return nil, m.createStreamErr
	}
	return &cloudwatchlogs.CreateLogStreamOutput{}, nil
}

func (m *mockCloudWatchLogsAPI) PutLogEvents(ctx context.Context, params *cloudwatchlogs.PutLogEventsInput, optFns ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.PutLogEventsOutput, error) {
	if m.putLogEventsFunc != nil {
		return m.putLogEventsFunc(ctx, params, optFns...)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.logEvents = append(m.logEvents, params.LogEvents...)
	return &cloudwatchlogs.PutLogEventsOutput{}, nil
}

func (m *mockCloudWatchLogsAPI) DescribeLogStreams(ctx context.Context, params *cloudwatchlogs.DescribeLogStreamsInput, optFns ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.DescribeLogStreamsOutput, error) {
	if m.describeLogStreamsFunc != nil {
		return m.describeLogStreamsFunc(ctx, params, optFns...)
	}
	return &cloudwatchlogs.DescribeLogStreamsOutput{}, nil
}

func (m *mockCloudWatchLogsAPI) getLogEvents() []types.InputLogEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.logEvents
}

// noopLogger is a silent logger for CloudWatch tests that don't need message capture
type noopLogger struct{}

func (m *noopLogger) Printf(_ string, _ ...interface{}) {}

func (m *noopLogger) Println(_ ...interface{}) {}

func TestCloudWatchLogger_Log(t *testing.T) {
	mock := &mockCloudWatchLogsAPI{}
	logger := &CloudWatchLogger{
		client:       mock,
		logGroup:     "test-group",
		logStream:    "test-stream",
		logger:       &noopLogger{},
		events:       make([]types.InputLogEvent, 0, MaxBatchSize),
		stopCh:       make(chan struct{}),
		doneCh:       make(chan struct{}),
		flushTimeout: 5 * time.Second,
	}

	// Log some messages
	logger.Log("test message 1")
	logger.Log("test message 2")
	logger.Log("test message 3")

	// Check events are buffered
	logger.eventsMu.Lock()
	eventCount := len(logger.events)
	logger.eventsMu.Unlock()

	if eventCount != 3 {
		t.Errorf("expected 3 buffered events, got %d", eventCount)
	}
}

func TestCloudWatchLogger_Flush(t *testing.T) {
	mock := &mockCloudWatchLogsAPI{}
	logger := &CloudWatchLogger{
		client:       mock,
		logGroup:     "test-group",
		logStream:    "test-stream",
		logger:       &noopLogger{},
		events:       make([]types.InputLogEvent, 0, MaxBatchSize),
		stopCh:       make(chan struct{}),
		doneCh:       make(chan struct{}),
		flushTimeout: 5 * time.Second,
	}

	// Log messages
	logger.Log("flush test 1")
	logger.Log("flush test 2")

	// Manually flush
	logger.flush(context.Background())

	// Check events were sent to CloudWatch
	events := mock.getLogEvents()
	if len(events) != 2 {
		t.Errorf("expected 2 flushed events, got %d", len(events))
	}

	// Check buffer is cleared
	logger.eventsMu.Lock()
	remainingEvents := len(logger.events)
	logger.eventsMu.Unlock()

	if remainingEvents != 0 {
		t.Errorf("expected 0 remaining events after flush, got %d", remainingEvents)
	}
}

func TestCloudWatchLogger_Printf(t *testing.T) {
	mock := &mockCloudWatchLogsAPI{}
	logger := &CloudWatchLogger{
		client:       mock,
		logGroup:     "test-group",
		logStream:    "test-stream",
		logger:       &noopLogger{},
		events:       make([]types.InputLogEvent, 0, MaxBatchSize),
		stopCh:       make(chan struct{}),
		doneCh:       make(chan struct{}),
		flushTimeout: 5 * time.Second,
	}

	logger.Printf("formatted %s %d", "message", 42)

	logger.eventsMu.Lock()
	eventCount := len(logger.events)
	var message string
	if eventCount > 0 {
		message = *logger.events[0].Message
	}
	logger.eventsMu.Unlock()

	if eventCount != 1 {
		t.Errorf("expected 1 event, got %d", eventCount)
	}

	if message != "formatted message 42" {
		t.Errorf("expected 'formatted message 42', got '%s'", message)
	}
}

func TestCloudWatchLogger_Println(t *testing.T) {
	mock := &mockCloudWatchLogsAPI{}
	logger := &CloudWatchLogger{
		client:       mock,
		logGroup:     "test-group",
		logStream:    "test-stream",
		logger:       &noopLogger{},
		events:       make([]types.InputLogEvent, 0, MaxBatchSize),
		stopCh:       make(chan struct{}),
		doneCh:       make(chan struct{}),
		flushTimeout: 5 * time.Second,
	}

	logger.Println("println test")

	logger.eventsMu.Lock()
	eventCount := len(logger.events)
	var message string
	if eventCount > 0 {
		message = *logger.events[0].Message
	}
	logger.eventsMu.Unlock()

	if eventCount != 1 {
		t.Errorf("expected 1 event, got %d", eventCount)
	}

	if message != "println test" {
		t.Errorf("expected 'println test', got '%s'", message)
	}
}

func TestCloudWatchLogger_StreamReader(t *testing.T) {
	mock := &mockCloudWatchLogsAPI{}
	logger := &CloudWatchLogger{
		client:       mock,
		logGroup:     "test-group",
		logStream:    "test-stream",
		logger:       &noopLogger{},
		events:       make([]types.InputLogEvent, 0, MaxBatchSize),
		stopCh:       make(chan struct{}),
		doneCh:       make(chan struct{}),
		flushTimeout: 5 * time.Second,
	}

	input := "line 1\nline 2\nline 3"
	reader := strings.NewReader(input)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		logger.StreamReader(ctx, reader, "test")
		close(done)
	}()

	// Wait for reader to finish (it will finish when EOF is reached)
	<-done
	cancel()

	logger.eventsMu.Lock()
	eventCount := len(logger.events)
	logger.eventsMu.Unlock()

	if eventCount != 3 {
		t.Errorf("expected 3 events from stream, got %d", eventCount)
	}
}

func TestCloudWatchLogger_FlushOnStop(t *testing.T) {
	mock := &mockCloudWatchLogsAPI{}
	logger := &CloudWatchLogger{
		client:       mock,
		logGroup:     "test-group",
		logStream:    "test-stream",
		logger:       &noopLogger{},
		events:       make([]types.InputLogEvent, 0, MaxBatchSize),
		stopCh:       make(chan struct{}),
		doneCh:       make(chan struct{}),
		flushTimeout: 5 * time.Second,
	}

	// Start flush loop
	ctx, cancel := context.WithCancel(context.Background())
	go logger.flushLoop(ctx)

	// Log some messages
	logger.Log("stop test 1")
	logger.Log("stop test 2")

	// Stop the logger
	cancel()

	// Wait for done
	select {
	case <-logger.doneCh:
		// Expected
	case <-time.After(2 * time.Second):
		t.Error("timeout waiting for flush loop to stop")
	}

	// Check events were flushed
	events := mock.getLogEvents()
	if len(events) != 2 {
		t.Errorf("expected 2 events flushed on stop, got %d", len(events))
	}
}
