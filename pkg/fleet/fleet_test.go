package fleet

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/circuit"
	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

const testInstanceID = "i-123456789"

//nolint:dupl // Intentionally mirrors EC2API interface for testing
type mockEC2Client struct {
	CreateFleetFunc                   func(ctx context.Context, params *ec2.CreateFleetInput, optFns ...func(*ec2.Options)) (*ec2.CreateFleetOutput, error)
	CreateTagsFunc                    func(ctx context.Context, params *ec2.CreateTagsInput, optFns ...func(*ec2.Options)) (*ec2.CreateTagsOutput, error)
	DeleteFleetsFunc                  func(ctx context.Context, params *ec2.DeleteFleetsInput, optFns ...func(*ec2.Options)) (*ec2.DeleteFleetsOutput, error)
	DescribeFleetInstancesFunc        func(ctx context.Context, params *ec2.DescribeFleetInstancesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeFleetInstancesOutput, error)
	DescribeSpotPriceHistoryFunc      func(ctx context.Context, params *ec2.DescribeSpotPriceHistoryInput, optFns ...func(*ec2.Options)) (*ec2.DescribeSpotPriceHistoryOutput, error)
	DescribeInstanceTypeOfferingsFunc func(ctx context.Context, params *ec2.DescribeInstanceTypeOfferingsInput, optFns ...func(*ec2.Options)) (*ec2.DescribeInstanceTypeOfferingsOutput, error)
	DescribeInstancesFunc             func(ctx context.Context, params *ec2.DescribeInstancesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
	RunInstancesFunc                  func(ctx context.Context, params *ec2.RunInstancesInput, optFns ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error)
}

func (m *mockEC2Client) CreateFleet(ctx context.Context, params *ec2.CreateFleetInput, optFns ...func(*ec2.Options)) (*ec2.CreateFleetOutput, error) {
	if m.CreateFleetFunc != nil {
		return m.CreateFleetFunc(ctx, params, optFns...)
	}
	return &ec2.CreateFleetOutput{}, nil
}

func (m *mockEC2Client) CreateTags(ctx context.Context, params *ec2.CreateTagsInput, optFns ...func(*ec2.Options)) (*ec2.CreateTagsOutput, error) {
	if m.CreateTagsFunc != nil {
		return m.CreateTagsFunc(ctx, params, optFns...)
	}
	return &ec2.CreateTagsOutput{}, nil
}

func (m *mockEC2Client) DeleteFleets(ctx context.Context, params *ec2.DeleteFleetsInput, optFns ...func(*ec2.Options)) (*ec2.DeleteFleetsOutput, error) {
	if m.DeleteFleetsFunc != nil {
		return m.DeleteFleetsFunc(ctx, params, optFns...)
	}
	return &ec2.DeleteFleetsOutput{}, nil
}

func (m *mockEC2Client) DescribeFleetInstances(ctx context.Context, params *ec2.DescribeFleetInstancesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeFleetInstancesOutput, error) {
	if m.DescribeFleetInstancesFunc != nil {
		return m.DescribeFleetInstancesFunc(ctx, params, optFns...)
	}
	// Default: return one active instance
	return &ec2.DescribeFleetInstancesOutput{
		ActiveInstances: []types.ActiveInstance{
			{InstanceId: aws.String(testInstanceID)},
		},
	}, nil
}

func (m *mockEC2Client) DescribeSpotPriceHistory(ctx context.Context, params *ec2.DescribeSpotPriceHistoryInput, optFns ...func(*ec2.Options)) (*ec2.DescribeSpotPriceHistoryOutput, error) {
	if m.DescribeSpotPriceHistoryFunc != nil {
		return m.DescribeSpotPriceHistoryFunc(ctx, params, optFns...)
	}
	// Default: return arm64 as cheaper
	return &ec2.DescribeSpotPriceHistoryOutput{
		SpotPriceHistory: []types.SpotPrice{
			{InstanceType: types.InstanceTypeT4gMedium, SpotPrice: aws.String("0.0101")},
			{InstanceType: types.InstanceTypeC7gXlarge, SpotPrice: aws.String("0.0435")},
		},
	}, nil
}

func (m *mockEC2Client) DescribeInstanceTypeOfferings(ctx context.Context, params *ec2.DescribeInstanceTypeOfferingsInput, optFns ...func(*ec2.Options)) (*ec2.DescribeInstanceTypeOfferingsOutput, error) {
	if m.DescribeInstanceTypeOfferingsFunc != nil {
		return m.DescribeInstanceTypeOfferingsFunc(ctx, params, optFns...)
	}
	// Default: return common instance types as available
	return &ec2.DescribeInstanceTypeOfferingsOutput{
		InstanceTypeOfferings: []types.InstanceTypeOffering{
			{InstanceType: types.InstanceTypeT4gMedium},
			{InstanceType: types.InstanceTypeT4gLarge},
			{InstanceType: types.InstanceTypeC7gXlarge},
			{InstanceType: types.InstanceTypeC7g2xlarge},
			{InstanceType: types.InstanceTypeM7gXlarge},
			{InstanceType: types.InstanceTypeT3Medium},
			{InstanceType: types.InstanceTypeC6iXlarge},
		},
	}, nil
}

func (m *mockEC2Client) DescribeInstances(ctx context.Context, params *ec2.DescribeInstancesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	if m.DescribeInstancesFunc != nil {
		return m.DescribeInstancesFunc(ctx, params, optFns...)
	}
	return &ec2.DescribeInstancesOutput{}, nil
}

func (m *mockEC2Client) RunInstances(ctx context.Context, params *ec2.RunInstancesInput, optFns ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error) {
	if m.RunInstancesFunc != nil {
		return m.RunInstancesFunc(ctx, params, optFns...)
	}
	return &ec2.RunInstancesOutput{}, nil
}

type mockCircuitBreaker struct {
	checkFunc func(ctx context.Context, instanceType string) (circuit.State, error)
}

func (m *mockCircuitBreaker) CheckCircuit(ctx context.Context, instanceType string) (circuit.State, error) {
	return m.checkFunc(ctx, instanceType)
}

func TestCreateFleet(t *testing.T) {
	tests := []struct {
		name             string
		spec             *LaunchSpec
		config           *config.Config
		wantSpot         bool
		wantOnDemand     bool
		wantInstanceType string
		wantPool         string
	}{
		{
			name: "Spot Request",
			spec: &LaunchSpec{
				RunID: 12345,
				InstanceType: "t4g.medium",
				SubnetID:     "subnet-1",
				Spot:         true,
			},
			config: &config.Config{
				SpotEnabled: true,
			},
			wantSpot:         true,
			wantInstanceType: "t4g.medium",
		},
		{
			name: "With Pool",
			spec: &LaunchSpec{
				RunID: 12345,
				InstanceType: "t4g.medium",
				SubnetID:     "subnet-1",
				Spot:         true,
				Pool:         "default",
			},
			config: &config.Config{
				SpotEnabled: true,
			},
			wantSpot:         true,
			wantInstanceType: "t4g.medium",
			wantPool:         "default",
		},
		{
			name: "Explicit On-Demand",
			spec: &LaunchSpec{
				RunID: 12345,
				InstanceType: "c7g.xlarge",
				SubnetID:     "subnet-1",
				Spot:         false,
			},
			config: &config.Config{
				SpotEnabled: true,
			},
			wantOnDemand:     true,
			wantInstanceType: "c7g.xlarge",
		},
		{
			name: "Spot Disabled Globally",
			spec: &LaunchSpec{
				RunID: 12345,
				InstanceType: "t4g.medium",
				SubnetID:     "subnet-1",
				Spot:         true,
			},
			config: &config.Config{
				SpotEnabled: false,
			},
			wantOnDemand:     true,
			wantInstanceType: "t4g.medium",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &mockEC2Client{
				CreateFleetFunc: func(_ context.Context, params *ec2.CreateFleetInput, _ ...func(*ec2.Options)) (*ec2.CreateFleetOutput, error) {
					if len(params.LaunchTemplateConfigs) > 0 && len(params.LaunchTemplateConfigs[0].Overrides) > 0 {
						gotType := params.LaunchTemplateConfigs[0].Overrides[0].InstanceType
						if string(gotType) != tt.wantInstanceType {
							t.Errorf("InstanceType = %v, want %v", gotType, tt.wantInstanceType)
						}
					}

					if tt.wantSpot {
						if params.TargetCapacitySpecification.DefaultTargetCapacityType != types.DefaultTargetCapacityTypeSpot {
							t.Errorf("TargetCapacityType = %v, want Spot", params.TargetCapacitySpecification.DefaultTargetCapacityType)
						}
					} else if tt.wantOnDemand {
						if params.TargetCapacitySpecification.DefaultTargetCapacityType != types.DefaultTargetCapacityTypeOnDemand {
							t.Errorf("TargetCapacityType = %v, want OnDemand", params.TargetCapacitySpecification.DefaultTargetCapacityType)
						}
					}

					if tt.wantPool != "" {
						found := false
						for _, tagSpec := range params.TagSpecifications {
							for _, tag := range tagSpec.Tags {
								if aws.ToString(tag.Key) == "runs-fleet:pool" && aws.ToString(tag.Value) == tt.wantPool {
									found = true
									break
								}
							}
						}
						if !found {
							t.Errorf("Pool tag not found or incorrect, want pool=%s", tt.wantPool)
						}
					}

					return &ec2.CreateFleetOutput{
						Instances: []types.CreateFleetInstance{
							{
								InstanceIds: []string{testInstanceID},
							},
						},
					}, nil
				},
			}

			manager := &Manager{
				ec2Client: mock,
				config:    tt.config,
			}

			instanceIDs, err := manager.CreateFleet(context.Background(), tt.spec)
			if err != nil {
				t.Errorf("CreateFleet() error = %v", err)
			}
			if len(instanceIDs) != 1 {
				t.Errorf("CreateFleet() returned %d instance IDs, want 1", len(instanceIDs))
			}
			if len(instanceIDs) > 0 && instanceIDs[0] != testInstanceID {
				t.Errorf("CreateFleet() returned instance ID %s, want testInstanceID", instanceIDs[0])
			}
		})
	}
}

func TestCreateFleet_Errors(t *testing.T) {
	tests := []struct {
		name    string
		spec    *LaunchSpec
		config  *config.Config
		mock    *mockEC2Client
		wantErr string
	}{
		{
			name: "API error",
			spec: &LaunchSpec{
				RunID: 12345,
				InstanceType: "t4g.medium",
				SubnetID:     "subnet-1",
				Spot:         true,
			},
			config: &config.Config{
				SpotEnabled: true,
			},
			mock: &mockEC2Client{
				CreateFleetFunc: func(_ context.Context, _ *ec2.CreateFleetInput, _ ...func(*ec2.Options)) (*ec2.CreateFleetOutput, error) {
					return nil, errors.New("aws api error")
				},
			},
			wantErr: "failed to create fleet",
		},
		{
			name: "Fleet creation errors",
			spec: &LaunchSpec{
				RunID: 12345,
				InstanceType: "t4g.medium",
				SubnetID:     "subnet-1",
				Spot:         true,
			},
			config: &config.Config{
				SpotEnabled: true,
			},
			mock: &mockEC2Client{
				CreateFleetFunc: func(_ context.Context, _ *ec2.CreateFleetInput, _ ...func(*ec2.Options)) (*ec2.CreateFleetOutput, error) {
					return &ec2.CreateFleetOutput{
						Errors: []types.CreateFleetError{
							{
								ErrorMessage: aws.String("insufficient capacity"),
							},
						},
					}, nil
				},
			},
			wantErr: "fleet creation had errors: insufficient capacity",
		},
		{
			name: "Fleet creation errors with nil message",
			spec: &LaunchSpec{
				RunID: 12345,
				InstanceType: "t4g.medium",
				SubnetID:     "subnet-1",
				Spot:         true,
			},
			config: &config.Config{
				SpotEnabled: true,
			},
			mock: &mockEC2Client{
				CreateFleetFunc: func(_ context.Context, _ *ec2.CreateFleetInput, _ ...func(*ec2.Options)) (*ec2.CreateFleetOutput, error) {
					return &ec2.CreateFleetOutput{
						Errors: []types.CreateFleetError{
							{
								ErrorMessage: nil,
							},
						},
					}, nil
				},
			},
			wantErr: "fleet creation had errors: unknown error",
		},
		{
			name: "No instances created",
			spec: &LaunchSpec{
				RunID: 12345,
				InstanceType: "t4g.medium",
				SubnetID:     "subnet-1",
				Spot:         true,
			},
			config: &config.Config{
				SpotEnabled: true,
			},
			mock: &mockEC2Client{
				CreateFleetFunc: func(_ context.Context, _ *ec2.CreateFleetInput, _ ...func(*ec2.Options)) (*ec2.CreateFleetOutput, error) {
					return &ec2.CreateFleetOutput{
						Instances: []types.CreateFleetInstance{},
					}, nil
				},
			},
			wantErr: "no instances were created",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager := &Manager{
				ec2Client: tt.mock,
				config:    tt.config,
			}

			instanceIDs, err := manager.CreateFleet(context.Background(), tt.spec)
			if err == nil {
				t.Fatalf("CreateFleet() expected error containing %q, got nil", tt.wantErr)
			}
			if instanceIDs != nil {
				t.Errorf("CreateFleet() returned non-nil instance IDs on error: %v", instanceIDs)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("CreateFleet() error = %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestCreateFleet_MultipleInstances(t *testing.T) {
	mock := &mockEC2Client{
		CreateFleetFunc: func(_ context.Context, _ *ec2.CreateFleetInput, _ ...func(*ec2.Options)) (*ec2.CreateFleetOutput, error) {
			return &ec2.CreateFleetOutput{
				Instances: []types.CreateFleetInstance{
					{InstanceIds: []string{"i-aaa", "i-bbb"}},
					{InstanceIds: []string{"i-ccc"}},
				},
			}, nil
		},
	}

	manager := &Manager{ec2Client: mock, config: &config.Config{SpotEnabled: true}}
	spec := &LaunchSpec{RunID: 1, InstanceType: "t4g.medium", SubnetID: "subnet-1", Spot: true}

	instanceIDs, err := manager.CreateFleet(context.Background(), spec)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(instanceIDs) != 3 {
		t.Fatalf("expected 3 instance IDs, got %d: %v", len(instanceIDs), instanceIDs)
	}
	want := []string{"i-aaa", "i-bbb", "i-ccc"}
	for i, id := range instanceIDs {
		if id != want[i] {
			t.Errorf("instanceIDs[%d] = %q, want %q", i, id, want[i])
		}
	}
}

func TestCreateFleet_MultipleErrors(t *testing.T) {
	mock := &mockEC2Client{
		CreateFleetFunc: func(_ context.Context, _ *ec2.CreateFleetInput, _ ...func(*ec2.Options)) (*ec2.CreateFleetOutput, error) {
			return &ec2.CreateFleetOutput{
				Errors: []types.CreateFleetError{
					{ErrorMessage: aws.String("insufficient capacity in az-1")},
					{ErrorMessage: aws.String("price too low in az-2")},
					{ErrorMessage: nil},
				},
			}, nil
		},
	}

	manager := &Manager{ec2Client: mock, config: &config.Config{SpotEnabled: true}}
	spec := &LaunchSpec{RunID: 1, InstanceType: "t4g.medium", SubnetID: "subnet-1", Spot: true}

	_, err := manager.CreateFleet(context.Background(), spec)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "insufficient capacity in az-1") {
		t.Errorf("error should contain first error message, got: %v", err)
	}
	if !strings.Contains(err.Error(), "price too low in az-2") {
		t.Errorf("error should contain second error message, got: %v", err)
	}
	if !strings.Contains(err.Error(), "unknown error") {
		t.Errorf("error should contain 'unknown error' for nil message, got: %v", err)
	}
}

func TestCreateFleet_CircuitBreaker(t *testing.T) {
	t.Run("Open circuit forces on-demand", func(t *testing.T) {
		var capturedCapType types.DefaultTargetCapacityType
		mock := &mockEC2Client{
			CreateFleetFunc: func(_ context.Context, params *ec2.CreateFleetInput, _ ...func(*ec2.Options)) (*ec2.CreateFleetOutput, error) {
				capturedCapType = params.TargetCapacitySpecification.DefaultTargetCapacityType
				return &ec2.CreateFleetOutput{
					Instances: []types.CreateFleetInstance{{InstanceIds: []string{testInstanceID}}},
				}, nil
			},
		}

		cb := &mockCircuitBreaker{
			checkFunc: func(_ context.Context, _ string) (circuit.State, error) {
				return circuit.StateOpen, nil
			},
		}

		manager := &Manager{ec2Client: mock, config: &config.Config{SpotEnabled: true}, circuitBreaker: cb}
		spec := &LaunchSpec{RunID: 1, InstanceType: "t4g.medium", SubnetID: "subnet-1", Spot: true}

		_, err := manager.CreateFleet(context.Background(), spec)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if capturedCapType != types.DefaultTargetCapacityTypeOnDemand {
			t.Errorf("CapacityType = %v, want OnDemand when circuit breaker is open", capturedCapType)
		}
	})

	t.Run("Closed circuit uses spot", func(t *testing.T) {
		var capturedCapType types.DefaultTargetCapacityType
		mock := &mockEC2Client{
			CreateFleetFunc: func(_ context.Context, params *ec2.CreateFleetInput, _ ...func(*ec2.Options)) (*ec2.CreateFleetOutput, error) {
				capturedCapType = params.TargetCapacitySpecification.DefaultTargetCapacityType
				return &ec2.CreateFleetOutput{
					Instances: []types.CreateFleetInstance{{InstanceIds: []string{testInstanceID}}},
				}, nil
			},
		}

		cb := &mockCircuitBreaker{
			checkFunc: func(_ context.Context, _ string) (circuit.State, error) {
				return circuit.StateClosed, nil
			},
		}

		manager := &Manager{ec2Client: mock, config: &config.Config{SpotEnabled: true}, circuitBreaker: cb}
		spec := &LaunchSpec{RunID: 1, InstanceType: "t4g.medium", SubnetID: "subnet-1", Spot: true}

		_, err := manager.CreateFleet(context.Background(), spec)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if capturedCapType != types.DefaultTargetCapacityTypeSpot {
			t.Errorf("CapacityType = %v, want Spot when circuit breaker is closed", capturedCapType)
		}
	})

	t.Run("Circuit breaker error defaults to spot", func(t *testing.T) {
		var capturedCapType types.DefaultTargetCapacityType
		mock := &mockEC2Client{
			CreateFleetFunc: func(_ context.Context, params *ec2.CreateFleetInput, _ ...func(*ec2.Options)) (*ec2.CreateFleetOutput, error) {
				capturedCapType = params.TargetCapacitySpecification.DefaultTargetCapacityType
				return &ec2.CreateFleetOutput{
					Instances: []types.CreateFleetInstance{{InstanceIds: []string{testInstanceID}}},
				}, nil
			},
		}

		cb := &mockCircuitBreaker{
			checkFunc: func(_ context.Context, _ string) (circuit.State, error) {
				return circuit.StateClosed, errors.New("dynamo error")
			},
		}

		manager := &Manager{ec2Client: mock, config: &config.Config{SpotEnabled: true}, circuitBreaker: cb}
		spec := &LaunchSpec{RunID: 1, InstanceType: "t4g.medium", SubnetID: "subnet-1", Spot: true}

		_, err := manager.CreateFleet(context.Background(), spec)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if capturedCapType != types.DefaultTargetCapacityTypeSpot {
			t.Errorf("CapacityType = %v, want Spot when circuit breaker errors", capturedCapType)
		}
	})

	t.Run("Nil circuit breaker uses spot", func(t *testing.T) {
		var capturedCapType types.DefaultTargetCapacityType
		mock := &mockEC2Client{
			CreateFleetFunc: func(_ context.Context, params *ec2.CreateFleetInput, _ ...func(*ec2.Options)) (*ec2.CreateFleetOutput, error) {
				capturedCapType = params.TargetCapacitySpecification.DefaultTargetCapacityType
				return &ec2.CreateFleetOutput{
					Instances: []types.CreateFleetInstance{{InstanceIds: []string{testInstanceID}}},
				}, nil
			},
		}

		manager := &Manager{ec2Client: mock, config: &config.Config{SpotEnabled: true}, circuitBreaker: nil}
		spec := &LaunchSpec{RunID: 1, InstanceType: "t4g.medium", SubnetID: "subnet-1", Spot: true}

		_, err := manager.CreateFleet(context.Background(), spec)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if capturedCapType != types.DefaultTargetCapacityTypeSpot {
			t.Errorf("CapacityType = %v, want Spot when no circuit breaker", capturedCapType)
		}
	})
}

func TestCreateFleet_TagSpecUsesInstanceType(t *testing.T) {
	var capturedTagSpecs []types.TagSpecification
	mock := &mockEC2Client{
		CreateFleetFunc: func(_ context.Context, params *ec2.CreateFleetInput, _ ...func(*ec2.Options)) (*ec2.CreateFleetOutput, error) {
			capturedTagSpecs = params.TagSpecifications
			return &ec2.CreateFleetOutput{
				Instances: []types.CreateFleetInstance{{InstanceIds: []string{testInstanceID}}},
			}, nil
		},
	}

	manager := &Manager{ec2Client: mock, config: &config.Config{SpotEnabled: true}}
	spec := &LaunchSpec{RunID: 1, InstanceType: "t4g.medium", SubnetID: "subnet-1", Spot: true, Pool: "test"}

	_, err := manager.CreateFleet(context.Background(), spec)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(capturedTagSpecs) != 1 {
		t.Fatalf("expected 1 TagSpecification, got %d", len(capturedTagSpecs))
	}
	if capturedTagSpecs[0].ResourceType != types.ResourceTypeInstance {
		t.Errorf("ResourceType = %v, want instance", capturedTagSpecs[0].ResourceType)
	}
}

func TestGetLaunchTemplateForArch(t *testing.T) {
	tests := []struct {
		name     string
		config   *config.Config
		os       string
		arch     string
		expected string
	}{
		{
			name:     "ARM64 Linux",
			config:   &config.Config{},
			os:       "linux",
			arch:     "arm64",
			expected: "runs-fleet-runner-arm64",
		},
		{
			name:     "amd64 Linux",
			config:   &config.Config{},
			os:       "linux",
			arch:     "amd64",
			expected: "runs-fleet-runner-amd64",
		},
		{
			name:     "Windows ignores arch",
			config:   &config.Config{},
			os:       "windows",
			arch:     "amd64",
			expected: "runs-fleet-runner-windows",
		},
		{
			name:     "Windows with ARM64 arch still uses windows template",
			config:   &config.Config{},
			os:       "windows",
			arch:     "arm64",
			expected: "runs-fleet-runner-windows",
		},
		{
			name:     "Custom base name ARM64",
			config:   &config.Config{LaunchTemplateName: "custom-runner"},
			os:       "linux",
			arch:     "arm64",
			expected: "custom-runner-arm64",
		},
		{
			name:     "Custom base name amd64",
			config:   &config.Config{LaunchTemplateName: "custom-runner"},
			os:       "linux",
			arch:     "amd64",
			expected: "custom-runner-amd64",
		},
		{
			name:     "Empty arch defaults to arm64",
			config:   &config.Config{},
			os:       "linux",
			arch:     "",
			expected: "runs-fleet-runner-arm64",
		},
		{
			name:     "Unsupported OS defaults to Linux with specified arch",
			config:   &config.Config{},
			os:       "macos",
			arch:     "arm64",
			expected: "runs-fleet-runner-arm64",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &Manager{config: tt.config}
			got := m.getLaunchTemplateForArch(tt.os, tt.arch)
			if got != tt.expected {
				t.Errorf("getLaunchTemplateForArch() = %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestCreateFleet_Storage(t *testing.T) {
	tests := []struct {
		name           string
		spec           *LaunchSpec
		config         *config.Config
		wantStorageGiB int32
		wantNoStorage  bool
	}{
		{
			name: "Custom storage 100 GiB",
			spec: &LaunchSpec{
				RunID: 12345,
				InstanceType: "t4g.medium",
				SubnetID:     "subnet-1",
				Spot:         true,
				StorageGiB:   100,
			},
			config: &config.Config{
				SpotEnabled: true,
			},
			wantStorageGiB: 100,
		},
		{
			name: "Large storage 500 GiB",
			spec: &LaunchSpec{
				RunID: 12345,
				InstanceType: "c7g.xlarge",
				SubnetID:     "subnet-1",
				Spot:         true,
				StorageGiB:   500,
			},
			config: &config.Config{
				SpotEnabled: true,
			},
			wantStorageGiB: 500,
		},
		{
			name: "No storage specified (use launch template default)",
			spec: &LaunchSpec{
				RunID: 12345,
				InstanceType: "t4g.medium",
				SubnetID:     "subnet-1",
				Spot:         true,
				StorageGiB:   0,
			},
			config: &config.Config{
				SpotEnabled: true,
			},
			wantNoStorage: true,
		},
		{
			name: "Storage with on-demand",
			spec: &LaunchSpec{
				RunID: 12345,
				InstanceType: "c7g.xlarge",
				SubnetID:     "subnet-1",
				Spot:         false,
				StorageGiB:   200,
				Arch:         "arm64",
			},
			config: &config.Config{
				SpotEnabled: true,
			},
			wantStorageGiB: 200,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &mockEC2Client{
				CreateFleetFunc: func(_ context.Context, params *ec2.CreateFleetInput, _ ...func(*ec2.Options)) (*ec2.CreateFleetOutput, error) {
					if len(params.LaunchTemplateConfigs) > 0 && len(params.LaunchTemplateConfigs[0].Overrides) > 0 {
						override := params.LaunchTemplateConfigs[0].Overrides[0]

						if tt.wantNoStorage {
							if len(override.BlockDeviceMappings) > 0 {
								t.Errorf("BlockDeviceMappings should be empty when StorageGiB=0")
							}
						} else {
							if len(override.BlockDeviceMappings) == 0 {
								t.Errorf("BlockDeviceMappings should not be empty when StorageGiB=%d", tt.wantStorageGiB)
							} else {
								bdm := override.BlockDeviceMappings[0]
								if aws.ToString(bdm.DeviceName) != "/dev/xvda" {
									t.Errorf("DeviceName = %v, want /dev/xvda", aws.ToString(bdm.DeviceName))
								}
								if bdm.Ebs == nil {
									t.Error("Ebs block device settings should not be nil")
								} else {
									if aws.ToInt32(bdm.Ebs.VolumeSize) != tt.wantStorageGiB {
										t.Errorf("VolumeSize = %d, want %d", aws.ToInt32(bdm.Ebs.VolumeSize), tt.wantStorageGiB)
									}
									if bdm.Ebs.VolumeType != types.VolumeTypeGp3 {
										t.Errorf("VolumeType = %v, want gp3", bdm.Ebs.VolumeType)
									}
									if !aws.ToBool(bdm.Ebs.DeleteOnTermination) {
										t.Error("DeleteOnTermination should be true")
									}
									if !aws.ToBool(bdm.Ebs.Encrypted) {
										t.Error("Encrypted should be true")
									}
								}
							}
						}
					}

					return &ec2.CreateFleetOutput{
						Instances: []types.CreateFleetInstance{
							{
								InstanceIds: []string{testInstanceID},
							},
						},
					}, nil
				},
			}

			manager := &Manager{
				ec2Client: mock,
				config:    tt.config,
			}

			_, err := manager.CreateFleet(context.Background(), tt.spec)
			if err != nil {
				t.Errorf("CreateFleet() error = %v", err)
			}
		})
	}
}

func TestIsValidWindowsInstanceType(t *testing.T) {
	tests := []struct {
		instanceType string
		want         bool
	}{
		// Valid Windows instance types
		{"t3.medium", true},
		{"t3.large", true},
		{"t3.xlarge", true},
		{"m6i.large", true},
		{"m6i.xlarge", true},
		{"m6i.2xlarge", true},
		{"c6i.large", true},
		{"c6i.xlarge", true},
		{"c6i.2xlarge", true},
		// Invalid Windows instance types
		{"t4g.medium", false}, // ARM64, not supported for Windows
		{"c7g.xlarge", false}, // ARM64, not supported for Windows
		{"t3.micro", false},   // Too small
		{"m6i.4xlarge", false},
		{"invalid", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.instanceType, func(t *testing.T) {
			if got := IsValidWindowsInstanceType(tt.instanceType); got != tt.want {
				t.Errorf("IsValidWindowsInstanceType(%q) = %v, want %v", tt.instanceType, got, tt.want)
			}
		})
	}
}

func TestBuildTags(t *testing.T) {
	tests := []struct {
		name       string
		config     *config.Config
		spec       *LaunchSpec
		wantTags   map[string]string
		wantAbsent []string
	}{
		{
			name:   "Basic tags",
			config: &config.Config{},
			spec: &LaunchSpec{
				RunID: 12345,
			},
			wantTags: map[string]string{
				"Name":               "runs-fleet-runner",
				"runs-fleet:run-id":  "12345",
				"runs-fleet:managed": "true",
			},
		},
		{
			name:   "With pool",
			config: &config.Config{},
			spec: &LaunchSpec{
				RunID: 12345,
				Pool:  "default",
			},
			wantTags: map[string]string{
				"Name":               "runs-fleet-runner-default",
				"runs-fleet:run-id":  "12345",
				"runs-fleet:managed": "true",
				"runs-fleet:pool":    "default",
			},
		},
		{
			name: "With custom tags",
			config: &config.Config{
				Tags: map[string]string{
					"team":        "platform",
					"project":     "ci-runners",
					"cost-center": "engineering",
				},
			},
			spec: &LaunchSpec{
				RunID: 12345,
			},
			wantTags: map[string]string{
				"Name":               "runs-fleet-runner",
				"runs-fleet:run-id":  "12345",
				"runs-fleet:managed": "true",
				"team":               "platform",
				"project":            "ci-runners",
				"cost-center":        "engineering",
			},
		},
		{
			name: "With OS and arch",
			config: &config.Config{},
			spec: &LaunchSpec{
				RunID: 12345,
				OS:    "linux",
				Arch:  "arm64",
			},
			wantTags: map[string]string{
				"Name":               "runs-fleet-runner",
				"runs-fleet:run-id":  "12345",
				"runs-fleet:managed": "true",
				"runs-fleet:os":      "linux",
				"runs-fleet:arch":    "arm64",
			},
		},
		{
			name: "With environment",
			config: &config.Config{},
			spec: &LaunchSpec{
				RunID: 12345,
				Environment: "production",
			},
			wantTags: map[string]string{
				"Name":                   "runs-fleet-runner",
				"runs-fleet:run-id":      "12345",
				"runs-fleet:managed":     "true",
				"runs-fleet:environment": "production",
				"Environment":            "production",
			},
		},
		{
			name:   "Cold-start with repo and conditions",
			config: &config.Config{},
			spec: &LaunchSpec{
				RunID:      12345,
				Repo:       "org/myapp",
				Arch:       "arm64",
				Conditions: "arm64-cpu4",
			},
			wantTags: map[string]string{
				"Name":               "runs-fleet-runner-myapp-arm64-cpu4",
				"runs-fleet:run-id":  "12345",
				"runs-fleet:managed": "true",
				"runs-fleet:arch":    "arm64",
				"Role":               "org/myapp",
			},
		},
		{
			name:   "Empty pool should not create tag",
			config: &config.Config{},
			spec: &LaunchSpec{
				RunID: 12345,
				Pool:  "",
			},
			wantTags: map[string]string{
				"Name":               "runs-fleet-runner",
				"runs-fleet:run-id":  "12345",
				"runs-fleet:managed": "true",
			},
			wantAbsent: []string{"runs-fleet:pool"},
		},
		{
			name:   "With region tag",
			config: &config.Config{},
			spec: &LaunchSpec{
				RunID: 12345,
				Region: "us-west-2",
			},
			wantTags: map[string]string{
				"Name":               "runs-fleet-runner",
				"runs-fleet:run-id":  "12345",
				"runs-fleet:managed": "true",
				"runs-fleet:region":  "us-west-2",
			},
		},
		{
			name:   "Empty region should not create tag",
			config: &config.Config{},
			spec: &LaunchSpec{
				RunID: 12345,
				Region: "",
			},
			wantTags: map[string]string{
				"Name":               "runs-fleet-runner",
				"runs-fleet:run-id":  "12345",
				"runs-fleet:managed": "true",
			},
			wantAbsent: []string{"runs-fleet:region"},
		},
		{
			name: "With config-based tags",
			config: &config.Config{
				RunnerImage:         "ghcr.io/org/runner:latest",
				TerminationQueueURL: "https://sqs.us-west-2.amazonaws.com/123/term-queue",
				CacheURL:            "https://cache.example.com",
			},
			spec: &LaunchSpec{
				RunID: 12345,
			},
			wantTags: map[string]string{
				"Name":                             "runs-fleet-runner",
				"runs-fleet:run-id":                "12345",
				"runs-fleet:managed":               "true",
				"runs-fleet:runner-image":           "ghcr.io/org/runner:latest",
				"runs-fleet:termination-queue-url": "https://sqs.us-west-2.amazonaws.com/123/term-queue",
				"runs-fleet:cache-url":             "https://cache.example.com",
			},
		},
		{
			name:   "Empty config values should not create tags",
			config: &config.Config{},
			spec: &LaunchSpec{
				RunID: 12345,
			},
			wantTags: map[string]string{
				"Name":               "runs-fleet-runner",
				"runs-fleet:run-id":  "12345",
				"runs-fleet:managed": "true",
			},
			wantAbsent: []string{
				"runs-fleet:runner-image",
				"runs-fleet:termination-queue-url",
				"runs-fleet:cache-url",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &Manager{config: tt.config}
			tags := m.buildTags(tt.spec)

			// Convert to map for easier checking
			tagMap := make(map[string]string)
			for _, tag := range tags {
				tagMap[aws.ToString(tag.Key)] = aws.ToString(tag.Value)
			}

			// Check expected tags exist
			for wantKey, wantValue := range tt.wantTags {
				if gotValue, exists := tagMap[wantKey]; !exists {
					t.Errorf("tag %q not found", wantKey)
				} else if gotValue != wantValue {
					t.Errorf("tag %q = %q, want %q", wantKey, gotValue, wantValue)
				}
			}

			// Check absent tags
			for _, absentKey := range tt.wantAbsent {
				if _, exists := tagMap[absentKey]; exists {
					t.Errorf("tag %q should not exist", absentKey)
				}
			}
		})
	}
}

func TestGetPrimaryInstanceType(t *testing.T) {
	tests := []struct {
		name string
		spec *LaunchSpec
		want string
	}{
		{
			name: "Uses first from InstanceTypes array",
			spec: &LaunchSpec{
				InstanceTypes: []string{"c7g.xlarge", "t4g.medium"},
				InstanceType:  "fallback.type",
			},
			want: "c7g.xlarge",
		},
		{
			name: "Falls back to InstanceType when array empty",
			spec: &LaunchSpec{
				InstanceTypes: []string{},
				InstanceType:  "t4g.medium",
			},
			want: "t4g.medium",
		},
		{
			name: "Falls back to InstanceType when array nil",
			spec: &LaunchSpec{
				InstanceTypes: nil,
				InstanceType:  "c7g.large",
			},
			want: "c7g.large",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &Manager{}
			got := m.getPrimaryInstanceType(tt.spec)
			if got != tt.want {
				t.Errorf("getPrimaryInstanceType() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBuildLaunchTemplateConfigs(t *testing.T) {
	// Mock EC2 client that returns arm64 as cheaper
	mockClient := &mockEC2Client{
		DescribeSpotPriceHistoryFunc: func(_ context.Context, params *ec2.DescribeSpotPriceHistoryInput, _ ...func(*ec2.Options)) (*ec2.DescribeSpotPriceHistoryOutput, error) {
			prices := []types.SpotPrice{}
			for _, instType := range params.InstanceTypes {
				price := "0.10" // Default AMD64 price
				if strings.HasPrefix(string(instType), "t4g") || strings.HasPrefix(string(instType), "c7g") || strings.HasPrefix(string(instType), "m7g") {
					price = "0.05" // ARM64 is cheaper
				}
				prices = append(prices, types.SpotPrice{InstanceType: instType, SpotPrice: aws.String(price)})
			}
			return &ec2.DescribeSpotPriceHistoryOutput{SpotPriceHistory: prices}, nil
		},
	}

	tests := []struct {
		name              string
		config            *config.Config
		spec              *LaunchSpec
		wantConfigCount   int
		wantTemplateNames []string
		wantErr           bool
		wantErrContains   string
	}{
		{
			name:   "Specified arch creates single config",
			config: &config.Config{},
			spec: &LaunchSpec{
				OS:            "linux",
				Arch:          "arm64",
				InstanceTypes: []string{"c7g.xlarge", "t4g.medium"},
				SubnetID:      "subnet-1",
			},
			wantConfigCount:   1,
			wantTemplateNames: []string{"runs-fleet-runner-arm64"},
		},
		{
			name:   "Empty arch with ARM64 only creates single config",
			config: &config.Config{},
			spec: &LaunchSpec{
				OS:            "linux",
				Arch:          "",
				InstanceTypes: []string{"c7g.xlarge", "t4g.medium"},
				SubnetID:      "subnet-1",
			},
			wantConfigCount:   1,
			wantTemplateNames: []string{"runs-fleet-runner-arm64"},
		},
		{
			name:   "Empty arch with AMD64 only creates single config",
			config: &config.Config{},
			spec: &LaunchSpec{
				OS:            "linux",
				Arch:          "",
				InstanceTypes: []string{"c6i.xlarge", "t3.medium"},
				SubnetID:      "subnet-1",
			},
			wantConfigCount:   1,
			wantTemplateNames: []string{"runs-fleet-runner-amd64"},
		},
		{
			name:   "Empty arch with mixed architectures selects cheapest (arm64)",
			config: &config.Config{},
			spec: &LaunchSpec{
				OS:            "linux",
				Arch:          "",
				InstanceTypes: []string{"c7g.xlarge", "t3.medium", "t4g.medium", "c6i.large"},
				SubnetID:      "subnet-1",
			},
			wantConfigCount:   1,
			wantTemplateNames: []string{"runs-fleet-runner-arm64"}, // arm64 is cheaper in mock
		},
		{
			name:   "Empty arch with unknown types returns error",
			config: &config.Config{},
			spec: &LaunchSpec{
				OS:            "linux",
				Arch:          "",
				InstanceTypes: []string{"unknown.type"},
				SubnetID:      "subnet-1",
			},
			wantErr:         true,
			wantErrContains: "no instance types available in region",
		},
		{
			name:   "Arch mismatch returns error",
			config: &config.Config{},
			spec: &LaunchSpec{
				OS:            "linux",
				Arch:          "arm64",
				InstanceTypes: []string{"t3.medium"}, // AMD64 type with ARM64 arch
				SubnetID:      "subnet-1",
			},
			wantErr:         true,
			wantErrContains: "conflicts with specified arch",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &Manager{config: tt.config, ec2Client: mockClient}
			configs, err := m.buildLaunchTemplateConfigs(context.Background(), tt.spec)

			if tt.wantErr {
				if err == nil {
					t.Fatal("buildLaunchTemplateConfigs() expected error, got nil")
				}
				if tt.wantErrContains != "" && !strings.Contains(err.Error(), tt.wantErrContains) {
					t.Errorf("buildLaunchTemplateConfigs() error = %q, want error containing %q", err, tt.wantErrContains)
				}
				return
			}

			if err != nil {
				t.Fatalf("buildLaunchTemplateConfigs() unexpected error: %v", err)
			}

			if len(configs) != tt.wantConfigCount {
				t.Errorf("buildLaunchTemplateConfigs() returned %d configs, want %d", len(configs), tt.wantConfigCount)
			}

			if tt.wantTemplateNames != nil {
				for i, wantName := range tt.wantTemplateNames {
					if i < len(configs) {
						gotName := *configs[i].LaunchTemplateSpecification.LaunchTemplateName
						if gotName != wantName {
							t.Errorf("config[%d] template name = %q, want %q", i, gotName, wantName)
						}
					}
				}
			}
		})
	}
}

func TestFilterAvailableInstanceTypes(t *testing.T) {
	tests := []struct {
		name           string
		inputTypes     []string
		availableTypes []string
		wantTypes      []string
		apiError       error
	}{
		{
			name:           "all types available",
			inputTypes:     []string{"t4g.medium", "c7g.xlarge"},
			availableTypes: []string{"t4g.medium", "c7g.xlarge", "m7g.large"},
			wantTypes:      []string{"t4g.medium", "c7g.xlarge"},
		},
		{
			name:           "some types unavailable",
			inputTypes:     []string{"t4g.medium", "c8g.xlarge", "m8g.large"},
			availableTypes: []string{"t4g.medium", "c7g.xlarge"},
			wantTypes:      []string{"t4g.medium"},
		},
		{
			name:           "all types unavailable",
			inputTypes:     []string{"c8g.xlarge", "m8g.large"},
			availableTypes: []string{"t4g.medium", "c7g.xlarge"},
			wantTypes:      nil,
		},
		{
			name:           "API error returns all types",
			inputTypes:     []string{"t4g.medium", "c8g.xlarge"},
			availableTypes: nil,
			apiError:       errors.New("API error"),
			wantTypes:      []string{"t4g.medium", "c8g.xlarge"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockClient := &mockEC2Client{
				DescribeInstanceTypeOfferingsFunc: func(_ context.Context, _ *ec2.DescribeInstanceTypeOfferingsInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstanceTypeOfferingsOutput, error) {
					if tt.apiError != nil {
						return nil, tt.apiError
					}
					offerings := make([]types.InstanceTypeOffering, len(tt.availableTypes))
					for i, t := range tt.availableTypes {
						offerings[i] = types.InstanceTypeOffering{
							InstanceType: types.InstanceType(t),
						}
					}
					return &ec2.DescribeInstanceTypeOfferingsOutput{
						InstanceTypeOfferings: offerings,
					}, nil
				},
			}

			m := &Manager{
				ec2Client: mockClient,
				config:    &config.Config{},
			}

			got := m.filterAvailableInstanceTypes(context.Background(), tt.inputTypes)

			if len(got) != len(tt.wantTypes) {
				t.Errorf("filterAvailableInstanceTypes() got %d types, want %d", len(got), len(tt.wantTypes))
				return
			}
			for i, wantType := range tt.wantTypes {
				if got[i] != wantType {
					t.Errorf("filterAvailableInstanceTypes()[%d] = %q, want %q", i, got[i], wantType)
				}
			}
		})
	}
}

func TestBuildLaunchTemplateConfigs_FiltersUnavailableTypes(t *testing.T) {
	mockClient := &mockEC2Client{
		DescribeInstanceTypeOfferingsFunc: func(_ context.Context, _ *ec2.DescribeInstanceTypeOfferingsInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstanceTypeOfferingsOutput, error) {
			// Only t4g.medium and c7g.xlarge are available, c8g types are not
			return &ec2.DescribeInstanceTypeOfferingsOutput{
				InstanceTypeOfferings: []types.InstanceTypeOffering{
					{InstanceType: types.InstanceTypeT4gMedium},
					{InstanceType: types.InstanceTypeC7gXlarge},
				},
			}, nil
		},
	}

	m := &Manager{
		ec2Client: mockClient,
		config: &config.Config{
			LaunchTemplateName: "test-template",
		},
	}

	spec := &LaunchSpec{
		InstanceTypes: []string{"c8g.xlarge", "t4g.medium", "c7g.xlarge"},
		SubnetID:      "subnet-123",
		Arch:          "arm64",
	}

	configs, err := m.buildLaunchTemplateConfigs(context.Background(), spec)
	if err != nil {
		t.Fatalf("buildLaunchTemplateConfigs() error = %v", err)
	}

	if len(configs) != 1 {
		t.Fatalf("expected 1 config, got %d", len(configs))
	}

	// Should only have t4g.medium and c7g.xlarge (c8g.xlarge filtered out)
	overrides := configs[0].Overrides
	if len(overrides) != 2 {
		t.Errorf("expected 2 overrides (filtered), got %d", len(overrides))
	}

	hasC8g := false
	for _, o := range overrides {
		if strings.HasPrefix(string(o.InstanceType), "c8g") {
			hasC8g = true
		}
	}
	if hasC8g {
		t.Error("c8g instance type should have been filtered out")
	}
}

func TestBuildLaunchTemplateConfigs_AllTypesUnavailable(t *testing.T) {
	mockClient := &mockEC2Client{
		DescribeInstanceTypeOfferingsFunc: func(_ context.Context, _ *ec2.DescribeInstanceTypeOfferingsInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstanceTypeOfferingsOutput, error) {
			// Only t3 types are available
			return &ec2.DescribeInstanceTypeOfferingsOutput{
				InstanceTypeOfferings: []types.InstanceTypeOffering{
					{InstanceType: types.InstanceTypeT3Medium},
				},
			}, nil
		},
	}

	m := &Manager{
		ec2Client: mockClient,
		config:    &config.Config{},
	}

	spec := &LaunchSpec{
		InstanceTypes: []string{"c8g.xlarge", "m8g.large"},
		SubnetID:      "subnet-123",
		Arch:          "arm64",
	}

	_, err := m.buildLaunchTemplateConfigs(context.Background(), spec)
	if err == nil {
		t.Error("expected error when all instance types are unavailable")
	}
	if !strings.Contains(err.Error(), "no instance types available") {
		t.Errorf("expected 'no instance types available' error, got: %v", err)
	}
}

func TestCreateOnDemandInstance(t *testing.T) {
	t.Run("Creates on-demand instance", func(t *testing.T) {
		spec := &LaunchSpec{RunID: 12345, InstanceType: "t4g.medium", SubnetID: "subnet-1", Pool: "default", Arch: "arm64"}
		mock := &mockEC2Client{
			RunInstancesFunc: func(_ context.Context, params *ec2.RunInstancesInput, _ ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error) {
				if params.InstanceMarketOptions != nil {
					t.Error("InstanceMarketOptions should be nil for on-demand")
				}
				if params.InstanceType != types.InstanceType(spec.InstanceType) {
					t.Errorf("InstanceType = %v, want %v", params.InstanceType, spec.InstanceType)
				}
				if aws.ToString(params.SubnetId) != spec.SubnetID {
					t.Errorf("SubnetId = %v, want %v", aws.ToString(params.SubnetId), spec.SubnetID)
				}
				if aws.ToInt32(params.MinCount) != 1 || aws.ToInt32(params.MaxCount) != 1 {
					t.Errorf("MinCount/MaxCount should be 1")
				}
				if len(params.TagSpecifications) != 1 {
					t.Fatalf("TagSpecifications should have 1 entry (instance only), got %d", len(params.TagSpecifications))
				}
				if params.TagSpecifications[0].ResourceType != types.ResourceTypeInstance {
					t.Errorf("TagSpecification resource type = %v, want instance", params.TagSpecifications[0].ResourceType)
				}
				return &ec2.RunInstancesOutput{
					Instances: []types.Instance{{InstanceId: aws.String(testInstanceID)}},
				}, nil
			},
		}
		manager := &Manager{ec2Client: mock, config: &config.Config{LaunchTemplateName: "runs-fleet-runner"}}

		instanceID, err := manager.CreateOnDemandInstance(context.Background(), spec)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if instanceID != testInstanceID {
			t.Errorf("instanceID = %q, want %q", instanceID, testInstanceID)
		}
	})

	t.Run("Creates on-demand instance with storage", func(t *testing.T) {
		spec := &LaunchSpec{RunID: 12345, InstanceType: "c7g.xlarge", SubnetID: "subnet-1", Pool: "default", Arch: "arm64", StorageGiB: 100}
		mock := &mockEC2Client{
			RunInstancesFunc: func(_ context.Context, params *ec2.RunInstancesInput, _ ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error) {
				if len(params.BlockDeviceMappings) == 0 {
					t.Error("BlockDeviceMappings should not be empty when StorageGiB > 0")
				} else {
					bdm := params.BlockDeviceMappings[0]
					if aws.ToInt32(bdm.Ebs.VolumeSize) != 100 {
						t.Errorf("VolumeSize = %d, want 100", aws.ToInt32(bdm.Ebs.VolumeSize))
					}
					if !aws.ToBool(bdm.Ebs.Encrypted) {
						t.Error("EBS volume should be encrypted")
					}
					if bdm.Ebs.VolumeType != types.VolumeTypeGp3 {
						t.Errorf("VolumeType = %v, want gp3", bdm.Ebs.VolumeType)
					}
				}
				return &ec2.RunInstancesOutput{
					Instances: []types.Instance{{InstanceId: aws.String(testInstanceID)}},
				}, nil
			},
		}
		manager := &Manager{ec2Client: mock, config: &config.Config{LaunchTemplateName: "runs-fleet-runner"}}

		_, err := manager.CreateOnDemandInstance(context.Background(), spec)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("Returns error for unknown architecture", func(t *testing.T) {
		spec := &LaunchSpec{RunID: 12345, InstanceType: "x99.mystery", SubnetID: "subnet-1", Pool: "default"}
		manager := &Manager{ec2Client: &mockEC2Client{}, config: &config.Config{LaunchTemplateName: "runs-fleet-runner"}}

		_, err := manager.CreateOnDemandInstance(context.Background(), spec)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "cannot determine architecture") {
			t.Errorf("error = %q, want error containing 'cannot determine architecture'", err)
		}
	})

	t.Run("Returns error on API failure", func(t *testing.T) {
		mock := &mockEC2Client{
			RunInstancesFunc: func(_ context.Context, _ *ec2.RunInstancesInput, _ ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error) {
				return nil, errors.New("API error")
			},
		}
		spec := &LaunchSpec{RunID: 12345, InstanceType: "t4g.medium", SubnetID: "subnet-1", Pool: "default", Arch: "arm64"}
		manager := &Manager{ec2Client: mock, config: &config.Config{LaunchTemplateName: "runs-fleet-runner"}}

		_, err := manager.CreateOnDemandInstance(context.Background(), spec)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "failed to run instance") {
			t.Errorf("error = %q, want error containing 'failed to run instance'", err)
		}
	})

	t.Run("Returns error when no instances created", func(t *testing.T) {
		mock := &mockEC2Client{
			RunInstancesFunc: func(_ context.Context, _ *ec2.RunInstancesInput, _ ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error) {
				return &ec2.RunInstancesOutput{Instances: []types.Instance{}}, nil
			},
		}
		spec := &LaunchSpec{RunID: 12345, InstanceType: "t4g.medium", SubnetID: "subnet-1", Pool: "default", Arch: "arm64"}
		manager := &Manager{ec2Client: mock, config: &config.Config{LaunchTemplateName: "runs-fleet-runner"}}

		_, err := manager.CreateOnDemandInstance(context.Background(), spec)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "no instance created") {
			t.Errorf("error = %q, want error containing 'no instance created'", err)
		}
	})
}

func TestCreateOnDemandInstance_ArchAutoDetection(t *testing.T) {
	var capturedParams *ec2.RunInstancesInput

	mock := &mockEC2Client{
		RunInstancesFunc: func(_ context.Context, params *ec2.RunInstancesInput, _ ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error) {
			capturedParams = params
			return &ec2.RunInstancesOutput{
				Instances: []types.Instance{{InstanceId: aws.String(testInstanceID)}},
			}, nil
		},
	}

	t.Run("Auto-detects arm64 from instance type", func(t *testing.T) {
		manager := &Manager{ec2Client: mock, config: &config.Config{LaunchTemplateName: "runs-fleet-runner"}}
		spec := &LaunchSpec{RunID: 1, InstanceType: "t4g.medium", SubnetID: "subnet-1"}

		_, err := manager.CreateOnDemandInstance(context.Background(), spec)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got := aws.ToString(capturedParams.LaunchTemplate.LaunchTemplateName); got != "runs-fleet-runner-arm64" {
			t.Errorf("LaunchTemplateName = %q, want runs-fleet-runner-arm64", got)
		}
	})

	t.Run("Auto-detects amd64 from instance type", func(t *testing.T) {
		manager := &Manager{ec2Client: mock, config: &config.Config{LaunchTemplateName: "runs-fleet-runner"}}
		spec := &LaunchSpec{RunID: 1, InstanceType: "c6i.large", SubnetID: "subnet-1"}

		_, err := manager.CreateOnDemandInstance(context.Background(), spec)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got := aws.ToString(capturedParams.LaunchTemplate.LaunchTemplateName); got != "runs-fleet-runner-amd64" {
			t.Errorf("LaunchTemplateName = %q, want runs-fleet-runner-amd64", got)
		}
	})

	t.Run("Explicit arch overrides auto-detection", func(t *testing.T) {
		manager := &Manager{ec2Client: mock, config: &config.Config{LaunchTemplateName: "runs-fleet-runner"}}
		spec := &LaunchSpec{RunID: 1, InstanceType: "t4g.medium", SubnetID: "subnet-1", Arch: "amd64"}

		_, err := manager.CreateOnDemandInstance(context.Background(), spec)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got := aws.ToString(capturedParams.LaunchTemplate.LaunchTemplateName); got != "runs-fleet-runner-amd64" {
			t.Errorf("LaunchTemplateName = %q, want runs-fleet-runner-amd64 (explicit override)", got)
		}
	})
}

func TestCreateOnDemandInstance_ContextCancellation(t *testing.T) {
	mock := &mockEC2Client{
		RunInstancesFunc: func(ctx context.Context, _ *ec2.RunInstancesInput, _ ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error) {
			return nil, ctx.Err()
		},
	}

	manager := &Manager{ec2Client: mock, config: &config.Config{LaunchTemplateName: "runs-fleet-runner"}}
	spec := &LaunchSpec{RunID: 1, InstanceType: "t4g.medium", SubnetID: "subnet-1", Arch: "arm64"}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := manager.CreateOnDemandInstance(ctx, spec)
	if err == nil {
		t.Fatal("expected error on cancelled context")
	}
	if !strings.Contains(err.Error(), "failed to run instance") {
		t.Errorf("error = %q, want error containing 'failed to run instance'", err)
	}
}

func TestCreateOnDemandInstance_StorageBoundaries(t *testing.T) {
	tests := []struct {
		name       string
		storageGiB int
		wantBDM    bool
	}{
		{"Zero storage uses template default", 0, false},
		{"Minimum 1 GiB", 1, true},
		{"Maximum 16384 GiB", 16384, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var capturedParams *ec2.RunInstancesInput
			mock := &mockEC2Client{
				RunInstancesFunc: func(_ context.Context, params *ec2.RunInstancesInput, _ ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error) {
					capturedParams = params
					return &ec2.RunInstancesOutput{
						Instances: []types.Instance{{InstanceId: aws.String(testInstanceID)}},
					}, nil
				},
			}

			manager := &Manager{ec2Client: mock, config: &config.Config{LaunchTemplateName: "runs-fleet-runner"}}
			spec := &LaunchSpec{RunID: 1, InstanceType: "t4g.medium", SubnetID: "subnet-1", Arch: "arm64", StorageGiB: tt.storageGiB}

			_, err := manager.CreateOnDemandInstance(context.Background(), spec)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			hasBDM := len(capturedParams.BlockDeviceMappings) > 0
			if hasBDM != tt.wantBDM {
				t.Errorf("BlockDeviceMappings present = %v, want %v", hasBDM, tt.wantBDM)
			}
			if tt.wantBDM {
				bdm := capturedParams.BlockDeviceMappings[0]
				if aws.ToInt32(bdm.Ebs.VolumeSize) != int32(tt.storageGiB) {
					t.Errorf("VolumeSize = %d, want %d", aws.ToInt32(bdm.Ebs.VolumeSize), tt.storageGiB)
				}
				if !aws.ToBool(bdm.Ebs.Encrypted) {
					t.Error("EBS volume should be encrypted")
				}
				if !aws.ToBool(bdm.Ebs.DeleteOnTermination) {
					t.Error("EBS volume should be set to delete on termination")
				}
				if bdm.Ebs.VolumeType != types.VolumeTypeGp3 {
					t.Errorf("VolumeType = %v, want gp3", bdm.Ebs.VolumeType)
				}
			}
		})
	}
}

func TestCreateOnDemandInstance_Tags(t *testing.T) {
	var capturedTags []types.TagSpecification

	mock := &mockEC2Client{
		RunInstancesFunc: func(_ context.Context, params *ec2.RunInstancesInput, _ ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error) {
			capturedTags = params.TagSpecifications
			return &ec2.RunInstancesOutput{
				Instances: []types.Instance{{InstanceId: aws.String(testInstanceID)}},
			}, nil
		},
	}

	manager := &Manager{
		ec2Client: mock,
		config: &config.Config{
			LaunchTemplateName: "runs-fleet-runner",
			RunnerImage:        "ghcr.io/org/runner:latest",
		},
	}

	spec := &LaunchSpec{
		RunID:        12345,
		InstanceType: "t4g.medium",
		SubnetID:     "subnet-1",
		Pool:         "test-pool",
		Arch:         "arm64",
		Repo:         "my-repo",
	}

	_, err := manager.CreateOnDemandInstance(context.Background(), spec)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(capturedTags) != 1 {
		t.Fatalf("TagSpecifications should have 1 entry (instance only), got %d", len(capturedTags))
	}
	if capturedTags[0].ResourceType != types.ResourceTypeInstance {
		t.Errorf("ResourceType = %v, want instance", capturedTags[0].ResourceType)
	}

	tagMap := make(map[string]string)
	for _, tag := range capturedTags[0].Tags {
		tagMap[aws.ToString(tag.Key)] = aws.ToString(tag.Value)
	}

	expectedTags := map[string]string{
		"runs-fleet:run-id":  "12345",
		"runs-fleet:managed": "true",
		"runs-fleet:pool":    "test-pool",
		"runs-fleet:arch":    "arm64",
		"Role":               "my-repo",
	}

	for key, wantValue := range expectedTags {
		if gotValue, exists := tagMap[key]; !exists {
			t.Errorf("tag %q not found", key)
		} else if gotValue != wantValue {
			t.Errorf("tag %q = %q, want %q", key, gotValue, wantValue)
		}
	}
}

func TestRankInstanceTypesByPrice(t *testing.T) {
	tests := []struct {
		name          string
		instanceTypes []string
		prices        map[string]float64
		want          []string
	}{
		{
			name:          "weighted interleave cheapest first",
			instanceTypes: []string{"c7g.xlarge", "m7g.xlarge", "t4g.xlarge"},
			prices: map[string]float64{
				"c7g.xlarge": 0.0435,
				"m7g.xlarge": 0.0520,
				"t4g.xlarge": 0.0202,
			},
			// t4g: round(0.052/0.0202)=3, c7g: round(0.052/0.0435)=1, m7g: 1
			want: []string{"t4g.xlarge", "c7g.xlarge", "m7g.xlarge", "t4g.xlarge", "t4g.xlarge"},
		},
		{
			name:          "no prices preserves order",
			instanceTypes: []string{"c7g.xlarge", "m7g.xlarge"},
			prices:        nil,
			want:          []string{"c7g.xlarge", "m7g.xlarge"},
		},
		{
			name:          "single type returns it",
			instanceTypes: []string{"t4g.medium"},
			prices:        map[string]float64{"t4g.medium": 0.01},
			want:          []string{"t4g.medium"},
		},
		{
			name:          "unpriced types get weight 1",
			instanceTypes: []string{"c7g.xlarge", "unknown.type", "t4g.xlarge"},
			prices: map[string]float64{
				"c7g.xlarge": 0.0435,
				"t4g.xlarge": 0.0202,
			},
			// t4g: round(0.0435/0.0202)=2, c7g: 1, unknown: 1
			want: []string{"t4g.xlarge", "c7g.xlarge", "unknown.type", "t4g.xlarge"},
		},
		{
			name:          "equal prices equal weights",
			instanceTypes: []string{"a.large", "b.large"},
			prices: map[string]float64{
				"a.large": 0.05,
				"b.large": 0.05,
			},
			want: []string{"a.large", "b.large"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mgr := &Manager{
				ec2Client: &mockEC2Client{},
				config:    &config.Config{},
			}

			if tt.prices != nil {
				mgr.spotCache.mu.Lock()
				mgr.spotCache.prices = tt.prices
				mgr.spotCache.expires = time.Now().Add(5 * time.Minute)
				mgr.spotCache.mu.Unlock()
			}

			got := mgr.RankInstanceTypesByPrice(context.Background(), tt.instanceTypes)
			if len(got) != len(tt.want) {
				t.Fatalf("RankInstanceTypesByPrice() returned %d types, want %d: got %v", len(got), len(tt.want), got)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("index %d: got %q, want %q (full: %v)", i, got[i], tt.want[i], got)
				}
			}
		})
	}
}
