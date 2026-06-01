package worker

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/Shavakan/runs-fleet/pkg/db"
	"github.com/Shavakan/runs-fleet/pkg/fleet"
	"github.com/Shavakan/runs-fleet/pkg/metrics"
	"github.com/Shavakan/runs-fleet/pkg/queue"
	"github.com/Shavakan/runs-fleet/pkg/runner"
)

func TestPrepareRunners_ReturnsFailedInstanceIDs(t *testing.T) {
	preparer := &mockRunnerPreparer{
		prepareFunc: func(_ context.Context, req runner.PrepareRunnerRequest) error {
			if req.InstanceID == "i-bad" {
				return errors.New("ssm throttled")
			}
			return nil
		},
	}

	failed := PrepareRunners(context.Background(), preparer,
		&queue.JobMessage{JobID: 1, RunID: 2, Repo: "owner/repo"},
		[]string{"i-ok", "i-bad"})

	if len(failed) != 1 || failed[0] != "i-bad" {
		t.Fatalf("PrepareRunners() failed = %v, want [i-bad]", failed)
	}
}

func TestPrepareRunners_AllSucceedReturnsNil(t *testing.T) {
	failed := PrepareRunners(context.Background(), &mockRunnerPreparer{},
		&queue.JobMessage{Repo: "owner/repo"}, []string{"i-1", "i-2"})
	if len(failed) != 0 {
		t.Fatalf("PrepareRunners() failed = %v, want none", failed)
	}
}

func TestProcessEC2Message_PrepFailure_ReleasesClaimAndLeavesMessage(t *testing.T) {
	job := queue.JobMessage{JobID: 12345, RunID: 67890, Repo: "owner/repo", InstanceType: "t3.micro"}
	jobBytes, _ := json.Marshal(job)

	mockDynamo := &mockDynamoForClaim{}
	dbClient := db.NewClientWithAPI(mockDynamo, "pools", "jobs")

	deleteMsgCalled := false
	var subnetIndex uint64
	deps := EC2WorkerDeps{
		Queue: &mockQueueEC2{
			DeleteMessageFunc: func(_ context.Context, _ string) error {
				deleteMsgCalled = true
				return nil
			},
		},
		Fleet:       &fleet.Manager{},
		Metrics:     metrics.NoopPublisher{},
		DB:          dbClient,
		Config:      &config.Config{SubnetIDs: []string{"subnet-a"}},
		SubnetIndex: &subnetIndex,
		CreateFleetFn: func(_ context.Context, _ *fleet.LaunchSpec) ([]string, error) {
			return []string{"i-launched"}, nil
		},
		PrepareRunnersFn: func(_ context.Context, _ *queue.JobMessage, ids []string) []string {
			return ids // every instance failed preparation
		},
	}

	msg := queue.Message{ID: "msg-1", Body: string(jobBytes), Handle: "handle-1"}
	processEC2Message(context.Background(), deps, msg)

	if !mockDynamo.deleteCalled {
		t.Error("expected DeleteJobClaim on prep failure so redelivery can re-claim the job")
	}
	if deleteMsgCalled {
		t.Error("SQS message must NOT be deleted on prep failure (left for redelivery)")
	}
}

func TestProcessJobDirect_PrepFailure_ReleasesClaimAndReturnsFalse(t *testing.T) {
	mockDynamo := &mockDynamoForClaim{}
	dbClient := db.NewClientWithAPI(mockDynamo, "pools", "jobs")

	var subnetIndex uint64
	p := &DirectProcessor{
		Fleet:       &fleet.Manager{},
		Metrics:     metrics.NoopPublisher{},
		DB:          dbClient,
		Config:      &config.Config{SubnetIDs: []string{"subnet-a"}},
		SubnetIndex: &subnetIndex,
		CreateFleetFn: func(_ context.Context, _ *fleet.LaunchSpec) ([]string, error) {
			return []string{"i-launched"}, nil
		},
		PrepareRunnersFn: func(_ context.Context, _ *queue.JobMessage, ids []string) []string {
			return ids
		},
	}

	job := &queue.JobMessage{JobID: 999, RunID: 111, Repo: "owner/repo", InstanceType: "t3.micro"}
	if p.ProcessJobDirect(context.Background(), job) {
		t.Error("ProcessJobDirect must return false on prep failure (defer to queue)")
	}
	if !mockDynamo.deleteCalled {
		t.Error("expected DeleteJobClaim on prep failure so the queue worker can re-claim the job")
	}
}
