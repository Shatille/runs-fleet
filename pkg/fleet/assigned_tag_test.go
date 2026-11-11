package fleet

import (
	"context"
	"testing"

	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

func hasAssignedTag(tags []types.Tag) bool {
	for _, tg := range tags {
		if aws.ToString(tg.Key) == TagInstanceAssigned && aws.ToString(tg.Value) == "true" {
			return true
		}
	}
	return false
}

func TestCreateFleet_TagsInstanceAssigned(t *testing.T) {
	tests := []struct {
		name   string
		config *config.Config
		spec   *LaunchSpec
	}{
		{
			name:   "on-demand pool job",
			config: &config.Config{},
			spec:   &LaunchSpec{RunID: 1, InstanceType: "t4g.medium", SubnetID: "subnet-1", Pool: "test"},
		},
		{
			name:   "spot pool job",
			config: &config.Config{SpotEnabled: true},
			spec:   &LaunchSpec{RunID: 1, InstanceType: "t4g.medium", SubnetID: "subnet-1", Pool: "test", Spot: true},
		},
		{
			name:   "job without a pool",
			config: &config.Config{},
			spec:   &LaunchSpec{RunID: 1, InstanceType: "t4g.medium", SubnetID: "subnet-1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var launchTags, fallbackTags []types.Tag
			mock := &mockEC2Client{
				CreateFleetFunc: func(_ context.Context, params *ec2.CreateFleetInput, _ ...func(*ec2.Options)) (*ec2.CreateFleetOutput, error) {
					for _, ts := range params.TagSpecifications {
						launchTags = append(launchTags, ts.Tags...)
					}
					return &ec2.CreateFleetOutput{
						Instances: []types.CreateFleetInstance{{InstanceIds: []string{testInstanceID}}},
					}, nil
				},
				CreateTagsFunc: func(_ context.Context, params *ec2.CreateTagsInput, _ ...func(*ec2.Options)) (*ec2.CreateTagsOutput, error) {
					fallbackTags = params.Tags
					return &ec2.CreateTagsOutput{}, nil
				},
			}
			manager := &Manager{ec2Client: mock, config: tt.config}

			if _, err := manager.CreateFleet(context.Background(), tt.spec); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if !hasAssignedTag(launchTags) {
				t.Errorf("CreateFleet TagSpecifications must carry %s=true", TagInstanceAssigned)
			}
			if !hasAssignedTag(fallbackTags) {
				t.Errorf("CreateTags fallback must carry %s=true", TagInstanceAssigned)
			}
		})
	}
}

func TestCreateOnDemandInstance_LeavesSpareUnassigned(t *testing.T) {
	var launchTags []types.Tag
	mock := &mockEC2Client{
		RunInstancesFunc: func(_ context.Context, params *ec2.RunInstancesInput, _ ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error) {
			for _, ts := range params.TagSpecifications {
				launchTags = append(launchTags, ts.Tags...)
			}
			return &ec2.RunInstancesOutput{
				Instances: []types.Instance{{InstanceId: aws.String(testInstanceID)}},
			}, nil
		},
	}
	manager := &Manager{ec2Client: mock, config: &config.Config{LaunchTemplateName: "runs-fleet-runner"}}
	spec := &LaunchSpec{RunID: 1, InstanceType: "t4g.medium", SubnetID: "subnet-1", Pool: "test", Arch: "arm64"}

	if _, err := manager.CreateOnDemandInstance(context.Background(), spec); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(launchTags) == 0 {
		t.Fatal("expected launch tags on the pool spare")
	}
	if hasAssignedTag(launchTags) {
		t.Errorf("a pool spare must not carry %s, got %v", TagInstanceAssigned, launchTags)
	}
}
