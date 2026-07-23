package buildxshim

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestIMDSClient_FetchCredentials(t *testing.T) {
	const role = "runs-fleet-runner"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/latest/api/token":
			if r.Method != http.MethodPut {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte("test-token"))
		case "/latest/meta-data/iam/security-credentials/":
			if r.Header.Get("X-aws-ec2-metadata-token") != "test-token" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(role))
		case "/latest/meta-data/iam/security-credentials/" + role:
			if r.Header.Get("X-aws-ec2-metadata-token") != "test-token" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{
				"AccessKeyId": "AKIA_XYZ",
				"SecretAccessKey": "secretval",
				"Token": "sessiontok"
			}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := &IMDSClient{baseURL: srv.URL, httpClient: srv.Client(), timeout: 2 * time.Second}
	creds, err := c.FetchCredentials(context.Background())
	if err != nil {
		t.Fatalf("FetchCredentials error = %v", err)
	}
	if creds.AccessKeyID != "AKIA_XYZ" || creds.SecretAccessKey != "secretval" || creds.SessionToken != "sessiontok" {
		t.Errorf("unexpected creds: %+v", creds)
	}
}

func TestIMDSClient_TokenFailureReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	c := &IMDSClient{baseURL: srv.URL, httpClient: srv.Client(), timeout: 2 * time.Second}
	if _, err := c.FetchCredentials(context.Background()); err == nil {
		t.Error("expected error when IMDS token fetch fails")
	}
}

func TestIMDSClient_HonorsContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		_, _ = w.Write([]byte("token"))
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := &IMDSClient{baseURL: srv.URL, httpClient: srv.Client(), timeout: 2 * time.Second}
	if _, err := c.FetchCredentials(ctx); err == nil {
		t.Error("expected error when context already cancelled")
	}
}

// fakeCredsFetcher lets cmd/buildx-shim be tested without touching IMDS.
type fakeCredsFetcher struct {
	creds Credentials
	err   error
}

func (f fakeCredsFetcher) FetchCredentials(context.Context) (Credentials, error) {
	return f.creds, f.err
}

func TestCredsFetcherInterfaceSatisfiedByFake(_ *testing.T) {
	var _ CredsFetcher = fakeCredsFetcher{}
	var _ CredsFetcher = NewIMDSClient()
}
