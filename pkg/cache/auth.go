package cache

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"log"
	"net/http"
	"strings"
)

// contextKey is a custom type for context keys to avoid collisions.
type contextKey string

const (
	// scopeContextKey is the context key for cache scope.
	scopeContextKey contextKey = "cache_scope"
)

const (
	// CacheTokenHeader is the HTTP header used to pass the cache authentication token.
	CacheTokenHeader = "X-Cache-Token"
)

// GenerateCacheToken creates a self-contained HMAC token for cache authentication.
// Token format: base64url(job_id:instance_id:scope).hmac_hex
// This allows stateless validation - the server can extract the payload and verify the signature.
// The scope parameter should be in "org/repo" format for repository-scoped caching.
func GenerateCacheToken(secret, jobID, instanceID, scope string) string {
	if secret == "" || jobID == "" || instanceID == "" {
		return ""
	}

	// Create payload with optional scope
	payload := jobID + ":" + instanceID + ":" + scope
	encodedPayload := base64.RawURLEncoding.EncodeToString([]byte(payload))

	// Create HMAC signature
	h := hmac.New(sha256.New, []byte(secret))
	h.Write([]byte(payload))
	signature := hex.EncodeToString(h.Sum(nil))

	return encodedPayload + "." + signature
}

// ParseCacheToken extracts job_id, instance_id, and scope from a self-contained token.
// Returns empty strings if token is malformed.
// Scope may be empty for backwards compatibility with older tokens.
func ParseCacheToken(token string) (jobID, instanceID, scope string, ok bool) {
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 {
		return "", "", "", false
	}

	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", "", "", false
	}

	payload := string(payloadBytes)
	payloadParts := strings.SplitN(payload, ":", 3)
	if len(payloadParts) < 2 {
		return "", "", "", false
	}

	jobID = payloadParts[0]
	instanceID = payloadParts[1]
	if len(payloadParts) > 2 {
		scope = payloadParts[2]
	}

	return jobID, instanceID, scope, true
}

// ValidateCacheToken verifies that the provided token has a valid HMAC signature.
// This is stateless - it extracts the payload from the token and recomputes the HMAC.
func ValidateCacheToken(secret, token string) bool {
	if secret == "" || token == "" {
		return false
	}

	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 {
		return false
	}

	encodedPayload := parts[0]
	providedSignature := parts[1]

	// Decode payload
	payloadBytes, err := base64.RawURLEncoding.DecodeString(encodedPayload)
	if err != nil {
		return false
	}

	// Recompute HMAC
	h := hmac.New(sha256.New, []byte(secret))
	h.Write(payloadBytes)
	expectedSignature := hex.EncodeToString(h.Sum(nil))

	return hmac.Equal([]byte(providedSignature), []byte(expectedSignature))
}

// AuthMiddleware wraps an http.Handler to require cache token authentication.
// This is completely stateless - no token registration required.
type AuthMiddleware struct {
	secret  string
	enabled bool
}

// NewAuthMiddleware creates authentication middleware for cache endpoints.
// If secret is empty, authentication is disabled (for backwards compatibility).
func NewAuthMiddleware(secret string) *AuthMiddleware {
	return &AuthMiddleware{
		secret:  secret,
		enabled: secret != "",
	}
}

// Wrap returns an http.Handler that validates the cache token before calling the next handler.
// If valid, it extracts the scope from the token and stores it in the request context.
func (m *AuthMiddleware) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !m.enabled {
			next.ServeHTTP(w, r)
			return
		}

		token := r.Header.Get(CacheTokenHeader)
		if token == "" {
			// Also check Authorization header for Bearer token
			auth := r.Header.Get("Authorization")
			if strings.HasPrefix(auth, "Bearer ") {
				token = strings.TrimPrefix(auth, "Bearer ")
			}
		}

		if token == "" {
			log.Printf("Cache auth failed: missing token from %s", r.RemoteAddr)
			http.Error(w, "Unauthorized: missing cache token", http.StatusUnauthorized)
			return
		}

		if !ValidateCacheToken(m.secret, token) {
			log.Printf("Cache auth failed: invalid token from %s", r.RemoteAddr)
			http.Error(w, "Unauthorized: invalid cache token", http.StatusUnauthorized)
			return
		}

		// Extract scope from token and add to context
		_, _, scope, ok := ParseCacheToken(token)
		if ok && scope != "" {
			ctx := context.WithValue(r.Context(), scopeContextKey, scope)
			r = r.WithContext(ctx)
		}

		next.ServeHTTP(w, r)
	})
}

// ScopeFromContext extracts the cache scope from a request context.
// Returns empty string if no scope was set.
func ScopeFromContext(ctx context.Context) string {
	if scope, ok := ctx.Value(scopeContextKey).(string); ok {
		return scope
	}
	return ""
}

// WrapFunc is a convenience method for wrapping http.HandlerFunc.
func (m *AuthMiddleware) WrapFunc(next http.HandlerFunc) http.Handler {
	return m.Wrap(next)
}

// IsEnabled returns whether authentication is enabled.
func (m *AuthMiddleware) IsEnabled() bool {
	return m.enabled
}
