package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

const testSessionSecret = "test-session-secret"

func newTestAuthHandler(t *testing.T, idp *fakeIDP) *AuthHandler {
	t.Helper()
	var oidcClient *OIDCClient
	if idp != nil {
		oidcClient = newTestOIDCClient(t, idp, "groups")
	}
	return NewAuthHandler(oidcClient, testSessionSecret, time.Hour)
}

func newAuthMux(t *testing.T, idp *fakeIDP) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	newTestAuthHandler(t, idp).RegisterRoutes(mux)
	return mux
}

func TestAuthHandler_Config(t *testing.T) {
	t.Run("auth disabled", func(t *testing.T) {
		mux := newAuthMux(t, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/auth/config", nil))

		var body map[string]any
		if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if body["auth_enabled"] != false {
			t.Errorf("auth_enabled = %v, want false", body["auth_enabled"])
		}
		if _, ok := body["login_url"]; ok {
			t.Error("login_url should be omitted when auth is disabled")
		}
	})

	t.Run("auth enabled", func(t *testing.T) {
		idp := newFakeIDP(t)
		mux := newAuthMux(t, idp)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/auth/config", nil))

		var body map[string]any
		if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if body["auth_enabled"] != true {
			t.Errorf("auth_enabled = %v, want true", body["auth_enabled"])
		}
		if body["login_url"] != "/api/auth/login" {
			t.Errorf("login_url = %v, want /api/auth/login", body["login_url"])
		}
	})
}

func TestAuthHandler_Login(t *testing.T) {
	idp := newFakeIDP(t)
	mux := newAuthMux(t, idp)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/auth/login?redirect=/admin/jobs", nil)
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusFound)
	}

	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	if loc.Query().Get("state") == "" {
		t.Error("redirect URL missing state param")
	}
	if loc.Query().Get("nonce") == "" {
		t.Error("redirect URL missing nonce param")
	}
	if loc.Query().Get("code_challenge") == "" {
		t.Error("redirect URL missing PKCE code_challenge param")
	}
	if loc.Query().Get("code_challenge_method") != "S256" {
		t.Errorf("code_challenge_method = %q, want S256", loc.Query().Get("code_challenge_method"))
	}

	found := false
	for _, c := range rec.Result().Cookies() {
		if c.Name == txnCookieName {
			found = true
			if !c.HttpOnly {
				t.Error("transaction cookie must be HttpOnly")
			}
			if !c.Secure {
				t.Error("transaction cookie must be Secure")
			}
			if c.SameSite != http.SameSiteLaxMode {
				t.Errorf("transaction cookie SameSite = %v, want Lax", c.SameSite)
			}
		}
	}
	if !found {
		t.Errorf("expected a %s cookie to be set", txnCookieName)
	}
}

func TestAuthHandler_Login_RedirectSanitization(t *testing.T) {
	tests := []struct {
		name     string
		redirect string
	}{
		{"absolute URL", "https://evil.com/steal"},
		{"protocol-relative", "//evil.com/steal"},
		{"backslash bypass", "/\\evil.com/steal"},
		{"missing leading slash", "admin/jobs"},
		{"empty", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			idp := newFakeIDP(t)
			mux := newAuthMux(t, idp)

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/api/auth/login?redirect="+url.QueryEscape(tt.redirect), nil)
			mux.ServeHTTP(rec, req)

			// Extract the signed transaction cookie and confirm the stored
			// redirect target was sanitized to the safe default, not the
			// attacker-controlled value.
			for _, c := range rec.Result().Cookies() {
				if c.Name != txnCookieName {
					continue
				}
				var txn oidcTransaction
				if err := verifyJSON(testSessionSecret, c.Value, &txn); err != nil {
					t.Fatalf("verify transaction cookie: %v", err)
				}
				if txn.Redirect != defaultLoginRedirect {
					t.Errorf("Redirect = %q, want sanitized default %q", txn.Redirect, defaultLoginRedirect)
				}
			}
		})
	}
}

func TestSanitizeRedirect(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"absolute URL", "https://evil.com/steal", defaultLoginRedirect},
		{"protocol-relative", "//evil.com/steal", defaultLoginRedirect},
		{"leading backslash", "/\\evil.com/steal", defaultLoginRedirect},
		{"missing leading slash", "admin/jobs", defaultLoginRedirect},
		{"empty", "", defaultLoginRedirect},
		{
			// A backslash that isn't the first character never changes the
			// resolved host (verified against net/url.ResolveReference and
			// WHATWG URL parsing): it stays a same-origin path segment, so
			// it's safe to pass through rather than reject.
			"non-leading backslash", "/admin/\\evil.com", "/admin/\\evil.com",
		},
		{
			// ".." segments collapse within the path component only; they
			// cannot cross the authority, so this resolves to a same-origin
			// path and isn't an open redirect.
			"dot-dot traversal", "/admin/../../evil.com", "/admin/../../evil.com",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sanitizeRedirect(tt.raw); got != tt.want {
				t.Errorf("sanitizeRedirect(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

func TestAuthHandler_Callback_FullRoundTrip(t *testing.T) {
	idp := newFakeIDP(t)
	mux := newAuthMux(t, idp)

	// Step 1: login.
	loginRec := httptest.NewRecorder()
	loginReq := httptest.NewRequest(http.MethodGet, "/api/auth/login?redirect=/admin/jobs", nil)
	mux.ServeHTTP(loginRec, loginReq)

	loc, err := url.Parse(loginRec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	state := loc.Query().Get("state")
	nonce := loc.Query().Get("nonce")

	var txnCookie *http.Cookie
	for _, c := range loginRec.Result().Cookies() {
		if c.Name == txnCookieName {
			txnCookie = c
		}
	}
	if txnCookie == nil {
		t.Fatal("no transaction cookie set by login")
	}

	// Step 2: the fake IdP mints an ID token matching the nonce it was given.
	claims := idp.baseClaims(testClientID, nonce)
	claims["groups"] = []string{"admins"}
	idp.nextClaims = claims

	// Step 3: callback, carrying the transaction cookie and matching state.
	cbRec := httptest.NewRecorder()
	cbReq := httptest.NewRequest(http.MethodGet, "/api/auth/callback?code=fake-code&state="+state, nil)
	cbReq.AddCookie(txnCookie)
	mux.ServeHTTP(cbRec, cbReq)

	if cbRec.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d, body=%s", cbRec.Code, http.StatusFound, cbRec.Body.String())
	}
	if got := cbRec.Header().Get("Location"); got != "/admin/jobs" {
		t.Errorf("Location = %q, want /admin/jobs", got)
	}

	var sessionCookie *http.Cookie
	var clearedTxn bool
	for _, c := range cbRec.Result().Cookies() {
		if c.Name == sessionCookieName {
			sessionCookie = c
		}
		if c.Name == txnCookieName && c.MaxAge < 0 {
			clearedTxn = true
		}
	}
	if sessionCookie == nil {
		t.Fatal("no session cookie set by callback")
	}
	if !clearedTxn {
		t.Error("transaction cookie should be cleared after callback")
	}

	session, err := ValidateSessionCookie(testSessionSecret, sessionCookie.Value)
	if err != nil {
		t.Fatalf("ValidateSessionCookie() error = %v", err)
	}
	if session.Username != "alice" {
		t.Errorf("session Username = %q, want alice", session.Username)
	}
	if len(session.Groups) != 1 || session.Groups[0] != "admins" {
		t.Errorf("session Groups = %v, want [admins]", session.Groups)
	}
}

func TestAuthHandler_Callback_Rejections(t *testing.T) {
	t.Run("missing transaction cookie", func(t *testing.T) {
		idp := newFakeIDP(t)
		mux := newAuthMux(t, idp)

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/auth/callback?code=fake-code&state=abc", nil)
		mux.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
		}
	})

	t.Run("state mismatch", func(t *testing.T) {
		idp := newFakeIDP(t)
		mux := newAuthMux(t, idp)

		loginRec := httptest.NewRecorder()
		mux.ServeHTTP(loginRec, httptest.NewRequest(http.MethodGet, "/api/auth/login", nil))
		var txnCookie *http.Cookie
		for _, c := range loginRec.Result().Cookies() {
			if c.Name == txnCookieName {
				txnCookie = c
			}
		}

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/auth/callback?code=fake-code&state=wrong-state", nil)
		req.AddCookie(txnCookie)
		mux.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
		}
	})
}

func TestAuthHandler_Logout(t *testing.T) {
	idp := newFakeIDP(t)
	mux := newAuthMux(t, idp)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil)
	mux.ServeHTTP(rec, req)

	var cleared bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Error("logout should clear the session cookie")
	}
}

func TestAuthHandler_LoginAndCallback_DisabledWhenNoOIDC(t *testing.T) {
	mux := newAuthMux(t, nil)

	for _, path := range []string{"/api/auth/login", "/api/auth/callback"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s status = %d, want %d", path, rec.Code, http.StatusNotFound)
		}
	}
}
