package runner_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"

	"github.com/Shavakan/runs-fleet/pkg/agent"
	gh "github.com/Shavakan/runs-fleet/pkg/github"
	"github.com/Shavakan/runs-fleet/pkg/runner"
	"github.com/Shavakan/runs-fleet/pkg/secrets"
)

// These tests wire the real JIT chain end to end: the real github.Client against
// a stub GitHub, the real SSM-backed secrets.Store, the real runner.Manager, and
// the real agent Registrar/Executor.
//
// Every other test in this feature mocks at least one of those boundaries, so a
// contract mismatch between two layers — a field the manager writes and the agent
// never reads, a config that round-trips lossily, a runner registered twice —
// passes all of them. These are the only tests that would catch that.

const (
	flowRepo      = "devsisters/cc-data"
	flowJobID     = "93368064922"
	flowRunID     = "31360419439"
	flowInstance  = "i-0abc123"
	flowJITBlob   = "eyJ0ZXN0Ijoiaml0In0="
	flowRegToken  = "AABB-registration-token"
	flowLabel     = "runs-fleet/arch=arm64/gen=8/pool=cc"
	flowGroupName = "runs-fleet"
)

type stubGitHub struct {
	url string

	jitCalls    int
	groupCalls  int
	lastJITBody map[string]any

	failJIT       bool
	omitGroupName bool
}

func newStubGitHub(t *testing.T) *stubGitHub {
	t.Helper()
	s := &stubGitHub{}

	mux := http.NewServeMux()
	mux.HandleFunc("/orgs/devsisters/installation", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"id":      123,
			"account": map[string]any{"type": "Organization"},
		})
	})
	mux.HandleFunc("/app/installations/123/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusCreated, map[string]any{
			"token":      "ghs_installation_token",
			"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		})
	})
	mux.HandleFunc("/repos/"+flowRepo+"/actions/runners/registration-token", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusCreated, map[string]any{"token": flowRegToken})
	})
	mux.HandleFunc("/repos/"+flowRepo+"/actions/runner-groups", func(w http.ResponseWriter, _ *http.Request) {
		s.groupCalls++
		groups := []map[string]any{{"id": 1, "name": "Default"}}
		if !s.omitGroupName {
			groups = append(groups, map[string]any{"id": 7, "name": flowGroupName})
		}
		writeJSON(w, http.StatusOK, map[string]any{"runner_groups": groups})
	})
	mux.HandleFunc("/repos/"+flowRepo+"/actions/runners/generate-jitconfig", func(w http.ResponseWriter, r *http.Request) {
		s.jitCalls++
		_ = json.NewDecoder(r.Body).Decode(&s.lastJITBody)
		if s.failJIT {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"message": "nope"})
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"encoded_jit_config": flowJITBlob})
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	s.url = server.URL
	return s
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// flowSSM is an in-memory Parameter Store, so configs cross the production
// SSMStore serialization path rather than a mock of it.
type flowSSM struct {
	stored map[string]string
	tags   []string
}

func (f *flowSSM) PutParameter(_ context.Context, in *ssm.PutParameterInput, _ ...func(*ssm.Options)) (*ssm.PutParameterOutput, error) {
	// The real API rejects this combination outright, so a store that sends both
	// stores nothing in production. Reproduced here because a fake that accepts
	// it turns a dispatch-wide outage into a green test run.
	if len(in.Tags) > 0 && aws.ToBool(in.Overwrite) {
		return nil, errors.New("ValidationException: tags and overwrite can't be used together")
	}
	f.stored[aws.ToString(in.Name)] = aws.ToString(in.Value)
	for _, t := range in.Tags {
		f.tags = append(f.tags, aws.ToString(t.Key)+"="+aws.ToString(t.Value))
	}
	return &ssm.PutParameterOutput{}, nil
}

func (f *flowSSM) AddTagsToResource(_ context.Context, in *ssm.AddTagsToResourceInput, _ ...func(*ssm.Options)) (*ssm.AddTagsToResourceOutput, error) {
	for _, t := range in.Tags {
		f.tags = append(f.tags, aws.ToString(t.Key)+"="+aws.ToString(t.Value))
	}
	return &ssm.AddTagsToResourceOutput{}, nil
}

func (f *flowSSM) GetParameter(_ context.Context, in *ssm.GetParameterInput, _ ...func(*ssm.Options)) (*ssm.GetParameterOutput, error) {
	v, ok := f.stored[aws.ToString(in.Name)]
	if !ok {
		return nil, &ssmtypes.ParameterNotFound{}
	}
	return &ssm.GetParameterOutput{Parameter: &ssmtypes.Parameter{Value: aws.String(v)}}, nil
}

func (f *flowSSM) DeleteParameter(_ context.Context, in *ssm.DeleteParameterInput, _ ...func(*ssm.Options)) (*ssm.DeleteParameterOutput, error) {
	delete(f.stored, aws.ToString(in.Name))
	return &ssm.DeleteParameterOutput{}, nil
}

func (f *flowSSM) GetParametersByPath(_ context.Context, _ *ssm.GetParametersByPathInput, _ ...func(*ssm.Options)) (*ssm.GetParametersByPathOutput, error) {
	return &ssm.GetParametersByPathOutput{}, nil
}

// flowRunnerDir mimics what the AMI leaves on disk: a bare tarball extract with
// config.sh and run.sh and NO .runner/.credentials, since the AMI never runs
// config.sh. The scripts record how they were invoked.
func flowRunnerDir(t *testing.T, configExit int) (dir, configMarker, envFile string) {
	t.Helper()
	dir = t.TempDir()
	configMarker = filepath.Join(dir, "config-ran")
	envFile = filepath.Join(dir, "run-env.txt")

	cfgScript := fmt.Sprintf("#!/bin/sh\ntouch %s\nexit %d\n", configMarker, configExit)
	if err := os.WriteFile(filepath.Join(dir, "config.sh"), []byte(cfgScript), 0o755); err != nil {
		t.Fatalf("write config.sh: %v", err)
	}

	// Distinguishes an unset variable from an empty one; a bare printf cannot.
	runScript := "#!/bin/sh\n" +
		"if [ -z \"${ACTIONS_RUNNER_INPUT_JITCONFIG+x}\" ]; then\n" +
		"  printf 'UNSET' > " + envFile + "\n" +
		"else\n" +
		"  printf 'SET:%s' \"$ACTIONS_RUNNER_INPUT_JITCONFIG\" > " + envFile + "\n" +
		"fi\n" +
		"printf '|ARGV:%s' \"$*\" >> " + envFile + "\n" +
		"exit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "run.sh"), []byte(runScript), 0o755); err != nil {
		t.Fatalf("write run.sh: %v", err)
	}
	return dir, configMarker, envFile
}

// flowTestKey mints a throwaway App key so the real github.Client can sign JWTs.
// Generated per run rather than checked in.
func flowTestKey(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	return base64.StdEncoding.EncodeToString(pemBytes)
}

type flowLogger struct{ lines []string }

func (l *flowLogger) Printf(f string, v ...interface{}) {
	l.lines = append(l.lines, fmt.Sprintf(f, v...))
}
func (l *flowLogger) Println(v ...interface{}) { l.lines = append(l.lines, fmt.Sprintln(v...)) }
func (l *flowLogger) String() string           { return strings.Join(l.lines, "\n") }

func newFlowManager(t *testing.T, stub *stubGitHub, group string) (*runner.Manager, secrets.Store, *flowSSM) {
	t.Helper()
	client, err := gh.NewClient("12345", flowTestKey(t), gh.WithBaseURL(stub.url))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	backing := &flowSSM{stored: map[string]string{}}
	store := secrets.NewSSMStoreWithClient(backing, "/runs-fleet/runners")
	return runner.NewManager(client, store, runner.ManagerConfig{RunnerGroup: group}), store, backing
}

func flowRequest() runner.PrepareRunnerRequest {
	return runner.PrepareRunnerRequest{
		InstanceID: flowInstance,
		JobID:      flowJobID,
		RunID:      flowRunID,
		Repo:       flowRepo,
		Labels:     []string{flowLabel},
		Pool:       "cc",
	}
}

func readFlowFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// The headline behavior change, proven across every real boundary: the
// orchestrator mints a job-bound config, it survives the secrets round trip, the
// agent skips config.sh, and the runner receives the config via the environment.
func TestJITFlow_EndToEnd(t *testing.T) {
	stub := newStubGitHub(t)
	manager, store, backing := newFlowManager(t, stub, flowGroupName)

	if err := manager.PrepareRunner(context.Background(), flowRequest()); err != nil {
		t.Fatalf("PrepareRunner: %v", err)
	}
	if stub.jitCalls != 1 {
		t.Fatalf("generate-jitconfig called %d times, want 1", stub.jitCalls)
	}

	// The resolved numeric group must reach GitHub, not the group name.
	if got := stub.lastJITBody["runner_group_id"]; got != float64(7) {
		t.Errorf("runner_group_id = %v, want 7", got)
	}
	// The runs-fleet- prefix is what HandleWorkflowJobInProgress matches to derive
	// job_startup_seconds — the metric that proves this fix worked in production.
	name, _ := stub.lastJITBody["name"].(string)
	if !strings.HasPrefix(name, "runs-fleet-") {
		t.Errorf("runner name = %q, want a runs-fleet- prefix", name)
	}

	dir, configMarker, envFile := flowRunnerDir(t, 1) // config.sh fails if called
	logger := &flowLogger{}
	registrar := agent.NewRegistrar(store, logger)

	cfg, err := registrar.FetchConfig(context.Background(), flowInstance)
	if err != nil {
		t.Fatalf("FetchConfig: %v", err)
	}
	if cfg.JITConfig != flowJITBlob {
		t.Fatalf("JITConfig = %q, want %q (lost crossing the secrets backend)", cfg.JITConfig, flowJITBlob)
	}
	if cfg.RegistrationToken != flowRegToken {
		t.Errorf("RegistrationToken = %q, want the fallback token to remain stored", cfg.RegistrationToken)
	}

	if regErr := registrar.RegisterRunner(context.Background(), cfg, dir); regErr != nil {
		t.Fatalf("RegisterRunner: %v", regErr)
	}
	if _, statErr := os.Stat(configMarker); statErr == nil {
		t.Error("config.sh ran on the JIT path")
	}

	result, err := agent.NewExecutor(logger, nil).ExecuteJobWithConfig(context.Background(), dir, cfg.JITConfig)
	if err != nil {
		t.Fatalf("ExecuteJobWithConfig: %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("exit code = %d, want 0", result.ExitCode)
	}

	got := readFlowFile(t, envFile)
	if got != "SET:"+flowJITBlob+"|ARGV:" {
		t.Errorf("runner invocation = %q, want the config in the env and nothing in argv", got)
	}
	if strings.Contains(logger.String(), flowJITBlob) {
		t.Errorf("agent logs leak the JIT config:\n%s", logger.String())
	}
	for _, tag := range backing.tags {
		if strings.Contains(tag, flowJITBlob) || strings.Contains(tag, flowRegToken) {
			t.Errorf("SSM tag %q leaks a credential", tag)
		}
	}
}

// The most dangerous failure mode: a broken JIT path degrades to token
// registration, which still runs jobs, so nothing outwardly fails. This asserts
// the degradation is complete rather than half-applied — the agent must actually
// register via config.sh and must not receive a JIT variable.
func TestJITFlow_DegradesToTokenRegistration(t *testing.T) {
	stub := newStubGitHub(t)
	stub.failJIT = true
	manager, store, _ := newFlowManager(t, stub, flowGroupName)

	if err := manager.PrepareRunner(context.Background(), flowRequest()); err != nil {
		t.Fatalf("PrepareRunner: %v", err)
	}

	dir, configMarker, envFile := flowRunnerDir(t, 0) // config.sh must succeed here
	logger := &flowLogger{}
	registrar := agent.NewRegistrar(store, logger)

	cfg, err := registrar.FetchConfig(context.Background(), flowInstance)
	if err != nil {
		t.Fatalf("FetchConfig: %v", err)
	}
	if cfg.JITConfig != "" {
		t.Errorf("JITConfig = %q, want empty after a mint failure", cfg.JITConfig)
	}
	if cfg.RegistrationToken != flowRegToken {
		t.Fatalf("RegistrationToken = %q, want the fallback token", cfg.RegistrationToken)
	}

	if err := registrar.RegisterRunner(context.Background(), cfg, dir); err != nil {
		t.Fatalf("RegisterRunner: %v", err)
	}
	if _, err := os.Stat(configMarker); err != nil {
		t.Error("config.sh did not run on the token fallback path")
	}

	if _, err := agent.NewExecutor(logger, nil).ExecuteJobWithConfig(context.Background(), dir, cfg.JITConfig); err != nil {
		t.Fatalf("ExecuteJobWithConfig: %v", err)
	}
	if got := readFlowFile(t, envFile); got != "UNSET|ARGV:" {
		t.Errorf("runner invocation = %q, want the JIT variable absent on the token path", got)
	}
}

// An unresolvable group must not cost the job its job-binding: it still gets a
// JIT config, just in the Default group.
func TestJITFlow_UnresolvableGroupStillMintsJIT(t *testing.T) {
	stub := newStubGitHub(t)
	stub.omitGroupName = true
	manager, store, _ := newFlowManager(t, stub, flowGroupName)

	if err := manager.PrepareRunner(context.Background(), flowRequest()); err != nil {
		t.Fatalf("PrepareRunner: %v", err)
	}
	if stub.jitCalls != 1 {
		t.Fatalf("generate-jitconfig called %d times, want 1", stub.jitCalls)
	}
	if got := stub.lastJITBody["runner_group_id"]; got != float64(1) {
		t.Errorf("runner_group_id = %v, want 1 (Default)", got)
	}

	cfg, err := agent.NewRegistrar(store, &flowLogger{}).FetchConfig(context.Background(), flowInstance)
	if err != nil {
		t.Fatalf("FetchConfig: %v", err)
	}
	if cfg.JITConfig != flowJITBlob {
		t.Errorf("JITConfig = %q, want a config despite the group falling back", cfg.JITConfig)
	}
}

// The production configuration today: ManagerConfig.RunnerGroup is unwired, so
// the empty name must resolve to Default without spending an API call.
func TestJITFlow_EmptyGroupSkipsTheLookup(t *testing.T) {
	stub := newStubGitHub(t)
	manager, _, _ := newFlowManager(t, stub, "")

	if err := manager.PrepareRunner(context.Background(), flowRequest()); err != nil {
		t.Fatalf("PrepareRunner: %v", err)
	}
	if stub.groupCalls != 0 {
		t.Errorf("runner-groups called %d times, want 0 for an empty group name", stub.groupCalls)
	}
	if got := stub.lastJITBody["runner_group_id"]; got != float64(1) {
		t.Errorf("runner_group_id = %v, want 1", got)
	}
}

// The labels the job asked for must reach GitHub unchanged. A JIT runner still
// carries them (GitHub matches the job against them), so dropping or rewriting
// them here would make the runner unmatchable and starve the job.
func TestJITFlow_PreservesRequestedLabels(t *testing.T) {
	stub := newStubGitHub(t)
	manager, _, _ := newFlowManager(t, stub, flowGroupName)

	if err := manager.PrepareRunner(context.Background(), flowRequest()); err != nil {
		t.Fatalf("PrepareRunner: %v", err)
	}

	raw, ok := stub.lastJITBody["labels"].([]any)
	if !ok || len(raw) != 1 {
		t.Fatalf("labels = %v, want exactly one", stub.lastJITBody["labels"])
	}
	if raw[0] != flowLabel {
		t.Errorf("label = %v, want %q", raw[0], flowLabel)
	}
}
