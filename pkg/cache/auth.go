package cache

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"log"
	"net/http"
	"strings"
)

const (
	// CacheTokenHeader is the HTTP header used to pass the cache authentication token.
	CacheTokenHeader = "X-Cache-Token"
)

// GenerateCacheToken creates a self-contained HMAC token for cache authentication.
// Token format: base64url(job_id:instance_id).hmac_hex
// This allows stateless validation - the server can extract the payload and verify the signature.
func GenerateCacheToken(secret, jobID, instanceID string) string {
	if secret == "" || jobID == "" || instanceID == "" {
		return ""
	}

	// Create payload
	payload := jobID + ":" + instanceID
	encodedPayload := base64.RawURLEncoding.EncodeToString([]byte(payload))

	// Create HMAC signature
	h := hmac.New(sha256.New, []byte(secret))
	h.Write([]byte(payload))
	signature := hex.EncodeToString(h.Sum(nil))

	return encodedPayload + "." + signature
}

// ParseCacheToken extracts job_id and instance_id from a self-contained token.
// Returns empty strings if token is malformed.
func ParseCacheToken(token string) (jobID, instanceID string, ok bool) {
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 {
		return "", "", false
	}

	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", "", false
	}

	payload := string(payloadBytes)
	colonIdx := strings.Index(payload, ":")
	if colonIdx == -1 {
		return "", "", false
	}

	return payload[:colonIdx], payload[colonIdx+1:], true
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

		next.ServeHTTP(w, r)
	})
}

// WrapFunc is a convenience method for wrapping http.HandlerFunc.
func (m *AuthMiddleware) WrapFunc(next http.HandlerFunc) http.Handler {
	return m.Wrap(http.HandlerFunc(next))
}

// IsEnabled returns whether authentication is enabled.
func (m *AuthMiddleware) IsEnabled() bool {
	return m.enabled
}
