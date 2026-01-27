package admin

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"log/slog"
	"net/http"
	"strings"

	"github.com/Shavakan/runs-fleet/pkg/logging"
)

var adminAuthLog = logging.WithComponent(logging.LogTypeAdmin, "auth")

// AuthMiddleware provides authentication for admin API endpoints.
type AuthMiddleware struct {
	secret  string
	enabled bool
}

// NewAuthMiddleware creates authentication middleware for admin endpoints.
// If secret is empty, authentication is disabled.
func NewAuthMiddleware(secret string) *AuthMiddleware {
	return &AuthMiddleware{
		secret:  secret,
		enabled: secret != "",
	}
}

// Wrap returns an http.Handler that validates the admin token before calling the next handler.
func (m *AuthMiddleware) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !m.enabled {
			next.ServeHTTP(w, r)
			return
		}

		token := m.extractToken(r)
		if token == "" {
			adminAuthLog.Warn("admin auth failed: missing token", slog.String("remote_addr", r.RemoteAddr))
			http.Error(w, "Unauthorized: missing admin token", http.StatusUnauthorized)
			return
		}

		if !m.validateToken(token) {
			adminAuthLog.Warn("admin auth failed: invalid token", slog.String("remote_addr", r.RemoteAddr))
			http.Error(w, "Unauthorized: invalid admin token", http.StatusUnauthorized)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// WrapFunc is a convenience method for wrapping http.HandlerFunc.
func (m *AuthMiddleware) WrapFunc(next http.HandlerFunc) http.Handler {
	return m.Wrap(next)
}

// IsEnabled returns whether authentication is enabled.
func (m *AuthMiddleware) IsEnabled() bool {
	return m.enabled
}

func (m *AuthMiddleware) extractToken(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimPrefix(auth, "Bearer ")
	}
	return ""
}

func (m *AuthMiddleware) validateToken(token string) bool {
	h := hmac.New(sha256.New, []byte(m.secret))
	h.Write([]byte("admin"))
	expected := h.Sum(nil)

	provided, err := decodeHex(token)
	if err != nil {
		return false
	}

	return subtle.ConstantTimeCompare(expected, provided) == 1
}

func decodeHex(s string) ([]byte, error) {
	if len(s)%2 != 0 {
		return nil, errInvalidHex
	}
	result := make([]byte, len(s)/2)
	for i := 0; i < len(s); i += 2 {
		a := hexVal(s[i])
		b := hexVal(s[i+1])
		if a < 0 || b < 0 {
			return nil, errInvalidHex
		}
		result[i/2] = byte(a<<4 | b)
	}
	return result, nil
}

func hexVal(c byte) int {
	switch {
	case '0' <= c && c <= '9':
		return int(c - '0')
	case 'a' <= c && c <= 'f':
		return int(c - 'a' + 10)
	case 'A' <= c && c <= 'F':
		return int(c - 'A' + 10)
	default:
		return -1
	}
}

type hexError struct{}

func (hexError) Error() string { return "invalid hex encoding" }

var errInvalidHex = hexError{}
