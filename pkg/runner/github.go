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
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/go-github/v57/github"
)

const (
	maxRetries    = 3
	maxRetryDelay = 10 * time.Second
)

// baseRetryDelay is the base delay for retry backoff.
// Exposed as a variable to allow testing with shorter durations.
var baseRetryDelay = 500 * time.Millisecond

// isRetryableError returns true if the HTTP response indicates a retryable error.
func isRetryableError(resp *http.Response, err error) bool {
	if err != nil {
		return true // Network errors are retryable
	}
	if resp == nil {
		return true
	}
	// Retry on rate limit (429) or server errors (5xx)
	return resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
}

// retryDelay calculates exponential backoff with jitter.
func retryDelay(attempt int) time.Duration {
	delay := baseRetryDelay * time.Duration(1<<attempt)
	if delay > maxRetryDelay {
		delay = maxRetryDelay
	}
	// Add jitter: 50-100% of calculated delay
	jitter := time.Duration(rand.Int64N(int64(delay / 2)))
	return delay/2 + jitter
}

// GitHubClient handles GitHub App authentication and runner registration.
type GitHubClient struct {
	appID      int64
	privateKey *rsa.PrivateKey
	httpClient *http.Client
	baseURL    string // API base URL, defaults to https://api.github.com
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
		baseURL:    "https://api.github.com",
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
	url := fmt.Sprintf("%s/orgs/%s/installation", c.baseURL, owner)
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

		url = fmt.Sprintf("%s/users/%s/installation", c.baseURL, owner)
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
	url = fmt.Sprintf("%s/app/installations/%d/access_tokens", c.baseURL, installation.ID)
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

// RegistrationResult contains the registration token.
type RegistrationResult struct {
	Token string
	IsOrg bool // Deprecated: always false since repo-level registration is always used
}

// GetRegistrationToken gets a registration token for GitHub Actions runners.
// Always uses repo-level endpoint to ensure runners only pick up jobs from the specific repository.
// Extracts owner from repo string (owner/repo format) for installation token.
// Retries transient errors with exponential backoff.
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

	// Always use repo-level endpoint to ensure runners only pick up jobs
	// from the specific repository. Org-level registration allows runners
	// to pick up jobs from ANY repo in the org, causing job misassignment.
	// See: https://docs.github.com/en/rest/actions/self-hosted-runners#create-a-registration-token-for-a-repository
	url := fmt.Sprintf("%s/repos/%s/actions/runners/registration-token", c.baseURL, repo)

	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			delay := retryDelay(attempt - 1)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}

		req, err := http.NewRequestWithContext(ctx, "POST", url, nil)
		if err != nil {
			return nil, fmt.Errorf("failed to create request: %w", err)
		}
		req.Header.Set("Authorization", "token "+info.Token)
		req.Header.Set("Accept", "application/vnd.github+json")

		resp, err := c.httpClient.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("failed to execute request: %w", err)
			continue
		}

		if resp.StatusCode == http.StatusCreated {
			var result struct {
				Token string `json:"token"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
				_ = resp.Body.Close()
				return nil, fmt.Errorf("failed to decode response: %w", err)
			}
			_ = resp.Body.Close()
			return &RegistrationResult{Token: result.Token, IsOrg: false}, nil
		}

		// Read error body for debugging
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		lastErr = fmt.Errorf("failed to create registration token for %s: status=%d body=%s", repo, resp.StatusCode, string(body))

		if !isRetryableError(resp, nil) {
			return nil, lastErr
		}
	}

	return nil, fmt.Errorf("exhausted retries: %w", lastErr)
}

// WorkflowJobInfo contains the status of a GitHub Actions workflow job.
type WorkflowJobInfo struct {
	Status     string // "queued", "in_progress", "completed"
	Conclusion string // "success", "failure", "cancelled", "timed_out", etc.
}

// GetWorkflowJobByID retrieves a workflow job by its ID from GitHub API.
// The repo parameter must be in "owner/repo" format.
func (c *GitHubClient) GetWorkflowJobByID(ctx context.Context, repo string, jobID int64) (*WorkflowJobInfo, error) {
	parts := strings.SplitN(repo, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return nil, fmt.Errorf("invalid repo format, expected owner/repo: %s", repo)
	}
	owner := parts[0]

	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			delay := retryDelay(attempt - 1)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}

		token, err := c.getInstallationToken(ctx, owner)
		if err != nil {
			lastErr = fmt.Errorf("failed to get installation token: %w", err)
			continue
		}

		client := github.NewClient(c.httpClient).WithAuthToken(token)
		if c.baseURL != "https://api.github.com" {
			u, parseErr := url.Parse(c.baseURL + "/")
			if parseErr != nil {
				return nil, fmt.Errorf("invalid base URL %q: %w", c.baseURL, parseErr)
			}
			client.BaseURL = u
		}

		job, resp, err := client.Actions.GetWorkflowJobByID(ctx, parts[0], parts[1], jobID)
		if err != nil {
			var httpResp *http.Response
			if resp != nil {
				httpResp = resp.Response
			}
			if httpResp != nil && httpResp.StatusCode == http.StatusNotFound {
				return nil, fmt.Errorf("job %d not found in %s", jobID, repo)
			}
			lastErr = fmt.Errorf("failed to get workflow job: %w", err)
			if !isRetryableError(httpResp, err) {
				return nil, lastErr
			}
			continue
		}

		return &WorkflowJobInfo{
			Status:     job.GetStatus(),
			Conclusion: job.GetConclusion(),
		}, nil
	}
	return nil, lastErr
}
