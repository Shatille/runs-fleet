package runner

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/Shavakan/runs-fleet/pkg/github"
	"github.com/Shavakan/runs-fleet/pkg/logging"
)

// testRegToken is the registration token the fallback path is expected to store.
const testRegToken = "tok"

// mockJITGitHubClient satisfies both registrationTokenGetter and the JIT
// capability, so PrepareRunner takes the JIT path.
type mockJITGitHubClient struct {
	mockGitHubClient

	jitConfig     string
	jitErr        error
	groupID       int64
	groupErr      error
	groupFellBack error

	jitCalls        int
	lastJITRequest  github.JITConfigRequest
	resolveCalls    int
	lastResolveName string
}

func (m *mockJITGitHubClient) GenerateJITConfig(_ context.Context, _ string, req github.JITConfigRequest) (string, error) {
	m.jitCalls++
	m.lastJITRequest = req
	if m.jitErr != nil {
		return "", m.jitErr
	}
	return m.jitConfig, nil
}

func (m *mockJITGitHubClient) ResolveRunnerGroupID(_ context.Context, _, groupName string) (*github.RunnerGroupResolution, error) {
	m.resolveCalls++
	m.lastResolveName = groupName
	if m.groupErr != nil {
		return nil, m.groupErr
	}
	id := m.groupID
	if id == 0 {
		id = 1
	}
	return &github.RunnerGroupResolution{ID: id, FallbackErr: m.groupFellBack}, nil
}

func newJITManager(t *testing.T, gh registrationTokenGetter, cfg ManagerConfig) (*Manager, *mockSecretsStore) {
	t.Helper()
	store := &mockSecretsStore{}
	return NewManager(gh, store, cfg), store
}

func validPrepareRequest() PrepareRunnerRequest {
	return PrepareRunnerRequest{
		InstanceID: "i-0abc",
		JobID:      "93368064922",
		RunID:      "31360419439",
		Repo:       "devsisters/cc-data",
		Labels:     []string{"runs-fleet/arch=arm64/pool=cc"},
		Pool:       "cc",
	}
}

// The whole point of the change: when the client can mint a JIT config, the
// stored config carries it, so the agent registers a job-bound runner.
func TestPrepareRunner_StoresJITConfigWhenAvailable(t *testing.T) {
	gh := &mockJITGitHubClient{jitConfig: "ENCODEDJIT"}
	gh.regToken = testRegToken

	manager, store := newJITManager(t, gh, ManagerConfig{})
	if err := manager.PrepareRunner(context.Background(), validPrepareRequest()); err != nil {
		t.Fatalf("PrepareRunner() error = %v", err)
	}

	if store.lastPutCfg == nil {
		t.Fatal("no config stored")
	}
	if store.lastPutCfg.JITConfig != "ENCODEDJIT" {
		t.Errorf("JITConfig = %q, want %q", store.lastPutCfg.JITConfig, "ENCODEDJIT")
	}
	if gh.jitCalls != 1 {
		t.Errorf("GenerateJITConfig called %d times, want 1", gh.jitCalls)
	}

	// The runner name and labels must match what token registration would have
	// used, or the JIT path silently changes runner identity.
	if gh.lastJITRequest.Name != store.lastPutCfg.RunnerName {
		t.Errorf("JIT request name = %q, want %q", gh.lastJITRequest.Name, store.lastPutCfg.RunnerName)
	}
	if len(gh.lastJITRequest.Labels) != 1 || gh.lastJITRequest.Labels[0] != validPrepareRequest().Labels[0] {
		t.Errorf("JIT request labels = %v, want %v", gh.lastJITRequest.Labels, validPrepareRequest().Labels)
	}
	if gh.lastJITRequest.RunnerGroupID <= 0 {
		t.Errorf("JIT request runner group = %d, want > 0", gh.lastJITRequest.RunnerGroupID)
	}
}

// A client without the JIT capability must keep working exactly as before, so
// this can ship before every provider supports it.
func TestPrepareRunner_FallsBackToTokenWhenClientLacksJIT(t *testing.T) {
	gh := &mockGitHubClient{regToken: testRegToken}

	manager, store := newJITManager(t, gh, ManagerConfig{})
	if err := manager.PrepareRunner(context.Background(), validPrepareRequest()); err != nil {
		t.Fatalf("PrepareRunner() error = %v", err)
	}

	if store.lastPutCfg.JITConfig != "" {
		t.Errorf("JITConfig = %q, want empty for a non-JIT client", store.lastPutCfg.JITConfig)
	}
	if store.lastPutCfg.RegistrationToken != testRegToken {
		t.Errorf("RegistrationToken = %q, want %q", store.lastPutCfg.RegistrationToken, testRegToken)
	}
}

// A JIT mint failure must not cost the job its runner: fall back to the token
// path, which still produces a working (if steal-able) runner.
func TestPrepareRunner_FallsBackToTokenWhenJITFails(t *testing.T) {
	gh := &mockJITGitHubClient{jitErr: errors.New("jitconfig boom")}
	gh.regToken = testRegToken

	manager, store := newJITManager(t, gh, ManagerConfig{})
	if err := manager.PrepareRunner(context.Background(), validPrepareRequest()); err != nil {
		t.Fatalf("PrepareRunner() error = %v", err)
	}

	if store.lastPutCfg.JITConfig != "" {
		t.Errorf("JITConfig = %q, want empty after a mint failure", store.lastPutCfg.JITConfig)
	}
	if store.lastPutCfg.RegistrationToken != testRegToken {
		t.Errorf("RegistrationToken = %q, want the token fallback", store.lastPutCfg.RegistrationToken)
	}
}

// Step 1 deliberately surfaces a Default-group substitution instead of logging
// it, because pkg/github does no logging. If this layer does not log it, a
// misconfigured group places every runner in Default silently.
func TestPrepareRunner_LogsRunnerGroupFallback(t *testing.T) {
	var buf bytes.Buffer
	restore := captureRunnerLog(&buf)
	defer restore()

	gh := &mockJITGitHubClient{
		jitConfig:     "ENCODEDJIT",
		groupFellBack: errors.New("runner group \"runs-fleet\" not found"),
	}
	gh.regToken = testRegToken

	manager, _ := newJITManager(t, gh, ManagerConfig{RunnerGroup: "runs-fleet"})
	if err := manager.PrepareRunner(context.Background(), validPrepareRequest()); err != nil {
		t.Fatalf("PrepareRunner() error = %v", err)
	}

	logged := buf.String()
	if !strings.Contains(logged, "runner group") {
		t.Errorf("runner group fallback was not logged; got:\n%s", logged)
	}
}

// Both ways a group can fail to resolve share one log message, so alerting greps
// a single string; the outcome field distinguishes them. Pinning this because a
// dashboard built on one message would silently miss the other.
func TestPrepareRunner_GroupFailuresShareOneLogMessage(t *testing.T) {
	tests := []struct {
		name        string
		gh          *mockJITGitHubClient
		wantOutcome string
	}{
		{
			name:        "resolver errored",
			gh:          &mockJITGitHubClient{groupErr: errors.New("resolver boom")},
			wantOutcome: "token_registration",
		},
		{
			name:        "default substituted",
			gh:          &mockJITGitHubClient{jitConfig: "ENCODEDJIT", groupFellBack: errors.New("not found")},
			wantOutcome: "default_group",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			restore := captureRunnerLog(&buf)
			defer restore()

			tt.gh.regToken = testRegToken
			manager, _ := newJITManager(t, tt.gh, ManagerConfig{RunnerGroup: "runs-fleet"})
			if err := manager.PrepareRunner(context.Background(), validPrepareRequest()); err != nil {
				t.Fatalf("PrepareRunner() error = %v", err)
			}

			logged := buf.String()
			if !strings.Contains(logged, "runner group unresolved") {
				t.Errorf("missing the shared log message; got:\n%s", logged)
			}
			if !strings.Contains(logged, tt.wantOutcome) {
				t.Errorf("missing outcome %q; got:\n%s", tt.wantOutcome, logged)
			}
		})
	}
}

// A resolved group must not be logged as a fallback, or the signal is noise.
func TestPrepareRunner_DoesNotLogWhenGroupResolves(t *testing.T) {
	var buf bytes.Buffer
	restore := captureRunnerLog(&buf)
	defer restore()

	gh := &mockJITGitHubClient{jitConfig: "ENCODEDJIT", groupID: 7}
	gh.regToken = testRegToken

	manager, _ := newJITManager(t, gh, ManagerConfig{RunnerGroup: "runs-fleet"})
	if err := manager.PrepareRunner(context.Background(), validPrepareRequest()); err != nil {
		t.Fatalf("PrepareRunner() error = %v", err)
	}

	if got := gh.lastJITRequest.RunnerGroupID; got != 7 {
		t.Errorf("JIT request runner group = %d, want 7", got)
	}
	if strings.Contains(buf.String(), "fallback") {
		t.Errorf("logged a fallback for a resolved group:\n%s", buf.String())
	}
}

// The JIT config registers a runner. It must never reach a log line, on any path.
func TestPrepareRunner_NeverLogsTheJITConfig(t *testing.T) {
	const secret = "SUPERSECRETJITBLOB"

	var buf bytes.Buffer
	restore := captureRunnerLog(&buf)
	defer restore()

	gh := &mockJITGitHubClient{jitConfig: secret}
	gh.regToken = testRegToken

	manager, _ := newJITManager(t, gh, ManagerConfig{})
	if err := manager.PrepareRunner(context.Background(), validPrepareRequest()); err != nil {
		t.Fatalf("PrepareRunner() error = %v", err)
	}

	if strings.Contains(buf.String(), secret) {
		t.Errorf("logs leak the JIT config:\n%s", buf.String())
	}
}

// The production client must satisfy the JIT capability. Without this, a signature
// drift in pkg/github turns the type assertion in mintJITConfig into a silent
// no-op and every runner quietly goes back to being label-bound — green tests,
// dead feature.
func TestRealGitHubClientSatisfiesJITCapability(t *testing.T) {
	var client interface{} = (*github.Client)(nil)
	if _, ok := client.(jitConfigGenerator); !ok {
		t.Error("*github.Client does not satisfy jitConfigGenerator; the JIT path would be dead code in production")
	}
	if _, ok := client.(registrationTokenGetter); !ok {
		t.Error("*github.Client does not satisfy registrationTokenGetter")
	}
}

// captureRunnerLog redirects log output to buf for the duration of a test.
// Package loggers resolve slog.Default() at log time (see logging.lazyHandler),
// so swapping the default is enough.
func captureRunnerLog(buf *bytes.Buffer) func() {
	prev := slog.Default()
	inner := slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	slog.SetDefault(slog.New(logging.NewContextHandler(inner)))
	return func() { slog.SetDefault(prev) }
}
