package admin

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// fakeIDP is a minimal OIDC provider for testing: discovery document, JWKS,
// and a token endpoint that mints an ID token from whatever claims the test
// installs via nextClaims before calling Exchange.
type fakeIDP struct {
	server      *httptest.Server
	key         *rsa.PrivateKey
	kid         string
	nextClaims  jwt.MapClaims
	accessToken string
}

func newFakeIDP(t *testing.T) *fakeIDP {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}

	idp := &fakeIDP{key: key, kid: "test-key-1", accessToken: "fake-access-token"}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", idp.handleDiscovery)
	mux.HandleFunc("/jwks", idp.handleJWKS)
	mux.HandleFunc("/token", idp.handleToken)
	mux.HandleFunc("/auth", func(http.ResponseWriter, *http.Request) {})

	idp.server = httptest.NewServer(mux)
	t.Cleanup(idp.server.Close)
	return idp
}

func (idp *fakeIDP) handleDiscovery(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"issuer":                                idp.server.URL,
		"authorization_endpoint":                idp.server.URL + "/auth",
		"token_endpoint":                        idp.server.URL + "/token",
		"jwks_uri":                              idp.server.URL + "/jwks",
		"userinfo_endpoint":                     idp.server.URL + "/userinfo",
		"id_token_signing_alg_values_supported": []string{"RS256"},
	})
}

func (idp *fakeIDP) handleJWKS(w http.ResponseWriter, _ *http.Request) {
	pub := idp.key.PublicKey
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"keys": []map[string]any{
			{
				"kty": "RSA",
				"use": "sig",
				"alg": "RS256",
				"kid": idp.kid,
				"n":   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
				"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
			},
		},
	})
}

func (idp *fakeIDP) handleToken(w http.ResponseWriter, _ *http.Request) {
	if idp.nextClaims == nil {
		http.Error(w, "no claims installed for this test", http.StatusInternalServerError)
		return
	}

	token := jwt.NewWithClaims(jwt.SigningMethodRS256, idp.nextClaims)
	token.Header["kid"] = idp.kid
	signed, err := token.SignedString(idp.key)
	if err != nil {
		http.Error(w, fmt.Sprintf("sign id_token: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token": idp.accessToken,
		"token_type":   "Bearer",
		"id_token":     signed,
	})
}

// baseClaims returns a valid, non-expired claim set for issuer/subject/audience
// pinned to this fake IdP and clientID, which tests then mutate.
func (idp *fakeIDP) baseClaims(clientID, nonce string) jwt.MapClaims {
	now := time.Now()
	return jwt.MapClaims{
		"iss":                idp.server.URL,
		"sub":                "user-123",
		"aud":                clientID,
		"exp":                now.Add(time.Hour).Unix(),
		"iat":                now.Unix(),
		"nonce":              nonce,
		"preferred_username": "alice",
		"email":              "alice@example.com",
	}
}

const testClientID = "test-client-id"

func newTestOIDCClient(t *testing.T, idp *fakeIDP, groupsClaim string) *OIDCClient {
	t.Helper()
	ctx := context.Background()
	client, err := NewOIDCClient(ctx, OIDCClientConfig{
		IssuerURL:    idp.server.URL,
		ClientID:     testClientID,
		ClientSecret: "test-client-secret",
		RedirectURL:  "https://admin.example.com/api/auth/callback",
		Scopes:       []string{"openid", "profile", "email"},
		GroupsClaim:  groupsClaim,
	})
	if err != nil {
		t.Fatalf("NewOIDCClient() error = %v", err)
	}
	return client
}

func TestOIDCClient_Exchange_Success(t *testing.T) {
	idp := newFakeIDP(t)
	client := newTestOIDCClient(t, idp, "groups")

	claims := idp.baseClaims(testClientID, "expected-nonce")
	claims["groups"] = []string{"admins", "sre"}
	idp.nextClaims = claims

	got, err := client.Exchange(context.Background(), "fake-code", "expected-nonce")
	if err != nil {
		t.Fatalf("Exchange() error = %v", err)
	}
	if got.Username != "alice" {
		t.Errorf("Username = %q, want alice", got.Username)
	}
	if got.Email != "alice@example.com" {
		t.Errorf("Email = %q, want alice@example.com", got.Email)
	}
	if len(got.Groups) != 2 || got.Groups[0] != "admins" || got.Groups[1] != "sre" {
		t.Errorf("Groups = %v, want [admins sre]", got.Groups)
	}
}

func TestOIDCClient_Exchange_UsernameFallback(t *testing.T) {
	idp := newFakeIDP(t)
	client := newTestOIDCClient(t, idp, "groups")

	// No preferred_username claim -- must fall back to email.
	claims := idp.baseClaims(testClientID, "n")
	delete(claims, "preferred_username")
	idp.nextClaims = claims

	got, err := client.Exchange(context.Background(), "fake-code", "n")
	if err != nil {
		t.Fatalf("Exchange() error = %v", err)
	}
	if got.Username != "alice@example.com" {
		t.Errorf("Username = %q, want fallback to email alice@example.com", got.Username)
	}
}

func TestOIDCClient_Exchange_MissingGroupsClaimIsEmpty(t *testing.T) {
	idp := newFakeIDP(t)
	client := newTestOIDCClient(t, idp, "groups")

	claims := idp.baseClaims(testClientID, "n")
	idp.nextClaims = claims // no "groups" key at all

	got, err := client.Exchange(context.Background(), "fake-code", "n")
	if err != nil {
		t.Fatalf("Exchange() error = %v", err)
	}
	if len(got.Groups) != 0 {
		t.Errorf("Groups = %v, want empty", got.Groups)
	}
}

func TestOIDCClient_Exchange_CustomGroupsClaimName(t *testing.T) {
	idp := newFakeIDP(t)
	client := newTestOIDCClient(t, idp, "https://example.com/groups")

	claims := idp.baseClaims(testClientID, "n")
	claims["https://example.com/groups"] = []string{"platform"}
	idp.nextClaims = claims

	got, err := client.Exchange(context.Background(), "fake-code", "n")
	if err != nil {
		t.Fatalf("Exchange() error = %v", err)
	}
	if len(got.Groups) != 1 || got.Groups[0] != "platform" {
		t.Errorf("Groups = %v, want [platform]", got.Groups)
	}
}

func TestOIDCClient_Exchange_Rejections(t *testing.T) {
	tests := []struct {
		name         string
		mutateClaims func(jwt.MapClaims)
		nonce        string
	}{
		{
			name: "expired token",
			mutateClaims: func(c jwt.MapClaims) {
				c["exp"] = time.Now().Add(-time.Hour).Unix()
			},
			nonce: "n",
		},
		{
			name: "wrong audience",
			mutateClaims: func(c jwt.MapClaims) {
				c["aud"] = "some-other-client-id"
			},
			nonce: "n",
		},
		{
			name:         "wrong nonce",
			mutateClaims: func(jwt.MapClaims) {},
			nonce:        "mismatched-nonce",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			idp := newFakeIDP(t)
			client := newTestOIDCClient(t, idp, "groups")

			claims := idp.baseClaims(testClientID, "n")
			tt.mutateClaims(claims)
			idp.nextClaims = claims

			if _, err := client.Exchange(context.Background(), "fake-code", tt.nonce); err == nil {
				t.Error("Exchange() expected error, got nil")
			}
		})
	}
}
