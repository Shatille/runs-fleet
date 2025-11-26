package cache

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGenerateCacheToken(t *testing.T) {
	tests := []struct {
		name       string
		secret     string
		jobID      string
		instanceID string
		wantEmpty  bool
	}{
		{
			name:       "generates token with valid inputs",
			secret:     "my-secret-key",
			jobID:      "job-123",
			instanceID: "i-abc123",
			wantEmpty:  false,
		},
		{
			name:       "returns empty for empty secret",
			secret:     "",
			jobID:      "job-123",
			instanceID: "i-abc123",
			wantEmpty:  true,
		},
		{
			name:       "generates different tokens for different jobs",
			secret:     "my-secret-key",
			jobID:      "job-456",
			instanceID: "i-abc123",
			wantEmpty:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			token := GenerateCacheToken(tt.secret, tt.jobID, tt.instanceID)
			if tt.wantEmpty && token != "" {
				t.Errorf("expected empty token, got %q", token)
			}
			if !tt.wantEmpty && token == "" {
				t.Error("expected non-empty token, got empty")
			}
		})
	}
}

func TestGenerateCacheToken_Deterministic(t *testing.T) {
	secret := "test-secret"
	jobID := "job-123"
	instanceID := "i-abc123"

	token1 := GenerateCacheToken(secret, jobID, instanceID)
	token2 := GenerateCacheToken(secret, jobID, instanceID)

	if token1 != token2 {
		t.Errorf("tokens should be deterministic: %q != %q", token1, token2)
	}
}

func TestGenerateCacheToken_DifferentInputs(t *testing.T) {
	secret := "test-secret"

	token1 := GenerateCacheToken(secret, "job-1", "i-abc")
	token2 := GenerateCacheToken(secret, "job-2", "i-abc")
	token3 := GenerateCacheToken(secret, "job-1", "i-xyz")

	if token1 == token2 {
		t.Error("different job IDs should produce different tokens")
	}
	if token1 == token3 {
		t.Error("different instance IDs should produce different tokens")
	}
}

func TestValidateCacheToken(t *testing.T) {
	secret := "my-secret-key"
	jobID := "job-123"
	instanceID := "i-abc123"
	validToken := GenerateCacheToken(secret, jobID, instanceID)

	tests := []struct {
		name       string
		secret     string
		token      string
		jobID      string
		instanceID string
		want       bool
	}{
		{
			name:       "valid token",
			secret:     secret,
			token:      validToken,
			jobID:      jobID,
			instanceID: instanceID,
			want:       true,
		},
		{
			name:       "invalid token",
			secret:     secret,
			token:      "invalid-token",
			jobID:      jobID,
			instanceID: instanceID,
			want:       false,
		},
		{
			name:       "empty secret",
			secret:     "",
			token:      validToken,
			jobID:      jobID,
			instanceID: instanceID,
			want:       false,
		},
		{
			name:       "empty token",
			secret:     secret,
			token:      "",
			jobID:      jobID,
			instanceID: instanceID,
			want:       false,
		},
		{
			name:       "wrong job ID",
			secret:     secret,
			token:      validToken,
			jobID:      "different-job",
			instanceID: instanceID,
			want:       false,
		},
		{
			name:       "wrong instance ID",
			secret:     secret,
			token:      validToken,
			jobID:      jobID,
			instanceID: "different-instance",
			want:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ValidateCacheToken(tt.secret, tt.token, tt.jobID, tt.instanceID)
			if got != tt.want {
				t.Errorf("ValidateCacheToken() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestHMACTokenStore(t *testing.T) {
	store := NewHMACTokenStore("test-secret")

	// Initially no tokens registered
	if _, valid := store.ValidateToken("any-token"); valid {
		t.Error("expected invalid for unregistered token")
	}

	// Register a token
	store.RegisterToken("token-123", "job-456")
	if jobID, valid := store.ValidateToken("token-123"); !valid || jobID != "job-456" {
		t.Errorf("expected valid token with jobID job-456, got valid=%v, jobID=%s", valid, jobID)
	}

	// Unregister the token
	store.UnregisterToken("token-123")
	if _, valid := store.ValidateToken("token-123"); valid {
		t.Error("expected invalid after unregistering")
	}
}

func TestHMACTokenStore_EmptyValues(t *testing.T) {
	store := NewHMACTokenStore("test-secret")

	// Empty token should not be registered
	store.RegisterToken("", "job-123")
	if _, valid := store.ValidateToken(""); valid {
		t.Error("empty token should not be valid")
	}

	// Empty job ID should not be registered
	store.RegisterToken("token-123", "")
	if _, valid := store.ValidateToken("token-123"); valid {
		t.Error("token with empty job ID should not be registered")
	}
}

func TestAuthMiddleware_Disabled(t *testing.T) {
	// Empty secret disables auth
	middleware := NewAuthMiddleware("", nil)

	if middleware.IsEnabled() {
		t.Error("middleware should be disabled with empty secret")
	}

	// Should pass through without auth
	called := false
	handler := middleware.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !called {
		t.Error("handler should be called when auth is disabled")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}
}

func TestAuthMiddleware_Enabled_NoToken(t *testing.T) {
	store := NewHMACTokenStore("test-secret")
	middleware := NewAuthMiddleware("test-secret", store)

	if !middleware.IsEnabled() {
		t.Error("middleware should be enabled with secret")
	}

	called := false
	handler := middleware.Wrap(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		called = true
	}))

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if called {
		t.Error("handler should not be called without token")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected status 401, got %d", rec.Code)
	}
}

func TestAuthMiddleware_Enabled_InvalidToken(t *testing.T) {
	store := NewHMACTokenStore("test-secret")
	middleware := NewAuthMiddleware("test-secret", store)

	called := false
	handler := middleware.Wrap(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		called = true
	}))

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set(CacheTokenHeader, "invalid-token")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if called {
		t.Error("handler should not be called with invalid token")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected status 401, got %d", rec.Code)
	}
}

func TestAuthMiddleware_Enabled_ValidToken(t *testing.T) {
	store := NewHMACTokenStore("test-secret")
	store.RegisterToken("valid-token", "job-123")
	middleware := NewAuthMiddleware("test-secret", store)

	called := false
	handler := middleware.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set(CacheTokenHeader, "valid-token")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !called {
		t.Error("handler should be called with valid token")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}
}

func TestAuthMiddleware_BearerToken(t *testing.T) {
	store := NewHMACTokenStore("test-secret")
	store.RegisterToken("bearer-token", "job-123")
	middleware := NewAuthMiddleware("test-secret", store)

	called := false
	handler := middleware.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Bearer bearer-token")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !called {
		t.Error("handler should be called with valid Bearer token")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}
}

func TestAuthMiddleware_XCacheTokenPrecedence(t *testing.T) {
	store := NewHMACTokenStore("test-secret")
	store.RegisterToken("x-cache-token", "job-123")
	middleware := NewAuthMiddleware("test-secret", store)

	called := false
	handler := middleware.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	// X-Cache-Token should take precedence over Authorization header
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set(CacheTokenHeader, "x-cache-token")
	req.Header.Set("Authorization", "Bearer invalid-bearer")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !called {
		t.Error("handler should be called - X-Cache-Token should take precedence")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}
}
