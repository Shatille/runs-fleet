package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// defaultRunnerGroupID is GitHub's built-in "Default" runner group. Every repo
// has it, so it is the safe fallback when a named group cannot be resolved: a
// dispatch that cannot find its group must still produce a runner, since a job
// with no runner starves.
const defaultRunnerGroupID = 1

// runnerGroupCacheTTL bounds how long a resolved name->ID mapping is reused.
// Group IDs are stable in practice, so this is not a freshness requirement — it
// is the recovery path for a group that was renamed or recreated, which would
// otherwise be pinned for the life of the process.
const runnerGroupCacheTTL = 30 * time.Minute

// runnerGroupFallbackTTL bounds a cached Default substitution. Far shorter than
// runnerGroupCacheTTL because the costs are asymmetric: caching a success is
// nearly free, but caching a failure misroutes every dispatch for that repo into
// Default until it expires. A transient outage that outlasts the retry budget
// must not cost more than a minute of misplacement, and a genuinely misconfigured
// group name gains nothing from a long TTL — it just keeps failing, at the price
// of one cheap idempotent GET per minute.
const runnerGroupFallbackTTL = time.Minute

// runnerGroupEntry is a cached name->ID mapping with its expiry.
type runnerGroupEntry struct {
	id        int64
	expiresAt time.Time
}

// JITConfigRequest describes the runner to mint a just-in-time config for.
//
// A JIT config makes the runner ephemeral — it runs exactly one job and GitHub
// deregisters it — but it does NOT bind the runner to a specific job: the API
// takes only name, group, and labels, and GitHub's scheduler hands the runner
// whichever queued job matches those labels. When several jobs share a label
// set, runners still serve each other's jobs; the termination handler's
// still-queued redispatch and the stale-jobs sweep are what make the fleet
// converge afterward.
type JITConfigRequest struct {
	Name          string
	RunnerGroupID int64
	Labels        []string
	WorkFolder    string
}

// GenerateJITConfig mints a just-in-time runner configuration for repo and
// returns the opaque encoded config the agent hands to run.sh.
//
// The returned value is a credential that registers a runner; it is never
// logged, and API error bodies are reduced to their status so a config echoed
// back by GitHub cannot leak through an error string.
func (c *Client) GenerateJITConfig(ctx context.Context, repo string, req JITConfigRequest) (string, error) {
	owner, _, err := splitRepo(repo)
	if err != nil {
		return "", err
	}
	if req.Name == "" {
		return "", fmt.Errorf("runner name is required")
	}
	// GitHub requires a real group; 0 would be sent as a valid-looking JSON zero.
	if req.RunnerGroupID <= 0 {
		return "", fmt.Errorf("runner group id is required (got %d)", req.RunnerGroupID)
	}
	if len(req.Labels) == 0 {
		return "", fmt.Errorf("at least one label is required")
	}

	body := map[string]interface{}{
		"name":            req.Name,
		"runner_group_id": req.RunnerGroupID,
		"labels":          req.Labels,
	}
	if req.WorkFolder != "" {
		body["work_folder"] = req.WorkFolder
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("failed to marshal jitconfig request: %w", err)
	}

	url := fmt.Sprintf("%s/repos/%s/actions/runners/generate-jitconfig", c.baseURL, repo)

	var lastErr error
	var nextDelay time.Duration
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(nextDelay):
			}
		}

		token, err := c.getInstallationToken(ctx, owner)
		if err != nil {
			lastErr = fmt.Errorf("failed to get installation token: %w", err)
			nextDelay = retryDelay(attempt)
			continue
		}

		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
		if err != nil {
			return "", fmt.Errorf("failed to create request: %w", err)
		}
		httpReq.Header.Set("Authorization", "token "+token)
		httpReq.Header.Set("Accept", "application/vnd.github+json")
		httpReq.Header.Set("Content-Type", "application/json")

		encoded, attemptErr := c.doJITConfigRequest(httpReq, attempt)
		if attemptErr == nil {
			return encoded, nil
		}
		lastErr = attemptErr.err
		if !attemptErr.retryable {
			return "", lastErr
		}
		nextDelay = attemptErr.delay
	}

	return "", lastErr
}

// jitAttemptError carries one failed attempt's outcome: whether retrying could
// plausibly help, and how long to wait (honouring GitHub's Retry-After when the
// response supplied one).
type jitAttemptError struct {
	err       error
	retryable bool
	delay     time.Duration
}

// doJITConfigRequest performs one jitconfig attempt and always closes the
// response body before returning, so no path can leak it.
//
// Non-2xx responses yield a status-only error: the body may echo the config
// back, and that must not reach an error string.
func (c *Client) doJITConfigRequest(req *http.Request, attempt int) (string, *jitAttemptError) {
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", &jitAttemptError{
			err:       fmt.Errorf("failed to execute request: %w", err),
			retryable: true,
			delay:     retryDelay(attempt),
		}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", &jitAttemptError{
			err:       fmt.Errorf("jitconfig request failed with status %d", resp.StatusCode),
			retryable: isRetryableError(resp, nil),
			delay:     backoffDelay(resp, attempt),
		}
	}

	var result struct {
		EncodedJITConfig string `json:"encoded_jit_config"`
	}
	if decodeErr := json.NewDecoder(resp.Body).Decode(&result); decodeErr != nil {
		return "", &jitAttemptError{
			err:       fmt.Errorf("failed to decode jitconfig response: %w", decodeErr),
			retryable: true,
			delay:     retryDelay(attempt),
		}
	}
	// A 2xx with no config would boot a runner that can never register.
	if result.EncodedJITConfig == "" {
		return "", &jitAttemptError{err: fmt.Errorf("jitconfig response contained no config")}
	}
	return result.EncodedJITConfig, nil
}

// RunnerGroupResolution reports how a runner group name was resolved. A non-nil
// FallbackErr means the named group could not be resolved and Default was
// substituted, and carries why. Callers that log or emit metrics should surface
// that — silently placing runners in the wrong group is a misconfiguration worth
// seeing, even though it is not worth failing a dispatch over.
type RunnerGroupResolution struct {
	ID          int64
	FallbackErr error
}

// ResolveRunnerGroupID maps a runner group name to the numeric ID that
// generate-jitconfig requires. An empty name resolves to Default without an API
// call.
//
// Any failure — no access to the runner-groups endpoint, or a name absent from
// the list — resolves to Default rather than returning an error: the group is a
// placement preference, and losing it must not cost the job its runner. The
// substitution is reported via the result so it stays observable; this package
// does no logging of its own.
func (c *Client) ResolveRunnerGroupID(ctx context.Context, repo, groupName string) (*RunnerGroupResolution, error) {
	if groupName == "" {
		return &RunnerGroupResolution{ID: defaultRunnerGroupID}, nil
	}

	owner, _, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}

	if id, ok := c.cachedRunnerGroupID(repo, groupName); ok {
		return &RunnerGroupResolution{ID: id}, nil
	}

	id, err := c.fetchRunnerGroupID(ctx, owner, repo, groupName)
	if err != nil {
		// Cached as well as returned: a repo whose group is permanently
		// unresolvable would otherwise re-query GitHub on every single dispatch.
		c.storeRunnerGroupIDFor(repo, groupName, defaultRunnerGroupID, runnerGroupFallbackTTL)
		return &RunnerGroupResolution{ID: defaultRunnerGroupID, FallbackErr: err}, nil
	}

	c.storeRunnerGroupID(repo, groupName, id)
	return &RunnerGroupResolution{ID: id}, nil
}

func (c *Client) fetchRunnerGroupID(ctx context.Context, owner, repo, groupName string) (int64, error) {
	url := fmt.Sprintf("%s/repos/%s/actions/runner-groups", c.baseURL, repo)

	var lastErr error
	var nextDelay time.Duration
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return 0, ctx.Err()
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
			return 0, fmt.Errorf("failed to create request: %w", err)
		}
		req.Header.Set("Authorization", "token "+token)
		req.Header.Set("Accept", "application/vnd.github+json")

		id, attemptErr := c.doRunnerGroupRequest(req, groupName, attempt)
		if attemptErr == nil {
			return id, nil
		}
		lastErr = attemptErr.err
		if !attemptErr.retryable {
			return 0, lastErr
		}
		nextDelay = attemptErr.delay
	}

	return 0, lastErr
}

// doRunnerGroupRequest performs one runner-groups lookup, always closing the
// response body, and returns the ID of the group named groupName.
func (c *Client) doRunnerGroupRequest(req *http.Request, groupName string, attempt int) (int64, *jitAttemptError) {
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, &jitAttemptError{
			err:       fmt.Errorf("failed to execute request: %w", err),
			retryable: true,
			delay:     retryDelay(attempt),
		}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, &jitAttemptError{
			err:       fmt.Errorf("runner-groups request failed with status %d", resp.StatusCode),
			retryable: isRetryableError(resp, nil),
			delay:     backoffDelay(resp, attempt),
		}
	}

	var result struct {
		RunnerGroups []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"runner_groups"`
	}
	if decodeErr := json.NewDecoder(resp.Body).Decode(&result); decodeErr != nil {
		return 0, &jitAttemptError{
			err:       fmt.Errorf("failed to decode runner-groups response: %w", decodeErr),
			retryable: true,
			delay:     retryDelay(attempt),
		}
	}

	for _, g := range result.RunnerGroups {
		if strings.EqualFold(g.Name, groupName) {
			if g.ID <= 0 {
				return 0, &jitAttemptError{err: fmt.Errorf("runner group %q has invalid id %d", groupName, g.ID)}
			}
			return g.ID, nil
		}
	}
	return 0, &jitAttemptError{err: fmt.Errorf("runner group %q not found", groupName)}
}

func runnerGroupCacheKey(repo, groupName string) string {
	return repo + "\x00" + groupName
}

func (c *Client) cachedRunnerGroupID(repo, groupName string) (int64, bool) {
	c.runnerGroupMu.Lock()
	defer c.runnerGroupMu.Unlock()
	entry, ok := c.runnerGroupCache[runnerGroupCacheKey(repo, groupName)]
	if !ok || time.Now().After(entry.expiresAt) {
		return 0, false
	}
	return entry.id, true
}

func (c *Client) storeRunnerGroupID(repo, groupName string, id int64) {
	c.storeRunnerGroupIDFor(repo, groupName, id, runnerGroupCacheTTL)
}

func (c *Client) storeRunnerGroupIDFor(repo, groupName string, id int64, ttl time.Duration) {
	c.runnerGroupMu.Lock()
	defer c.runnerGroupMu.Unlock()
	if c.runnerGroupCache == nil {
		c.runnerGroupCache = make(map[string]runnerGroupEntry)
	}
	c.runnerGroupCache[runnerGroupCacheKey(repo, groupName)] = runnerGroupEntry{
		id:        id,
		expiresAt: time.Now().Add(ttl),
	}
}
