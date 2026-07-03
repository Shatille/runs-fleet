package admin

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

const testAuthSecret = "test-auth-secret"

func TestAuthMiddleware_Disabled(t *testing.T) {
	t.Parallel()

	m := NewAuthMiddleware("")
	if m.IsEnabled() {
		t.Error("expected middleware to be disabled with empty secret")
	}

	called := false
	handler := m.WrapFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest("GET", "/api/pools", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !called {
		t.Error("handler should be called when auth is disabled")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}
}

func TestAuthMiddleware_MissingSessionCookie(t *testing.T) {
	t.Parallel()

	m := NewAuthMiddleware(testAuthSecret)

	called := false
	handler := m.WrapFunc(func(_ http.ResponseWriter, _ *http.Request) {
		called = true
	})

	req := httptest.NewRequest("GET", "/api/pools", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if called {
		t.Error("handler should not be called without a session cookie")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected status 401, got %d", rec.Code)
	}
}

func TestAuthMiddleware_InvalidSessionCookie(t *testing.T) {
	t.Parallel()

	m := NewAuthMiddleware(testAuthSecret)

	called := false
	handler := m.WrapFunc(func(_ http.ResponseWriter, _ *http.Request) {
		called = true
	})

	req := httptest.NewRequest("GET", "/api/pools", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "not-a-valid-token"})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if called {
		t.Error("handler should not be called with an invalid session cookie")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected status 401, got %d", rec.Code)
	}
}

func TestAuthMiddleware_ValidSessionCookie(t *testing.T) {
	t.Parallel()

	m := NewAuthMiddleware(testAuthSecret)

	token, err := GenerateSessionCookie(testAuthSecret, SessionClaims{
		Username:  "test-user",
		Email:     "test@example.com",
		Groups:    []string{"admins", "developers", "ops"},
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatalf("setup: GenerateSessionCookie() error = %v", err)
	}

	var capturedUser UserInfo
	handler := m.WrapFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedUser = GetUser(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest("GET", "/api/pools", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}
	if capturedUser.Username != "test-user" {
		t.Errorf("expected username 'test-user', got %q", capturedUser.Username)
	}
	if capturedUser.Email != "test@example.com" {
		t.Errorf("expected email 'test@example.com', got %q", capturedUser.Email)
	}
	if len(capturedUser.Groups) != 3 {
		t.Errorf("expected 3 groups, got %d", len(capturedUser.Groups))
	}
}

func TestAuthMiddleware_ExpiredSessionCookie(t *testing.T) {
	t.Parallel()

	m := NewAuthMiddleware(testAuthSecret)

	token, err := GenerateSessionCookie(testAuthSecret, SessionClaims{
		Username:  "test-user",
		ExpiresAt: time.Now().Add(-time.Hour).Unix(),
	})
	if err != nil {
		t.Fatalf("setup: GenerateSessionCookie() error = %v", err)
	}

	called := false
	handler := m.WrapFunc(func(_ http.ResponseWriter, _ *http.Request) {
		called = true
	})

	req := httptest.NewRequest("GET", "/api/pools", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if called {
		t.Error("handler should not be called with an expired session")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected status 401, got %d", rec.Code)
	}
}

func TestAuthMiddleware_DisabledAllowsAllRequests(t *testing.T) {
	t.Parallel()

	m := NewAuthMiddleware("")

	var capturedUser UserInfo
	handler := m.WrapFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedUser = GetUser(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest("GET", "/api/pools", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}
	if capturedUser.Username != "" {
		t.Errorf("expected empty username when auth is disabled, got %q", capturedUser.Username)
	}
}

func TestGetUsername(t *testing.T) {
	t.Parallel()

	m := NewAuthMiddleware(testAuthSecret)

	token, err := GenerateSessionCookie(testAuthSecret, SessionClaims{
		Username:  "context-user",
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatalf("setup: GenerateSessionCookie() error = %v", err)
	}

	var username string
	handler := m.WrapFunc(func(w http.ResponseWriter, r *http.Request) {
		username = GetUsername(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest("GET", "/api/pools", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if username != "context-user" {
		t.Errorf("expected username 'context-user', got %q", username)
	}
}

func TestGetUsername_Unauthenticated(t *testing.T) {
	t.Parallel()

	if got := GetUsername(httptest.NewRequest("GET", "/", nil).Context()); got != "" {
		t.Errorf("expected empty username for unauthenticated context, got %q", got)
	}
}
