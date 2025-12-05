package fleet

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

type mockEC2Client struct {
	CreateFleetFunc func(ctx context.Context, params *ec2.CreateFleetInput, optFns ...func(*ec2.Options)) (*ec2.CreateFleetOutput, error)
}

func (m *mockEC2Client) CreateFleet(ctx context.Context, params *ec2.CreateFleetInput, optFns ...func(*ec2.Options)) (*ec2.CreateFleetOutput, error) {
	if m.CreateFleetFunc != nil {
		return m.CreateFleetFunc(ctx, params, optFns...)
	}
	return &ec2.CreateFleetOutput{}, nil
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
				RunID:        "run-1",
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
				RunID:        "run-1",
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
				RunID:        "run-2",
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
				RunID:        "run-3",
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
								InstanceIds: []string{"i-123456789"},
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
			if len(instanceIDs) > 0 && instanceIDs[0] != "i-123456789" {
				t.Errorf("CreateFleet() returned instance ID %s, want i-123456789", instanceIDs[0])
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
				RunID:        "run-1",
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
				RunID:        "run-2",
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
				RunID:        "run-3",
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
				RunID:        "run-4",
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
				RunID:        "run-1",
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
				RunID:        "run-2",
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
				RunID:        "run-3",
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
				RunID:        "run-4",
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
								InstanceIds: []string{"i-123456789"},
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
				RunID: "run-123",
			},
			wantTags: map[string]string{
				"runs-fleet:run-id":  "run-123",
				"runs-fleet:managed": "true",
			},
		},
		{
			name:   "With pool",
			config: &config.Config{},
			spec: &LaunchSpec{
				RunID: "run-123",
				Pool:  "default",
			},
			wantTags: map[string]string{
				"runs-fleet:run-id":  "run-123",
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
				RunID: "run-123",
			},
			wantTags: map[string]string{
				"runs-fleet:run-id":  "run-123",
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
				RunID: "run-123",
				OS:    "linux",
				Arch:  "arm64",
			},
			wantTags: map[string]string{
				"runs-fleet:run-id":  "run-123",
				"runs-fleet:managed": "true",
				"runs-fleet:os":      "linux",
				"runs-fleet:arch":    "arm64",
			},
		},
		{
			name: "With environment",
			config: &config.Config{},
			spec: &LaunchSpec{
				RunID:       "run-123",
				Environment: "production",
			},
			wantTags: map[string]string{
				"runs-fleet:run-id":      "run-123",
				"runs-fleet:managed":     "true",
				"runs-fleet:environment": "production",
				"Environment":            "production",
			},
		},
		{
			name:   "Empty pool should not create tag",
			config: &config.Config{},
			spec: &LaunchSpec{
				RunID: "run-123",
				Pool:  "",
			},
			wantTags: map[string]string{
				"runs-fleet:run-id":  "run-123",
				"runs-fleet:managed": "true",
			},
			wantAbsent: []string{"runs-fleet:pool"},
		},
		{
			name:   "With region tag",
			config: &config.Config{},
			spec: &LaunchSpec{
				RunID:  "run-123",
				Region: "us-west-2",
			},
			wantTags: map[string]string{
				"runs-fleet:run-id":  "run-123",
				"runs-fleet:managed": "true",
				"runs-fleet:region":  "us-west-2",
			},
		},
		{
			name:   "Empty region should not create tag",
			config: &config.Config{},
			spec: &LaunchSpec{
				RunID:  "run-123",
				Region: "",
			},
			wantTags: map[string]string{
				"runs-fleet:run-id":  "run-123",
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
				ConfigBucketName:    "my-config-bucket",
			},
			spec: &LaunchSpec{
				RunID: "run-123",
			},
			wantTags: map[string]string{
				"runs-fleet:run-id":              "run-123",
				"runs-fleet:managed":             "true",
				"runs-fleet:runner-image":        "ghcr.io/org/runner:latest",
				"runs-fleet:termination-queue-url": "https://sqs.us-west-2.amazonaws.com/123/term-queue",
				"runs-fleet:cache-url":           "https://cache.example.com",
				"runs-fleet:config-bucket":       "my-config-bucket",
			},
		},
		{
			name:   "Empty config values should not create tags",
			config: &config.Config{},
			spec: &LaunchSpec{
				RunID: "run-123",
			},
			wantTags: map[string]string{
				"runs-fleet:run-id":  "run-123",
				"runs-fleet:managed": "true",
			},
			wantAbsent: []string{
				"runs-fleet:runner-image",
				"runs-fleet:termination-queue-url",
				"runs-fleet:cache-url",
				"runs-fleet:config-bucket",
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
			name:   "Empty arch with mixed architectures creates two configs sorted",
			config: &config.Config{},
			spec: &LaunchSpec{
				OS:            "linux",
				Arch:          "",
				InstanceTypes: []string{"c7g.xlarge", "t3.medium", "t4g.medium", "c6i.large"},
				SubnetID:      "subnet-1",
			},
			wantConfigCount: 2,
			// Sorted alphabetically: amd64, arm64
			wantTemplateNames: []string{"runs-fleet-runner-amd64", "runs-fleet-runner-arm64"},
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
			wantErrContains: "no valid instance types found",
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
			m := &Manager{config: tt.config}
			configs, err := m.buildLaunchTemplateConfigs(tt.spec)

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
