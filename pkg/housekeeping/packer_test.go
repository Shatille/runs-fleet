package housekeeping

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/config"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

func TestExecuteOrphanedPackerInstances_NoOrphans(t *testing.T) {
	t.Parallel()

	youngLaunch := time.Now().Add(-5 * time.Minute)
	ec2Client := &mockEC2API{
		instances: []ec2types.Reservation{
			{
				Instances: []ec2types.Instance{
					{
						InstanceId: strPtr("i-young-1"),
						LaunchTime: &youngLaunch,
						State:      &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning},
					},
				},
			},
		},
	}
	metrics := &mockTaskMetricsAPI{}

	tasks := &Tasks{
		ec2Client: ec2Client,
		metrics:   metrics,
		config:    &config.Config{},
	}

	if err := tasks.ExecuteOrphanedPackerInstances(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if ec2Client.terminateCalls != 0 {
		t.Errorf("expected 0 terminate calls, got %d", ec2Client.terminateCalls)
	}
	if metrics.orphanedCount != 0 {
		t.Errorf("expected metric count 0, got %d", metrics.orphanedCount)
	}
}

func TestExecuteOrphanedPackerInstances_WithOrphans(t *testing.T) {
	t.Parallel()

	oldLaunch := time.Now().Add(-2 * time.Hour)
	youngLaunch := time.Now().Add(-5 * time.Minute)
	ec2Client := &mockEC2API{
		instances: []ec2types.Reservation{
			{
				Instances: []ec2types.Instance{
					{
						InstanceId: strPtr("i-orphan-1"),
						LaunchTime: &oldLaunch,
						State:      &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning},
					},
					{
						InstanceId: strPtr("i-young-1"),
						LaunchTime: &youngLaunch,
						State:      &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning},
					},
				},
			},
		},
	}
	metrics := &mockTaskMetricsAPI{}

	tasks := &Tasks{
		ec2Client: ec2Client,
		metrics:   metrics,
		config:    &config.Config{},
	}

	if err := tasks.ExecuteOrphanedPackerInstances(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if ec2Client.terminateCalls != 1 {
		t.Fatalf("expected 1 terminate call, got %d", ec2Client.terminateCalls)
	}
	if len(ec2Client.terminatedIDs) != 1 || ec2Client.terminatedIDs[0] != "i-orphan-1" {
		t.Errorf("expected to terminate 'i-orphan-1', got %v", ec2Client.terminatedIDs)
	}
	if metrics.orphanedCount != 1 {
		t.Errorf("expected metric count 1, got %d", metrics.orphanedCount)
	}
}

func TestExecuteOrphanedPackerInstances_ScansStoppedState(t *testing.T) {
	t.Parallel()

	// A workflow killed after packer stops the builder for AMI creation leaves it
	// in "stopped" state; the reaper must scan that state to catch the leak, while
	// the 1h LaunchTime guard still spares a builder legitimately stopped for an
	// in-progress snapshot.
	oldLaunch := time.Now().Add(-2 * time.Hour)
	youngLaunch := time.Now().Add(-5 * time.Minute)
	ec2Client := &mockEC2API{
		instances: []ec2types.Reservation{
			{
				Instances: []ec2types.Instance{
					{
						InstanceId: strPtr("i-stopped-builder"),
						LaunchTime: &oldLaunch,
						State:      &ec2types.InstanceState{Name: ec2types.InstanceStateNameStopped},
					},
					{
						InstanceId: strPtr("i-stopped-young"),
						LaunchTime: &youngLaunch,
						State:      &ec2types.InstanceState{Name: ec2types.InstanceStateNameStopped},
					},
				},
			},
		},
	}
	metrics := &mockTaskMetricsAPI{}

	tasks := &Tasks{
		ec2Client: ec2Client,
		metrics:   metrics,
		config:    &config.Config{},
	}

	if err := tasks.ExecuteOrphanedPackerInstances(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	states := instanceStateFilter(t, ec2Client.describeInput)
	for _, want := range []string{"stopping", "stopped"} {
		if !slices.Contains(states, want) {
			t.Errorf("expected describe filter to include %q state, got %v", want, states)
		}
	}

	if ec2Client.terminateCalls != 1 || len(ec2Client.terminatedIDs) != 1 || ec2Client.terminatedIDs[0] != "i-stopped-builder" {
		t.Errorf("expected to terminate only 'i-stopped-builder', got calls=%d ids=%v",
			ec2Client.terminateCalls, ec2Client.terminatedIDs)
	}
}

func TestExecuteOrphanedPackerInstances_DescribeError(t *testing.T) {
	t.Parallel()

	ec2Client := &mockEC2API{describeErr: errors.New("describe failed")}
	tasks := &Tasks{ec2Client: ec2Client, config: &config.Config{}}

	if err := tasks.ExecuteOrphanedPackerInstances(context.Background()); err == nil {
		t.Fatal("expected error from describe")
	}
	if ec2Client.terminateCalls != 0 {
		t.Errorf("expected no terminate calls on describe failure, got %d", ec2Client.terminateCalls)
	}
}

func TestExecuteOrphanedPackerInstances_TerminateError(t *testing.T) {
	t.Parallel()

	oldLaunch := time.Now().Add(-2 * time.Hour)
	ec2Client := &mockEC2API{
		instances: []ec2types.Reservation{
			{
				Instances: []ec2types.Instance{
					{
						InstanceId: strPtr("i-orphan-1"),
						LaunchTime: &oldLaunch,
						State:      &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning},
					},
				},
			},
		},
		terminateErr: errors.New("terminate failed"),
	}
	metrics := &mockTaskMetricsAPI{}

	tasks := &Tasks{
		ec2Client: ec2Client,
		metrics:   metrics,
		config:    &config.Config{},
	}

	if err := tasks.ExecuteOrphanedPackerInstances(context.Background()); err == nil {
		t.Fatal("expected error from terminate")
	}
	if metrics.orphanedCount != 0 {
		t.Errorf("expected metric count 0 on terminate failure, got %d", metrics.orphanedCount)
	}
}
