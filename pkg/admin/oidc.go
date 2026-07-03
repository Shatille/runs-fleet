package admin

import (
	"context"
	"crypto/subtle"
	"fmt"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// IdentityClaims is the user identity extracted from a verified ID token,
// independent of session/cookie concerns.
type IdentityClaims struct {
	Username string
	Email    string
	Groups   []string
}

// OIDCClientConfig configures an OIDCClient.
type OIDCClientConfig struct {
	IssuerURL    string
	ClientID     string
	ClientSecret string
	RedirectURL  string
	Scopes       []string
	// GroupsClaim is the ID token claim name holding the user's groups.
	// Providers vary: Keycloak/Okta use "groups", Auth0 commonly namespaces
	// it (e.g. "https://example.com/groups"), Google has none.
	GroupsClaim string
}

// OIDCClient wraps OIDC discovery, the authorization-code flow, and ID token
// verification for the admin login flow.
type OIDCClient struct {
	oauth2Config oauth2.Config
	verifier     *oidc.IDTokenVerifier
	groupsClaim  string
}

// NewOIDCClient performs OIDC discovery against cfg.IssuerURL and builds a
// client ready to drive the authorization-code flow. Discovery is a network
// call; callers should apply their own timeout via ctx.
func NewOIDCClient(ctx context.Context, cfg OIDCClientConfig) (*OIDCClient, error) {
	provider, err := oidc.NewProvider(ctx, cfg.IssuerURL)
	if err != nil {
		return nil, fmt.Errorf("oidc discovery against %q: %w", cfg.IssuerURL, err)
	}

	return &OIDCClient{
		oauth2Config: oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			RedirectURL:  cfg.RedirectURL,
			Endpoint:     provider.Endpoint(),
			Scopes:       cfg.Scopes,
		},
		verifier:    provider.Verifier(&oidc.Config{ClientID: cfg.ClientID}),
		groupsClaim: cfg.GroupsClaim,
	}, nil
}

// AuthCodeURL builds the authorization-endpoint redirect URL for state and
// the given options (e.g. PKCE challenge, nonce).
func (c *OIDCClient) AuthCodeURL(state string, opts ...oauth2.AuthCodeOption) string {
	return c.oauth2Config.AuthCodeURL(state, opts...)
}

// Exchange trades an authorization code for tokens, verifies the ID token
// (issuer, audience, expiry via the verifier; nonce here since go-oidc
// doesn't check it), and extracts identity claims.
func (c *OIDCClient) Exchange(ctx context.Context, code, expectedNonce string, opts ...oauth2.AuthCodeOption) (*IdentityClaims, error) {
	token, err := c.oauth2Config.Exchange(ctx, code, opts...)
	if err != nil {
		return nil, fmt.Errorf("exchange authorization code: %w", err)
	}

	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok || rawIDToken == "" {
		return nil, fmt.Errorf("token response missing id_token")
	}

	idToken, err := c.verifier.Verify(ctx, rawIDToken)
	if err != nil {
		return nil, fmt.Errorf("verify id_token: %w", err)
	}

	if !constantTimeEqual(idToken.Nonce, expectedNonce) {
		return nil, fmt.Errorf("id_token nonce mismatch")
	}

	var std struct {
		Subject           string `json:"sub"`
		Email             string `json:"email"`
		PreferredUsername string `json:"preferred_username"`
	}
	if err := idToken.Claims(&std); err != nil {
		return nil, fmt.Errorf("parse id_token claims: %w", err)
	}

	username := std.PreferredUsername
	if username == "" {
		username = std.Email
	}
	if username == "" {
		username = std.Subject
	}
	if username == "" {
		return nil, fmt.Errorf("id_token has no usable identity claim (preferred_username, email, or sub)")
	}

	var groups []string
	if c.groupsClaim != "" {
		var raw map[string]any
		if err := idToken.Claims(&raw); err == nil {
			if v, ok := raw[c.groupsClaim].([]any); ok {
				for _, g := range v {
					if s, ok := g.(string); ok {
						groups = append(groups, s)
					}
				}
			}
		}
	}

	return &IdentityClaims{
		Username: username,
		Email:    std.Email,
		Groups:   groups,
	}, nil
}

// constantTimeEqual compares a and b without leaking their contents via
// comparison-time timing. subtle.ConstantTimeCompare itself requires equal
// lengths, so unequal-length inputs (a cheap, non-secret check) short-circuit
// before it.
func constantTimeEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
