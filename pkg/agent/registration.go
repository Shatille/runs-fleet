package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
)

// SSMAPI defines SSM operations for fetching runner configuration.
type SSMAPI interface {
	GetParameter(ctx context.Context, params *ssm.GetParameterInput, optFns ...func(*ssm.Options)) (*ssm.GetParameterOutput, error)
}

// Logger defines logging interface for the registrar.
type Logger interface {
	Printf(format string, v ...interface{})
	Println(v ...interface{})
}

// registrationRetryBaseDelay is the base delay for retry backoff.
// Exposed as a variable to allow testing with shorter durations.
var registrationRetryBaseDelay = 1 * time.Second

// RunnerConfig represents configuration for registering a runner.
type RunnerConfig struct {
	Org         string   `json:"org"`
	Repo        string   `json:"repo,omitempty"`
	JITToken    string   `json:"jit_token"`
	Labels      []string `json:"labels"`
	RunnerGroup string   `json:"runner_group,omitempty"`
	JobID       string   `json:"job_id,omitempty"`
	CacheToken  string   `json:"cache_token,omitempty"`
	IsOrg       bool     `json:"is_org"` // Deprecated: kept for JSON compatibility
}

// Registrar handles runner registration with GitHub.
type Registrar struct {
	ssmClient SSMAPI
	logger    Logger
}

// NewRegistrar creates a new registrar for GitHub Actions runners.
func NewRegistrar(cfg aws.Config, logger Logger) *Registrar {
	return &Registrar{
		ssmClient: ssm.NewFromConfig(cfg),
		logger:    logger,
	}
}

// NewRegistrarWithoutAWS creates a registrar without AWS SSM client.
// Used in K8s mode where config is loaded from files instead of SSM.
func NewRegistrarWithoutAWS(logger Logger) *Registrar {
	return &Registrar{
		ssmClient: nil,
		logger:    logger,
	}
}

// FetchConfig retrieves runner configuration from SSM Parameter Store.
func (r *Registrar) FetchConfig(ctx context.Context, parameterPath string) (*RunnerConfig, error) {
	var config *RunnerConfig
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
			r.logger.Printf("Retrying SSM fetch (attempt %d/3)...", attempt+1)
		}

		output, err := r.ssmClient.GetParameter(ctx, &ssm.GetParameterInput{
			Name:           aws.String(parameterPath),
			WithDecryption: aws.Bool(true),
		})
		if err != nil {
			lastErr = err
			continue
		}

		if output.Parameter == nil || output.Parameter.Value == nil {
			lastErr = fmt.Errorf("parameter value is nil")
			continue
		}

		config = &RunnerConfig{}
		if err := json.Unmarshal([]byte(*output.Parameter.Value), config); err != nil {
			return nil, fmt.Errorf("failed to parse runner config: %w", err)
		}

		return config, nil
	}

	return nil, fmt.Errorf("failed to fetch config after 3 attempts: %w", lastErr)
}

// RegisterRunner registers the runner with GitHub using the config.sh script.
func (r *Registrar) RegisterRunner(ctx context.Context, config *RunnerConfig, runnerPath string) error {
	configScript := filepath.Join(runnerPath, "config.sh")

	// Verify config.sh exists
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

	// Build command arguments
	args := []string{
		"--unattended",
		"--ephemeral",
		"--url", repoURL,
		"--token", config.JITToken,
	}

	// Add labels if specified
	if len(config.Labels) > 0 {
		args = append(args, "--labels", strings.Join(config.Labels, ","))
	}

	// Add runner group if specified
	if config.RunnerGroup != "" {
		args = append(args, "--runnergroup", config.RunnerGroup)
	}

	// Add a unique runner name
	hostname, _ := os.Hostname()
	runnerName := fmt.Sprintf("runs-fleet-%s", hostname)
	args = append(args, "--name", runnerName)

	// Don't replace if already registered
	args = append(args, "--replace")

	r.logger.Printf("Registering runner with URL: %s", repoURL)

	// Execute config.sh
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
