package agent

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
)

const (
	// MaxBatchSize is the maximum number of log events per batch.
	MaxBatchSize = 10000
	// MaxBatchBytes is the maximum size of a batch in bytes (1MB with buffer).
	MaxBatchBytes = 1000000
	// MaxBufferedEvents is the maximum events to buffer before dropping oldest.
	MaxBufferedEvents = 50000
	// FlushInterval is how often to flush logs to CloudWatch.
	FlushInterval = 5 * time.Second
	// MaxBackoffDuration caps exponential backoff.
	MaxBackoffDuration = 8 * time.Second
)

// CloudWatchLogsAPI defines CloudWatch Logs operations.
type CloudWatchLogsAPI interface {
	CreateLogStream(ctx context.Context, params *cloudwatchlogs.CreateLogStreamInput, optFns ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.CreateLogStreamOutput, error)
	PutLogEvents(ctx context.Context, params *cloudwatchlogs.PutLogEventsInput, optFns ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.PutLogEventsOutput, error)
	DescribeLogStreams(ctx context.Context, params *cloudwatchlogs.DescribeLogStreamsInput, optFns ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.DescribeLogStreamsOutput, error)
}

// CloudWatchLogger streams logs to CloudWatch Logs.
type CloudWatchLogger struct {
	client       CloudWatchLogsAPI
	logGroup     string
	logStream    string
	logger       Logger
	events       []types.InputLogEvent
	eventsMu     sync.Mutex
	batchSize    int
	stopCh       chan struct{}
	doneCh       chan struct{}
	flushTimeout time.Duration
}

// NewCloudWatchLogger creates a new CloudWatch logger.
func NewCloudWatchLogger(cfg aws.Config, logGroup, logStream string, logger Logger) *CloudWatchLogger {
	return &CloudWatchLogger{
		client:       cloudwatchlogs.NewFromConfig(cfg),
		logGroup:     logGroup,
		logStream:    logStream,
		logger:       logger,
		events:       make([]types.InputLogEvent, 0, MaxBatchSize),
		stopCh:       make(chan struct{}),
		doneCh:       make(chan struct{}),
		flushTimeout: 30 * time.Second,
	}
}

// Start initializes the log stream and starts the background flusher.
func (c *CloudWatchLogger) Start(ctx context.Context) error {
	if err := c.createLogStream(ctx); err != nil {
		return fmt.Errorf("failed to create log stream: %w", err)
	}

	go c.flushLoop(ctx)
	return nil
}

// createLogStream creates the log stream if it doesn't exist.
func (c *CloudWatchLogger) createLogStream(ctx context.Context) error {
	_, err := c.client.CreateLogStream(ctx, &cloudwatchlogs.CreateLogStreamInput{
		LogGroupName:  aws.String(c.logGroup),
		LogStreamName: aws.String(c.logStream),
	})
	if err != nil {
		// Check if stream already exists
		if strings.Contains(err.Error(), "ResourceAlreadyExistsException") {
			return nil
		}
		return err
	}
	return nil
}

// Log adds a log message to the buffer.
func (c *CloudWatchLogger) Log(message string) {
	c.eventsMu.Lock()
	defer c.eventsMu.Unlock()

	// Drop oldest events if buffer is full to prevent unbounded growth
	if len(c.events) >= MaxBufferedEvents {
		dropCount := MaxBufferedEvents / 10 // Drop 10% when full
		c.events = c.events[dropCount:]
		// Recalculate batch size (approximate)
		c.batchSize = 0
		for _, e := range c.events {
			c.batchSize += len(*e.Message) + 26
		}
	}

	c.events = append(c.events, types.InputLogEvent{
		Message:   aws.String(message),
		Timestamp: aws.Int64(time.Now().UnixMilli()),
	})

	c.batchSize += len(message) + 26 // 26 bytes overhead per event

	// Signal flush if batch is full (flush loop handles actual flush)
	if len(c.events) >= MaxBatchSize || c.batchSize >= MaxBatchBytes {
		// Use bounded goroutine with timeout to avoid leaks
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		go func() {
			defer cancel()
			c.flush(ctx)
		}()
	}
}

// Printf implements Logger interface.
func (c *CloudWatchLogger) Printf(format string, v ...interface{}) {
	msg := fmt.Sprintf(format, v...)
	c.Log(msg)
	if c.logger != nil {
		c.logger.Printf(format, v...)
	}
}

// Println implements Logger interface.
func (c *CloudWatchLogger) Println(v ...interface{}) {
	msg := fmt.Sprintln(v...)
	c.Log(strings.TrimSuffix(msg, "\n"))
	if c.logger != nil {
		c.logger.Println(v...)
	}
}

// flushLoop periodically flushes logs to CloudWatch.
func (c *CloudWatchLogger) flushLoop(ctx context.Context) {
	defer close(c.doneCh)

	ticker := time.NewTicker(FlushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			c.flush(context.Background())
			return
		case <-c.stopCh:
			c.flush(context.Background())
			return
		case <-ticker.C:
			c.flush(ctx)
		}
	}
}

// flush sends buffered events to CloudWatch.
func (c *CloudWatchLogger) flush(ctx context.Context) {
	c.eventsMu.Lock()
	if len(c.events) == 0 {
		c.eventsMu.Unlock()
		return
	}

	events := c.events
	c.events = make([]types.InputLogEvent, 0, MaxBatchSize)
	c.batchSize = 0
	c.eventsMu.Unlock()

	// Sort events by timestamp (required by CloudWatch)
	sort.Slice(events, func(i, j int) bool {
		return *events[i].Timestamp < *events[j].Timestamp
	})

	// Retry up to 3 times with capped exponential backoff
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(1<<uint(attempt)) * time.Second
			if backoff > MaxBackoffDuration {
				backoff = MaxBackoffDuration
			}
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return
			}
		}

		_, err := c.client.PutLogEvents(ctx, &cloudwatchlogs.PutLogEventsInput{
			LogGroupName:  aws.String(c.logGroup),
			LogStreamName: aws.String(c.logStream),
			LogEvents:     events,
		})
		if err != nil {
			lastErr = err
			if c.logger != nil {
				c.logger.Printf("Failed to put log events (attempt %d/3): %v", attempt+1, err)
			}
			continue
		}
		return
	}

	if c.logger != nil && lastErr != nil {
		c.logger.Printf("Failed to flush logs after 3 attempts: %v", lastErr)
	}
}

// Stop stops the background flusher and flushes remaining logs.
func (c *CloudWatchLogger) Stop() {
	close(c.stopCh)

	select {
	case <-c.doneCh:
	case <-time.After(c.flushTimeout):
		if c.logger != nil {
			c.logger.Println("CloudWatch logger flush timeout")
		}
	}
}

// StreamReader streams output from a reader to CloudWatch.
func (c *CloudWatchLogger) StreamReader(ctx context.Context, r io.Reader, prefix string) {
	buf := make([]byte, 4096)
	var lineBuffer strings.Builder

	for {
		select {
		case <-ctx.Done():
			// Flush remaining line buffer
			if lineBuffer.Len() > 0 {
				c.Log(fmt.Sprintf("[%s] %s", prefix, lineBuffer.String()))
			}
			return
		default:
		}

		n, err := r.Read(buf)
		if n > 0 {
			data := string(buf[:n])
			lines := strings.Split(data, "\n")

			for i, line := range lines {
				if i == 0 {
					lineBuffer.WriteString(line)
				} else {
					if lineBuffer.Len() > 0 {
						c.Log(fmt.Sprintf("[%s] %s", prefix, lineBuffer.String()))
						lineBuffer.Reset()
					}
					if i < len(lines)-1 || (i == len(lines)-1 && line != "") {
						if line != "" {
							lineBuffer.WriteString(line)
						}
					}
				}
			}
		}

		if err != nil {
			if err != io.EOF {
				c.Log(fmt.Sprintf("[%s] Read error: %v", prefix, err))
			}
			// Flush remaining line buffer
			if lineBuffer.Len() > 0 {
				c.Log(fmt.Sprintf("[%s] %s", prefix, lineBuffer.String()))
			}
			return
		}
	}
}
