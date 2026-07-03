package admin

import (
	"strings"
	"testing"
	"time"
)

func TestGenerateAndValidateSessionCookie_RoundTrip(t *testing.T) {
	t.Parallel()

	claims := SessionClaims{
		Username:  "alice",
		Email:     "alice@example.com",
		Groups:    []string{"admins", "sre"},
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
	}

	token, err := GenerateSessionCookie("test-secret", claims)
	if err != nil {
		t.Fatalf("GenerateSessionCookie() error = %v", err)
	}
	if token == "" {
		t.Fatal("GenerateSessionCookie() returned empty token")
	}

	got, err := ValidateSessionCookie("test-secret", token)
	if err != nil {
		t.Fatalf("ValidateSessionCookie() error = %v", err)
	}
	if got.Username != claims.Username {
		t.Errorf("Username = %q, want %q", got.Username, claims.Username)
	}
	if got.Email != claims.Email {
		t.Errorf("Email = %q, want %q", got.Email, claims.Email)
	}
	if len(got.Groups) != len(claims.Groups) || got.Groups[0] != claims.Groups[0] || got.Groups[1] != claims.Groups[1] {
		t.Errorf("Groups = %v, want %v", got.Groups, claims.Groups)
	}
	if got.ExpiresAt != claims.ExpiresAt {
		t.Errorf("ExpiresAt = %d, want %d", got.ExpiresAt, claims.ExpiresAt)
	}
}

func TestGenerateSessionCookie_Validation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		secret string
		claims SessionClaims
	}{
		{
			name:   "empty secret",
			secret: "",
			claims: SessionClaims{Username: "alice", ExpiresAt: time.Now().Add(time.Hour).Unix()},
		},
		{
			name:   "empty username",
			secret: "test-secret",
			claims: SessionClaims{Username: "", ExpiresAt: time.Now().Add(time.Hour).Unix()},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if _, err := GenerateSessionCookie(tt.secret, tt.claims); err == nil {
				t.Error("GenerateSessionCookie() expected error, got nil")
			}
		})
	}
}

func TestValidateSessionCookie_Rejections(t *testing.T) {
	t.Parallel()

	validClaims := SessionClaims{Username: "alice", ExpiresAt: time.Now().Add(time.Hour).Unix()}
	validToken, err := GenerateSessionCookie("test-secret", validClaims)
	if err != nil {
		t.Fatalf("setup: GenerateSessionCookie() error = %v", err)
	}

	expiredToken, err := GenerateSessionCookie("test-secret", SessionClaims{
		Username:  "alice",
		ExpiresAt: time.Now().Add(-time.Hour).Unix(),
	})
	if err != nil {
		t.Fatalf("setup: GenerateSessionCookie() error = %v", err)
	}

	tests := []struct {
		name   string
		secret string
		token  string
	}{
		{"empty secret", "", validToken},
		{"empty token", "test-secret", ""},
		{"malformed token, no separator", "test-secret", "not-a-valid-token"},
		{"tampered payload", "test-secret", tamperPayload(t, validToken)},
		{"wrong secret", "wrong-secret", validToken},
		{"expired session", "test-secret", expiredToken},
		{"garbage base64 payload", "test-secret", "not!base64!." + strings.Repeat("a", 64)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if _, err := ValidateSessionCookie(tt.secret, tt.token); err == nil {
				t.Error("ValidateSessionCookie() expected error, got nil")
			}
		})
	}
}

// tamperPayload flips the first character of the encoded payload while
// leaving the signature untouched, so signature verification must fail.
func tamperPayload(t *testing.T, token string) string {
	t.Helper()
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 || len(parts[0]) == 0 {
		t.Fatalf("test setup: token %q has unexpected shape", token)
	}
	tampered := "X" + parts[0][1:]
	return tampered + "." + parts[1]
}
