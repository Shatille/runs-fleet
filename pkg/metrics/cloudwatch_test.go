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

const testNamespace = "RunsFleet"

// capturedDatum records the single MetricDatum emitted by a publish call.
type capturedDatum struct {
	name       string
	value      float64
	hasValue   bool
	hasStats   bool
	statsSum   float64
	unit       types.StandardUnit
	dimensions map[string]string
}

func capturingPublisher(t *testing.T, out *capturedDatum) *CloudWatchPublisher {
	t.Helper()
	mockClient := &MockCloudWatchAPI{
		PutMetricDataFunc: func(_ context.Context, params *cloudwatch.PutMetricDataInput, _ ...func(*cloudwatch.Options)) (*cloudwatch.PutMetricDataOutput, error) {
			if *params.Namespace != testNamespace {
				t.Errorf("Namespace = %s, want %s", *params.Namespace, testNamespace)
			}
			if len(params.MetricData) != 1 {
				t.Fatalf("MetricData length = %d, want 1", len(params.MetricData))
			}
			d := params.MetricData[0]
			out.name = *d.MetricName
			out.unit = d.Unit
			if d.Value != nil {
				out.value = *d.Value
				out.hasValue = true
			}
			if d.StatisticValues != nil {
				out.hasStats = true
				out.statsSum = *d.StatisticValues.Sum
			}
			out.dimensions = map[string]string{}
			for _, dim := range d.Dimensions {
				out.dimensions[*dim.Name] = *dim.Value
			}
			return &cloudwatch.PutMetricDataOutput{}, nil
		},
	}
	return &CloudWatchPublisher{client: mockClient, namespace: testNamespace}
}

func TestCloudWatchPublisher_CounterMetrics(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		wantName string
		wantDims map[string]string
		publish  func(p *CloudWatchPublisher) error
	}{
		{
			name: "JobEnqueued", wantName: "JobsEnqueued",
			wantDims: map[string]string{"Pool": "default", "Arch": "arm64", "Capacity": "4", "Repo": "o/r"},
			publish: func(p *CloudWatchPublisher) error {
				return p.PublishJobEnqueued(context.Background(), "default", "arm64", "4", "o/r")
			},
		},
		{
			name: "JobAssigned", wantName: "JobsAssigned",
			wantDims: map[string]string{"Pool": "default", "Source": "warm_pool", "Repo": "o/r"},
			publish: func(p *CloudWatchPublisher) error {
				return p.PublishJobAssigned(context.Background(), "default", "warm_pool", "o/r")
			},
		},
		{
			name: "JobCompleted", wantName: "JobsCompleted",
			wantDims: map[string]string{"Pool": "default", "Result": "success", "Repo": "o/r"},
			publish: func(p *CloudWatchPublisher) error {
				return p.PublishJobCompleted(context.Background(), "default", "success", "o/r")
			},
		},
		{
			name: "JobRequeued", wantName: "JobsRequeued",
			wantDims: map[string]string{"Reason": "spot_interruption"},
			publish: func(p *CloudWatchPublisher) error {
				return p.PublishJobRequeued(context.Background(), "spot_interruption")
			},
		},
		{
			name: "FleetCreate", wantName: "FleetCreate",
			wantDims: map[string]string{"Capacity": "1", "Result": "success"},
			publish: func(p *CloudWatchPublisher) error {
				return p.PublishFleetCreate(context.Background(), "1", "success")
			},
		},
		{
			name: "SpotInterruption", wantName: "SpotInterruptions",
			wantDims: map[string]string{"Family": "c7g"},
			publish: func(p *CloudWatchPublisher) error {
				return p.PublishSpotInterruption(context.Background(), "c7g")
			},
		},
		{
			name: "CircuitBreakerTrip", wantName: "CircuitBreakerTrip",
			wantDims: map[string]string{"InstanceType": "c7g.xlarge"},
			publish: func(p *CloudWatchPublisher) error {
				return p.PublishCircuitBreakerTrip(context.Background(), "c7g.xlarge")
			},
		},
		{
			name: "PoolAction", wantName: "PoolActions",
			wantDims: map[string]string{"PoolName": "default", "Action": "create", "Reason": "ready_deficit"},
			publish: func(p *CloudWatchPublisher) error {
				return p.PublishPoolAction(context.Background(), "default", "create", "ready_deficit")
			},
		},
		{
			name: "QueueReceive", wantName: "QueueReceive",
			wantDims: map[string]string{"Queue": "main", "Result": "messages"},
			publish: func(p *CloudWatchPublisher) error {
				return p.PublishQueueReceive(context.Background(), "main", "messages")
			},
		},
		{
			name: "CacheRequest", wantName: "CacheRequests",
			wantDims: map[string]string{"Result": "hit"},
			publish: func(p *CloudWatchPublisher) error {
				return p.PublishCacheRequest(context.Background(), "hit")
			},
		},
		{
			name: "SchedulingFailure", wantName: "SchedulingFailure",
			wantDims: map[string]string{"TaskType": "job_claim"},
			publish: func(p *CloudWatchPublisher) error {
				return p.PublishSchedulingFailure(context.Background(), "job_claim")
			},
		},
		{
			name: "MessageDeletionFailure", wantName: "MessageDeletionFailures",
			wantDims: map[string]string{"Queue": "events"},
			publish: func(p *CloudWatchPublisher) error {
				return p.PublishMessageDeletionFailure(context.Background(), "events")
			},
		},
		{
			name: "AWSCallFailure", wantName: "AWSCallFailures",
			wantDims: map[string]string{"Service": "DynamoDB", "Operation": "GetItem", "Result": "timeout"},
			publish: func(p *CloudWatchPublisher) error {
				return p.PublishAWSCallFailure(context.Background(), "DynamoDB", "GetItem", "timeout")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var got capturedDatum
			p := capturingPublisher(t, &got)
			if err := tt.publish(p); err != nil {
				t.Fatalf("publish error = %v", err)
			}
			if got.name != tt.wantName {
				t.Errorf("name = %s, want %s", got.name, tt.wantName)
			}
			if !got.hasValue || got.value != 1 {
				t.Errorf("value = %v (hasValue=%v), want counter 1", got.value, got.hasValue)
			}
			if got.unit != types.StandardUnitCount {
				t.Errorf("unit = %v, want Count", got.unit)
			}
			assertDimMap(t, got.dimensions, tt.wantDims)
		})
	}
}

func TestCloudWatchPublisher_HousekeepingActionUsesCount(t *testing.T) {
	t.Parallel()
	var got capturedDatum
	p := capturingPublisher(t, &got)
	if err := p.PublishHousekeepingAction(context.Background(), "ssm_params", 7); err != nil {
		t.Fatalf("publish error = %v", err)
	}
	if got.name != "HousekeepingActions" {
		t.Errorf("name = %s, want HousekeepingActions", got.name)
	}
	if got.value != 7 {
		t.Errorf("value = %v, want 7", got.value)
	}
	assertDimMap(t, got.dimensions, map[string]string{"Action": "ssm_params"})
}

func TestCloudWatchPublisher_GaugeMetrics(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		wantName string
		wantSum  float64
		wantDims map[string]string
		publish  func(p *CloudWatchPublisher) error
	}{
		{
			name: "QueueDepth", wantName: "QueueDepth", wantSum: 1,
			wantDims: map[string]string{"Queue": "main"},
			publish: func(p *CloudWatchPublisher) error {
				return p.PublishQueueDepth(context.Background(), "main", 1)
			},
		},
		{
			name: "Instances", wantName: "Instances", wantSum: 3,
			wantDims: map[string]string{"State": "running", "Capacity": "4", "Pool": "default"},
			publish: func(p *CloudWatchPublisher) error {
				return p.PublishInstances(context.Background(), "running", "4", "default", 3)
			},
		},
		{
			name: "PoolInstances", wantName: "PoolInstances", wantSum: 5,
			wantDims: map[string]string{"PoolName": "default", "State": "ready"},
			publish: func(p *CloudWatchPublisher) error {
				return p.PublishPoolInstances(context.Background(), "default", "ready", 5)
			},
		},
		{
			name: "PoolDesired", wantName: "PoolDesired", wantSum: 2,
			wantDims: map[string]string{"PoolName": "default", "Kind": "running"},
			publish: func(p *CloudWatchPublisher) error {
				return p.PublishPoolDesired(context.Background(), "default", "running", 2)
			},
		},
		{
			name: "WorkerInflight", wantName: "WorkerInflight", wantSum: 4,
			wantDims: map[string]string{"Queue": "main"},
			publish: func(p *CloudWatchPublisher) error {
				return p.PublishWorkerInflight(context.Background(), "main", 4)
			},
		},
		{
			name: "CircuitBreakerOpen", wantName: "CircuitBreakerOpen", wantSum: 1,
			wantDims: map[string]string{"InstanceType": "c7g.xlarge"},
			publish: func(p *CloudWatchPublisher) error {
				return p.PublishCircuitBreakerOpen(context.Background(), "c7g.xlarge", true)
			},
		},
		{
			name: "EstimatedCost", wantName: "EstimatedCost", wantSum: 12.5,
			wantDims: map[string]string{},
			publish: func(p *CloudWatchPublisher) error {
				return p.PublishEstimatedCost(context.Background(), 12.5)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var got capturedDatum
			p := capturingPublisher(t, &got)
			if err := tt.publish(p); err != nil {
				t.Fatalf("publish error = %v", err)
			}
			if got.name != tt.wantName {
				t.Errorf("name = %s, want %s", got.name, tt.wantName)
			}
			if !got.hasStats || got.statsSum != tt.wantSum {
				t.Errorf("stats sum = %v (hasStats=%v), want %v", got.statsSum, got.hasStats, tt.wantSum)
			}
			assertDimMap(t, got.dimensions, tt.wantDims)
		})
	}
}

func TestCloudWatchPublisher_LowFrequencyLatencyAsStatistic(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		wantName string
		wantDims map[string]string
		publish  func(p *CloudWatchPublisher) error
	}{
		{
			name: "JobWaitSeconds", wantName: "JobWaitSeconds",
			wantDims: map[string]string{"Pool": "default", "Source": "cold_start"},
			publish: func(p *CloudWatchPublisher) error {
				return p.PublishJobWaitSeconds(context.Background(), "default", "cold_start", 12)
			},
		},
		{
			name: "JobExecutionSeconds", wantName: "JobExecutionSeconds",
			wantDims: map[string]string{"Pool": "default", "Result": "success"},
			publish: func(p *CloudWatchPublisher) error {
				return p.PublishJobExecutionSeconds(context.Background(), "default", "success", 90)
			},
		},
		{
			name: "InstanceProvisionSeconds", wantName: "InstanceProvisionSeconds",
			wantDims: map[string]string{"Source": "cold_start", "Family": "c7g"},
			publish: func(p *CloudWatchPublisher) error {
				return p.PublishInstanceProvisionSeconds(context.Background(), "cold_start", "c7g", 30)
			},
		},
		{
			name: "FleetCreateSeconds", wantName: "FleetCreateSeconds",
			wantDims: map[string]string{"Capacity": "1"},
			publish: func(p *CloudWatchPublisher) error {
				return p.PublishFleetCreateSeconds(context.Background(), "1", 5)
			},
		},
		{
			name: "PoolReconcileSeconds", wantName: "PoolReconcileSeconds",
			wantDims: map[string]string{},
			publish: func(p *CloudWatchPublisher) error {
				return p.PublishPoolReconcileSeconds(context.Background(), 2)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var got capturedDatum
			p := capturingPublisher(t, &got)
			if err := tt.publish(p); err != nil {
				t.Fatalf("publish error = %v", err)
			}
			if got.name != tt.wantName {
				t.Errorf("name = %s, want %s", got.name, tt.wantName)
			}
			if !got.hasStats {
				t.Errorf("%s should emit a StatisticSet", tt.wantName)
			}
			if got.unit != types.StandardUnitSeconds {
				t.Errorf("unit = %v, want Seconds", got.unit)
			}
			assertDimMap(t, got.dimensions, tt.wantDims)
		})
	}
}

func TestCloudWatchPublisher_HighFrequencyLatencyNoOps(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		publish func(p *CloudWatchPublisher) error
	}{
		{"AWSCallDuration", func(p *CloudWatchPublisher) error {
			return p.PublishAWSCallDuration(context.Background(), "SQS", "ReceiveMessage", 1.5)
		}},
		{"MessageProcessingSeconds", func(p *CloudWatchPublisher) error {
			return p.PublishMessageProcessingSeconds(context.Background(), "main", "success", 0.2)
		}},
		{"LockWaitSeconds", func(p *CloudWatchPublisher) error {
			return p.PublishLockWaitSeconds(context.Background(), "pool_reconcile", 0.05)
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			mockClient := &MockCloudWatchAPI{
				PutMetricDataFunc: func(_ context.Context, _ *cloudwatch.PutMetricDataInput, _ ...func(*cloudwatch.Options)) (*cloudwatch.PutMetricDataOutput, error) {
					t.Errorf("%s must not call PutMetricData on the CloudWatch backend", tt.name)
					return &cloudwatch.PutMetricDataOutput{}, nil
				},
			}
			p := &CloudWatchPublisher{client: mockClient, namespace: testNamespace}
			if err := tt.publish(p); err != nil {
				t.Errorf("%s() error = %v", tt.name, err)
			}
		})
	}
}

func TestCloudWatchPublisher_OmitsEmptyDimensions(t *testing.T) {
	t.Parallel()
	var got capturedDatum
	p := capturingPublisher(t, &got)
	// Empty pool and repo must be dropped, leaving only source.
	if err := p.PublishJobAssigned(context.Background(), "", "cold_start", ""); err != nil {
		t.Fatalf("publish error = %v", err)
	}
	assertDimMap(t, got.dimensions, map[string]string{"Source": "cold_start"})
}

func assertDimMap(t *testing.T, got, want map[string]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("dimensions = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("dimension %s = %s, want %s", k, got[k], v)
		}
	}
}

func TestPublishMetricsError(t *testing.T) {
	t.Parallel()

	mockClient := &MockCloudWatchAPI{
		PutMetricDataFunc: func(_ context.Context, _ *cloudwatch.PutMetricDataInput, _ ...func(*cloudwatch.Options)) (*cloudwatch.PutMetricDataOutput, error) {
			return nil, errors.New("cloudwatch error")
		},
	}

	publisher := &CloudWatchPublisher{client: mockClient, namespace: testNamespace}

	if err := publisher.PublishQueueDepth(context.Background(), "main", 10.0); err == nil {
		t.Error("PublishQueueDepth() should return error when CloudWatch fails")
	}
	if err := publisher.PublishJobCompleted(context.Background(), "default", "success", "o/r"); err == nil {
		t.Error("PublishJobCompleted() should return error when CloudWatch fails")
	}
}

func TestCloudWatchPublisher_Close(t *testing.T) {
	t.Parallel()

	publisher := &CloudWatchPublisher{client: &MockCloudWatchAPI{}, namespace: testNamespace}
	if err := publisher.Close(); err != nil {
		t.Errorf("Close() error = %v, want nil", err)
	}
}

func TestCloudWatchPublisher_ImplementsInterface(_ *testing.T) {
	var _ Publisher = (*CloudWatchPublisher)(nil)
}
