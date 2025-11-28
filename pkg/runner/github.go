// Package runner manages GitHub Actions runner registration and configuration.
package runner

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
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
	httpClient *http.Client
}

// NewGitHubClient creates a new GitHub client for runner operations.
// privateKeyBase64 should be the base64-encoded PEM private key.
func NewGitHubClient(appID string, privateKeyBase64 string) (*GitHubClient, error) {
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
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}, nil
}

// generateJWT creates a JWT for GitHub App authentication.
func (c *GitHubClient) generateJWT() (string, error) {
	now := time.Now()
	claims := jwt.MapClaims{
		"iat": now.Add(-60 * time.Second).Unix(), // 60s buffer for clock skew
		"exp": now.Add(10 * time.Minute).Unix(),
		"iss": c.appID,
	}

	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	return token.SignedString(c.privateKey)
}

// installationInfo holds installation token and account type.
type installationInfo struct {
	Token string
	IsOrg bool // true for Organization, false for User
}

// getInstallationInfo gets an installation access token and account type.
// It tries org-level first, then falls back to user-level for personal accounts.
// Uses raw HTTP with Bearer prefix for JWT auth (go-github's WithAuthToken uses token prefix).
func (c *GitHubClient) getInstallationInfo(ctx context.Context, owner string) (*installationInfo, error) {
	if owner == "" {
		return nil, fmt.Errorf("owner is required")
	}

	jwt, err := c.generateJWT()
	if err != nil {
		return nil, fmt.Errorf("failed to generate JWT: %w", err)
	}

	// Find installation for org (using Bearer auth for JWT)
	url := fmt.Sprintf("https://api.github.com/orgs/%s/installation", owner)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create installation request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to find installation: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Fall back to user installation if org not found
	if resp.StatusCode == http.StatusNotFound {
		_ = resp.Body.Close() // Close first response before reassigning

		url = fmt.Sprintf("https://api.github.com/users/%s/installation", owner)
		req, err = http.NewRequestWithContext(ctx, "GET", url, nil)
		if err != nil {
			return nil, fmt.Errorf("failed to create user installation request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+jwt)
		req.Header.Set("Accept", "application/vnd.github+json")

		resp, err = c.httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("failed to find user installation: %w", err)
		}
		defer func() { _ = resp.Body.Close() }()
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("failed to find installation for %s: status=%d body=%s", owner, resp.StatusCode, string(body))
	}

	var installation struct {
		ID      int64 `json:"id"`
		Account struct {
			Type string `json:"type"`
		} `json:"account"`
	}
	if decodeErr := json.NewDecoder(resp.Body).Decode(&installation); decodeErr != nil {
		return nil, fmt.Errorf("failed to decode installation: %w", decodeErr)
	}

	isOrg := installation.Account.Type == "Organization"

	// Create installation token
	url = fmt.Sprintf("https://api.github.com/app/installations/%d/access_tokens", installation.ID)
	req, err = http.NewRequestWithContext(ctx, "POST", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create token request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err = c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to create installation token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("failed to create installation token: status=%d body=%s", resp.StatusCode, string(body))
	}

	var tokenResp struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return nil, fmt.Errorf("failed to decode token response: %w", err)
	}

	return &installationInfo{Token: tokenResp.Token, IsOrg: isOrg}, nil
}

// getInstallationToken is a convenience wrapper that returns just the token.
func (c *GitHubClient) getInstallationToken(ctx context.Context, owner string) (string, error) {
	info, err := c.getInstallationInfo(ctx, owner)
	if err != nil {
		return "", err
	}
	return info.Token, nil
}

// GetJITConfig gets a Just-In-Time runner configuration for registering a runner.
// Returns the JIT config that can be used with config.sh --jitconfig.
// The org parameter specifies which organization to register the runner for.
func (c *GitHubClient) GetJITConfig(ctx context.Context, org string, runnerName string, labels []string) (string, error) {
	if org == "" {
		return "", fmt.Errorf("org is required")
	}

	token, err := c.getInstallationToken(ctx, org)
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

	jitConfig, _, err := client.Actions.GenerateOrgJITConfig(ctx, org, req)
	if err != nil {
		return "", fmt.Errorf("failed to generate JIT config: %w", err)
	}

	return jitConfig.GetEncodedJITConfig(), nil
}

// RegistrationResult contains the registration token and account type.
type RegistrationResult struct {
	Token string
	IsOrg bool // true for Organization, false for User (personal account)
}

// GetRegistrationToken gets a registration token for GitHub Actions runners.
// For organizations, uses org-level endpoint. For personal accounts, uses repo-level.
// Extracts owner from repo string (owner/repo format) for installation token.
func (c *GitHubClient) GetRegistrationToken(ctx context.Context, repo string) (*RegistrationResult, error) {
	// Extract owner from repo string (required)
	if repo == "" {
		return nil, fmt.Errorf("repo is required (owner/repo format)")
	}
	parts := strings.SplitN(repo, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return nil, fmt.Errorf("invalid repo format, expected owner/repo: %s", repo)
	}
	owner := parts[0]

	info, err := c.getInstallationInfo(ctx, owner)
	if err != nil {
		return nil, err
	}

	// Use org-level endpoint for organizations, repo-level for personal accounts
	var url string
	if info.IsOrg {
		url = fmt.Sprintf("https://api.github.com/orgs/%s/actions/runners/registration-token", owner)
	} else {
		url = fmt.Sprintf("https://api.github.com/repos/%s/actions/runners/registration-token", repo)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Authorization", "token "+info.Token)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to execute request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusCreated {
		var result struct {
			Token string `json:"token"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			return nil, fmt.Errorf("failed to decode response: %w", err)
		}
		return &RegistrationResult{Token: result.Token, IsOrg: info.IsOrg}, nil
	}

	// Read error body for debugging
	body, _ := io.ReadAll(resp.Body)
	return nil, fmt.Errorf("failed to create registration token for %s: status=%d body=%s", repo, resp.StatusCode, string(body))
}
