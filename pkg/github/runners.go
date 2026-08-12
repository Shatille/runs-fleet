package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// runnersPageSize is GitHub's maximum page size for the runners endpoint.
const runnersPageSize = 100

// runnerListPageCap bounds how many pages one listing walks. A repo that has
// accumulated more registrations than this still gets swept, just across
// several cycles, rather than the sweep spending its whole budget in one repo.
const runnerListPageCap = 20

// Runner is a self-hosted runner registration as GitHub reports it.
type Runner struct {
	ID     int64
	Name   string
	Status string // "online" or "offline"
	Busy   bool
}

// ListRunners returns every self-hosted runner registered to repo, following
// pagination up to runnerListPageCap pages.
func (c *Client) ListRunners(ctx context.Context, repo string) ([]Runner, error) {
	owner, _, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}

	var all []Runner
	for page := 1; page <= runnerListPageCap; page++ {
		url := fmt.Sprintf("%s/repos/%s/actions/runners?per_page=%d&page=%d",
			c.baseURL, repo, runnersPageSize, page)

		batch, err := c.listRunnersPage(ctx, owner, url)
		if err != nil {
			return nil, err
		}
		all = append(all, batch...)
		if len(batch) < runnersPageSize {
			break
		}
	}
	return all, nil
}

func (c *Client) listRunnersPage(ctx context.Context, owner, url string) ([]Runner, error) {
	var lastErr error
	var nextDelay time.Duration
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(nextDelay):
			}
		}

		token, err := c.getInstallationToken(ctx, owner)
		if err != nil {
			lastErr = fmt.Errorf("failed to get installation token: %w", err)
			nextDelay = retryDelay(attempt)
			continue
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, fmt.Errorf("failed to create request: %w", err)
		}
		req.Header.Set("Authorization", "token "+token)
		req.Header.Set("Accept", "application/vnd.github+json")

		resp, err := c.httpClient.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("failed to execute request: %w", err)
			nextDelay = retryDelay(attempt)
			continue
		}

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			var result struct {
				Runners []struct {
					ID     int64  `json:"id"`
					Name   string `json:"name"`
					Status string `json:"status"`
					Busy   bool   `json:"busy"`
				} `json:"runners"`
			}
			decodeErr := json.NewDecoder(resp.Body).Decode(&result)
			_ = resp.Body.Close()
			if decodeErr != nil {
				return nil, fmt.Errorf("failed to decode runners response: %w", decodeErr)
			}
			out := make([]Runner, 0, len(result.Runners))
			for _, r := range result.Runners {
				out = append(out, Runner{ID: r.ID, Name: r.Name, Status: r.Status, Busy: r.Busy})
			}
			return out, nil
		}

		retryable := isRetryableError(resp, nil)
		nextDelay = backoffDelay(resp, attempt)
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		lastErr = fmt.Errorf("list runners failed: status=%d body=%s", resp.StatusCode, string(body))
		if !retryable {
			return nil, lastErr
		}
	}
	return nil, fmt.Errorf("exhausted retries: %w", lastErr)
}

// DeleteRunner removes a self-hosted runner registration from repo.
//
// A 404 is success: the registration is gone, which is all the caller wanted. A
// runner that picked up work between a listing and this call is deleted by
// GitHub itself once that job ends, so the race resolves to the same state.
func (c *Client) DeleteRunner(ctx context.Context, repo string, runnerID int64) error {
	owner, _, err := splitRepo(repo)
	if err != nil {
		return err
	}

	url := fmt.Sprintf("%s/repos/%s/actions/runners/%d", c.baseURL, repo, runnerID)

	var lastErr error
	var nextDelay time.Duration
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(nextDelay):
			}
		}

		token, err := c.getInstallationToken(ctx, owner)
		if err != nil {
			lastErr = fmt.Errorf("failed to get installation token: %w", err)
			nextDelay = retryDelay(attempt)
			continue
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
		if err != nil {
			return fmt.Errorf("failed to create request: %w", err)
		}
		req.Header.Set("Authorization", "token "+token)
		req.Header.Set("Accept", "application/vnd.github+json")

		resp, err := c.httpClient.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("failed to execute request: %w", err)
			nextDelay = retryDelay(attempt)
			continue
		}

		if (resp.StatusCode >= 200 && resp.StatusCode < 300) || resp.StatusCode == http.StatusNotFound {
			_ = resp.Body.Close()
			return nil
		}

		retryable := isRetryableError(resp, nil)
		nextDelay = backoffDelay(resp, attempt)
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		lastErr = fmt.Errorf("delete runner %d failed: status=%d body=%s", runnerID, resp.StatusCode, string(body))
		if !retryable {
			return lastErr
		}
	}
	return fmt.Errorf("exhausted retries: %w", lastErr)
}
