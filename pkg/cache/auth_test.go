package cache

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

const testSecret = "test-secret"

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
			name:       "returns empty for empty jobID",
			secret:     "my-secret-key",
			jobID:      "",
			instanceID: "i-abc123",
			wantEmpty:  true,
		},
		{
			name:       "returns empty for empty instanceID",
			secret:     "my-secret-key",
			jobID:      "job-123",
			instanceID: "",
			wantEmpty:  true,
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
	secret := testSecret
	jobID := "job-123"
	instanceID := "i-abc123"

	token1 := GenerateCacheToken(secret, jobID, instanceID)
	token2 := GenerateCacheToken(secret, jobID, instanceID)

	if token1 != token2 {
		t.Errorf("tokens should be deterministic: %q != %q", token1, token2)
	}
}

func TestGenerateCacheToken_DifferentInputs(t *testing.T) {
	secret := testSecret

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

func TestGenerateCacheToken_Format(t *testing.T) {
	token := GenerateCacheToken("secret", "job-123", "i-abc")

	// Token should be in format: base64.hmac
	if token == "" {
		t.Fatal("token should not be empty")
	}

	// Should contain exactly one dot
	dotCount := 0
	for _, c := range token {
		if c == '.' {
			dotCount++
		}
	}
	if dotCount != 1 {
		t.Errorf("token should contain exactly one dot, got %d", dotCount)
	}
}

func TestParseCacheToken(t *testing.T) {
	tests := []struct {
		name           string
		token          string
		wantJobID      string
		wantInstanceID string
		wantOK         bool
	}{
		{
			name:           "valid token",
			token:          GenerateCacheToken("secret", "job-123", "i-abc"),
			wantJobID:      "job-123",
			wantInstanceID: "i-abc",
			wantOK:         true,
		},
		{
			name:   "missing dot",
			token:  "nodot",
			wantOK: false,
		},
		{
			name:   "invalid base64",
			token:  "!!!invalid!!!.signature",
			wantOK: false,
		},
		{
			name:   "missing colon in payload",
			token:  "bm9jb2xvbg.signature", // "nocolon" in base64
			wantOK: false,
		},
		{
			name:   "empty token",
			token:  "",
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			jobID, instanceID, ok := ParseCacheToken(tt.token)
			if ok != tt.wantOK {
				t.Errorf("ParseCacheToken() ok = %v, want %v", ok, tt.wantOK)
			}
			if tt.wantOK {
				if jobID != tt.wantJobID {
					t.Errorf("jobID = %q, want %q", jobID, tt.wantJobID)
				}
				if instanceID != tt.wantInstanceID {
					t.Errorf("instanceID = %q, want %q", instanceID, tt.wantInstanceID)
				}
			}
		})
	}
}

func TestValidateCacheToken(t *testing.T) {
	secret := "my-secret-key"
	jobID := "job-123"
	instanceID := "i-abc123"
	validToken := GenerateCacheToken(secret, jobID, instanceID)

	tests := []struct {
		name   string
		secret string
		token  string
		want   bool
	}{
		{
			name:   "valid token",
			secret: secret,
			token:  validToken,
			want:   true,
		},
		{
			name:   "invalid signature",
			secret: secret,
			token:  "am9iLTEyMzppLWFiYzEyMw.invalidsignature",
			want:   false,
		},
		{
			name:   "wrong secret",
			secret: "different-secret",
			token:  validToken,
			want:   false,
		},
		{
			name:   "empty secret",
			secret: "",
			token:  validToken,
			want:   false,
		},
		{
			name:   "empty token",
			secret: secret,
			token:  "",
			want:   false,
		},
		{
			name:   "malformed token - no dot",
			secret: secret,
			token:  "nodot",
			want:   false,
		},
		{
			name:   "malformed token - invalid base64",
			secret: secret,
			token:  "!!!.signature",
			want:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ValidateCacheToken(tt.secret, tt.token)
			if got != tt.want {
				t.Errorf("ValidateCacheToken() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAuthMiddleware_Disabled(t *testing.T) {
	// Empty secret disables auth
	middleware := NewAuthMiddleware("")

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
	middleware := NewAuthMiddleware(testSecret)

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
	middleware := NewAuthMiddleware(testSecret)

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
	secret := testSecret
	middleware := NewAuthMiddleware(secret)

	// Generate a valid token
	validToken := GenerateCacheToken(secret, "job-123", "i-abc")

	called := false
	handler := middleware.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set(CacheTokenHeader, validToken)
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
	secret := testSecret
	middleware := NewAuthMiddleware(secret)

	// Generate a valid token
	validToken := GenerateCacheToken(secret, "job-123", "i-abc")

	called := false
	handler := middleware.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Bearer "+validToken)
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
	secret := testSecret
	middleware := NewAuthMiddleware(secret)

	// Generate a valid token
	validToken := GenerateCacheToken(secret, "job-123", "i-abc")

	called := false
	handler := middleware.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	// X-Cache-Token should take precedence over Authorization header
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set(CacheTokenHeader, validToken)
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

func TestAuthMiddleware_WrongSecret(t *testing.T) {
	// Server has one secret
	middleware := NewAuthMiddleware("server-secret")

	// Token generated with different secret
	wrongToken := GenerateCacheToken("different-secret", "job-123", "i-abc")

	called := false
	handler := middleware.Wrap(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		called = true
	}))

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set(CacheTokenHeader, wrongToken)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if called {
		t.Error("handler should not be called with token from wrong secret")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected status 401, got %d", rec.Code)
	}
}
