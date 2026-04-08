package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

const testRemoteAddr = "10.0.0.1:5000"

func TestRateLimiter_UnderLimit(t *testing.T) {
	t.Parallel()

	rl := NewRateLimiter(10)
	defer rl.Stop()

	handler := rl.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	for i := range 10 {
		req := httptest.NewRequest("GET", "/api/pools", nil)
		req.RemoteAddr = "192.168.1.1:1234"
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: expected 200, got %d", i+1, rec.Code)
		}
	}
}

func TestRateLimiter_OverLimit(t *testing.T) {
	t.Parallel()

	rl := NewRateLimiter(5)
	defer rl.Stop()

	handler := rl.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	for range 5 {
		req := httptest.NewRequest("GET", "/api/pools", nil)
		req.RemoteAddr = testRemoteAddr
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200 within limit, got %d", rec.Code)
		}
	}

	req := httptest.NewRequest("GET", "/api/pools", nil)
	req.RemoteAddr = testRemoteAddr
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", rec.Code)
	}

	var errResp ErrorResponse
	if err := json.NewDecoder(rec.Body).Decode(&errResp); err != nil {
		t.Fatalf("failed to decode error response: %v", err)
	}
	if errResp.Error != "Rate limit exceeded" {
		t.Errorf("expected 'Rate limit exceeded', got %q", errResp.Error)
	}
}

func TestRateLimiter_SeparateIPLimits(t *testing.T) {
	t.Parallel()

	rl := NewRateLimiter(2)
	defer rl.Stop()

	handler := rl.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	for range 2 {
		req := httptest.NewRequest("GET", "/api/pools", nil)
		req.RemoteAddr = testRemoteAddr
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatal("ip1 should be within limit")
		}
	}

	req := httptest.NewRequest("GET", "/api/pools", nil)
	req.RemoteAddr = testRemoteAddr
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatal("ip1 should be rate limited")
	}

	req = httptest.NewRequest("GET", "/api/pools", nil)
	req.RemoteAddr = "10.0.0.2:5000"
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatal("ip2 should still be within its own limit")
	}
}

func TestRateLimiter_XForwardedFor(t *testing.T) {
	t.Parallel()

	rl := NewRateLimiter(1)
	defer rl.Stop()

	handler := rl.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/api/pools", nil)
	req.RemoteAddr = testRemoteAddr
	req.Header.Set("X-Forwarded-For", "203.0.113.50, 70.41.3.18")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatal("first request from XFF IP should pass")
	}

	req = httptest.NewRequest("GET", "/api/pools", nil)
	req.RemoteAddr = testRemoteAddr
	req.Header.Set("X-Forwarded-For", "203.0.113.50, 70.41.3.18")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatal("second request from same XFF IP should be limited")
	}

	req = httptest.NewRequest("GET", "/api/pools", nil)
	req.RemoteAddr = testRemoteAddr
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatal("request from RemoteAddr (no XFF) should use different bucket")
	}
}

func TestRateLimiter_RetryAfterHeader(t *testing.T) {
	t.Parallel()

	rl := NewRateLimiter(1)
	defer rl.Stop()

	handler := rl.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/api/pools", nil)
	req.RemoteAddr = testRemoteAddr
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	req = httptest.NewRequest("GET", "/api/pools", nil)
	req.RemoteAddr = testRemoteAddr
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", rec.Code)
	}

	retryAfter := rec.Header().Get("Retry-After")
	if retryAfter == "" {
		t.Fatal("expected Retry-After header to be set")
	}
}

func TestExtractClientIP_RemoteAddrOnly(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "192.168.1.1:12345"
	ip := extractClientIP(req)
	if ip != "192.168.1.1" {
		t.Errorf("expected 192.168.1.1, got %q", ip)
	}
}

func TestExtractClientIP_XForwardedFor(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = testRemoteAddr
	req.Header.Set("X-Forwarded-For", "203.0.113.50")
	ip := extractClientIP(req)
	if ip != "203.0.113.50" {
		t.Errorf("expected 203.0.113.50, got %q", ip)
	}
}

func TestExtractClientIP_XForwardedForChain(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = testRemoteAddr
	req.Header.Set("X-Forwarded-For", "203.0.113.50, 70.41.3.18, 150.172.238.178")
	ip := extractClientIP(req)
	if ip != "203.0.113.50" {
		t.Errorf("expected first IP 203.0.113.50, got %q", ip)
	}
}

func TestRateLimiter_ZeroLimitDefaultsTo60(t *testing.T) {
	t.Parallel()

	rl := NewRateLimiter(0)
	defer rl.Stop()

	if rl.limit != 60 {
		t.Errorf("expected default limit 60, got %d", rl.limit)
	}
}
