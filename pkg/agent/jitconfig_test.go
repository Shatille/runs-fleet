package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Shavakan/runs-fleet/pkg/secrets"
)

// renderingLogger captures fully interpolated log output. The package's existing
// mockLogger stores only the format string, which cannot detect a credential
// passed as a format ARGUMENT — exactly the leak worth guarding against.
type renderingLogger struct {
	mu    sync.Mutex
	lines []string
}

func (l *renderingLogger) Printf(format string, v ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, v...))
}

func (l *renderingLogger) Println(v ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintln(v...))
}

func (l *renderingLogger) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

// writeArgRecorder creates a run.sh that records both the arguments and the
// JIT-config environment variable it was invoked with, so a test can assert on
// exactly how the agent handed the config over.
func writeArgRecorder(t *testing.T, dir string) (argsFile, envFile string) {
	t.Helper()
	argsFile = filepath.Join(dir, "args.txt")
	envFile = filepath.Join(dir, "env.txt")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > " + argsFile + "\n" +
		"printf '%s' \"$ACTIONS_RUNNER_INPUT_JITCONFIG\" > " + envFile + "\n" +
		"exit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "run.sh"), []byte(script), 0o755); err != nil {
		t.Fatalf("failed to write run.sh: %v", err)
	}
	return argsFile, envFile
}

func recordedArgs(t *testing.T, argsFile string) []string {
	t.Helper()
	data, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("failed to read recorded args: %v", err)
	}
	return strings.Fields(string(data))
}

func recordedEnv(t *testing.T, envFile string) string {
	t.Helper()
	data, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatalf("failed to read recorded env: %v", err)
	}
	return string(data)
}

// A JIT-configured runner is bound by GitHub to one job, which is the whole point
// of the change. The config is handed over via ACTIONS_RUNNER_INPUT_JITCONFIG —
// the runner's env fallback for any CLI arg, and what GitHub's own
// actions-runner-controller uses — so the credential stays out of argv.
func TestExecuteJob_PassesJITConfigViaEnv(t *testing.T) {
	tmpDir := t.TempDir()
	argsFile, envFile := writeArgRecorder(t, tmpDir)

	executor := NewExecutor(&renderingLogger{}, nil)
	result, err := executor.ExecuteJobWithConfig(t.Context(), tmpDir, "ENCODEDJIT")
	if err != nil {
		t.Fatalf("ExecuteJobWithConfig() error = %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("exit code = %d, want 0", result.ExitCode)
	}

	if got := recordedEnv(t, envFile); got != "ENCODEDJIT" {
		t.Errorf("ACTIONS_RUNNER_INPUT_JITCONFIG = %q, want %q", got, "ENCODEDJIT")
	}
	// argv must stay clean: it is world-readable via /proc/<pid>/cmdline.
	if args := recordedArgs(t, argsFile); len(args) != 0 {
		t.Errorf("run.sh args = %v, want none (config must not reach argv)", args)
	}
}

// Without a JIT config the runner was already registered by config.sh, so run.sh
// must be invoked bare — passing an empty --jitconfig would break it.
func TestExecuteJob_OmitsJITConfigWhenEmpty(t *testing.T) {
	tmpDir := t.TempDir()
	argsFile, envFile := writeArgRecorder(t, tmpDir)

	executor := NewExecutor(&renderingLogger{}, nil)
	if _, err := executor.ExecuteJobWithConfig(t.Context(), tmpDir, ""); err != nil {
		t.Fatalf("ExecuteJobWithConfig() error = %v", err)
	}

	if got := recordedEnv(t, envFile); got != "" {
		t.Errorf("ACTIONS_RUNNER_INPUT_JITCONFIG = %q, want empty", got)
	}
	if args := recordedArgs(t, argsFile); len(args) != 0 {
		t.Errorf("run.sh args = %v, want none", args)
	}
}

// ExecuteJob is the pre-existing entry point; it must keep behaving as the
// no-JIT case so existing callers are unaffected.
func TestExecuteJob_LegacyEntryPointPassesNoJITConfig(t *testing.T) {
	tmpDir := t.TempDir()
	argsFile, envFile := writeArgRecorder(t, tmpDir)

	executor := NewExecutor(&renderingLogger{}, nil)
	if _, err := executor.ExecuteJob(t.Context(), tmpDir); err != nil {
		t.Fatalf("ExecuteJob() error = %v", err)
	}

	if got := recordedEnv(t, envFile); got != "" {
		t.Errorf("ACTIONS_RUNNER_INPUT_JITCONFIG = %q, want empty", got)
	}
	if args := recordedArgs(t, argsFile); len(args) != 0 {
		t.Errorf("run.sh args = %v, want none", args)
	}
}

// The JIT config is a credential and must never reach the agent's own log stream,
// which ships off-host.
func TestExecuteJob_NeverLogsTheJITConfig(t *testing.T) {
	const secret = "SUPERSECRETJITBLOB"

	tmpDir := t.TempDir()
	writeArgRecorder(t, tmpDir)

	logger := &renderingLogger{}
	executor := NewExecutor(logger, nil)
	if _, err := executor.ExecuteJobWithConfig(t.Context(), tmpDir, secret); err != nil {
		t.Fatalf("ExecuteJobWithConfig() error = %v", err)
	}

	if strings.Contains(logger.String(), secret) {
		t.Errorf("agent logs leak the JIT config:\n%s", logger.String())
	}
}

// RegisterRunner runs config.sh, which a JIT runner must skip entirely: GitHub
// already created the registration when it minted the config, and running
// config.sh would both fail and (with --replace) disturb the JIT registration.
func TestRegisterRunner_SkippedForJITConfig(t *testing.T) {
	tmpDir := t.TempDir()

	// A config.sh that fails if invoked, so any call is unmistakable.
	marker := filepath.Join(tmpDir, "config-ran")
	script := "#!/bin/sh\ntouch " + marker + "\nexit 1\n"
	if err := os.WriteFile(filepath.Join(tmpDir, "config.sh"), []byte(script), 0o755); err != nil {
		t.Fatalf("failed to write config.sh: %v", err)
	}

	registrar := NewRegistrar(nil, &renderingLogger{})
	cfg := &secrets.RunnerConfig{
		Repo:      "myorg/myrepo",
		JITConfig: "ENCODEDJIT",
		JITToken:  "tok",
		Labels:    []string{"runs-fleet"},
	}

	if err := registrar.RegisterRunner(t.Context(), cfg, tmpDir); err != nil {
		t.Fatalf("RegisterRunner() error = %v; a JIT config should make it a no-op", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("config.sh was executed despite a JIT config being present")
	}
}

// Without a JIT config the token path must still run config.sh, or every
// non-JIT dispatch silently stops registering.
func TestRegisterRunner_StillRunsConfigShWithoutJITConfig(t *testing.T) {
	tmpDir := t.TempDir()

	marker := filepath.Join(tmpDir, "config-ran")
	script := "#!/bin/sh\ntouch " + marker + "\nexit 0\n"
	if err := os.WriteFile(filepath.Join(tmpDir, "config.sh"), []byte(script), 0o755); err != nil {
		t.Fatalf("failed to write config.sh: %v", err)
	}

	registrar := NewRegistrar(nil, &renderingLogger{})
	cfg := &secrets.RunnerConfig{
		Repo:     "myorg/myrepo",
		JITToken: "tok",
		Labels:   []string{"runs-fleet"},
	}

	if err := registrar.RegisterRunner(t.Context(), cfg, tmpDir); err != nil {
		t.Fatalf("RegisterRunner() error = %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Error("config.sh was not executed on the token path")
	}
}
