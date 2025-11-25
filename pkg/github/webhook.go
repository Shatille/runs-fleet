// Package github provides webhook validation and label parsing for GitHub Actions workflow jobs.
package github

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/google/go-github/v57/github"
)

// ValidateSignature verifies GitHub webhook HMAC-SHA256 signature against the expected secret.
func ValidateSignature(payload []byte, signatureHeader string, secret string) error {
	if secret == "" {
		return errors.New("webhook secret not configured")
	}

	signature := strings.TrimPrefix(signatureHeader, "sha256=")
	if signature == signatureHeader {
		return errors.New("signature missing sha256= prefix")
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	expectedMAC := mac.Sum(nil)
	expectedSignature := hex.EncodeToString(expectedMAC)

	if !hmac.Equal([]byte(signature), []byte(expectedSignature)) {
		return errors.New("signature mismatch")
	}

	return nil
}

// ParseWebhook validates HMAC signature and parses GitHub webhook payload with 1MB size limit.
func ParseWebhook(r *http.Request, secret string) (interface{}, error) {
	event := r.Header.Get("X-GitHub-Event")
	if event == "" {
		return nil, errors.New("missing X-GitHub-Event header")
	}

	signatureHeader := r.Header.Get("X-Hub-Signature-256")
	if !strings.HasPrefix(signatureHeader, "sha256=") {
		return nil, errors.New("invalid or missing signature header")
	}

	defer func() { _ = r.Body.Close() }()

	limitedReader := &io.LimitedReader{R: r.Body, N: config.MaxBodySize + 1}
	payload, err := io.ReadAll(limitedReader)
	if err != nil {
		return nil, fmt.Errorf("failed to read request body: %w", err)
	}

	if len(payload) > config.MaxBodySize {
		return nil, errors.New("request body exceeds 1MB limit")
	}

	if err := ValidateSignature(payload, signatureHeader, secret); err != nil {
		return nil, fmt.Errorf("validation failed: %w", err)
	}

	return github.ParseWebHook(event, payload)
}

// JobConfig represents parsed runner configuration from workflow job labels.
type JobConfig struct {
	RunID        string
	InstanceType string
	Pool         string
	Private      bool
	Spot         bool
	RunnerSpec   string
	Region       string // Multi-region support (Phase 3)
	Environment  string // Per-stack environment support (Phase 6)
	OS           string // Operating system (linux, windows)
	Arch         string // Architecture (x64, arm64)
}

// DefaultRunnerSpecs maps runner spec names to EC2 instance types.
var DefaultRunnerSpecs = map[string]string{
	// Linux ARM64
	"2cpu-linux-arm64": "t4g.medium",
	"4cpu-linux-arm64": "c7g.xlarge",
	"8cpu-linux-arm64": "c7g.2xlarge",
	// Linux x64
	"2cpu-linux-x64": "t3.medium",
	"4cpu-linux-x64": "c6i.xlarge",
	"8cpu-linux-x64": "c6i.2xlarge",
	// Windows x64 (Phase 4)
	"2cpu-windows-x64": "t3.medium",
	"4cpu-windows-x64": "m6i.xlarge",
	"8cpu-windows-x64": "m6i.2xlarge",
}

// ParseLabels extracts runner configuration from runs-fleet= workflow job labels.
func ParseLabels(labels []string) (*JobConfig, error) {
	cfg := &JobConfig{
		Spot:        true,
		OS:          "linux",
		Arch:        "arm64",
		Environment: "", // Default: no specific environment
		Region:      "", // Default: use primary region
	}

	var runsFleetLabel string
	for _, label := range labels {
		if strings.HasPrefix(label, "runs-fleet=") {
			runsFleetLabel = label
			break
		}
	}

	if runsFleetLabel == "" {
		return nil, errors.New("no runs-fleet label found")
	}

	parts := strings.Split(strings.TrimPrefix(runsFleetLabel, "runs-fleet="), "/")
	if len(parts) == 0 {
		return nil, errors.New("invalid runs-fleet label format")
	}

	cfg.RunID = parts[0]
	if cfg.RunID == "" {
		return nil, errors.New("empty run-id in label")
	}

	for _, part := range parts[1:] {
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			continue
		}
		key, value := kv[0], kv[1]

		switch key {
		case "runner":
			cfg.RunnerSpec = value
			cfg.InstanceType = resolveInstanceType(value)
			cfg.OS, cfg.Arch = parseRunnerOSArch(value)
		case "pool":
			cfg.Pool = value
		case "private":
			cfg.Private = value == "true"
		case "spot":
			cfg.Spot = value != "false"
		case "region":
			// Multi-region support (Phase 3)
			cfg.Region = value
		case "env":
			// Per-stack environment support (Phase 6)
			if value == "dev" || value == "staging" || value == "prod" {
				cfg.Environment = value
			}
		}
	}

	if cfg.RunnerSpec == "" {
		return nil, errors.New("missing runner key in runs-fleet label")
	}

	return cfg, nil
}

// resolveInstanceType maps a runner spec to an EC2 instance type.
func resolveInstanceType(runnerSpec string) string {
	if instanceType, ok := DefaultRunnerSpecs[runnerSpec]; ok {
		return instanceType
	}

	// Fallback parsing for unknown specs
	switch {
	case strings.HasPrefix(runnerSpec, "2cpu-linux-arm64"):
		return "t4g.medium"
	case strings.HasPrefix(runnerSpec, "4cpu-linux-arm64"):
		return "c7g.xlarge"
	case strings.HasPrefix(runnerSpec, "4cpu-linux-x64"):
		return "c6i.xlarge"
	case strings.HasPrefix(runnerSpec, "8cpu-linux-arm64"):
		return "c7g.2xlarge"
	case strings.HasPrefix(runnerSpec, "8cpu-linux-x64"):
		return "c6i.2xlarge"
	case strings.Contains(runnerSpec, "windows"):
		return "m6i.xlarge" // Default Windows instance
	default:
		return "t4g.medium" // Default fallback
	}
}

// parseRunnerOSArch extracts OS and architecture from runner spec.
func parseRunnerOSArch(runnerSpec string) (os, arch string) {
	os = "linux"
	arch = "arm64"

	if strings.Contains(runnerSpec, "windows") {
		os = "windows"
	}
	if strings.Contains(runnerSpec, "x64") {
		arch = "x64"
	} else if strings.Contains(runnerSpec, "arm64") {
		arch = "arm64"
	}

	return os, arch
}

// IsWindowsRunner returns true if the runner spec is for Windows.
func IsWindowsRunner(runnerSpec string) bool {
	return strings.Contains(runnerSpec, "windows")
}
