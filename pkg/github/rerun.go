package github

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/google/go-github/v57/github"
)

// RerunJob asks GitHub to re-run a single job and its dependent jobs.
//
// A spot reclaim kills the runner mid-job, and GitHub concludes the job failed
// before any replacement instance can register — a re-queued runner is always
// too late, because registration binds a runner to a label, not to a job, so
// GitHub never hands the dead job to it. Re-running is the only way to recover
// the work.
//
// This deliberately targets one job rather than the run's rerun-failed-jobs
// endpoint: that one would also re-run jobs that failed for real, spending
// capacity to reproduce genuine failures. Dependent jobs still come along,
// which is what recovers the gate jobs a reclaim cascades into.
func (c *Client) RerunJob(ctx context.Context, repo string, jobID int64) error {
	owner, repoName, err := splitRepo(repo)
	if err != nil {
		return err
	}

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

		ghClient := github.NewClient(&http.Client{Timeout: c.httpClient.Timeout}).WithAuthToken(token)
		if c.baseURL != defaultBaseURL {
			u, parseErr := url.Parse(c.baseURL + "/")
			if parseErr != nil {
				return fmt.Errorf("invalid base URL %q: %w", c.baseURL, parseErr)
			}
			ghClient.BaseURL = u
		}

		resp, err := ghClient.Actions.RerunJobByID(ctx, owner, repoName, jobID)
		if err == nil {
			return nil
		}

		var httpResp *http.Response
		if resp != nil {
			httpResp = resp.Response
		}
		// The error body can echo the request; reduce it to a status so an
		// installation token can never reach a log.
		if httpResp != nil {
			lastErr = fmt.Errorf("rerun job %d in %s: HTTP %d", jobID, repo, httpResp.StatusCode)
		} else {
			lastErr = fmt.Errorf("rerun job %d in %s failed", jobID, repo)
		}
		if !isRetryableError(httpResp, err) {
			return lastErr
		}
		nextDelay = backoffDelay(httpResp, attempt)
	}
	return lastErr
}
