package admin

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/Shavakan/runs-fleet/pkg/logging"
)

var adminAuthLog = logging.WithComponent(logging.LogTypeAdmin, "auth")

type contextKey string

const (
	// UserContextKey is the context key for the authenticated user.
	UserContextKey contextKey = "admin_user"
	// GroupsContextKey is the context key for the user's groups.
	GroupsContextKey contextKey = "admin_groups"
)

// UserInfo contains the authenticated user's identity, populated from a
// verified OIDC session.
type UserInfo struct {
	Username string
	Email    string
	Groups   []string
}

// AuthMiddleware validates the admin session cookie minted by AuthHandler
// after a successful OIDC login. When sessionSecret is empty, auth is
// disabled entirely (local dev without an IdP configured) and every request
// passes through unauthenticated.
type AuthMiddleware struct {
	sessionSecret string
	requireAuth   bool
}

// NewAuthMiddleware creates authentication middleware for admin endpoints.
// Pass an empty sessionSecret to disable auth (matches config.Validate's
// guarantee that OIDC config is either fully set or fully empty).
func NewAuthMiddleware(sessionSecret string) *AuthMiddleware {
	return &AuthMiddleware{
		sessionSecret: sessionSecret,
		requireAuth:   sessionSecret != "",
	}
}

// Wrap returns an http.Handler that validates the session cookie and adds
// the authenticated user to the request context.
func (m *AuthMiddleware) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !m.requireAuth {
			next.ServeHTTP(w, r)
			return
		}

		cookie, err := r.Cookie(sessionCookieName)
		if err != nil || cookie.Value == "" {
			adminAuthLog.Warn(r.Context(), "admin auth failed: missing session cookie",
				slog.String("remote_addr", r.RemoteAddr),
				slog.String("path", r.URL.Path))
			http.Error(w, "Unauthorized: authentication required", http.StatusUnauthorized)
			return
		}

		claims, err := ValidateSessionCookie(m.sessionSecret, cookie.Value)
		if err != nil {
			adminAuthLog.Warn(r.Context(), "admin auth failed: invalid session",
				slog.String("remote_addr", r.RemoteAddr),
				slog.String("path", r.URL.Path),
				slog.String("error", err.Error()))
			http.Error(w, "Unauthorized: invalid or expired session", http.StatusUnauthorized)
			return
		}

		user := UserInfo{Username: claims.Username, Email: claims.Email, Groups: claims.Groups}
		ctx := context.WithValue(r.Context(), UserContextKey, user)
		if len(user.Groups) > 0 {
			ctx = context.WithValue(ctx, GroupsContextKey, user.Groups)
		}

		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// WrapFunc is a convenience method for wrapping http.HandlerFunc.
func (m *AuthMiddleware) WrapFunc(next http.HandlerFunc) http.Handler {
	return m.Wrap(next)
}

// IsEnabled returns whether authentication is required.
func (m *AuthMiddleware) IsEnabled() bool {
	return m.requireAuth
}

// GetUser returns the authenticated user from the request context.
// Returns empty UserInfo if not authenticated.
func GetUser(ctx context.Context) UserInfo {
	if user, ok := ctx.Value(UserContextKey).(UserInfo); ok {
		return user
	}
	return UserInfo{}
}

// GetUsername returns the authenticated username from the request context.
// Returns empty string if not authenticated.
func GetUsername(ctx context.Context) string {
	return GetUser(ctx).Username
}
