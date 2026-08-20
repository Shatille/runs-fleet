package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

const testPathRerunJob = "/repos/myorg/myrepo/actions/jobs/4242/rerun"

func rerunTestServer(t *testing.T, handle func(w http.ResponseWriter, r *http.Request)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case testPathOrgInstallation:
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"id":      123,
				"account": map[string]interface{}{"type": "Organization"},
			})
		case testPathAccessTokens123:
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "ghs_test_token"})
		default:
			handle(w, r)
		}
	}))
}

func rerunTestClient(t *testing.T, serverURL string) *Client {
	t.Helper()
	client, err := NewClient("12345", generateTestKey(t))
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}
	client.baseURL = serverURL
	return client
}

// The per-job endpoint is deliberate: rerun-failed-jobs would also re-run the
// jobs that failed for real, re-running genuine failures on someone else's
// behalf. GitHub still pulls dependent jobs along, which is what recovers the
// gate jobs a reclaim cascades into.
func TestRerunJobCallsThePerJobEndpoint(t *testing.T) {
	var gotPath, gotMethod, gotAuth string
	server := rerunTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod, gotAuth = r.URL.Path, r.Method, r.Header.Get("Authorization")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("{}"))
	})
	defer server.Close()

	if err := rerunTestClient(t, server.URL).RerunJob(context.Background(), "myorg/myrepo", 4242); err != nil {
		t.Fatalf("RerunJob() error = %v", err)
	}
	if gotPath != testPathRerunJob {
		t.Errorf("path = %q, want %q", gotPath, testPathRerunJob)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if !strings.Contains(gotAuth, "ghs_test_token") {
		t.Errorf("request did not carry the installation token, got %q", gotAuth)
	}
}

func TestRerunJobRejectsAMalformedRepo(t *testing.T) {
	server := rerunTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("no request should be made for a malformed repo")
		w.WriteHeader(http.StatusCreated)
	})
	defer server.Close()

	if err := rerunTestClient(t, server.URL).RerunJob(context.Background(), "not-a-repo", 4242); err == nil {
		t.Fatal("RerunJob() error = nil, want an error for a repo without an owner")
	}
}

// A job GitHub refuses to re-run (already re-running, or too old) must surface
// as an error the caller can log, never as a silent success.
func TestRerunJobSurfacesRefusal(t *testing.T) {
	server := rerunTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"Job is already running"}`))
	})
	defer server.Close()

	err := rerunTestClient(t, server.URL).RerunJob(context.Background(), "myorg/myrepo", 4242)
	if err == nil {
		t.Fatal("RerunJob() error = nil, want an error on 403")
	}
	if strings.Contains(err.Error(), "ghs_test_token") {
		t.Errorf("error leaked the installation token: %v", err)
	}
}

// 5xx is transient; the client retries it like its other calls rather than
// dropping a recovery on one bad response.
func TestRerunJobRetriesTransientServerErrors(t *testing.T) {
	var calls atomic.Int32
	server := rerunTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("{}"))
	})
	defer server.Close()

	if err := rerunTestClient(t, server.URL).RerunJob(context.Background(), "myorg/myrepo", 4242); err != nil {
		t.Fatalf("RerunJob() error = %v, want the retry to succeed", err)
	}
	if got := calls.Load(); got < 2 {
		t.Errorf("made %d attempts, want a retry after the 500", got)
	}
}
