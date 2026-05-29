package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/config"
)

// A connection whose peer accepts the request but never sends response headers
// must be abandoned at newAWSHTTPClient's ResponseHeaderTimeout, not left to
// block until the caller's context expires.
func TestNewAWSHTTPClientAbortsOnStalledHeaders(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	defer close(release)

	client := newAWSHTTPClient(20 * time.Millisecond)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	start := time.Now()
	resp, err := client.Do(req)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("expected a timeout error, got a response")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("request blocked for %v; ResponseHeaderTimeout not applied", elapsed)
	}

	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Fatalf("expected a timeout net.Error, got %v", err)
	}
}

func TestAWSResponseHeaderTimeoutConstantIsValid(t *testing.T) {
	t.Parallel()

	if config.AWSResponseHeaderTimeout <= 0 {
		t.Fatalf("AWSResponseHeaderTimeout must be positive, got %v", config.AWSResponseHeaderTimeout)
	}
	if config.AWSResponseHeaderTimeout >= config.MessageProcessTimeout {
		t.Fatalf("AWSResponseHeaderTimeout (%v) must stay below MessageProcessTimeout (%v) so retries fit the job budget",
			config.AWSResponseHeaderTimeout, config.MessageProcessTimeout)
	}
	if newAWSHTTPClient(config.AWSResponseHeaderTimeout) == nil {
		t.Fatal("newAWSHTTPClient returned nil")
	}
}
