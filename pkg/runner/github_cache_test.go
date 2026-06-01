package runner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

func TestGitHubClient_CachesInstallationToken(t *testing.T) {
	keyBase64 := generateTestKey(t)

	var mintCount, regCount int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case testPathOrgInstallation:
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"id":      123,
				"account": map[string]interface{}{"type": "Organization"},
			})
		case testPathAccessTokens123:
			atomic.AddInt32(&mintCount, 1)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"token":      "ghs_test_token",
				"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			})
		case testPathRegTokenMyOrg:
			atomic.AddInt32(&regCount, 1)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "AABB123"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := NewGitHubClient("12345", keyBase64)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}
	client.baseURL = server.URL

	for i := 0; i < 2; i++ {
		if _, err := client.GetRegistrationToken(context.Background(), "myorg/myrepo"); err != nil {
			t.Fatalf("prep %d: unexpected error: %v", i, err)
		}
	}

	if got := atomic.LoadInt32(&mintCount); got != 1 {
		t.Errorf("installation token minted %d times, want 1 (second prep should reuse the cached token)", got)
	}
	if got := atomic.LoadInt32(&regCount); got != 2 {
		t.Errorf("registration token requested %d times, want 2", got)
	}
}

func TestIsRetryableError_SecondaryRateLimit(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		headerKey string
		headerVal string
		want      bool
	}{
		{"403 with Retry-After is retryable", http.StatusForbidden, "Retry-After", "1", true},
		{"403 with X-RateLimit-Remaining 0 is retryable", http.StatusForbidden, "X-RateLimit-Remaining", "0", true},
		{"403 without rate-limit signal is not retryable", http.StatusForbidden, "", "", false},
		{"429 with Retry-After is retryable", http.StatusTooManyRequests, "Retry-After", "30", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := &http.Response{StatusCode: tt.status, Header: http.Header{}}
			if tt.headerKey != "" {
				resp.Header.Set(tt.headerKey, tt.headerVal)
			}
			if got := isRetryableError(resp, nil); got != tt.want {
				t.Errorf("isRetryableError() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGitHubClient_GetRegistrationToken_RetriesOnSecondaryRateLimit(t *testing.T) {
	keyBase64 := generateTestKey(t)
	oldDelay := baseRetryDelay
	baseRetryDelay = time.Millisecond
	defer func() { baseRetryDelay = oldDelay }()

	var regAttempts int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		case testPathRegTokenMyOrg:
			// Secondary rate limit (403 + Retry-After) on the first attempt, then success.
			if atomic.AddInt32(&regAttempts, 1) == 1 {
				w.Header().Set("Retry-After", "0")
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"message":"You have exceeded a secondary rate limit"}`))
				return
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "AABB123"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := NewGitHubClient("12345", keyBase64)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}
	client.baseURL = server.URL

	result, err := client.GetRegistrationToken(context.Background(), "myorg/myrepo")
	if err != nil {
		t.Fatalf("expected secondary rate limit to be retried, got error: %v", err)
	}
	if result.Token != "AABB123" {
		t.Errorf("token = %q, want AABB123", result.Token)
	}
	if got := atomic.LoadInt32(&regAttempts); got != 2 {
		t.Errorf("registration attempts = %d, want 2 (one 403 retry + success)", got)
	}
}

func TestRetryAfterDelay(t *testing.T) {
	h := http.Header{}
	h.Set("Retry-After", "5")
	d, ok := retryAfterDelay(&http.Response{StatusCode: http.StatusForbidden, Header: h})
	if !ok || d != 5*time.Second {
		t.Errorf("retryAfterDelay(Retry-After: 5) = %v, %v; want 5s, true", d, ok)
	}

	reset := strconv.FormatInt(time.Now().Add(7*time.Second).Unix(), 10)
	h2 := http.Header{}
	h2.Set("X-RateLimit-Remaining", "0")
	h2.Set("X-RateLimit-Reset", reset)
	d2, ok2 := retryAfterDelay(&http.Response{StatusCode: http.StatusForbidden, Header: h2})
	if !ok2 || d2 <= 0 || d2 > 8*time.Second {
		t.Errorf("retryAfterDelay(reset ~7s) = %v, %v; want ~7s, true", d2, ok2)
	}

	if _, ok := retryAfterDelay(&http.Response{StatusCode: http.StatusForbidden, Header: http.Header{}}); ok {
		t.Error("retryAfterDelay with no rate-limit headers should return ok=false")
	}
}
