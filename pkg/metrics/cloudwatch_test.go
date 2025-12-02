package metrics

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
)

// MockCloudWatchAPI implements CloudWatchAPI interface
type MockCloudWatchAPI struct {
	PutMetricDataFunc func(ctx context.Context, params *cloudwatch.PutMetricDataInput, optFns ...func(*cloudwatch.Options)) (*cloudwatch.PutMetricDataOutput, error)
}

func (m *MockCloudWatchAPI) PutMetricData(ctx context.Context, params *cloudwatch.PutMetricDataInput, optFns ...func(*cloudwatch.Options)) (*cloudwatch.PutMetricDataOutput, error) {
	if m.PutMetricDataFunc != nil {
		return m.PutMetricDataFunc(ctx, params, optFns...)
	}
	return &cloudwatch.PutMetricDataOutput{}, nil
}

func TestPublishMetrics(t *testing.T) {
	tests := []struct {
		name       string
		metricName string
		value      float64
		unit       types.StandardUnit
		useStats   bool
		publish    func(p *Publisher) error
	}{
		{
			name:       "QueueDepth",
			metricName: "QueueDepth",
			value:      10.0,
			unit:       types.StandardUnitCount,
			useStats:   true,
			publish: func(p *Publisher) error {
				return p.PublishQueueDepth(context.Background(), 10.0)
			},
		},
		{
			name:       "FleetSizeIncrement",
			metricName: "FleetSizeIncrement",
			value:      1.0,
			unit:       types.StandardUnitCount,
			useStats:   false,
			publish: func(p *Publisher) error {
				return p.PublishFleetSizeIncrement(context.Background())
			},
		},
		{
			name:       "FleetSizeDecrement",
			metricName: "FleetSizeDecrement",
			value:      1.0,
			unit:       types.StandardUnitCount,
			useStats:   false,
			publish: func(p *Publisher) error {
				return p.PublishFleetSizeDecrement(context.Background())
			},
		},
		{
			name:       "JobDuration",
			metricName: "JobDuration",
			value:      120.0,
			unit:       types.StandardUnitSeconds,
			useStats:   false,
			publish: func(p *Publisher) error {
				return p.PublishJobDuration(context.Background(), 120)
			},
		},
		{
			name:       "SpotInterruptions",
			metricName: "SpotInterruptions",
			value:      1.0,
			unit:       types.StandardUnitCount,
			useStats:   false,
			publish: func(p *Publisher) error {
				return p.PublishSpotInterruption(context.Background())
			},
		},
		{
			name:       "MessageDeletionFailures",
			metricName: "MessageDeletionFailures",
			value:      1.0,
			unit:       types.StandardUnitCount,
			useStats:   false,
			publish: func(p *Publisher) error {
				return p.PublishMessageDeletionFailure(context.Background())
			},
		},
		{
			name:       "JobSuccess",
			metricName: "JobSuccess",
			value:      1.0,
			unit:       types.StandardUnitCount,
			useStats:   false,
			publish: func(p *Publisher) error {
				return p.PublishJobSuccess(context.Background())
			},
		},
		{
			name:       "JobFailure",
			metricName: "JobFailure",
			value:      1.0,
			unit:       types.StandardUnitCount,
			useStats:   false,
			publish: func(p *Publisher) error {
				return p.PublishJobFailure(context.Background())
			},
		},
		{
			name:       "JobQueued",
			metricName: "JobQueued",
			value:      1.0,
			unit:       types.StandardUnitCount,
			useStats:   false,
			publish: func(p *Publisher) error {
				return p.PublishJobQueued(context.Background())
			},
		},
		{
			name:       "CacheHits",
			metricName: "CacheHits",
			value:      1.0,
			unit:       types.StandardUnitCount,
			useStats:   false,
			publish: func(p *Publisher) error {
				return p.PublishCacheHit(context.Background())
			},
		},
		{
			name:       "CacheMisses",
			metricName: "CacheMisses",
			value:      1.0,
			unit:       types.StandardUnitCount,
			useStats:   false,
			publish: func(p *Publisher) error {
				return p.PublishCacheMiss(context.Background())
			},
		},
		{
			name:       "OrphanedInstancesTerminated",
			metricName: "OrphanedInstancesTerminated",
			value:      5.0,
			unit:       types.StandardUnitCount,
			useStats:   false,
			publish: func(p *Publisher) error {
				return p.PublishOrphanedInstancesTerminated(context.Background(), 5)
			},
		},
		{
			name:       "SSMParametersDeleted",
			metricName: "SSMParametersDeleted",
			value:      3.0,
			unit:       types.StandardUnitCount,
			useStats:   false,
			publish: func(p *Publisher) error {
				return p.PublishSSMParametersDeleted(context.Background(), 3)
			},
		},
		{
			name:       "JobRecordsArchived",
			metricName: "JobRecordsArchived",
			value:      10.0,
			unit:       types.StandardUnitCount,
			useStats:   false,
			publish: func(p *Publisher) error {
				return p.PublishJobRecordsArchived(context.Background(), 10)
			},
		},
	}

	const testNamespace = "RunsFleet" //nolint:goconst // Test constant, clearer inline

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockClient := &MockCloudWatchAPI{
				PutMetricDataFunc: func(_ context.Context, params *cloudwatch.PutMetricDataInput, _ ...func(*cloudwatch.Options)) (*cloudwatch.PutMetricDataOutput, error) {
					if *params.Namespace != testNamespace {
						t.Errorf("Namespace = %s, want %s", *params.Namespace, testNamespace)
					}
					if len(params.MetricData) != 1 {
						t.Errorf("MetricData length = %d, want 1", len(params.MetricData))
					}
					datum := params.MetricData[0]
					if *datum.MetricName != tt.metricName {
						t.Errorf("MetricName = %s, want %s", *datum.MetricName, tt.metricName)
					}
					if tt.useStats {
						if datum.StatisticValues == nil {
							t.Errorf("StatisticValues is nil, want non-nil")
						} else {
							if *datum.StatisticValues.Sum != tt.value {
								t.Errorf("StatisticValues.Sum = %f, want %f", *datum.StatisticValues.Sum, tt.value)
							}
							if *datum.StatisticValues.SampleCount != 1 {
								t.Errorf("StatisticValues.SampleCount = %f, want 1", *datum.StatisticValues.SampleCount)
							}
							if *datum.StatisticValues.Minimum != tt.value {
								t.Errorf("StatisticValues.Minimum = %f, want %f", *datum.StatisticValues.Minimum, tt.value)
							}
							if *datum.StatisticValues.Maximum != tt.value {
								t.Errorf("StatisticValues.Maximum = %f, want %f", *datum.StatisticValues.Maximum, tt.value)
							}
						}
					} else {
						if datum.Value == nil {
							t.Errorf("Value is nil, want %f", tt.value)
						} else if *datum.Value != tt.value {
							t.Errorf("Value = %f, want %f", *datum.Value, tt.value)
						}
					}
					if datum.Unit != tt.unit {
						t.Errorf("Unit = %v, want %v", datum.Unit, tt.unit)
					}
					return &cloudwatch.PutMetricDataOutput{}, nil
				},
			}

			publisher := &Publisher{
				client:    mockClient,
				namespace: testNamespace,
			}

			if err := tt.publish(publisher); err != nil {
				t.Errorf("publish() error = %v", err)
			}
		})
	}
}

func TestPublishPoolUtilization(t *testing.T) {
	tests := []struct {
		name        string
		poolName    string
		utilization float64
		mockErr     error
		wantErr     bool
	}{
		{
			name:        "success",
			poolName:    "default-pool",
			utilization: 75.5,
			mockErr:     nil,
			wantErr:     false,
		},
		{
			name:        "cloudwatch error",
			poolName:    "error-pool",
			utilization: 50.0,
			mockErr:     errors.New("cloudwatch error"),
			wantErr:     true,
		},
	}

	const namespace = "RunsFleet" //nolint:goconst // Test constant, clearer inline

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockClient := &MockCloudWatchAPI{
				PutMetricDataFunc: func(_ context.Context, params *cloudwatch.PutMetricDataInput, _ ...func(*cloudwatch.Options)) (*cloudwatch.PutMetricDataOutput, error) {
					if tt.mockErr != nil {
						return nil, tt.mockErr
					}
					if *params.Namespace != namespace {
						t.Errorf("Namespace = %s, want %s", *params.Namespace, namespace)
					}
					if len(params.MetricData) != 1 {
						t.Errorf("MetricData length = %d, want 1", len(params.MetricData))
					}
					datum := params.MetricData[0]
					if *datum.MetricName != "PoolUtilization" {
						t.Errorf("MetricName = %s, want PoolUtilization", *datum.MetricName)
					}
					if *datum.Value != tt.utilization {
						t.Errorf("Value = %f, want %f", *datum.Value, tt.utilization)
					}
					if datum.Unit != types.StandardUnitPercent {
						t.Errorf("Unit = %v, want Percent", datum.Unit)
					}
					if len(datum.Dimensions) != 1 {
						t.Errorf("Dimensions length = %d, want 1", len(datum.Dimensions))
					}
					if *datum.Dimensions[0].Name != "PoolName" {
						t.Errorf("Dimension Name = %s, want PoolName", *datum.Dimensions[0].Name)
					}
					if *datum.Dimensions[0].Value != tt.poolName {
						t.Errorf("Dimension Value = %s, want %s", *datum.Dimensions[0].Value, tt.poolName)
					}
					return &cloudwatch.PutMetricDataOutput{}, nil
				},
			}

			publisher := &Publisher{
				client:    mockClient,
				namespace: namespace,
			}

			err := publisher.PublishPoolUtilization(context.Background(), tt.poolName, tt.utilization)
			if (err != nil) != tt.wantErr {
				t.Errorf("PublishPoolUtilization() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

//nolint:dupl // Test functions have similar structure but test different methods - intentional pattern
func TestPublishSchedulingFailure(t *testing.T) {
	tests := []struct {
		name     string
		taskType string
		mockErr  error
		wantErr  bool
	}{
		{
			name:     "success",
			taskType: "runner-provision",
			mockErr:  nil,
			wantErr:  false,
		},
		{
			name:     "cloudwatch error",
			taskType: "cleanup",
			mockErr:  errors.New("cloudwatch error"),
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockClient := &MockCloudWatchAPI{
				PutMetricDataFunc: func(_ context.Context, params *cloudwatch.PutMetricDataInput, _ ...func(*cloudwatch.Options)) (*cloudwatch.PutMetricDataOutput, error) {
					if tt.mockErr != nil {
						return nil, tt.mockErr
					}
					if *params.Namespace != "RunsFleet" {
						t.Errorf("Namespace = %s, want RunsFleet", *params.Namespace)
					}
					datum := params.MetricData[0]
					if *datum.MetricName != "SchedulingFailure" {
						t.Errorf("MetricName = %s, want SchedulingFailure", *datum.MetricName)
					}
					if *datum.Value != 1 {
						t.Errorf("Value = %f, want 1", *datum.Value)
					}
					if len(datum.Dimensions) != 1 {
						t.Errorf("Dimensions length = %d, want 1", len(datum.Dimensions))
					}
					if *datum.Dimensions[0].Name != "TaskType" {
						t.Errorf("Dimension Name = %s, want TaskType", *datum.Dimensions[0].Name)
					}
					if *datum.Dimensions[0].Value != tt.taskType {
						t.Errorf("Dimension Value = %s, want %s", *datum.Dimensions[0].Value, tt.taskType)
					}
					return &cloudwatch.PutMetricDataOutput{}, nil
				},
			}

			publisher := &Publisher{
				client:    mockClient,
				namespace: "RunsFleet",
			}

			err := publisher.PublishSchedulingFailure(context.Background(), tt.taskType)
			if (err != nil) != tt.wantErr {
				t.Errorf("PublishSchedulingFailure() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

//nolint:dupl // Test functions have similar structure but test different methods - intentional pattern
func TestPublishCircuitBreakerTriggered(t *testing.T) {
	tests := []struct {
		name         string
		instanceType string
		mockErr      error
		wantErr      bool
	}{
		{
			name:         "success",
			instanceType: "t4g.medium",
			mockErr:      nil,
			wantErr:      false,
		},
		{
			name:         "cloudwatch error",
			instanceType: "c7g.xlarge",
			mockErr:      errors.New("cloudwatch error"),
			wantErr:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockClient := &MockCloudWatchAPI{
				PutMetricDataFunc: func(_ context.Context, params *cloudwatch.PutMetricDataInput, _ ...func(*cloudwatch.Options)) (*cloudwatch.PutMetricDataOutput, error) {
					if tt.mockErr != nil {
						return nil, tt.mockErr
					}
					if *params.Namespace != "RunsFleet" {
						t.Errorf("Namespace = %s, want RunsFleet", *params.Namespace)
					}
					datum := params.MetricData[0]
					if *datum.MetricName != "CircuitBreakerTriggered" {
						t.Errorf("MetricName = %s, want CircuitBreakerTriggered", *datum.MetricName)
					}
					if *datum.Value != 1 {
						t.Errorf("Value = %f, want 1", *datum.Value)
					}
					if len(datum.Dimensions) != 1 {
						t.Errorf("Dimensions length = %d, want 1", len(datum.Dimensions))
					}
					if *datum.Dimensions[0].Name != "InstanceType" {
						t.Errorf("Dimension Name = %s, want InstanceType", *datum.Dimensions[0].Name)
					}
					if *datum.Dimensions[0].Value != tt.instanceType {
						t.Errorf("Dimension Value = %s, want %s", *datum.Dimensions[0].Value, tt.instanceType)
					}
					return &cloudwatch.PutMetricDataOutput{}, nil
				},
			}

			publisher := &Publisher{
				client:    mockClient,
				namespace: "RunsFleet",
			}

			err := publisher.PublishCircuitBreakerTriggered(context.Background(), tt.instanceType)
			if (err != nil) != tt.wantErr {
				t.Errorf("PublishCircuitBreakerTriggered() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestPublishMetricsError(t *testing.T) {
	mockClient := &MockCloudWatchAPI{
		PutMetricDataFunc: func(_ context.Context, _ *cloudwatch.PutMetricDataInput, _ ...func(*cloudwatch.Options)) (*cloudwatch.PutMetricDataOutput, error) {
			return nil, errors.New("cloudwatch error")
		},
	}

	publisher := &Publisher{
		client:    mockClient,
		namespace: "RunsFleet",
	}

	// Test that errors are properly propagated
	if err := publisher.PublishQueueDepth(context.Background(), 10.0); err == nil {
		t.Error("PublishQueueDepth() should return error when CloudWatch fails")
	}
	if err := publisher.PublishJobSuccess(context.Background()); err == nil {
		t.Error("PublishJobSuccess() should return error when CloudWatch fails")
	}
}
