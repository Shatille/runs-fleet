package cache

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"log"
	"net/http"
	"strings"
)

const (
	// CacheTokenHeader is the HTTP header used to pass the cache authentication token.
	CacheTokenHeader = "X-Cache-Token"
)

// GenerateCacheToken creates an HMAC-SHA256 token for cache authentication.
// The token is derived from the secret and job/instance identifiers, making it
// unforgeable without knowledge of the secret and tied to a specific job.
func GenerateCacheToken(secret, jobID, instanceID string) string {
	if secret == "" {
		return ""
	}

	h := hmac.New(sha256.New, []byte(secret))
	h.Write([]byte(jobID + ":" + instanceID))
	return hex.EncodeToString(h.Sum(nil))
}

// ValidateCacheToken verifies that the provided token matches the expected HMAC.
// Returns true if the token is valid, false otherwise.
func ValidateCacheToken(secret, token, jobID, instanceID string) bool {
	if secret == "" || token == "" {
		return false
	}

	expected := GenerateCacheToken(secret, jobID, instanceID)
	return hmac.Equal([]byte(token), []byte(expected))
}

// TokenStore provides an interface for looking up job/instance info from a token.
// This allows the auth middleware to validate tokens without storing them.
type TokenStore interface {
	// ValidateToken checks if the token is valid and returns the associated job ID.
	// Returns empty string if token is invalid.
	ValidateToken(token string) (jobID string, valid bool)
}

// HMACTokenStore validates tokens using HMAC computation.
// It maintains a mapping of active tokens to their job IDs for validation.
type HMACTokenStore struct {
	secret string
	// activeTokens maps token -> jobID for active jobs
	// This is populated when jobs are created and cleaned up when jobs complete
	activeTokens map[string]string
}

// NewHMACTokenStore creates a new HMAC-based token store.
func NewHMACTokenStore(secret string) *HMACTokenStore {
	return &HMACTokenStore{
		secret:       secret,
		activeTokens: make(map[string]string),
	}
}

// RegisterToken adds a token for an active job.
func (s *HMACTokenStore) RegisterToken(token, jobID string) {
	if token != "" && jobID != "" {
		s.activeTokens[token] = jobID
	}
}

// UnregisterToken removes a token when a job completes.
func (s *HMACTokenStore) UnregisterToken(token string) {
	delete(s.activeTokens, token)
}

// ValidateToken checks if the token is registered and returns the job ID.
func (s *HMACTokenStore) ValidateToken(token string) (string, bool) {
	jobID, ok := s.activeTokens[token]
	return jobID, ok
}

// AuthMiddleware wraps an http.Handler to require cache token authentication.
type AuthMiddleware struct {
	store   TokenStore
	secret  string
	enabled bool
}

// NewAuthMiddleware creates authentication middleware for cache endpoints.
// If secret is empty, authentication is disabled (for backwards compatibility).
func NewAuthMiddleware(secret string, store TokenStore) *AuthMiddleware {
	return &AuthMiddleware{
		store:   store,
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

		if m.store != nil {
			if _, valid := m.store.ValidateToken(token); !valid {
				log.Printf("Cache auth failed: invalid token from %s", r.RemoteAddr)
				http.Error(w, "Unauthorized: invalid cache token", http.StatusUnauthorized)
				return
			}
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
