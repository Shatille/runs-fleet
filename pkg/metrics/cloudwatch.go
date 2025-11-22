package metrics

import (
	"context"
	"log"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
)

type CloudWatchAPI interface {
	PutMetricData(ctx context.Context, params *cloudwatch.PutMetricDataInput, optFns ...func(*cloudwatch.Options)) (*cloudwatch.PutMetricDataOutput, error)
}

type Publisher struct {
	client    CloudWatchAPI
	namespace string
}

func NewPublisher(cfg aws.Config) *Publisher {
	return &Publisher{
		client:    cloudwatch.NewFromConfig(cfg),
		namespace: "RunsFleet",
	}
}

func (p *Publisher) PublishQueueDepth(ctx context.Context, depth float64) {
	p.putMetric(ctx, "QueueDepth", depth, types.StandardUnitCount)
}

func (p *Publisher) PublishFleetSize(ctx context.Context, size float64) {
	p.putMetric(ctx, "FleetSize", size, types.StandardUnitCount)
}

func (p *Publisher) PublishJobDuration(ctx context.Context, durationSeconds float64) {
	p.putMetric(ctx, "JobDuration", durationSeconds, types.StandardUnitSeconds)
}

func (p *Publisher) PublishSpotInterruption(ctx context.Context) {
	p.putMetric(ctx, "SpotInterruptions", 1, types.StandardUnitCount)
}

func (p *Publisher) putMetric(ctx context.Context, name string, value float64, unit types.StandardUnit) {
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
		log.Printf("Failed to publish metric %s: %v", name, err)
	}
}
