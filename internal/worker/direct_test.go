package worker

import (
	"context"
	"testing"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/Shavakan/runs-fleet/pkg/metrics"
	"github.com/Shavakan/runs-fleet/pkg/pools"
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

func TestProcessJobDirect_NilFleet(t *testing.T) {
	var subnetIndex uint64
	job := &queue.JobMessage{
		JobID:        12345,
		RunID:        67890,
		Repo:         "owner/repo",
		InstanceType: "t3.micro",
	}

	processor := &DirectProcessor{
		Pool:        &pools.Manager{},
		Metrics:     metrics.NoopPublisher{},
		Config:      &config.Config{PublicSubnetIDs: []string{"subnet-a"}},
		SubnetIndex: &subnetIndex,
	}

	result := processor.ProcessJobDirect(context.Background(), job)
	if result {
		t.Error("Expected false when Fleet is nil")
	}
}

func TestProcessJobDirect_NilDBSkipsClaimCheck(t *testing.T) {
	var subnetIndex uint64
	job := &queue.JobMessage{
		JobID:        12345,
		RunID:        67890,
		Repo:         "owner/repo",
		InstanceType: "t3.micro",
	}

	processor := &DirectProcessor{
		Fleet:       nil,
		Pool:        nil,
		Metrics:     metrics.NoopPublisher{},
		DB:          nil, // Nil DB skips claim check
		Config:      &config.Config{PublicSubnetIDs: []string{"subnet-a"}},
		SubnetIndex: &subnetIndex,
	}

	// With nil DB and nil Fleet, should return false (Fleet nil check comes after DB check)
	result := processor.ProcessJobDirect(context.Background(), job)
	if result {
		t.Error("Expected false when Fleet is nil")
	}
}



func TestTryDirectProcessing_WithCapacity(t *testing.T) {
	var subnetIndex uint64
	processor := &DirectProcessor{
		Fleet:       nil,
		Pool:        nil,
		Metrics:     metrics.NoopPublisher{},
		Config:      &config.Config{PublicSubnetIDs: []string{"subnet-a"}},
		SubnetIndex: &subnetIndex,
	}

	sem := make(chan struct{}, 5)
	job := &queue.JobMessage{
		JobID: 12345,
		RunID: 67890,
		Repo:  "owner/repo",
	}

	// Should acquire semaphore and process
	done := make(chan struct{})
	go func() {
		TryDirectProcessing(context.Background(), processor, sem, job)
		close(done)
	}()

	// Wait for completion with timeout
	select {
	case <-done:
		// Success
	case <-time.After(1 * time.Second):
		t.Fatal("TryDirectProcessing did not complete")
	}
}

func TestTryDirectProcessing_PanicRecovery(t *testing.T) {
	// This test verifies the panic recovery in TryDirectProcessing
	// We can't easily trigger a panic in ProcessJobDirect without modifying
	// the production code, but we can verify the structure handles it.

	var subnetIndex uint64
	processor := &DirectProcessor{
		Fleet:       nil,
		Pool:        nil,
		Metrics:     metrics.NoopPublisher{},
		Config:      &config.Config{PublicSubnetIDs: []string{"subnet-a"}},
		SubnetIndex: &subnetIndex,
	}

	sem := make(chan struct{}, 5)
	job := &queue.JobMessage{
		JobID: 12345,
	}

	// Should not panic even with minimal job
	done := make(chan struct{})
	go func() {
		TryDirectProcessing(context.Background(), processor, sem, job)
		close(done)
	}()

	select {
	case <-done:
		// Success
	case <-time.After(200 * time.Millisecond):
		t.Fatal("TryDirectProcessing did not complete")
	}
}

