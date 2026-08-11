package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testPathJITConfigMyOrg   = "/repos/myorg/myrepo/actions/runners/generate-jitconfig"
	testPathRunnerGroupsRepo = "/repos/myorg/myrepo/actions/runner-groups"
	testEncodedJITConfig     = "eyJydW5uZXIiOiJqaXQifQ=="
)

// jitStubHandler serves the installation + access-token preamble every client
// call needs, then delegates to extra for the endpoint under test.
func jitStubHandler(t *testing.T, extra func(w http.ResponseWriter, r *http.Request) bool) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case testPathOrgInstallation:
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"id":      123,
				"account": map[string]interface{}{"type": "Organization"},
			})
			return
		case testPathAccessTokens123:
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"token":      "ghs_test_token",
				"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			})
			return
		}
		if extra != nil && extra(w, r) {
			return
		}
		t.Logf("unexpected request path: %s", r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

// expireRunnerGroupCache backdates every cached entry so the next lookup treats
// it as expired, without sleeping out the real TTL.
func expireRunnerGroupCache(c *Client) {
	c.runnerGroupMu.Lock()
	defer c.runnerGroupMu.Unlock()
	for k, entry := range c.runnerGroupCache {
		entry.expiresAt = time.Now().Add(-time.Second)
		c.runnerGroupCache[k] = entry
	}
}

func newJITTestClient(t *testing.T, h http.Handler) (*Client, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(h)
	t.Cleanup(server.Close)

	client, err := NewClient("12345", generateTestKey(t))
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}
	client.baseURL = server.URL
	return client, server
}

func TestClient_GenerateJITConfig_Success(t *testing.T) {
	var gotBody map[string]interface{}

	client, _ := newJITTestClient(t, jitStubHandler(t, func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != testPathJITConfigMyOrg {
			return false
		}
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("failed to decode request body: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"encoded_jit_config": testEncodedJITConfig,
			"runner":             map[string]interface{}{"id": 42, "name": "runner-a"},
		})
		return true
	}))

	req := JITConfigRequest{
		Name:          "runs-fleet-runner-cc-064922-80b53",
		RunnerGroupID: 1,
		Labels:        []string{"runs-fleet/arch=arm64/pool=cc"},
	}
	got, err := client.GenerateJITConfig(context.Background(), "myorg/myrepo", req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != testEncodedJITConfig {
		t.Errorf("encoded config = %q, want %q", got, testEncodedJITConfig)
	}

	if gotBody["name"] != req.Name {
		t.Errorf("body name = %v, want %q", gotBody["name"], req.Name)
	}
	// JSON numbers decode as float64.
	if gotBody["runner_group_id"] != float64(1) {
		t.Errorf("body runner_group_id = %v, want 1", gotBody["runner_group_id"])
	}
	labels, ok := gotBody["labels"].([]interface{})
	if !ok || len(labels) != 1 || labels[0] != req.Labels[0] {
		t.Errorf("body labels = %v, want [%q]", gotBody["labels"], req.Labels[0])
	}
}

// A JIT runner is bound to one job by GitHub only if the runner group is a real
// numeric ID. Sending 0 would be silently accepted as "no group" by some
// deployments, so the client must reject it before the call.
//
// The stub serves a VALID response for the endpoint, so each case can only fail
// via client-side validation. Without that, a missing guard would let the call
// through and return nil — which is exactly the regression being pinned.
func TestClient_GenerateJITConfig_RejectsInvalidInput(t *testing.T) {
	var reached int32

	client, _ := newJITTestClient(t, jitStubHandler(t, func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != testPathJITConfigMyOrg {
			return false
		}
		atomic.AddInt32(&reached, 1)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"encoded_jit_config": testEncodedJITConfig})
		return true
	}))

	valid := JITConfigRequest{Name: "r", RunnerGroupID: 1, Labels: []string{"l"}}

	tests := []struct {
		name string
		repo string
		req  JITConfigRequest
	}{
		{"empty repo", "", valid},
		{"repo without owner", "myrepo", valid},
		{"repo with empty owner", "/myrepo", valid},
		{"repo with empty name", "myorg/", valid},
		{"empty runner name", "myorg/myrepo", JITConfigRequest{RunnerGroupID: 1, Labels: []string{"l"}}},
		{"zero runner group", "myorg/myrepo", JITConfigRequest{Name: "r", Labels: []string{"l"}}},
		{"negative runner group", "myorg/myrepo", JITConfigRequest{Name: "r", RunnerGroupID: -1, Labels: []string{"l"}}},
		{"no labels", "myorg/myrepo", JITConfigRequest{Name: "r", RunnerGroupID: 1}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			atomic.StoreInt32(&reached, 0)
			if _, err := client.GenerateJITConfig(context.Background(), tt.repo, tt.req); err == nil {
				t.Error("expected error, got nil")
			}
			if got := atomic.LoadInt32(&reached); got != 0 {
				t.Errorf("endpoint reached %d times; invalid input must be rejected before the call", got)
			}
		})
	}
}

// A 403 without a rate-limit signal is a permission error: fail fast rather than
// burning the retry budget on a call that will never succeed.
func TestClient_GenerateJITConfig_PermissionErrorNotRetried(t *testing.T) {
	var calls int32

	client, _ := newJITTestClient(t, jitStubHandler(t, func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != testPathJITConfigMyOrg {
			return false
		}
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"message": "Resource not accessible by integration",
		})
		return true
	}))

	req := JITConfigRequest{Name: "r", RunnerGroupID: 1, Labels: []string{"l"}}
	_, err := client.GenerateJITConfig(context.Background(), "myorg/myrepo", req)
	if err == nil {
		t.Fatal("expected error for 403, got nil")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("endpoint called %d times, want 1 (permission errors must not retry)", got)
	}
}

func TestClient_GenerateJITConfig_RetriesOnServerError(t *testing.T) {
	oldDelay := baseRetryDelay
	baseRetryDelay = time.Millisecond
	t.Cleanup(func() { baseRetryDelay = oldDelay })

	var calls int32

	client, _ := newJITTestClient(t, jitStubHandler(t, func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != testPathJITConfigMyOrg {
			return false
		}
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return true
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"encoded_jit_config": testEncodedJITConfig,
		})
		return true
	}))

	req := JITConfigRequest{Name: "r", RunnerGroupID: 1, Labels: []string{"l"}}
	got, err := client.GenerateJITConfig(context.Background(), "myorg/myrepo", req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != testEncodedJITConfig {
		t.Errorf("encoded config = %q, want %q", got, testEncodedJITConfig)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("endpoint called %d times, want 2", got)
	}
}

// An empty encoded_jit_config is a successful HTTP call that yields an unusable
// runner. Treat it as an error so the caller falls back instead of booting a
// runner that can never register.
func TestClient_GenerateJITConfig_EmptyConfigIsError(t *testing.T) {
	client, _ := newJITTestClient(t, jitStubHandler(t, func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != testPathJITConfigMyOrg {
			return false
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"encoded_jit_config": ""})
		return true
	}))

	req := JITConfigRequest{Name: "r", RunnerGroupID: 1, Labels: []string{"l"}}
	if _, err := client.GenerateJITConfig(context.Background(), "myorg/myrepo", req); err == nil {
		t.Error("expected error for empty encoded_jit_config, got nil")
	}
}

func TestClient_ResolveRunnerGroupID_ByName(t *testing.T) {
	client, _ := newJITTestClient(t, jitStubHandler(t, func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != testPathRunnerGroupsRepo {
			return false
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"total_count": 2,
			"runner_groups": []map[string]interface{}{
				{"id": 1, "name": "Default"},
				{"id": 7, "name": "runs-fleet"},
			},
		})
		return true
	}))

	got, err := client.ResolveRunnerGroupID(context.Background(), "myorg/myrepo", "runs-fleet")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.ID != 7 {
		t.Errorf("group id = %d, want 7", got.ID)
	}
	if got.FallbackErr != nil {
		t.Errorf("FallbackErr = %v, want nil for a successful resolution", got.FallbackErr)
	}
}

// An empty group name means "caller expressed no preference" -> Default (1),
// without spending an API call.
func TestClient_ResolveRunnerGroupID_EmptyNameUsesDefaultWithoutAPICall(t *testing.T) {
	var calls int32

	client, _ := newJITTestClient(t, jitStubHandler(t, func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != testPathRunnerGroupsRepo {
			return false
		}
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"runner_groups": []map[string]interface{}{}})
		return true
	}))

	got, err := client.ResolveRunnerGroupID(context.Background(), "myorg/myrepo", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.ID != defaultRunnerGroupID {
		t.Errorf("group id = %d, want %d", got.ID, defaultRunnerGroupID)
	}
	// No name was asked for, so Default is the answer, not a substitution.
	if got.FallbackErr != nil {
		t.Errorf("FallbackErr = %v, want nil when no group name was requested", got.FallbackErr)
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Errorf("runner-groups called %d times, want 0", got)
	}
}

// A group lookup failure must never block a dispatch: fall back to Default
// rather than returning an error, since a job with no runner starves.
func TestClient_ResolveRunnerGroupID_FallsBackToDefaultOnFailure(t *testing.T) {
	oldDelay := baseRetryDelay
	baseRetryDelay = time.Millisecond
	t.Cleanup(func() { baseRetryDelay = oldDelay })

	tests := []struct {
		name   string
		status int
		body   interface{}
	}{
		{"permission error", http.StatusForbidden, map[string]string{"message": "no access"}},
		{"not found", http.StatusNotFound, map[string]string{"message": "nope"}},
		{"name absent from list", http.StatusOK, map[string]interface{}{
			"runner_groups": []map[string]interface{}{{"id": 1, "name": "Default"}},
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, _ := newJITTestClient(t, jitStubHandler(t, func(w http.ResponseWriter, r *http.Request) bool {
				if r.URL.Path != testPathRunnerGroupsRepo {
					return false
				}
				w.WriteHeader(tt.status)
				_ = json.NewEncoder(w).Encode(tt.body)
				return true
			}))

			got, err := client.ResolveRunnerGroupID(context.Background(), "myorg/myrepo", "missing-group")
			if err != nil {
				t.Fatalf("expected fallback, got error: %v", err)
			}
			if got.ID != defaultRunnerGroupID {
				t.Errorf("group id = %d, want %d", got.ID, defaultRunnerGroupID)
			}
			// The substitution must stay visible to the caller, since this
			// package cannot log it.
			if got.FallbackErr == nil {
				t.Error("FallbackErr = nil, want the reason Default was substituted")
			}
		})
	}
}

// Retry-then-exhaust must still land on the fallback: the composition between
// fetchRunnerGroupID's retry loop and the fallback is what a caller relies on.
func TestClient_ResolveRunnerGroupID_FallsBackAfterRetriesExhausted(t *testing.T) {
	oldDelay := baseRetryDelay
	baseRetryDelay = time.Millisecond
	t.Cleanup(func() { baseRetryDelay = oldDelay })

	var calls int32

	client, _ := newJITTestClient(t, jitStubHandler(t, func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != testPathRunnerGroupsRepo {
			return false
		}
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
		return true
	}))

	got, err := client.ResolveRunnerGroupID(context.Background(), "myorg/myrepo", "runs-fleet")
	if err != nil {
		t.Fatalf("expected fallback, got error: %v", err)
	}
	if got.ID != defaultRunnerGroupID {
		t.Errorf("group id = %d, want %d", got.ID, defaultRunnerGroupID)
	}
	if got.FallbackErr == nil {
		t.Error("FallbackErr = nil, want the reason Default was substituted")
	}
	if n := atomic.LoadInt32(&calls); n != maxRetries+1 {
		t.Errorf("runner-groups called %d times, want %d", n, maxRetries+1)
	}
}

// A group present in the list but carrying a nonsense ID must not be used: 0
// would be sent to generate-jitconfig as a valid-looking JSON zero.
func TestClient_ResolveRunnerGroupID_RejectsNonPositiveID(t *testing.T) {
	oldDelay := baseRetryDelay
	baseRetryDelay = time.Millisecond
	t.Cleanup(func() { baseRetryDelay = oldDelay })

	for _, id := range []int{0, -3} {
		t.Run(fmt.Sprintf("id_%d", id), func(t *testing.T) {
			client, _ := newJITTestClient(t, jitStubHandler(t, func(w http.ResponseWriter, r *http.Request) bool {
				if r.URL.Path != testPathRunnerGroupsRepo {
					return false
				}
				w.WriteHeader(http.StatusOK)
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"runner_groups": []map[string]interface{}{{"id": id, "name": "runs-fleet"}},
				})
				return true
			}))

			got, err := client.ResolveRunnerGroupID(context.Background(), "myorg/myrepo", "runs-fleet")
			if err != nil {
				t.Fatalf("expected fallback, got error: %v", err)
			}
			if got.ID != defaultRunnerGroupID {
				t.Errorf("group id = %d, want %d", got.ID, defaultRunnerGroupID)
			}
			if got.FallbackErr == nil {
				t.Error("FallbackErr = nil, want a reason for the non-positive group id")
			}
		})
	}
}

// A permanently unresolvable group must not re-query GitHub on every dispatch.
func TestClient_ResolveRunnerGroupID_CachesTheFallback(t *testing.T) {
	var calls int32

	client, _ := newJITTestClient(t, jitStubHandler(t, func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != testPathRunnerGroupsRepo {
			return false
		}
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"runner_groups": []map[string]interface{}{{"id": 1, "name": "Default"}},
		})
		return true
	}))

	for i := 0; i < 3; i++ {
		got, err := client.ResolveRunnerGroupID(context.Background(), "myorg/myrepo", "missing-group")
		if err != nil {
			t.Fatalf("call %d: unexpected error: %v", i, err)
		}
		if got.ID != defaultRunnerGroupID {
			t.Fatalf("call %d: group id = %d, want %d", i, got.ID, defaultRunnerGroupID)
		}
	}

	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Errorf("runner-groups called %d times, want 1 (fallback must be cached)", n)
	}
}

// A cached fallback must expire far sooner than a cached success: caching a
// success is nearly free, but caching a failure misroutes every dispatch for that
// repo into Default until it expires. A transient blip must not cost half an hour.
func TestClient_ResolveRunnerGroupID_FallbackExpiresSoonerThanSuccess(t *testing.T) {
	if runnerGroupFallbackTTL >= runnerGroupCacheTTL {
		t.Fatalf("fallback TTL %v must be shorter than success TTL %v",
			runnerGroupFallbackTTL, runnerGroupCacheTTL)
	}

	tests := []struct {
		name      string
		groupName string
		wantTTL   time.Duration
	}{
		{"resolved group keeps the long TTL", "runs-fleet", runnerGroupCacheTTL},
		{"substituted default gets the short TTL", "missing-group", runnerGroupFallbackTTL},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, _ := newJITTestClient(t, jitStubHandler(t, func(w http.ResponseWriter, r *http.Request) bool {
				if r.URL.Path != testPathRunnerGroupsRepo {
					return false
				}
				w.WriteHeader(http.StatusOK)
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"runner_groups": []map[string]interface{}{{"id": 7, "name": "runs-fleet"}},
				})
				return true
			}))

			before := time.Now()
			if _, err := client.ResolveRunnerGroupID(context.Background(), "myorg/myrepo", tt.groupName); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			client.runnerGroupMu.Lock()
			entry, ok := client.runnerGroupCache[runnerGroupCacheKey("myorg/myrepo", tt.groupName)]
			client.runnerGroupMu.Unlock()
			if !ok {
				t.Fatal("expected a cached entry")
			}

			// expiresAt is stamped after `before`, so the observed span is the TTL
			// plus a little elapsed time. Only the choice of TTL constant is under
			// test, not wall-clock precision.
			gotTTL := entry.expiresAt.Sub(before)
			if gotTTL < tt.wantTTL || gotTTL > tt.wantTTL+30*time.Second {
				t.Errorf("cached TTL = %v, want ~%v", gotTTL, tt.wantTTL)
			}
		})
	}
}

// An expired entry must be re-queried, so a renamed or recreated group recovers
// instead of being pinned for the life of the process.
func TestClient_ResolveRunnerGroupID_RefetchesAfterTTL(t *testing.T) {
	var calls int32

	client, _ := newJITTestClient(t, jitStubHandler(t, func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != testPathRunnerGroupsRepo {
			return false
		}
		n := atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusOK)
		id := 7
		if n > 1 {
			id = 9
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"runner_groups": []map[string]interface{}{{"id": id, "name": "runs-fleet"}},
		})
		return true
	}))

	first, err := client.ResolveRunnerGroupID(context.Background(), "myorg/myrepo", "runs-fleet")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if first.ID != 7 {
		t.Fatalf("first group id = %d, want 7", first.ID)
	}

	expireRunnerGroupCache(client)

	second, err := client.ResolveRunnerGroupID(context.Background(), "myorg/myrepo", "runs-fleet")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if second.ID != 9 {
		t.Errorf("second group id = %d, want 9 (expired entry must be re-fetched)", second.ID)
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Errorf("runner-groups called %d times, want 2", n)
	}
}

func TestClient_ResolveRunnerGroupID_CachesLookup(t *testing.T) {
	var calls int32

	client, _ := newJITTestClient(t, jitStubHandler(t, func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != testPathRunnerGroupsRepo {
			return false
		}
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"runner_groups": []map[string]interface{}{{"id": 7, "name": "runs-fleet"}},
		})
		return true
	}))

	for i := 0; i < 3; i++ {
		got, err := client.ResolveRunnerGroupID(context.Background(), "myorg/myrepo", "runs-fleet")
		if err != nil {
			t.Fatalf("call %d: unexpected error: %v", i, err)
		}
		if got.ID != 7 {
			t.Fatalf("call %d: group id = %d, want 7", i, got.ID)
		}
	}

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("runner-groups called %d times, want 1 (result must be cached)", got)
	}
}

// The JIT config is a credential: it must never reach logs or error strings.
func TestClient_GenerateJITConfig_ErrorsOmitConfig(t *testing.T) {
	secret := "SUPERSECRETJITBLOB"

	client, _ := newJITTestClient(t, jitStubHandler(t, func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != testPathJITConfigMyOrg {
			return false
		}
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"message":            "bad request",
			"encoded_jit_config": secret,
		})
		return true
	}))

	req := JITConfigRequest{Name: "r", RunnerGroupID: 1, Labels: []string{"l"}}
	_, err := client.GenerateJITConfig(context.Background(), "myorg/myrepo", req)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("error leaks the JIT config: %v", err)
	}
}

func TestClient_GenerateJITConfig_ContextCancelled(t *testing.T) {
	client, _ := newJITTestClient(t, jitStubHandler(t, func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != testPathJITConfigMyOrg {
			return false
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"encoded_jit_config": testEncodedJITConfig})
		return true
	}))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	req := JITConfigRequest{Name: "r", RunnerGroupID: 1, Labels: []string{"l"}}
	if _, err := client.GenerateJITConfig(ctx, "myorg/myrepo", req); err == nil {
		t.Error("expected error for cancelled context, got nil")
	}
}
