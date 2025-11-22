package metrics

import (
	"context"
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
		publish    func(p *Publisher)
	}{
		{
			name:       "QueueDepth",
			metricName: "QueueDepth",
			value:      10.0,
			unit:       types.StandardUnitCount,
			publish: func(p *Publisher) {
				p.PublishQueueDepth(context.Background(), 10.0)
			},
		},
		{
			name:       "FleetSize",
			metricName: "FleetSize",
			value:      5.0,
			unit:       types.StandardUnitCount,
			publish: func(p *Publisher) {
				p.PublishFleetSize(context.Background(), 5.0)
			},
		},
		{
			name:       "JobDuration",
			metricName: "JobDuration",
			value:      120.5,
			unit:       types.StandardUnitSeconds,
			publish: func(p *Publisher) {
				p.PublishJobDuration(context.Background(), 120.5)
			},
		},
		{
			name:       "SpotInterruptions",
			metricName: "SpotInterruptions",
			value:      1.0,
			unit:       types.StandardUnitCount,
			publish: func(p *Publisher) {
				p.PublishSpotInterruption(context.Background())
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockClient := &MockCloudWatchAPI{
				PutMetricDataFunc: func(ctx context.Context, params *cloudwatch.PutMetricDataInput, optFns ...func(*cloudwatch.Options)) (*cloudwatch.PutMetricDataOutput, error) {
					if *params.Namespace != "RunsFleet" {
						t.Errorf("Namespace = %s, want RunsFleet", *params.Namespace)
					}
					if len(params.MetricData) != 1 {
						t.Errorf("MetricData length = %d, want 1", len(params.MetricData))
					}
					datum := params.MetricData[0]
					if *datum.MetricName != tt.metricName {
						t.Errorf("MetricName = %s, want %s", *datum.MetricName, tt.metricName)
					}
					if *datum.Value != tt.value {
						t.Errorf("Value = %f, want %f", *datum.Value, tt.value)
					}
					if datum.Unit != tt.unit {
						t.Errorf("Unit = %v, want %v", datum.Unit, tt.unit)
					}
					return &cloudwatch.PutMetricDataOutput{}, nil
				},
			}

			publisher := &Publisher{
				client:    mockClient,
				namespace: "RunsFleet",
			}

			tt.publish(publisher)
		})
	}
}
