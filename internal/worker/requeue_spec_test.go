package worker

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/Shavakan/runs-fleet/pkg/db"
	"github.com/Shavakan/runs-fleet/pkg/fleet"
	gh "github.com/Shavakan/runs-fleet/pkg/github"
	"github.com/Shavakan/runs-fleet/pkg/housekeeping"
	"github.com/Shavakan/runs-fleet/pkg/pools"
	"github.com/Shavakan/runs-fleet/pkg/queue"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

const compatibleInstanceID = "i-compatible"

type recoveryHotPoolDB struct{ mockDBClient }

func (*recoveryHotPoolDB) GetPoolConfig(context.Context, string) (*db.PoolConfig, error) {
	return &db.PoolConfig{AutoTune: &db.AutoTuneRec{RecommendedLingerMinutes: 15, RecommendedMaxHot: 1}}, nil
}

func TestRequeuedJobNeverRegistersARM64LabelOnIncompatibleWarmRunner(t *testing.T) {
	for _, state := range []ec2types.InstanceStateName{ec2types.InstanceStateNameStopped, ec2types.InstanceStateNameRunning} {
		for _, compatible := range []bool{false, true} {
			name := "incompatible only"
			if compatible {
				name = "compatible available"
			}
			t.Run(string(state)+"/"+name, func(t *testing.T) {
				instances := []ec2types.Instance{{
					InstanceId: aws.String("i-incompatible"), InstanceType: ec2types.InstanceTypeM6iLarge,
					State: &ec2types.InstanceState{Name: state},
				}}
				instances = append(instances,
					ec2types.Instance{InstanceId: aws.String("i-wrong-arch"), InstanceType: ec2types.InstanceTypeM7i2xlarge, State: &ec2types.InstanceState{Name: state}},
					ec2types.Instance{InstanceId: aws.String("i-too-small"), InstanceType: ec2types.InstanceTypeM7gLarge, State: &ec2types.InstanceState{Name: state}},
					ec2types.Instance{InstanceId: aws.String("i-too-large"), InstanceType: ec2types.InstanceTypeM7g8xlarge, State: &ec2types.InstanceState{Name: state}},
				)
				if compatible {
					instances = append(instances, ec2types.Instance{
						InstanceId: aws.String(compatibleInstanceID), InstanceType: ec2types.InstanceTypeM7g2xlarge,
						State: &ec2types.InstanceState{Name: state},
					})
				}
				assigner := createTestAssigner(instances)
				if state == ec2types.InstanceStateNameRunning {
					manager := pools.NewManager(&recoveryHotPoolDB{}, nil, &config.Config{HotPoolsEnabled: true, HotPoolCaps: config.DefaultHotPoolCaps()})
					manager.SetEC2Client(&mockEC2API{describeInstancesFunc: func(context.Context, *ec2.DescribeInstancesInput, ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
						return &ec2.DescribeInstancesOutput{Reservations: []ec2types.Reservation{{Instances: instances}}}, nil
					}})
					assigner.Pool = manager
				}
				preparer := &mockRunnerPreparer{}
				assigner.Runner = preparer
				job := housekeeping.BuildRequeueMessage(housekeeping.RequeueableJob{
					JobID: 123, RunID: 456, Repo: "owner/repo", Pool: "test-pool",
					InstanceType:  string(ec2types.InstanceTypeM7g2xlarge),
					OriginalLabel: "runs-fleet/cpu=8/arch=arm64/pool=test-pool",
				})
				result, err := assigner.TryAssignToWarmPool(context.Background(), job)
				if err != nil {
					t.Fatal(err)
				}
				if result.Assigned != compatible {
					t.Fatalf("assigned = %v, want %v", result.Assigned, compatible)
				}
				if compatible {
					if result.InstanceID != compatibleInstanceID {
						t.Fatalf("selected %s instead of compatible ARM64 runner", result.InstanceID)
					}
					if preparer.lastRequest.InstanceID != compatibleInstanceID {
						t.Fatalf("registered requested ARM64 label on %s", preparer.lastRequest.InstanceID)
					}
				} else if preparer.prepareCalled {
					t.Fatal("registered requested ARM64 label without a compatible runner")
				}
			})
		}
	}
}

func TestRecoveredJobColdStartRetainsRequestedPlacement(t *testing.T) {
	job := housekeeping.BuildRequeueMessage(housekeeping.RequeueableJob{
		JobID: 123, RunID: 456, Repo: "owner/repo", InstanceType: "m6i.large",
		OriginalLabel: "runs-fleet/cpu=8/arch=arm64/disk=256",
	})
	body, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	launched, prepared, acknowledged := false, false, false
	var index uint64
	processEC2Message(context.Background(), EC2WorkerDeps{
		Fleet:  &fleet.Manager{},
		Config: &config.Config{SubnetIDs: []string{"subnet-test"}}, SubnetIndex: &index,
		Queue: &mockQueueEC2{DeleteMessageFunc: func(context.Context, string) error { acknowledged = true; return nil }},
		CreateFleetFn: func(_ context.Context, spec *fleet.LaunchSpec) ([]string, error) {
			launched = true
			if spec.Arch != gh.ArchARM64 || spec.StorageGiB != 256 || !spec.ForceOnDemand || spec.Spot {
				t.Errorf("incorrect recovered launch: %+v", spec)
			}
			types := append([]string{spec.InstanceType}, spec.InstanceTypes...)
			for _, name := range types {
				got, ok := fleet.GetInstanceSpec(name)
				if !ok || got.Arch != gh.ArchARM64 || got.CPU < 8 || got.CPU > 16 {
					t.Errorf("incompatible launch type %s", name)
				}
			}
			return []string{compatibleInstanceID}, nil
		},
		PrepareRunnersFn: func(_ context.Context, got *queue.JobMessage, _ []string) []string {
			prepared = true
			if got.OriginalLabel != job.OriginalLabel || got.RetryCount != 1 {
				t.Errorf("recovery changed job identity: %+v", got)
			}
			return nil
		},
	}, queue.Message{Body: string(body), Handle: "receipt"})
	if !launched || !prepared || !acknowledged {
		t.Fatalf("launch=%v prepare=%v acknowledged=%v", launched, prepared, acknowledged)
	}
}

func TestInvalidRecoveredPlacementDoesNotLaunchOrAcknowledge(t *testing.T) {
	body, err := json.Marshal(&queue.JobMessage{RetryCount: 1, OriginalLabel: "runs-fleet/cpu=invalid"})
	if err != nil {
		t.Fatal(err)
	}
	processEC2Message(context.Background(), EC2WorkerDeps{
		Config: &config.Config{}, Fleet: &fleet.Manager{},
		Queue: &mockQueueEC2{DeleteMessageFunc: func(context.Context, string) error {
			t.Fatal("invalid placement acknowledged instead of retained for redelivery")
			return nil
		}},
		CreateFleetFn: func(context.Context, *fleet.LaunchSpec) ([]string, error) {
			t.Fatal("invalid placement reached fleet launch")
			return nil, nil
		},
	}, queue.Message{Body: string(body), Handle: "receipt"})
}

func TestAliasRecoveryWithOnlyInstanceTypeDoesNotRegisterRunner(t *testing.T) {
	assigner := createTestAssigner([]ec2types.Instance{{InstanceId: aws.String(compatibleInstanceID), InstanceType: ec2types.InstanceTypeM7g2xlarge, State: &ec2types.InstanceState{Name: ec2types.InstanceStateNameStopped}}})
	preparer := &mockRunnerPreparer{}
	assigner.Runner = preparer
	job := housekeeping.BuildRequeueMessage(housekeeping.RequeueableJob{JobID: 123, RunID: 456, Repo: "owner/repo", Pool: "test-pool", OriginalLabel: "custom-arm-runners", InstanceType: string(ec2types.InstanceTypeM7g2xlarge)})
	result, err := assigner.TryAssignToWarmPool(context.Background(), job)
	if err == nil || result != nil || preparer.prepareCalled {
		t.Fatalf("alias with unknown disk requirements dispatched: result=%+v error=%v registered=%v", result, err, preparer.prepareCalled)
	}
}

func TestAliasSpotFailureCanRetryWithResolvedPlacement(t *testing.T) {
	job := &queue.JobMessage{JobID: 123, RunID: 456, Repo: "owner/repo", OriginalLabel: "custom-arm-runners", InstanceType: string(ec2types.InstanceTypeM7g2xlarge), InstanceTypes: []string{string(ec2types.InstanceTypeM7g2xlarge)}, Arch: gh.ArchARM64, CPUMin: 8, CPUMax: 16, RAMMin: 32, RAMMax: 64, Families: []string{"m7g"}, Gen: 7, StorageGiB: 256, Spot: true}
	var fallback *queue.JobMessage
	calls, prepared := 0, false
	var index uint64
	deps := EC2WorkerDeps{
		Config: &config.Config{SubnetIDs: []string{"subnet-test"}}, SubnetIndex: &index, Fleet: &fleet.Manager{},
		Queue: &mockQueueEC2{SendMessageFunc: func(_ context.Context, got *queue.JobMessage) error { fallback = got; return nil }},
		CreateFleetFn: func(_ context.Context, spec *fleet.LaunchSpec) ([]string, error) {
			calls++
			if calls == 1 {
				return nil, errors.New("spot capacity unavailable")
			}
			if spec.Spot || !spec.ForceOnDemand || spec.Arch != gh.ArchARM64 || spec.StorageGiB != 256 {
				t.Errorf("incorrect on-demand launch: %+v", spec)
			}
			return []string{compatibleInstanceID}, nil
		},
		PrepareRunnersFn: func(_ context.Context, got *queue.JobMessage, _ []string) []string {
			prepared = true
			if got.CPUMin != 8 || got.CPUMax != 16 || got.RAMMin != 32 || got.RAMMax != 64 || got.Gen != 7 || len(got.Families) != 1 || got.Families[0] != "m7g" {
				t.Errorf("lost alias requirements: %+v", got)
			}
			return nil
		},
	}
	body, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	processEC2Message(context.Background(), deps, queue.Message{Body: string(body), Handle: "spot-receipt"})
	if fallback == nil {
		t.Fatal("spot failure did not queue fallback")
	}
	body, err = json.Marshal(fallback)
	if err != nil {
		t.Fatal(err)
	}
	processEC2Message(context.Background(), deps, queue.Message{Body: string(body), Handle: "on-demand-receipt"})
	if calls != 2 || !prepared {
		t.Fatalf("on-demand fallback did not launch: calls=%d prepared=%v", calls, prepared)
	}
	if !job.Spot || job.ForceOnDemand || job.RetryCount != 0 {
		t.Fatalf("source request was mutated: %+v", job)
	}
}

func TestAliasRecoveryColdStartRestoresConfiguredPlacement(t *testing.T) {
	job := housekeeping.BuildRequeueMessage(housekeeping.RequeueableJob{
		JobID: 123, RunID: 456, Repo: "owner/repo", InstanceType: "m6i.large",
		OriginalLabel: "custom-arm-runners",
	})
	body, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	launched, acknowledged := false, false
	var index uint64
	processEC2Message(context.Background(), EC2WorkerDeps{
		Fleet:  &fleet.Manager{},
		Config: &config.Config{SubnetIDs: []string{"subnet-test"}}, SubnetIndex: &index,
		LabelAliases: testAliasResolver(t),
		Queue:        &mockQueueEC2{DeleteMessageFunc: func(context.Context, string) error { acknowledged = true; return nil }},
		CreateFleetFn: func(_ context.Context, spec *fleet.LaunchSpec) ([]string, error) {
			launched = true
			if spec.Arch != gh.ArchARM64 || spec.StorageGiB != 256 || !spec.ForceOnDemand || spec.Spot {
				t.Errorf("incorrect alias recovery launch: %+v", spec)
			}
			return []string{compatibleInstanceID}, nil
		},
		PrepareRunnersFn: func(_ context.Context, got *queue.JobMessage, _ []string) []string {
			if got.OriginalLabel != "custom-arm-runners" {
				t.Errorf("alias recovery changed registration label: %q", got.OriginalLabel)
			}
			return nil
		},
	}, queue.Message{Body: string(body), Handle: "receipt"})
	if !launched || !acknowledged {
		t.Fatalf("launch=%v acknowledged=%v", launched, acknowledged)
	}
}

func TestAliasRecoveryWarmPoolSelectsCompatibleRunner(t *testing.T) {
	assigner := createTestAssigner([]ec2types.Instance{
		{InstanceId: aws.String("i-wrong-arch"), InstanceType: ec2types.InstanceTypeM7i2xlarge, State: &ec2types.InstanceState{Name: ec2types.InstanceStateNameStopped}},
		{InstanceId: aws.String(compatibleInstanceID), InstanceType: ec2types.InstanceTypeM7g2xlarge, State: &ec2types.InstanceState{Name: ec2types.InstanceStateNameStopped}},
	})
	assigner.LabelAliases = testAliasResolver(t)
	preparer := &mockRunnerPreparer{}
	assigner.Runner = preparer
	job := housekeeping.BuildRequeueMessage(housekeeping.RequeueableJob{JobID: 123, RunID: 456, Repo: "owner/repo", Pool: "test-pool", OriginalLabel: "custom-arm-runners", InstanceType: string(ec2types.InstanceTypeM7i2xlarge)})
	result, err := assigner.TryAssignToWarmPool(context.Background(), job)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Assigned || result.InstanceID != compatibleInstanceID || preparer.lastRequest.InstanceID != compatibleInstanceID {
		t.Fatalf("alias recovery assigned %+v, registered on %s", result, preparer.lastRequest.InstanceID)
	}
}
