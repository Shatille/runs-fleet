// Package metrics publishes CloudWatch metrics.
package metrics

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
)

// CloudWatchAPI provides CloudWatch operations.
type CloudWatchAPI interface {
	PutMetricData(ctx context.Context, params *cloudwatch.PutMetricDataInput, optFns ...func(*cloudwatch.Options)) (*cloudwatch.PutMetricDataOutput, error)
}

// Publisher publishes metrics to CloudWatch.
type Publisher struct {
	client    CloudWatchAPI
	namespace string
}

// NewPublisher creates a CloudWatch metrics publisher.
func NewPublisher(cfg aws.Config) *Publisher {
	return &Publisher{
		client:    cloudwatch.NewFromConfig(cfg),
		namespace: "RunsFleet",
	}
}

// PublishQueueDepth publishes queue depth metric.
func (p *Publisher) PublishQueueDepth(ctx context.Context, depth float64) error {
	return p.putMetric(ctx, "QueueDepth", depth, types.StandardUnitCount)
}

// PublishFleetSizeIncrement publishes fleet size increment metric.
func (p *Publisher) PublishFleetSizeIncrement(ctx context.Context) error {
	return p.putMetric(ctx, "FleetSizeIncrement", 1, types.StandardUnitCount)
}

// PublishFleetSizeDecrement publishes fleet size decrement metric.
func (p *Publisher) PublishFleetSizeDecrement(ctx context.Context) error {
	return p.putMetric(ctx, "FleetSizeDecrement", 1, types.StandardUnitCount)
}

// PublishJobDuration publishes job duration metric.
func (p *Publisher) PublishJobDuration(ctx context.Context, durationSeconds float64) error {
	return p.putMetric(ctx, "JobDuration", durationSeconds, types.StandardUnitSeconds)
}

// PublishSpotInterruption publishes spot interruption metric.
func (p *Publisher) PublishSpotInterruption(ctx context.Context) error {
	return p.putMetric(ctx, "SpotInterruptions", 1, types.StandardUnitCount)
}

// PublishMessageDeletionFailure publishes message deletion failure metric.
func (p *Publisher) PublishMessageDeletionFailure(ctx context.Context) error {
	return p.putMetric(ctx, "MessageDeletionFailures", 1, types.StandardUnitCount)
}

func (p *Publisher) putMetric(ctx context.Context, name string, value float64, unit types.StandardUnit) error {
	_, err := p.client.PutMetricData(ctx, &cloudwatch.PutMetricDataInput{
		Namespace: aws.String(p.namespace),
		MetricData: []types.MetricDatum{
			{
				MetricName: aws.String(name),
				Value:      aws.Float64(value),
				Unit:       unit,
				Timestamp:  aws.Time(time.Now()),
			},
		},
	})
	if err != nil {
		return fmt.Errorf("failed to publish metric %s: %w", name, err)
	}
	return nil
}
