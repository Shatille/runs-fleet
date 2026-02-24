package agent

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/secrets"
)

// Logger defines logging interface for the registrar.
type Logger interface {
	Printf(format string, v ...interface{})
	Println(v ...interface{})
}

// registrationRetryBaseDelay is the base delay for retry backoff.
// Exposed as a variable to allow testing with shorter durations.
var registrationRetryBaseDelay = 1 * time.Second

// Registrar handles runner registration with GitHub.
type Registrar struct {
	secretsStore secrets.Store
	logger       Logger
}

// NewRegistrar creates a new registrar for GitHub Actions runners.
func NewRegistrar(secretsStore secrets.Store, logger Logger) *Registrar {
	return &Registrar{
		secretsStore: secretsStore,
		logger:       logger,
	}
}

// NewRegistrarWithoutSecrets creates a registrar without a secrets store.
// Used in K8s mode where config is loaded from files instead of secrets backend.
func NewRegistrarWithoutSecrets(logger Logger) *Registrar {
	return &Registrar{
		secretsStore: nil,
		logger:       logger,
	}
}

// FetchConfig retrieves runner configuration from the secrets backend.
func (r *Registrar) FetchConfig(ctx context.Context, runnerID string) (*secrets.RunnerConfig, error) {
	var config *secrets.RunnerConfig
	var lastErr error

	// Retry up to 3 times with backoff
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(1<<uint(attempt)) * registrationRetryBaseDelay
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			r.logger.Printf("Retrying secrets fetch (attempt %d/3)...", attempt+1)
		}

		cfg, err := r.secretsStore.Get(ctx, runnerID)
		if err != nil {
			lastErr = err
			continue
		}

		config = cfg
		return config, nil
	}

	return nil, fmt.Errorf("failed to fetch config after 3 attempts: %w", lastErr)
}

// RegisterRunner registers the runner with GitHub using the config.sh script.
func (r *Registrar) RegisterRunner(ctx context.Context, config *secrets.RunnerConfig, runnerPath string) error {
	configScript := filepath.Join(runnerPath, "config.sh")

	if _, err := os.Stat(configScript); err != nil {
		return fmt.Errorf("config.sh not found: %w", err)
	}

	// Always use repo-level registration to ensure runners only pick up jobs
	// from the specific repository. Org-level registration allows runners to
	// pick up jobs from ANY repo in the org, which can cause job misassignment.
	if config.Repo == "" {
		return fmt.Errorf("repo is required for registration (owner/repo format)")
	}
	repoURL := fmt.Sprintf("https://github.com/%s", config.Repo)

	args := []string{
		"--unattended",
		"--ephemeral",
		"--url", repoURL,
		"--token", config.JITToken,
	}

	if len(config.Labels) > 0 {
		args = append(args, "--labels", strings.Join(config.Labels, ","))
	}

	if config.RunnerGroup != "" {
		args = append(args, "--runnergroup", config.RunnerGroup)
	}

	runnerName := config.RunnerName
	if runnerName == "" {
		runnerName = "runs-fleet-runner"
	}
	args = append(args, "--name", runnerName)

	args = append(args, "--replace")

	r.logger.Printf("Registering runner with URL: %s", repoURL)

	cmd := exec.CommandContext(ctx, configScript, args...)
	cmd.Dir = runnerPath
	cmd.Env = append(os.Environ(),
		"RUNNER_ALLOW_RUNASROOT=1",
	)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		r.logger.Printf("Registration stdout: %s", stdout.String())
		r.logger.Printf("Registration stderr: %s", stderr.String())
		return fmt.Errorf("registration failed: %w", err)
	}

	r.logger.Printf("Registration output: %s", stdout.String())
	return nil
}

// SetRunnerEnvironment sets environment variables for the runner.
func (r *Registrar) SetRunnerEnvironment(runnerPath string, cacheURL string, cacheToken string) error {
	envFile := filepath.Join(runnerPath, ".env")

	envVars := []string{
		"RUNNER_ALLOW_RUNASROOT=1",
	}

	if cacheURL != "" {
		envVars = append(envVars, fmt.Sprintf("ACTIONS_CACHE_URL=%s", cacheURL))
	}

	if cacheToken != "" {
		envVars = append(envVars, fmt.Sprintf("ACTIONS_CACHE_TOKEN=%s", cacheToken))
	}

	content := strings.Join(envVars, "\n") + "\n"
	if err := os.WriteFile(envFile, []byte(content), 0644); err != nil {
		return fmt.Errorf("failed to write .env file: %w", err)
	}

	return nil
}
