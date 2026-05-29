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

func TestAWSSQSResponseHeaderTimeoutConstantIsValid(t *testing.T) {
	t.Parallel()

	// SQS long polls with WaitTimeSeconds=20; an empty queue withholds response
	// headers for that whole wait, so the SQS client's header timeout must clear
	// it while staying inside the per-message processing budget.
	const longPollWait = 20 * time.Second

	if config.AWSSQSResponseHeaderTimeout <= longPollWait {
		t.Fatalf("AWSSQSResponseHeaderTimeout (%v) must exceed the 20s long-poll wait",
			config.AWSSQSResponseHeaderTimeout)
	}
	if config.AWSSQSResponseHeaderTimeout <= config.AWSResponseHeaderTimeout {
		t.Fatalf("AWSSQSResponseHeaderTimeout (%v) must exceed the fast AWSResponseHeaderTimeout (%v)",
			config.AWSSQSResponseHeaderTimeout, config.AWSResponseHeaderTimeout)
	}
	if config.AWSSQSResponseHeaderTimeout >= config.MessageProcessTimeout {
		t.Fatalf("AWSSQSResponseHeaderTimeout (%v) must stay below MessageProcessTimeout (%v)",
			config.AWSSQSResponseHeaderTimeout, config.MessageProcessTimeout)
	}
	if newAWSHTTPClient(config.AWSSQSResponseHeaderTimeout) == nil {
		t.Fatal("newAWSHTTPClient returned nil")
	}
}

// The fast and SQS clients must behave differently against a peer that delays
// response headers: a header timeout shorter than the delay aborts the request,
// while a longer one waits through it. This is the property that lets the SQS
// client tolerate long-poll waits the fast client would abandon.
func TestNewAWSHTTPClientTimeoutBracketsHeaderDelay(t *testing.T) {
	t.Parallel()

	const (
		headerDelay  = 60 * time.Millisecond
		shortTimeout = 20 * time.Millisecond
		longTimeout  = 200 * time.Millisecond
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(headerDelay)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	doRequest := func(timeout time.Duration) error {
		client := newAWSHTTPClient(timeout)
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		return resp.Body.Close()
	}

	// timeout < delay: the client gives up before headers arrive.
	err := doRequest(shortTimeout)
	if err == nil {
		t.Fatal("client with timeout below the header delay should have failed")
	}
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Fatalf("expected a timeout net.Error from the short-timeout client, got %v", err)
	}

	// timeout > delay: the client waits through the delay and succeeds.
	if err := doRequest(longTimeout); err != nil {
		t.Fatalf("client with timeout above the header delay should have succeeded, got %v", err)
	}
}
