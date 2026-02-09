package admin

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAuthMiddleware_Disabled(t *testing.T) {
	m := NewAuthMiddleware("")
	if m.IsEnabled() {
		t.Error("expected middleware to be disabled with empty string")
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

func TestAuthMiddleware_MissingUserHeader(t *testing.T) {
	m := NewAuthMiddleware("require-auth")

	called := false
	handler := m.WrapFunc(func(_ http.ResponseWriter, _ *http.Request) {
		called = true
	})

	req := httptest.NewRequest("GET", "/api/pools", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if called {
		t.Error("handler should not be called without user header")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected status 401, got %d", rec.Code)
	}
}

func TestAuthMiddleware_ValidUserHeader(t *testing.T) {
	m := NewAuthMiddleware("require-auth")

	var capturedUser UserInfo
	handler := m.WrapFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedUser = GetUser(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest("GET", "/api/pools", nil)
	req.Header.Set(HeaderUser, "test-user")
	req.Header.Set(HeaderEmail, "test@example.com")
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
}

func TestAuthMiddleware_WithGroups(t *testing.T) {
	m := NewAuthMiddleware("require-auth")

	var capturedUser UserInfo
	handler := m.WrapFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedUser = GetUser(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest("GET", "/api/pools", nil)
	req.Header.Set(HeaderUser, "admin-user")
	req.Header.Set(HeaderGroups, "admins, developers, ops")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}
	if len(capturedUser.Groups) != 3 {
		t.Errorf("expected 3 groups, got %d", len(capturedUser.Groups))
	}
	expectedGroups := []string{"admins", "developers", "ops"}
	for i, g := range expectedGroups {
		if capturedUser.Groups[i] != g {
			t.Errorf("expected group %q at index %d, got %q", g, i, capturedUser.Groups[i])
		}
	}
}

func TestAuthMiddleware_DisabledAllowsAllRequests(t *testing.T) {
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
		t.Errorf("expected empty username when no header, got %q", capturedUser.Username)
	}
}

func TestAuthMiddleware_DisabledStillExtractsHeaders(t *testing.T) {
	m := NewAuthMiddleware("")

	var capturedUser UserInfo
	handler := m.WrapFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedUser = GetUser(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest("GET", "/api/pools", nil)
	req.Header.Set(HeaderUser, "optional-user")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}
	if capturedUser.Username != "optional-user" {
		t.Errorf("expected username 'optional-user', got %q", capturedUser.Username)
	}
}

func TestGetUsername(t *testing.T) {
	m := NewAuthMiddleware("")

	var username string
	handler := m.WrapFunc(func(w http.ResponseWriter, r *http.Request) {
		username = GetUsername(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest("GET", "/api/pools", nil)
	req.Header.Set(HeaderUser, "context-user")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if username != "context-user" {
		t.Errorf("expected username 'context-user', got %q", username)
	}
}
