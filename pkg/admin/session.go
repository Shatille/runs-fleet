package admin

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// signJSON HMAC-signs the JSON encoding of v. Token format:
// base64url(json).hex_hmac_signature — the same stateless-token shape as
// pkg/cache's cache-auth tokens, adapted to a JSON payload so callers can
// sign arbitrary claim structs (including ones with slice fields).
func signJSON(secret string, v any) (string, error) {
	if secret == "" {
		return "", fmt.Errorf("signing secret is required")
	}

	payload, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("marshal payload: %w", err)
	}

	encodedPayload := base64.RawURLEncoding.EncodeToString(payload)

	h := hmac.New(sha256.New, []byte(secret))
	h.Write(payload)
	signature := hex.EncodeToString(h.Sum(nil))

	return encodedPayload + "." + signature, nil
}

// verifyJSON checks the token's HMAC signature and, on success, unmarshals
// the payload into v. It does not know about expiry -- callers whose claim
// struct carries one must check it themselves after a successful call.
func verifyJSON(secret, token string, v any) error {
	if secret == "" || token == "" {
		return fmt.Errorf("missing signing secret or token")
	}

	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 {
		return fmt.Errorf("malformed token")
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return fmt.Errorf("malformed token: %w", err)
	}

	h := hmac.New(sha256.New, []byte(secret))
	h.Write(payload)
	expectedSignature := hex.EncodeToString(h.Sum(nil))

	if !hmac.Equal([]byte(parts[1]), []byte(expectedSignature)) {
		return fmt.Errorf("invalid token signature")
	}

	if err := json.Unmarshal(payload, v); err != nil {
		return fmt.Errorf("malformed token claims: %w", err)
	}

	return nil
}

// SessionClaims is the identity carried in a signed session cookie after a
// successful OIDC login. No refresh tokens: a session is valid until
// ExpiresAt, then the user re-authenticates. That's an acceptable tradeoff
// for an internal ops tool and deliberately simpler than refresh rotation.
type SessionClaims struct {
	Username  string   `json:"username"`
	Email     string   `json:"email,omitempty"`
	Groups    []string `json:"groups,omitempty"`
	ExpiresAt int64    `json:"exp"`
}

// GenerateSessionCookie creates a self-contained HMAC-signed session token.
func GenerateSessionCookie(secret string, claims SessionClaims) (string, error) {
	if claims.Username == "" {
		return "", fmt.Errorf("username is required")
	}
	return signJSON(secret, claims)
}

// ValidateSessionCookie verifies the token's HMAC signature and expiry,
// returning the claims on success. Signature verification happens before
// expiry is checked, and before the payload is trusted for anything.
func ValidateSessionCookie(secret, token string) (SessionClaims, error) {
	var claims SessionClaims
	if err := verifyJSON(secret, token, &claims); err != nil {
		return SessionClaims{}, err
	}
	if claims.ExpiresAt <= 0 || time.Now().Unix() > claims.ExpiresAt {
		return SessionClaims{}, fmt.Errorf("session expired")
	}
	return claims, nil
}
