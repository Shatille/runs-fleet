package cost_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/Shavakan/runs-fleet/pkg/cost"
	"github.com/Shavakan/runs-fleet/pkg/db"
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
	// Default: interruptions query returns 5, runner-minute queries return nothing.
	return &cloudwatch.GetMetricDataOutput{
		MetricDataResults: []cwtypes.MetricDataResult{
			{Id: aws.String("spot_interruptions"), Values: []float64{5}},
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
	return &sns.PublishOutput{MessageId: aws.String("test-message-id")}, nil
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

type mockJobLister struct {
	jobs         []db.AdminJobEntry
	err          error
	gotFilter    db.AdminJobFilter
	filterCalled bool
}

func (m *mockJobLister) ListJobsForAdmin(_ context.Context, filter db.AdminJobFilter) ([]db.AdminJobEntry, int, error) {
	m.gotFilter = filter
	m.filterCalled = true
	if m.err != nil {
		return nil, 0, m.err
	}
	return m.jobs, len(m.jobs), nil
}

func captureReport(putObjectBody *string) *mockS3Client {
	return &mockS3Client{
		putObjectFunc: func(_ context.Context, params *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
			body, _ := io.ReadAll(params.Body)
			*putObjectBody = string(body)
			return &s3.PutObjectOutput{}, nil
		},
	}
}

func TestReporter_GenerateDailyReport(t *testing.T) {
	tests := []struct {
		name          string
		cwClient      *mockCloudWatchClient
		s3Client      *mockS3Client
		snsClient     *mockSNSClient
		lister        *mockJobLister
		snsTopicARN   string
		reportsBucket string
		wantErr       bool
		wantS3Called  bool
		wantSNSCalled bool
	}{
		{
			name:          "successful report generation with S3 and SNS",
			cwClient:      &mockCloudWatchClient{},
			s3Client:      &mockS3Client{},
			snsClient:     &mockSNSClient{},
			lister:        &mockJobLister{},
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
			lister:        &mockJobLister{},
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
			lister:        &mockJobLister{},
			snsTopicARN:   "",
			reportsBucket: "test-bucket",
			wantErr:       false,
			wantS3Called:  true,
			wantSNSCalled: false,
		},
		{
			name:          "job lister error fails the run",
			cwClient:      &mockCloudWatchClient{},
			s3Client:      &mockS3Client{},
			snsClient:     &mockSNSClient{},
			lister:        &mockJobLister{err: errors.New("dynamo down")},
			snsTopicARN:   "arn:aws:sns:us-east-1:123456789:test-topic",
			reportsBucket: "test-bucket",
			wantErr:       true,
			wantS3Called:  false,
			wantSNSCalled: false,
		},
		{
			name: "cloudwatch interruptions error degrades, does not fail",
			cwClient: &mockCloudWatchClient{
				getMetricDataFunc: func(_ context.Context, _ *cloudwatch.GetMetricDataInput, _ ...func(*cloudwatch.Options)) (*cloudwatch.GetMetricDataOutput, error) {
					return nil, errors.New("cloudwatch error")
				},
			},
			s3Client:      &mockS3Client{},
			snsClient:     &mockSNSClient{},
			lister:        &mockJobLister{},
			snsTopicARN:   "arn:aws:sns:us-east-1:123456789:test-topic",
			reportsBucket: "test-bucket",
			wantErr:       false,
			wantS3Called:  true,
			wantSNSCalled: true,
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
			lister:        &mockJobLister{},
			snsTopicARN:   "arn:aws:sns:us-east-1:123456789:test-topic",
			reportsBucket: "test-bucket",
			wantErr:       false,
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
			lister:        &mockJobLister{},
			snsTopicARN:   "arn:aws:sns:us-east-1:123456789:test-topic",
			reportsBucket: "test-bucket",
			wantErr:       false,
			wantS3Called:  true,
			wantSNSCalled: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s3Called := false
			snsCalled := false

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
				tt.cwClient, tt.s3Client, tt.snsClient, tt.lister, &mockPriceFetcher{}, nil, cfg, tt.snsTopicARN, tt.reportsBucket,
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

func TestReporter_EC2CostFromJobRecords(t *testing.T) {
	// Two jobs on c7g.xlarge (hard-coded on-demand $0.145/hr): one spot 1h, one
	// on-demand 1h. nil pricers -> hard-coded table + fixed 70% spot discount.
	lister := &mockJobLister{
		jobs: []db.AdminJobEntry{
			{JobID: 1, InstanceType: "c7g.xlarge", Spot: true, DurationSeconds: 3600, Status: string(db.JobStatusCompleted)},
			{JobID: 2, InstanceType: "c7g.xlarge", Spot: false, DurationSeconds: 3600, Status: string(db.JobStatusCompleted)},
		},
	}

	// nil price fetcher -> hard-coded on-demand table + fixed 70% spot discount.
	var body string
	reporter := cost.NewReporterWithClients(
		&mockCloudWatchClient{}, captureReport(&body), &mockSNSClient{}, lister, nil, nil, &config.Config{}, "", "test-bucket",
	)
	if err := reporter.GenerateDailyReport(context.Background()); err != nil {
		t.Fatalf("GenerateDailyReport() error = %v", err)
	}

	od := cost.GetInstancePrice("c7g.xlarge")
	// Spot: 1h * od * 0.3 ; On-demand: 1h * od.
	wantSpot := od * (1 - cost.SpotDiscount)
	for _, want := range []string{
		"Spot instances: $" + fmtDollar(wantSpot),
		"On-demand instances: $" + fmtDollar(od),
		"Jobs completed: 2",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("report missing %q\n---\n%s", want, body)
		}
	}
}

func TestReporter_EC2SectionUsesRolling24hUTCWindow(t *testing.T) {
	lister := &mockJobLister{}
	reporter := cost.NewReporterWithClients(
		&mockCloudWatchClient{}, &mockS3Client{}, &mockSNSClient{}, lister, &mockPriceFetcher{}, nil, &config.Config{}, "", "test-bucket",
	)
	if err := reporter.GenerateDailyReport(context.Background()); err != nil {
		t.Fatalf("GenerateDailyReport() error = %v", err)
	}

	if !lister.filterCalled {
		t.Fatal("expected job lister to be called")
	}
	f := lister.gotFilter
	if f.Status != string(db.JobStatusCompleted) {
		t.Errorf("filter status = %q, want completed", f.Status)
	}
	if f.Limit != 10000 {
		t.Errorf("filter limit = %d, want 10000", f.Limit)
	}
	if f.Since.IsZero() || f.Until.IsZero() {
		t.Fatalf("expected bounded window, got since=%v until=%v", f.Since, f.Until)
	}
	if f.Since.Location() != time.UTC || f.Until.Location() != time.UTC {
		t.Errorf("window must be UTC, got since=%v until=%v", f.Since.Location(), f.Until.Location())
	}
	if d := f.Until.Sub(f.Since); d != 24*time.Hour {
		t.Errorf("window = %v, want 24h", d)
	}
}

func TestReporter_SpotInterruptionsFromSearch(t *testing.T) {
	var gotExpr string
	cwClient := &mockCloudWatchClient{
		getMetricDataFunc: func(_ context.Context, params *cloudwatch.GetMetricDataInput, _ ...func(*cloudwatch.Options)) (*cloudwatch.GetMetricDataOutput, error) {
			if len(params.MetricDataQueries) > 0 && params.MetricDataQueries[0].Id != nil &&
				*params.MetricDataQueries[0].Id == "spot_interruptions" {
				if params.MetricDataQueries[0].Expression != nil {
					gotExpr = *params.MetricDataQueries[0].Expression
				}
				return &cloudwatch.GetMetricDataOutput{
					MetricDataResults: []cwtypes.MetricDataResult{
						{Id: aws.String("spot_interruptions"), Values: []float64{3, 4}},
					},
				}, nil
			}
			return &cloudwatch.GetMetricDataOutput{}, nil
		},
	}

	var body string
	reporter := cost.NewReporterWithClients(
		cwClient, captureReport(&body), &mockSNSClient{}, &mockJobLister{}, &mockPriceFetcher{}, nil, &config.Config{}, "", "test-bucket",
	)
	if err := reporter.GenerateDailyReport(context.Background()); err != nil {
		t.Fatalf("GenerateDailyReport() error = %v", err)
	}

	if !strings.Contains(gotExpr, `MetricName="SpotInterruptions"`) {
		t.Errorf("interruption query missing metric name: %q", gotExpr)
	}
	if !strings.HasPrefix(gotExpr, "SUM(SEARCH(") {
		t.Errorf("interruption query should sum a search: %q", gotExpr)
	}
	if !strings.Contains(gotExpr, "Family") {
		t.Errorf("interruption query should search across the Family dimension: %q", gotExpr)
	}
	// 3 + 4 = 7 interruptions summed across the window.
	if !strings.Contains(body, "Spot interruptions: 7") {
		t.Errorf("report missing 'Spot interruptions: 7'\n---\n%s", body)
	}
}

func TestReporter_SpotInterruptionsZeroWithoutData(t *testing.T) {
	var body string
	reporter := cost.NewReporterWithClients(
		&mockCloudWatchClient{
			getMetricDataFunc: func(_ context.Context, _ *cloudwatch.GetMetricDataInput, _ ...func(*cloudwatch.Options)) (*cloudwatch.GetMetricDataOutput, error) {
				return &cloudwatch.GetMetricDataOutput{}, nil
			},
		},
		captureReport(&body), &mockSNSClient{}, &mockJobLister{}, &mockPriceFetcher{}, nil, &config.Config{}, "", "test-bucket",
	)
	if err := reporter.GenerateDailyReport(context.Background()); err != nil {
		t.Fatalf("GenerateDailyReport() error = %v", err)
	}
	if !strings.Contains(body, "Spot interruptions: 0") {
		t.Errorf("report should show zero interruptions without data\n---\n%s", body)
	}
}

func TestReporter_NilListerDegradesToZeros(t *testing.T) {
	var body string
	reporter := cost.NewReporterWithClients(
		&mockCloudWatchClient{}, captureReport(&body), &mockSNSClient{}, nil, &mockPriceFetcher{}, nil, &config.Config{}, "", "test-bucket",
	)
	if err := reporter.GenerateDailyReport(context.Background()); err != nil {
		t.Fatalf("GenerateDailyReport() error = %v (nil lister must degrade, not fail)", err)
	}
	if !strings.Contains(body, "Jobs completed: 0") {
		t.Errorf("nil lister should produce zero jobs\n---\n%s", body)
	}
}

func TestReporter_RunnerMinuteCost(t *testing.T) {
	// getCostMetrics issues a runner-execution (rxs_*) query for the runner-minute
	// cost. Distinguish it from the interruption query by the first query ID.
	cwClient := &mockCloudWatchClient{
		getMetricDataFunc: func(_ context.Context, params *cloudwatch.GetMetricDataInput, _ ...func(*cloudwatch.Options)) (*cloudwatch.GetMetricDataOutput, error) {
			if len(params.MetricDataQueries) > 0 && params.MetricDataQueries[0].Id != nil &&
				strings.HasPrefix(*params.MetricDataQueries[0].Id, "rxs_") {
				return &cloudwatch.GetMetricDataOutput{
					MetricDataResults: []cwtypes.MetricDataResult{
						// arm64 / 4 vCPU ran 7200s => 120 min => 480 vCPU-min => $0.60
						{Id: aws.String("rxs_arm64_4"), Values: []float64{3600, 3600}},
					},
				}, nil
			}
			return &cloudwatch.GetMetricDataOutput{}, nil
		},
	}

	var body string
	reporter := cost.NewReporterWithClients(
		cwClient, captureReport(&body), &mockSNSClient{}, &mockJobLister{}, &mockPriceFetcher{}, nil, &config.Config{}, "", "test-bucket",
	)
	if err := reporter.GenerateDailyReport(context.Background()); err != nil {
		t.Fatalf("GenerateDailyReport() error = %v", err)
	}

	for _, want := range []string{
		"## Runner-Minute Cost (standard per-vCPU-minute pricing)",
		"480 vCPU-minutes",
		"$0.60",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("report missing %q\n---\n%s", want, body)
		}
	}
}

func TestReporter_RunnerMinuteCostOmittedWithoutData(t *testing.T) {
	var body string
	reporter := cost.NewReporterWithClients(
		&mockCloudWatchClient{}, captureReport(&body), &mockSNSClient{}, &mockJobLister{}, &mockPriceFetcher{}, nil, &config.Config{}, "", "test-bucket",
	)
	if err := reporter.GenerateDailyReport(context.Background()); err != nil {
		t.Fatalf("GenerateDailyReport() error = %v", err)
	}
	if strings.Contains(body, "Runner-Minute Cost") {
		t.Errorf("runner-minute section should be omitted without metric data\n---\n%s", body)
	}
}

func TestReporter_S3KeyFormat(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Hour)
	start := now.Add(-24 * time.Hour)
	expectedPath := "cost/" + start.Format("2006") + "/" + start.Format("01") + "/" + start.Format("02") + ".md"

	var capturedKey string
	s3Client := &mockS3Client{
		putObjectFunc: func(_ context.Context, params *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
			capturedKey = *params.Key
			return &s3.PutObjectOutput{}, nil
		},
	}

	reporter := cost.NewReporterWithClients(
		&mockCloudWatchClient{}, s3Client, &mockSNSClient{}, &mockJobLister{}, &mockPriceFetcher{}, nil, &config.Config{}, "", "test-bucket",
	)
	if err := reporter.GenerateDailyReport(context.Background()); err != nil {
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

	reporter := cost.NewReporterWithClients(
		&mockCloudWatchClient{}, &mockS3Client{}, snsClient, &mockJobLister{}, &mockPriceFetcher{}, nil, &config.Config{}, "arn:aws:sns:us-east-1:123456789:test-topic", "",
	)
	if err := reporter.GenerateDailyReport(context.Background()); err != nil {
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

	reporter := cost.NewReporterWithClients(
		&mockCloudWatchClient{}, s3Client, &mockSNSClient{}, &mockJobLister{}, &mockPriceFetcher{}, nil, &config.Config{}, "", "test-bucket",
	)
	if err := reporter.GenerateDailyReport(context.Background()); err != nil {
		t.Fatalf("GenerateDailyReport() error = %v", err)
	}
	if capturedContentType != "text/markdown" {
		t.Errorf("Content-Type = %v, want text/markdown", capturedContentType)
	}
}

func TestReporter_ZeroJobs(t *testing.T) {
	var body string
	reporter := cost.NewReporterWithClients(
		&mockCloudWatchClient{}, captureReport(&body), &mockSNSClient{}, &mockJobLister{}, &mockPriceFetcher{}, nil, &config.Config{}, "", "test-bucket",
	)
	if err := reporter.GenerateDailyReport(context.Background()); err != nil {
		t.Fatalf("GenerateDailyReport() error = %v", err)
	}
	if strings.Contains(body, "Cost per job:") {
		t.Error("Report should not contain 'Cost per job' when there are zero jobs")
	}
	if !strings.Contains(body, "Jobs completed: 0") {
		t.Errorf("expected zero jobs completed\n---\n%s", body)
	}
}

func TestReporter_ReportContainsDisclaimer(t *testing.T) {
	var body string
	reporter := cost.NewReporterWithClients(
		&mockCloudWatchClient{}, captureReport(&body), &mockSNSClient{}, &mockJobLister{}, &mockPriceFetcher{}, nil, &config.Config{}, "", "test-bucket",
	)
	if err := reporter.GenerateDailyReport(context.Background()); err != nil {
		t.Fatalf("GenerateDailyReport() error = %v", err)
	}
	if !strings.Contains(body, "DISCLAIMER") || !strings.Contains(body, "estimates") {
		t.Error("Report should contain disclaimer about estimated costs")
	}
}

func fmtDollar(v float64) string {
	return fmt.Sprintf("%.2f", v)
}
