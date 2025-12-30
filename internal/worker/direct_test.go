package worker

import (
	"context"
	"testing"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/Shavakan/runs-fleet/pkg/queue"
)

func TestTryDirectProcessing_NilProcessor(t *testing.T) {
	sem := make(chan struct{}, 5)
	job := &queue.JobMessage{JobID: 12345}

	// Should not panic with nil processor
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("TryDirectProcessing panicked with nil processor: %v", r)
		}
	}()
	TryDirectProcessing(context.Background(), nil, sem, job)
}

func TestTryDirectProcessing_NilSemaphore(t *testing.T) {
	processor := &DirectProcessor{
		Config: &config.Config{},
	}
	job := &queue.JobMessage{JobID: 12345}

	// Should not panic with nil semaphore
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("TryDirectProcessing panicked with nil semaphore: %v", r)
		}
	}()
	TryDirectProcessing(context.Background(), processor, nil, job)
}

func TestTryDirectProcessing_BothNil(t *testing.T) {
	job := &queue.JobMessage{JobID: 12345}

	// Should not panic with both nil
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("TryDirectProcessing panicked with both nil: %v", r)
		}
	}()
	TryDirectProcessing(context.Background(), nil, nil, job)
}

func TestTryDirectProcessing_AtCapacity(t *testing.T) {
	processor := &DirectProcessor{
		Config: &config.Config{},
	}

	// Fill semaphore to capacity
	sem := make(chan struct{}, 1)
	sem <- struct{}{}

	job := &queue.JobMessage{JobID: 12345}

	// Should not block - will just log and return
	done := make(chan struct{})
	go func() {
		TryDirectProcessing(context.Background(), processor, sem, job)
		close(done)
	}()

	select {
	case <-done:
		// Success - didn't block
	case <-time.After(100 * time.Millisecond):
		t.Fatal("TryDirectProcessing blocked when at capacity")
	}
}

func TestDirectProcessor_ZeroValues(t *testing.T) {
	// Test that zero-value DirectProcessor struct compiles and has expected defaults
	processor := DirectProcessor{}

	if processor.Fleet != nil {
		t.Error("Expected Fleet to be nil for zero value")
	}
	if processor.Pool != nil {
		t.Error("Expected Pool to be nil for zero value")
	}
	if processor.Metrics != nil {
		t.Error("Expected Metrics to be nil for zero value")
	}
	if processor.Runner != nil {
		t.Error("Expected Runner to be nil for zero value")
	}
	if processor.DB != nil {
		t.Error("Expected DB to be nil for zero value")
	}
	if processor.Config != nil {
		t.Error("Expected Config to be nil for zero value")
	}
	if processor.SubnetIndex != nil {
		t.Error("Expected SubnetIndex to be nil for zero value")
	}
}
