// Package runner manages GitHub Actions runner registration and configuration.
package runner

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/go-github/v57/github"
)

// GitHubClient handles GitHub App authentication and runner registration.
type GitHubClient struct {
	appID      int64
	privateKey *rsa.PrivateKey
	org        string
	httpClient *http.Client
}

// NewGitHubClient creates a new GitHub client for runner operations.
// privateKeyBase64 should be the base64-encoded PEM private key.
func NewGitHubClient(appID string, privateKeyBase64 string, org string) (*GitHubClient, error) {
	id, err := strconv.ParseInt(appID, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid app ID: %w", err)
	}

	keyBytes, err := base64.StdEncoding.DecodeString(privateKeyBase64)
	if err != nil {
		return nil, fmt.Errorf("failed to decode private key: %w", err)
	}

	block, _ := pem.Decode(keyBytes)
	if block == nil {
		return nil, fmt.Errorf("failed to parse PEM block")
	}

	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		// Try PKCS8 format
		keyInterface, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("failed to parse private key: %w", err)
		}
		var ok bool
		key, ok = keyInterface.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("private key is not RSA")
		}
	}

	return &GitHubClient{
		appID:      id,
		privateKey: key,
		org:        org,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}, nil
}

// generateJWT creates a JWT for GitHub App authentication.
func (c *GitHubClient) generateJWT() (string, error) {
	now := time.Now()
	claims := jwt.MapClaims{
		"iat": now.Unix(),
		"exp": now.Add(10 * time.Minute).Unix(),
		"iss": c.appID,
	}

	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	return token.SignedString(c.privateKey)
}

// getInstallationToken gets an installation access token.
// It tries org-level first, then falls back to user-level for personal accounts.
func (c *GitHubClient) getInstallationToken(ctx context.Context) (string, error) {
	jwt, err := c.generateJWT()
	if err != nil {
		return "", fmt.Errorf("failed to generate JWT: %w", err)
	}

	// Create client with JWT auth
	client := github.NewClient(c.httpClient).WithAuthToken(jwt)

	// Try org installation first
	installation, _, err := client.Apps.FindOrganizationInstallation(ctx, c.org)
	if err != nil {
		// Fall back to user installation for personal accounts
		installation, _, err = client.Apps.FindUserInstallation(ctx, c.org)
		if err != nil {
			return "", fmt.Errorf("failed to find installation for %s (tried org and user): %w", c.org, err)
		}
	}

	// Get installation token
	token, _, err := client.Apps.CreateInstallationToken(ctx, installation.GetID(), nil)
	if err != nil {
		return "", fmt.Errorf("failed to create installation token: %w", err)
	}

	return token.GetToken(), nil
}

// GetJITConfig gets a Just-In-Time runner configuration for registering a runner.
// Returns the JIT config that can be used with config.sh --jitconfig.
func (c *GitHubClient) GetJITConfig(ctx context.Context, runnerName string, labels []string) (string, error) {
	token, err := c.getInstallationToken(ctx)
	if err != nil {
		return "", err
	}

	// Create client with installation token
	client := github.NewClient(c.httpClient).WithAuthToken(token)

	// Create JIT runner config
	req := &github.GenerateJITConfigRequest{
		Name:          runnerName,
		RunnerGroupID: 1, // Default runner group
		Labels:        labels,
	}

	jitConfig, _, err := client.Actions.GenerateOrgJITConfig(ctx, c.org, req)
	if err != nil {
		return "", fmt.Errorf("failed to generate JIT config: %w", err)
	}

	return jitConfig.GetEncodedJITConfig(), nil
}

// GetRegistrationToken gets a registration token.
// If repo is provided (owner/repo format), uses repo-level registration for personal accounts.
// Otherwise uses org-level registration.
func (c *GitHubClient) GetRegistrationToken(ctx context.Context, repo string) (string, error) {
	token, err := c.getInstallationToken(ctx)
	if err != nil {
		return "", err
	}

	// Create client with installation token
	client := github.NewClient(c.httpClient).WithAuthToken(token)

	var regToken *github.RegistrationToken

	// Use repo-level registration if repo is provided
	if repo != "" {
		parts := strings.SplitN(repo, "/", 2)
		if len(parts) != 2 {
			return "", fmt.Errorf("invalid repo format, expected owner/repo: %s", repo)
		}
		owner, repoName := parts[0], parts[1]
		regToken, _, err = client.Actions.CreateRegistrationToken(ctx, owner, repoName)
		if err != nil {
			return "", fmt.Errorf("failed to create repo registration token for %s: %w", repo, err)
		}
	} else {
		// Try org-level registration
		regToken, _, err = client.Actions.CreateOrganizationRegistrationToken(ctx, c.org)
		if err != nil {
			return "", fmt.Errorf("failed to create org registration token: %w", err)
		}
	}

	return regToken.GetToken(), nil
}
