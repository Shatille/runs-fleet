package admin

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

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

// Keycloak gatekeeper proxy headers
const (
	HeaderUser   = "X-Auth-Request-User"
	HeaderGroups = "X-Auth-Request-Groups"
	HeaderEmail  = "X-Auth-Request-Email"
)

// UserInfo contains authenticated user information from Keycloak.
type UserInfo struct {
	Username string
	Email    string
	Groups   []string
}

// AuthMiddleware validates requests authenticated by Keycloak gatekeeper proxy.
// It expects the proxy to set X-Auth-Request-User and optionally X-Auth-Request-Groups headers.
type AuthMiddleware struct {
	// requireAuth when true, rejects requests without valid Keycloak headers.
	// Set to false for local development without Keycloak.
	requireAuth bool
}

// NewAuthMiddleware creates authentication middleware for admin endpoints.
// When requireAuth is true (non-empty string passed), requests without Keycloak headers are rejected.
// For backwards compatibility, pass empty string to disable auth requirement.
func NewAuthMiddleware(requireAuth string) *AuthMiddleware {
	return &AuthMiddleware{
		requireAuth: requireAuth != "",
	}
}

// Wrap returns an http.Handler that extracts user identity from Keycloak headers.
func (m *AuthMiddleware) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user := m.extractUserInfo(r)

		if m.requireAuth && user.Username == "" {
			adminAuthLog.Warn("admin auth failed: missing user header",
				slog.String("remote_addr", r.RemoteAddr),
				slog.String("path", r.URL.Path))
			http.Error(w, "Unauthorized: authentication required", http.StatusUnauthorized)
			return
		}

		// Add user info to context
		ctx := r.Context()
		ctx = context.WithValue(ctx, UserContextKey, user)
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

func (m *AuthMiddleware) extractUserInfo(r *http.Request) UserInfo {
	user := UserInfo{
		Username: r.Header.Get(HeaderUser),
		Email:    r.Header.Get(HeaderEmail),
	}

	if groups := r.Header.Get(HeaderGroups); groups != "" {
		user.Groups = strings.Split(groups, ",")
		for i := range user.Groups {
			user.Groups[i] = strings.TrimSpace(user.Groups[i])
		}
	}

	return user
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
