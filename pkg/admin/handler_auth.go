package admin

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/Shavakan/runs-fleet/pkg/logging"
)

var authHandlerLog = logging.WithComponent(logging.LogTypeAdmin, "auth")

const (
	txnCookieName        = "rf_oidc_txn"
	sessionCookieName    = "rf_admin_session"
	txnCookieTTL         = 5 * time.Minute
	defaultLoginRedirect = "/admin/"
)

// oidcTransaction is the short-lived, signed state carried between
// /api/auth/login and /api/auth/callback: the OAuth state/nonce/PKCE
// verifier this browser was issued, and where to send it after login.
type oidcTransaction struct {
	State        string `json:"state"`
	Nonce        string `json:"nonce"`
	CodeVerifier string `json:"code_verifier"`
	Redirect     string `json:"redirect"`
	ExpiresAt    int64  `json:"exp"`
}

// AuthHandler serves the OIDC login/callback/logout/config endpoints. A nil
// oidc means auth is not configured: login/callback 404, config reports
// auth_enabled=false, and other handlers' AuthMiddleware passes everything
// through (matching today's local-dev-without-auth ergonomic).
type AuthHandler struct {
	oidc          *OIDCClient
	sessionSecret string
	sessionTTL    time.Duration
}

// NewAuthHandler creates the auth endpoint handler. Pass a nil oidcClient to
// disable auth entirely.
func NewAuthHandler(oidcClient *OIDCClient, sessionSecret string, sessionTTL time.Duration) *AuthHandler {
	return &AuthHandler{
		oidc:          oidcClient,
		sessionSecret: sessionSecret,
		sessionTTL:    sessionTTL,
	}
}

// RegisterRoutes wires the auth endpoints onto mux.
func (h *AuthHandler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/auth/config", h.Config)
	mux.HandleFunc("GET /api/auth/login", h.Login)
	mux.HandleFunc("GET /api/auth/callback", h.Callback)
	mux.HandleFunc("POST /api/auth/logout", h.Logout)
}

// Config reports whether OIDC auth is enabled, so the frontend can render a
// login affordance without hardcoding deployment assumptions. Intentionally
// unauthenticated: the frontend needs this before it knows whether the user
// is logged in, and the response carries no sensitive data (a boolean and a
// fixed, well-known path -- never the issuer URL, client ID, or secret).
func (h *AuthHandler) Config(w http.ResponseWriter, r *http.Request) {
	resp := map[string]any{"auth_enabled": h.oidc != nil}
	if h.oidc != nil {
		resp["login_url"] = "/api/auth/login"
	}

	// Encode into a buffer first (matching pkg/admin/handler.go's writeJSON):
	// a mid-encode failure must not leave a partially-written 200 response
	// with the status/headers already flushed.
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(resp); err != nil {
		authHandlerLog.Error(r.Context(), "config: encode response failed", slog.String("error", err.Error()))
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if _, err := buf.WriteTo(w); err != nil {
		authHandlerLog.Error(r.Context(), "config: write response failed", slog.String("error", err.Error()))
	}
}

// Login starts the authorization-code + PKCE flow: mint state/nonce/verifier,
// stash them (plus the sanitized post-login redirect target) in a short-lived
// signed cookie, and send the browser to the IdP.
func (h *AuthHandler) Login(w http.ResponseWriter, r *http.Request) {
	if h.oidc == nil {
		http.NotFound(w, r)
		return
	}

	state, err := randomToken()
	if err != nil {
		authHandlerLog.Error(r.Context(), "login: generate state failed", slog.String("error", err.Error()))
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	nonce, err := randomToken()
	if err != nil {
		authHandlerLog.Error(r.Context(), "login: generate nonce failed", slog.String("error", err.Error()))
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	verifier := oauth2.GenerateVerifier()

	txn := oidcTransaction{
		State:        state,
		Nonce:        nonce,
		CodeVerifier: verifier,
		Redirect:     sanitizeRedirect(r.URL.Query().Get("redirect")),
		ExpiresAt:    time.Now().Add(txnCookieTTL).Unix(),
	}
	token, err := signJSON(h.sessionSecret, txn)
	if err != nil {
		authHandlerLog.Error(r.Context(), "login: sign transaction failed", slog.String("error", err.Error()))
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	setCookie(w, txnCookieName, token, int(txnCookieTTL.Seconds()))

	authCodeURL := h.oidc.AuthCodeURL(state,
		oidc.Nonce(nonce),
		oauth2.S256ChallengeOption(verifier),
	)
	http.Redirect(w, r, authCodeURL, http.StatusFound)
}

// Callback completes the flow: verify the transaction cookie and state match,
// exchange the code, verify the ID token (including nonce), and mint a
// session cookie carrying the identity claims.
func (h *AuthHandler) Callback(w http.ResponseWriter, r *http.Request) {
	if h.oidc == nil {
		http.NotFound(w, r)
		return
	}

	txnCookie, err := r.Cookie(txnCookieName)
	if err != nil || txnCookie.Value == "" {
		http.Error(w, "login session expired or missing, please try logging in again", http.StatusBadRequest)
		return
	}

	var txn oidcTransaction
	if verifyErr := verifyJSON(h.sessionSecret, txnCookie.Value, &txn); verifyErr != nil {
		authHandlerLog.Warn(r.Context(), "callback: invalid transaction cookie", slog.String("error", verifyErr.Error()))
		http.Error(w, "login session invalid, please try logging in again", http.StatusBadRequest)
		return
	}
	if txn.ExpiresAt <= 0 || time.Now().Unix() > txn.ExpiresAt {
		http.Error(w, "login session expired, please try logging in again", http.StatusBadRequest)
		return
	}

	clearCookie(w, txnCookieName)

	if r.URL.Query().Get("state") != txn.State {
		authHandlerLog.Warn(r.Context(), "callback: state mismatch")
		http.Error(w, "login state mismatch, please try logging in again", http.StatusBadRequest)
		return
	}

	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "missing authorization code", http.StatusBadRequest)
		return
	}

	identity, err := h.oidc.Exchange(r.Context(), code, txn.Nonce, oauth2.VerifierOption(txn.CodeVerifier))
	if err != nil {
		authHandlerLog.Error(r.Context(), "callback: token exchange/verification failed", slog.String("error", err.Error()))
		http.Error(w, "login failed, please try again", http.StatusBadGateway)
		return
	}

	sessionToken, err := GenerateSessionCookie(h.sessionSecret, SessionClaims{
		Username:  identity.Username,
		Email:     identity.Email,
		Groups:    identity.Groups,
		ExpiresAt: time.Now().Add(h.sessionTTL).Unix(),
	})
	if err != nil {
		authHandlerLog.Error(r.Context(), "callback: mint session failed", slog.String("error", err.Error()))
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	setCookie(w, sessionCookieName, sessionToken, int(h.sessionTTL.Seconds()))

	authHandlerLog.Info(r.Context(), "admin login succeeded", slog.String(logging.KeyUser, identity.Username))
	http.Redirect(w, r, txn.Redirect, http.StatusFound)
}

// Logout clears the session cookie. Safe to call whether or not a session
// currently exists.
func (h *AuthHandler) Logout(w http.ResponseWriter, _ *http.Request) {
	clearCookie(w, sessionCookieName)
	w.WriteHeader(http.StatusNoContent)
}

// setCookie sets an HttpOnly/Secure/SameSite=Lax cookie scoped to the whole
// site (both /api/* and /admin/* are served from the same origin, so Path=/
// is what makes it readable from both). Lax, not Strict, because the IdP's
// redirect back to /api/auth/callback is a cross-site top-level navigation
// that Strict would drop the transaction cookie on.
func setCookie(w http.ResponseWriter, name, value string, maxAgeSeconds int) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		MaxAge:   maxAgeSeconds,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
}

func clearCookie(w http.ResponseWriter, name string) {
	setCookie(w, name, "", -1)
}

// sanitizeRedirect only allows a same-origin relative path, defending
// against open redirects via the login endpoint's redirect param: reject an
// absolute URL (has a scheme/host), a protocol-relative "//host/path"
// (browsers treat leading "//" as scheme-relative to another host), or a
// backslash before the first "/" segment ends (browsers normalize a leading
// "/\host" the same as "//host" when resolving against a base URL, so it's
// an equivalent bypass).
func sanitizeRedirect(raw string) string {
	if raw == "" || raw[0] != '/' || strings.HasPrefix(raw, "//") || strings.HasPrefix(raw, "/\\") {
		return defaultLoginRedirect
	}
	return raw
}

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
