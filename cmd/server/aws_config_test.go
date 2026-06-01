package main

import (
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws/retry"

	"github.com/Shavakan/runs-fleet/pkg/config"
)

func TestAWSHTTPClientPoolSizing(t *testing.T) {
	tr := newAWSHTTPClient(config.AWSResponseHeaderTimeout).GetTransport()
	if got := tr.MaxIdleConnsPerHost; got != awsMaxIdleConnsPerHost {
		t.Errorf("MaxIdleConnsPerHost = %d, want %d", got, awsMaxIdleConnsPerHost)
	}
	if got := tr.MaxIdleConns; got != awsMaxIdleConns {
		t.Errorf("MaxIdleConns = %d, want %d", got, awsMaxIdleConns)
	}
	// The SDK default is 10/host; a high-fan-out orchestrator must exceed it to
	// reuse warm connections instead of churning TCP+TLS under burst load.
	if tr.MaxIdleConnsPerHost <= 10 {
		t.Errorf("MaxIdleConnsPerHost = %d, want > 10 (SDK default)", tr.MaxIdleConnsPerHost)
	}
	// Overriding idle-pool limits must not disable HTTP/2 negotiation.
	if !tr.ForceAttemptHTTP2 {
		t.Error("ForceAttemptHTTP2 = false, want true (only idle-conn limits should be overridden)")
	}
}

func TestAWSRetryerIsAdaptive(t *testing.T) {
	r := awsRetryer()
	if _, ok := r.(*retry.AdaptiveMode); !ok {
		t.Fatalf("awsRetryer() = %T, want *retry.AdaptiveMode (client-side rate limiting to avoid retry-storm amplification)", r)
	}
	if got := r.MaxAttempts(); got < 3 {
		t.Errorf("MaxAttempts() = %d, want >= 3", got)
	}
}
