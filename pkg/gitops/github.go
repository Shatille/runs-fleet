// Package gitops provides GitHub API integration for configuration management.
package gitops

import (
	"context"
	"fmt"
	"io"
	"net/http"

	"github.com/google/go-github/v57/github"
)

// GitHubClient wraps the go-github client for configuration operations.
type GitHubClient struct {
	client *github.Client
}

// NewGitHubClient creates a new GitHub client for configuration operations.
func NewGitHubClient(client *github.Client) *GitHubClient {
	return &GitHubClient{client: client}
}

// GetFileContent fetches file content from a GitHub repository.
func (c *GitHubClient) GetFileContent(ctx context.Context, owner, repo, path, ref string) ([]byte, error) {
	opts := &github.RepositoryContentGetOptions{}
	if ref != "" {
		opts.Ref = ref
	}

	fileContent, _, resp, err := c.client.Repositories.GetContents(ctx, owner, repo, path, opts)
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return nil, fmt.Errorf("file not found: %s", path)
		}
		return nil, fmt.Errorf("failed to get file content: %w", err)
	}

	if fileContent == nil {
		return nil, fmt.Errorf("file content is nil for %s", path)
	}

	content, err := fileContent.GetContent()
	if err != nil {
		return nil, fmt.Errorf("failed to decode file content: %w", err)
	}

	return []byte(content), nil
}

// PushEvent represents a GitHub push webhook event.
type PushEvent struct {
	Ref        string   `json:"ref"`
	Repository RepoInfo `json:"repository"`
	Commits    []Commit `json:"commits"`
}

// RepoInfo contains repository information from webhook.
type RepoInfo struct {
	Owner    OwnerInfo `json:"owner"`
	Name     string    `json:"name"`
	FullName string    `json:"full_name"`
}

// OwnerInfo contains owner information from webhook.
type OwnerInfo struct {
	Login string `json:"login"`
}

// Commit represents a commit from a push event.
type Commit struct {
	ID       string   `json:"id"`
	Added    []string `json:"added"`
	Modified []string `json:"modified"`
	Removed  []string `json:"removed"`
}

// GetModifiedFiles returns all files that were added, modified, or removed in the push.
func (e *PushEvent) GetModifiedFiles() []string {
	seen := make(map[string]bool)
	var files []string

	for _, commit := range e.Commits {
		for _, f := range commit.Added {
			if !seen[f] {
				seen[f] = true
				files = append(files, f)
			}
		}
		for _, f := range commit.Modified {
			if !seen[f] {
				seen[f] = true
				files = append(files, f)
			}
		}
		for _, f := range commit.Removed {
			if !seen[f] {
				seen[f] = true
				files = append(files, f)
			}
		}
	}

	return files
}

// GetBranch extracts the branch name from the ref.
func (e *PushEvent) GetBranch() string {
	const prefix = "refs/heads/"
	if len(e.Ref) > len(prefix) && e.Ref[:len(prefix)] == prefix {
		return e.Ref[len(prefix):]
	}
	return e.Ref
}

// HTTPClientGitHub provides a simple HTTP-based GitHub client for file fetching.
// This is an alternative when the go-github client is not available.
type HTTPClientGitHub struct {
	httpClient *http.Client
	token      string
	baseURL    string
}

// NewHTTPClientGitHub creates a new HTTP-based GitHub client.
func NewHTTPClientGitHub(token string) *HTTPClientGitHub {
	return &HTTPClientGitHub{
		httpClient: &http.Client{},
		token:      token,
		baseURL:    "https://api.github.com",
	}
}

// GetFileContent fetches file content from GitHub using the raw API.
func (c *HTTPClientGitHub) GetFileContent(ctx context.Context, owner, repo, path, ref string) ([]byte, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/contents/%s", c.baseURL, owner, repo, path)
	if ref != "" {
		url += "?ref=" + ref
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Accept", "application/vnd.github.raw+json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch file: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("file not found: %s", path)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	return body, nil
}
