package cost_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/Shavakan/runs-fleet/pkg/cost"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sns"
)

type mockCloudWatchClient struct {
	getMetricDataFunc func(ctx context.Context, params *cloudwatch.GetMetricDataInput, optFns ...func(*cloudwatch.Options)) (*cloudwatch.GetMetricDataOutput, error)
}

func (m *mockCloudWatchClient) GetMetricData(ctx context.Context, params *cloudwatch.GetMetricDataInput, optFns ...func(*cloudwatch.Options)) (*cloudwatch.GetMetricDataOutput, error) {
	if m.getMetricDataFunc != nil {
		return m.getMetricDataFunc(ctx, params, optFns...)
	}
	return &cloudwatch.GetMetricDataOutput{
		MetricDataResults: []cwtypes.MetricDataResult{
			{
				Id:     aws.String("fleet_size_increment"),
				Values: []float64{100},
			},
			{
				Id:     aws.String("spot_interruptions"),
				Values: []float64{5},
			},
			{
				Id:     aws.String("job_success"),
				Values: []float64{90},
			},
			{
				Id:     aws.String("job_failure"),
				Values: []float64{5},
			},
			{
				Id:     aws.String("job_duration"),
				Values: []float64{18000}, // 5 hours total
			},
		},
	}, nil
}

type mockS3Client struct {
	putObjectFunc func(ctx context.Context, params *s3.PutObjectInput, optFns ...func(*s3.Options)) (*s3.PutObjectOutput, error)
}

func (m *mockS3Client) PutObject(ctx context.Context, params *s3.PutObjectInput, optFns ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	if m.putObjectFunc != nil {
		return m.putObjectFunc(ctx, params, optFns...)
	}
	return &s3.PutObjectOutput{}, nil
}

type mockSNSClient struct {
	publishFunc func(ctx context.Context, params *sns.PublishInput, optFns ...func(*sns.Options)) (*sns.PublishOutput, error)
}

func (m *mockSNSClient) Publish(ctx context.Context, params *sns.PublishInput, optFns ...func(*sns.Options)) (*sns.PublishOutput, error) {
	if m.publishFunc != nil {
		return m.publishFunc(ctx, params, optFns...)
	}
	return &sns.PublishOutput{
		MessageId: aws.String("test-message-id"),
	}, nil
}

type mockPriceFetcher struct {
	getPriceFunc func(ctx context.Context, instanceType string) (float64, error)
}

func (m *mockPriceFetcher) GetPrice(ctx context.Context, instanceType string) (float64, error) {
	if m.getPriceFunc != nil {
		return m.getPriceFunc(ctx, instanceType)
	}
	return 0.0336, nil // Default to t4g.medium price
}

func TestReporter_GenerateDailyReport(t *testing.T) {
	tests := []struct {
		name           string
		cwClient       *mockCloudWatchClient
		s3Client       *mockS3Client
		snsClient      *mockSNSClient
		priceFetcher   *mockPriceFetcher
		snsTopicARN    string
		reportsBucket  string
		wantErr        bool
		wantS3Called   bool
		wantSNSCalled  bool
	}{
		{
			name:          "successful report generation with S3 and SNS",
			cwClient:      &mockCloudWatchClient{},
			s3Client:      &mockS3Client{},
			snsClient:     &mockSNSClient{},
			priceFetcher:  &mockPriceFetcher{},
			snsTopicARN:   "arn:aws:sns:us-east-1:123456789:test-topic",
			reportsBucket: "test-bucket",
			wantErr:       false,
			wantS3Called:  true,
			wantSNSCalled: true,
		},
		{
			name:          "successful report generation without S3",
			cwClient:      &mockCloudWatchClient{},
			s3Client:      &mockS3Client{},
			snsClient:     &mockSNSClient{},
			priceFetcher:  &mockPriceFetcher{},
			snsTopicARN:   "arn:aws:sns:us-east-1:123456789:test-topic",
			reportsBucket: "",
			wantErr:       false,
			wantS3Called:  false,
			wantSNSCalled: true,
		},
		{
			name:          "successful report generation without SNS",
			cwClient:      &mockCloudWatchClient{},
			s3Client:      &mockS3Client{},
			snsClient:     &mockSNSClient{},
			priceFetcher:  &mockPriceFetcher{},
			snsTopicARN:   "",
			reportsBucket: "test-bucket",
			wantErr:       false,
			wantS3Called:  true,
			wantSNSCalled: false,
		},
		{
			name: "cloudwatch error",
			cwClient: &mockCloudWatchClient{
				getMetricDataFunc: func(_ context.Context, _ *cloudwatch.GetMetricDataInput, _ ...func(*cloudwatch.Options)) (*cloudwatch.GetMetricDataOutput, error) {
					return nil, errors.New("cloudwatch error")
				},
			},
			s3Client:      &mockS3Client{},
			snsClient:     &mockSNSClient{},
			priceFetcher:  &mockPriceFetcher{},
			snsTopicARN:   "arn:aws:sns:us-east-1:123456789:test-topic",
			reportsBucket: "test-bucket",
			wantErr:       true,
			wantS3Called:  false,
			wantSNSCalled: false,
		},
		{
			name:     "S3 error continues execution",
			cwClient: &mockCloudWatchClient{},
			s3Client: &mockS3Client{
				putObjectFunc: func(_ context.Context, _ *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
					return nil, errors.New("s3 error")
				},
			},
			snsClient:     &mockSNSClient{},
			priceFetcher:  &mockPriceFetcher{},
			snsTopicARN:   "arn:aws:sns:us-east-1:123456789:test-topic",
			reportsBucket: "test-bucket",
			wantErr:       false, // S3 errors are logged but don't fail
			wantS3Called:  true,
			wantSNSCalled: true,
		},
		{
			name:     "SNS error continues execution",
			cwClient: &mockCloudWatchClient{},
			s3Client: &mockS3Client{},
			snsClient: &mockSNSClient{
				publishFunc: func(_ context.Context, _ *sns.PublishInput, _ ...func(*sns.Options)) (*sns.PublishOutput, error) {
					return nil, errors.New("sns error")
				},
			},
			priceFetcher:  &mockPriceFetcher{},
			snsTopicARN:   "arn:aws:sns:us-east-1:123456789:test-topic",
			reportsBucket: "test-bucket",
			wantErr:       false, // SNS errors are logged but don't fail
			wantS3Called:  true,
			wantSNSCalled: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s3Called := false
			snsCalled := false

			// Track S3 calls
			if tt.s3Client != nil && tt.s3Client.putObjectFunc == nil {
				tt.s3Client.putObjectFunc = func(_ context.Context, _ *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
					s3Called = true
					return &s3.PutObjectOutput{}, nil
				}
			} else if tt.s3Client != nil {
				origFunc := tt.s3Client.putObjectFunc
				tt.s3Client.putObjectFunc = func(ctx context.Context, params *s3.PutObjectInput, optFns ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
					s3Called = true
					return origFunc(ctx, params, optFns...)
				}
			}

			// Track SNS calls
			if tt.snsClient != nil && tt.snsClient.publishFunc == nil {
				tt.snsClient.publishFunc = func(_ context.Context, _ *sns.PublishInput, _ ...func(*sns.Options)) (*sns.PublishOutput, error) {
					snsCalled = true
					return &sns.PublishOutput{MessageId: aws.String("test-message-id")}, nil
				}
			} else if tt.snsClient != nil {
				origFunc := tt.snsClient.publishFunc
				tt.snsClient.publishFunc = func(ctx context.Context, params *sns.PublishInput, optFns ...func(*sns.Options)) (*sns.PublishOutput, error) {
					snsCalled = true
					return origFunc(ctx, params, optFns...)
				}
			}

			cfg := &config.Config{}
			reporter := cost.NewReporterWithClients(
				tt.cwClient,
				tt.s3Client,
				tt.snsClient,
				tt.priceFetcher,
				cfg,
				tt.snsTopicARN,
				tt.reportsBucket,
			)

			err := reporter.GenerateDailyReport(context.Background())
			if (err != nil) != tt.wantErr {
				t.Errorf("GenerateDailyReport() error = %v, wantErr %v", err, tt.wantErr)
			}

			if tt.wantS3Called && !s3Called && tt.reportsBucket != "" {
				t.Error("Expected S3 to be called but it wasn't")
			}

			if tt.wantSNSCalled && !snsCalled && tt.snsTopicARN != "" {
				t.Error("Expected SNS to be called but it wasn't")
			}
		})
	}
}

func TestCostBreakdown_Calculations(t *testing.T) {
	// Test that cost calculations are reasonable
	cwClient := &mockCloudWatchClient{
		getMetricDataFunc: func(_ context.Context, _ *cloudwatch.GetMetricDataInput, _ ...func(*cloudwatch.Options)) (*cloudwatch.GetMetricDataOutput, error) {
			return &cloudwatch.GetMetricDataOutput{
				MetricDataResults: []cwtypes.MetricDataResult{
					{Id: aws.String("fleet_size_increment"), Values: []float64{100}},
					{Id: aws.String("spot_interruptions"), Values: []float64{5}},
					{Id: aws.String("job_success"), Values: []float64{95}},
					{Id: aws.String("job_failure"), Values: []float64{5}},
					{Id: aws.String("job_duration"), Values: []float64{36000}}, // 10 hours total for 100 jobs
				},
			}, nil
		},
	}

	var capturedBody string
	s3Client := &mockS3Client{
		putObjectFunc: func(_ context.Context, params *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
			body, _ := io.ReadAll(params.Body)
			capturedBody = string(body)
			return &s3.PutObjectOutput{}, nil
		},
	}

	cfg := &config.Config{}
	reporter := cost.NewReporterWithClients(
		cwClient,
		s3Client,
		&mockSNSClient{},
		&mockPriceFetcher{},
		cfg,
		"",
		"test-bucket",
	)

	err := reporter.GenerateDailyReport(context.Background())
	if err != nil {
		t.Fatalf("GenerateDailyReport() error = %v", err)
	}

	// Verify the report contains expected sections
	expectedSections := []string{
		"# runs-fleet Daily Cost Report",
		"## EC2 Compute",
		"## Supporting Services",
		"## Job Statistics",
		"Jobs completed:",
		"Spot interruptions:",
	}

	for _, section := range expectedSections {
		if !strings.Contains(capturedBody, section) {
			t.Errorf("Report missing expected section: %s", section)
		}
	}
}

func TestReporter_S3KeyFormat(t *testing.T) {
	now := time.Now()
	expectedPath := "cost/" + now.Add(-24*time.Hour).Format("2006") + "/" +
		now.Add(-24*time.Hour).Format("01") + "/" +
		now.Add(-24*time.Hour).Format("02") + ".md"

	var capturedKey string
	s3Client := &mockS3Client{
		putObjectFunc: func(_ context.Context, params *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
			capturedKey = *params.Key
			return &s3.PutObjectOutput{}, nil
		},
	}

	cfg := &config.Config{}
	reporter := cost.NewReporterWithClients(
		&mockCloudWatchClient{},
		s3Client,
		&mockSNSClient{},
		&mockPriceFetcher{},
		cfg,
		"",
		"test-bucket",
	)

	err := reporter.GenerateDailyReport(context.Background())
	if err != nil {
		t.Fatalf("GenerateDailyReport() error = %v", err)
	}

	if capturedKey != expectedPath {
		t.Errorf("S3 key = %v, want %v", capturedKey, expectedPath)
	}
}

func TestReporter_SNSSubjectFormat(t *testing.T) {
	var capturedSubject string
	snsClient := &mockSNSClient{
		publishFunc: func(_ context.Context, params *sns.PublishInput, _ ...func(*sns.Options)) (*sns.PublishOutput, error) {
			capturedSubject = *params.Subject
			return &sns.PublishOutput{MessageId: aws.String("test")}, nil
		},
	}

	cfg := &config.Config{}
	reporter := cost.NewReporterWithClients(
		&mockCloudWatchClient{},
		&mockS3Client{},
		snsClient,
		&mockPriceFetcher{},
		cfg,
		"arn:aws:sns:us-east-1:123456789:test-topic",
		"",
	)

	err := reporter.GenerateDailyReport(context.Background())
	if err != nil {
		t.Fatalf("GenerateDailyReport() error = %v", err)
	}

	if !strings.HasPrefix(capturedSubject, "runs-fleet Daily Cost Report -") {
		t.Errorf("SNS subject = %v, want prefix 'runs-fleet Daily Cost Report -'", capturedSubject)
	}
}

func TestReporter_ContentType(t *testing.T) {
	var capturedContentType string
	s3Client := &mockS3Client{
		putObjectFunc: func(_ context.Context, params *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
			capturedContentType = *params.ContentType
			return &s3.PutObjectOutput{}, nil
		},
	}

	cfg := &config.Config{}
	reporter := cost.NewReporterWithClients(
		&mockCloudWatchClient{},
		s3Client,
		&mockSNSClient{},
		&mockPriceFetcher{},
		cfg,
		"",
		"test-bucket",
	)

	err := reporter.GenerateDailyReport(context.Background())
	if err != nil {
		t.Fatalf("GenerateDailyReport() error = %v", err)
	}

	if capturedContentType != "text/markdown" {
		t.Errorf("Content-Type = %v, want text/markdown", capturedContentType)
	}
}

func TestReporter_ZeroJobs(t *testing.T) {
	cwClient := &mockCloudWatchClient{
		getMetricDataFunc: func(_ context.Context, _ *cloudwatch.GetMetricDataInput, _ ...func(*cloudwatch.Options)) (*cloudwatch.GetMetricDataOutput, error) {
			return &cloudwatch.GetMetricDataOutput{
				MetricDataResults: []cwtypes.MetricDataResult{
					{Id: aws.String("fleet_size_increment"), Values: []float64{0}},
					{Id: aws.String("spot_interruptions"), Values: []float64{0}},
					{Id: aws.String("job_success"), Values: []float64{0}},
					{Id: aws.String("job_failure"), Values: []float64{0}},
					{Id: aws.String("job_duration"), Values: []float64{0}},
				},
			}, nil
		},
	}

	var capturedBody string
	s3Client := &mockS3Client{
		putObjectFunc: func(_ context.Context, params *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
			body, _ := io.ReadAll(params.Body)
			capturedBody = string(body)
			return &s3.PutObjectOutput{}, nil
		},
	}

	cfg := &config.Config{}
	reporter := cost.NewReporterWithClients(
		cwClient,
		s3Client,
		&mockSNSClient{},
		&mockPriceFetcher{},
		cfg,
		"",
		"test-bucket",
	)

	err := reporter.GenerateDailyReport(context.Background())
	if err != nil {
		t.Fatalf("GenerateDailyReport() error = %v", err)
	}

	// Verify no cost per job line when zero jobs
	if strings.Contains(capturedBody, "Cost per job:") {
		t.Error("Report should not contain 'Cost per job' when there are zero jobs")
	}
}

func TestReporter_ReportContainsDisclaimer(t *testing.T) {
	var capturedBody string
	s3Client := &mockS3Client{
		putObjectFunc: func(_ context.Context, params *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
			body, _ := io.ReadAll(params.Body)
			capturedBody = string(body)
			return &s3.PutObjectOutput{}, nil
		},
	}

	cfg := &config.Config{}
	reporter := cost.NewReporterWithClients(
		&mockCloudWatchClient{},
		s3Client,
		&mockSNSClient{},
		&mockPriceFetcher{},
		cfg,
		"",
		"test-bucket",
	)

	err := reporter.GenerateDailyReport(context.Background())
	if err != nil {
		t.Fatalf("GenerateDailyReport() error = %v", err)
	}

	// The report should include the disclaimer about estimated costs
	if !strings.Contains(capturedBody, "DISCLAIMER") || !strings.Contains(capturedBody, "estimates") {
		t.Error("Report should contain disclaimer about estimated costs")
	}
}
