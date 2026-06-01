package awsobs

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/smithy-go/middleware"
)

// blockingHTTPClient is an aws.HTTPClient that waits on the request context and
// returns its error once the context is done. It records whether each request
// carried a context deadline, letting the real-stack tests observe whether the
// per-operation timeout reached the HTTP layer. No network is involved.
type blockingHTTPClient struct {
	sawDeadline atomic.Bool
}

func (b *blockingHTTPClient) Do(req *http.Request) (*http.Response, error) {
	if _, ok := req.Context().Deadline(); ok {
		b.sawDeadline.Store(true)
	}
	<-req.Context().Done()
	return nil, req.Context().Err()
}

// realStackConfig builds an aws.Config wired exactly as cmd/server/main.go wires
// it: the observability middleware followed by the per-operation timeout
// middleware (with the given exemptions). It uses the supplied HTTP client so no
// network is involved, disables retries so a bounded operation surfaces its
// deadline error directly, and captures emitted AWS-call metrics via the
// returned fakePublisher.
func realStackConfig(
	httpClient aws.HTTPClient, perOp time.Duration, exempt map[string]bool,
) (aws.Config, *fakePublisher) {
	fake := &fakePublisher{}
	rec := NewRecorder()
	rec.SetPublisher(fake)

	apiOptions := append(
		[]func(*middleware.Stack) error{register(rec)},
		PerOperationTimeout(perOp, exempt),
	)

	cfg := aws.Config{
		Region:           "ap-northeast-1",
		Credentials:      credentials.NewStaticCredentialsProvider("AKID", "SECRET", ""),
		HTTPClient:       httpClient,
		APIOptions:       apiOptions,
		RetryMaxAttempts: 1,
	}
	return cfg, fake
}

// delayHTTPClient is an aws.HTTPClient that sleeps for delay before returning a
// canned awsjson1.0 response, so the call completes successfully through the
// real stack. "{}" deserializes to an empty result for both GetItem and
// ReceiveMessage.
type delayHTTPClient struct {
	delay time.Duration
}

func (d *delayHTTPClient) Do(req *http.Request) (*http.Response, error) {
	select {
	case <-time.After(d.delay):
	case <-req.Context().Done():
		return nil, req.Context().Err()
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/x-amz-json-1.0"}},
		Body:       io.NopCloser(strings.NewReader("{}")),
		Request:    req,
	}, nil
}

// TestRealStack_ObservabilityCapturesServiceAndOperation builds real DynamoDB and
// SQS clients through the actual SDK middleware stack and asserts that the
// emitted duration metric carries a non-empty service and operation. It fails
// against a head-of-Initialize registration (where RegisterServiceMetadata has
// not yet populated the context) and passes once the middleware anchors after
// RegisterServiceMetadata.
func TestRealStack_ObservabilityCapturesServiceAndOperation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		invoke        func(ctx context.Context, cfg aws.Config) error
		wantService   string
		wantOperation string
	}{
		{
			name: "DynamoDB GetItem",
			invoke: func(ctx context.Context, cfg aws.Config) error {
				_, err := dynamodb.NewFromConfig(cfg).GetItem(ctx, &dynamodb.GetItemInput{
					TableName: aws.String("t"),
					Key:       map[string]types.AttributeValue{"id": &types.AttributeValueMemberS{Value: "x"}},
				})
				return err
			},
			wantService:   "DynamoDB",
			wantOperation: "GetItem",
		},
		{
			name: "SQS ReceiveMessage",
			invoke: func(ctx context.Context, cfg aws.Config) error {
				_, err := sqs.NewFromConfig(cfg).ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
					QueueUrl: aws.String("https://sqs.example/q"),
				})
				return err
			},
			wantService:   "SQS",
			wantOperation: "ReceiveMessage",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// A successful call must still emit one duration metric carrying the
			// service and operation; no per-op timeout is applied here.
			cfg, fake := realStackConfig(&delayHTTPClient{delay: time.Millisecond}, 0, nil)

			if err := tt.invoke(context.Background(), cfg); err != nil {
				t.Fatalf("invoke %s: %v", tt.name, err)
			}

			durations := fake.snapshotDurations()
			if len(durations) != 1 {
				t.Fatalf("expected exactly one duration metric, got %d: %v", len(durations), durations)
			}
			d := durations[0]
			if d.service != tt.wantService {
				t.Errorf("service = %q, want %q (empty means the middleware ran before RegisterServiceMetadata)", d.service, tt.wantService)
			}
			if d.operation != tt.wantOperation {
				t.Errorf("operation = %q, want %q (empty means the middleware ran before RegisterServiceMetadata)", d.operation, tt.wantOperation)
			}
		})
	}
}

// TestRealStack_ReceiveMessageExemptFromPerOpTimeout proves, through the real SDK
// stack, that the ReceiveMessage exemption matches: the per-operation timeout is
// not applied to ReceiveMessage (its long-poll is preserved) while a non-exempt
// operation on the same config is bounded and cut at the per-op deadline. The
// outer context carries no deadline, so any deadline reaching the HTTP layer can
// only come from the per-op timeout. Against a head-of-Initialize registration
// the exemption never matches (the operation name is empty), so ReceiveMessage is
// wrongly bounded and this test fails.
func TestRealStack_ReceiveMessageExemptFromPerOpTimeout(t *testing.T) {
	t.Parallel()

	const perOp = 30 * time.Millisecond
	exempt := map[string]bool{"ReceiveMessage": true}

	t.Run("ReceiveMessage is not bounded", func(t *testing.T) {
		t.Parallel()

		httpClient := &blockingHTTPClient{}
		cfg, _ := realStackConfig(httpClient, perOp, exempt)

		// No outer deadline: the only deadline that could reach the stub is the
		// per-op timeout. The exemption must keep it off, so the call blocks until
		// we cancel it.
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			_, err := sqs.NewFromConfig(cfg).ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
				QueueUrl: aws.String("https://sqs.example/q"),
			})
			done <- err
		}()

		select {
		case err := <-done:
			cancel()
			t.Fatalf("ReceiveMessage returned after %v (before cancel) with err=%v: the per-op timeout was applied despite the exemption", perOp, err)
		case <-time.After(3 * perOp):
		}

		if httpClient.sawDeadline.Load() {
			t.Error("ReceiveMessage request carried a deadline: the per-op timeout was applied despite the exemption")
		}

		cancel()
		<-done
	})

	t.Run("non-exempt operation is bounded", func(t *testing.T) {
		t.Parallel()

		httpClient := &blockingHTTPClient{}
		cfg, _ := realStackConfig(httpClient, perOp, exempt)

		// No outer deadline: the only thing that can cut the call is the per-op
		// timeout.
		start := time.Now()
		_, err := dynamodb.NewFromConfig(cfg).GetItem(context.Background(), &dynamodb.GetItemInput{
			TableName: aws.String("t"),
			Key:       map[string]types.AttributeValue{"id": &types.AttributeValueMemberS{Value: "x"}},
		})
		elapsed := time.Since(start)

		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("GetItem error = %v, want context.DeadlineExceeded from the per-op timeout", err)
		}
		if !httpClient.sawDeadline.Load() {
			t.Error("non-exempt GetItem request carried no deadline: the per-op timeout was not applied")
		}
		if elapsed >= 5*perOp {
			t.Errorf("GetItem ended after %v, want it cut near the per-op timeout %v", elapsed, perOp)
		}
	})
}
