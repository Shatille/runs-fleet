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
	"strconv"
	"strings"

	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/Shavakan/runs-fleet/pkg/fleet"
	"github.com/google/go-github/v57/github"
)

// Architecture constants.
const (
	ArchARM64 = "arm64"
	ArchX64   = "x64"
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
	Backend      string // Compute backend: "ec2" or "k8s" (empty = use default)

	// Flexible instance selection (Phase 10)
	InstanceTypes []string // Multiple instance types for spot diversification
	CPUMin        int      // Minimum vCPUs (from cpu= label)
	CPUMax        int      // Maximum vCPUs (from cpu= label, e.g., cpu=4+16)
	RAMMin        float64  // Minimum RAM in GB (from ram= label)
	RAMMax        float64  // Maximum RAM in GB (from ram= label, e.g., ram=8+32)
	Families      []string // Instance families (from family= label, e.g., family=c7g+m7g)
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

// SpotDiversificationTypes maps runner specs to alternative instance types for spot capacity.
// When spot is enabled, EC2 Fleet selects from this pool for better availability.
// All instance types must meet the minimum CPU requirement of the runner spec.
// Note: 4cpu and 8cpu specs use only sustained-performance instances (no burstable)
// to avoid CPU throttling after burst credits exhaust.
var SpotDiversificationTypes = map[string][]string{
	// Linux ARM64 - 2 vCPU options (burstable OK for short jobs)
	"2cpu-linux-arm64": {"t4g.medium", "t4g.large"},
	// Linux ARM64 - 4 vCPU sustained-performance options
	"4cpu-linux-arm64": {"c7g.xlarge", "m7g.xlarge", "c6g.xlarge"},
	// Linux ARM64 - 8 vCPU sustained-performance options
	"8cpu-linux-arm64": {"c7g.2xlarge", "m7g.2xlarge", "c6g.2xlarge"},
	// Linux x64 - 2 vCPU options (burstable OK for short jobs)
	"2cpu-linux-x64": {"t3.medium", "t3.large"},
	// Linux x64 - 4 vCPU sustained-performance options
	"4cpu-linux-x64": {"c6i.xlarge", "m6i.xlarge", "c7i.xlarge"},
	// Linux x64 - 8 vCPU sustained-performance options
	"8cpu-linux-x64": {"c6i.2xlarge", "m6i.2xlarge", "c7i.2xlarge"},
	// Windows x64 - 2 vCPU options (burstable OK for short jobs)
	"2cpu-windows-x64": {"t3.medium", "t3.large"},
	// Windows x64 - 4 vCPU sustained-performance options
	"4cpu-windows-x64": {"m6i.xlarge", "m7i.xlarge", "c6i.xlarge"},
	// Windows x64 - 8 vCPU sustained-performance options
	"8cpu-windows-x64": {"m6i.2xlarge", "m7i.2xlarge", "c6i.2xlarge"},
}

// ParseLabels extracts runner configuration from runs-fleet= workflow job labels.
// Supports both legacy runner= specs and flexible cpu=/ram=/family=/arch= labels.
//
// Legacy format:
//
//	runs-fleet=<run-id>/runner=4cpu-linux-arm64/pool=default/spot=true
//
// Flexible format:
//
//	runs-fleet=<run-id>/cpu=4+16/ram=8+32/family=c7g+m7g/arch=arm64/spot=true
//
// The flexible format allows specifying CPU and RAM ranges (min+max) for better
// spot instance availability. Multiple instance families can be specified with +.
func ParseLabels(labels []string) (*JobConfig, error) {
	cfg := &JobConfig{
		Spot:        true,
		OS:          "linux",
		Arch:        "", // Empty = arch doesn't matter, uses non-suffixed launch template
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

	hasFlexibleSpec, err := parseLabelParts(cfg, parts[1:])
	if err != nil {
		return nil, err
	}

	// If using flexible specs, resolve instance types
	if hasFlexibleSpec {
		if err := resolveFlexibleSpec(cfg); err != nil {
			return nil, err
		}
	} else if cfg.RunnerSpec == "" {
		return nil, errors.New("missing runner or cpu/ram specification in runs-fleet label")
	}

	// Validate Windows only supports x64 architecture
	if cfg.OS == "windows" && cfg.Arch != "" && cfg.Arch != ArchX64 {
		return nil, fmt.Errorf("windows runners only support x64 architecture, got %q", cfg.Arch)
	}

	return cfg, nil
}

// parseLabelParts parses key=value pairs from the runs-fleet label and populates the JobConfig.
// Returns true if flexible specs (cpu/ram/family) are used.
func parseLabelParts(cfg *JobConfig, parts []string) (bool, error) {
	hasFlexibleSpec := false

	for _, part := range parts {
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			continue
		}
		key, value := kv[0], kv[1]

		switch key {
		case "runner":
			cfg.RunnerSpec = value
			cfg.InstanceType = resolveInstanceType(value)
			cfg.InstanceTypes = resolveSpotDiversificationTypes(value)
			cfg.OS, cfg.Arch = parseRunnerOSArch(value)

		case "cpu":
			hasFlexibleSpec = true
			cpuMin, cpuMax, err := parseRange(value)
			if err != nil {
				return false, fmt.Errorf("invalid cpu label: %w", err)
			}
			cfg.CPUMin = cpuMin
			cfg.CPUMax = cpuMax

		case "ram":
			hasFlexibleSpec = true
			ramMin, ramMax, err := parseRangeFloat(value)
			if err != nil {
				return false, fmt.Errorf("invalid ram label: %w", err)
			}
			cfg.RAMMin = ramMin
			cfg.RAMMax = ramMax

		case "family":
			hasFlexibleSpec = true
			cfg.Families = strings.Split(value, "+")

		case "arch":
			if value == ArchARM64 || value == ArchX64 {
				cfg.Arch = value
			}

		case "pool":
			cfg.Pool = value

		case "private":
			cfg.Private = value == "true"

		case "spot":
			cfg.Spot = value != "false"

		case "region":
			cfg.Region = value

		case "env":
			if value == "dev" || value == "staging" || value == "prod" {
				cfg.Environment = value
			}

		case "backend":
			if value == "ec2" || value == "k8s" {
				cfg.Backend = value
			} else {
				return false, fmt.Errorf("invalid backend label: must be 'ec2' or 'k8s', got %q", value)
			}
		}
	}

	return hasFlexibleSpec, nil
}

// parseRange parses an integer range like "4" or "4+16" into min and max values.
// Returns an error if the values cannot be parsed as integers.
func parseRange(s string) (minVal, maxVal int, err error) {
	if s == "" {
		return 0, 0, nil
	}
	parts := strings.SplitN(s, "+", 2)
	minVal, err = strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, fmt.Errorf("invalid cpu min value %q: %w", parts[0], err)
	}
	if len(parts) == 2 {
		maxVal, err = strconv.Atoi(parts[1])
		if err != nil {
			return 0, 0, fmt.Errorf("invalid cpu max value %q: %w", parts[1], err)
		}
	}
	return minVal, maxVal, nil
}

// parseRangeFloat parses a float range like "8" or "8+32" into min and max values.
// Returns an error if the values cannot be parsed as floats.
func parseRangeFloat(s string) (minVal, maxVal float64, err error) {
	if s == "" {
		return 0, 0, nil
	}
	parts := strings.SplitN(s, "+", 2)
	minVal, err = strconv.ParseFloat(parts[0], 64)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid ram min value %q: %w", parts[0], err)
	}
	if len(parts) == 2 {
		maxVal, err = strconv.ParseFloat(parts[1], 64)
		if err != nil {
			return 0, 0, fmt.Errorf("invalid ram max value %q: %w", parts[1], err)
		}
	}
	return minVal, maxVal, nil
}

// resolveFlexibleSpec resolves instance types from flexible CPU/RAM/family specifications.
func resolveFlexibleSpec(cfg *JobConfig) error {
	// Set defaults for CPU if not specified
	if cfg.CPUMin == 0 {
		cfg.CPUMin = 2 // Default minimum 2 vCPUs
	}

	// Determine OS from arch (flexible specs default to linux)
	cfg.OS = "linux"

	// Build flexible spec for resolution
	spec := fleet.FlexibleSpec{
		CPUMin:   cfg.CPUMin,
		CPUMax:   cfg.CPUMax,
		RAMMin:   cfg.RAMMin,
		RAMMax:   cfg.RAMMax,
		Arch:     cfg.Arch,
		Families: cfg.Families,
	}

	// If no families specified, use defaults for the architecture
	if len(spec.Families) == 0 {
		spec.Families = fleet.DefaultFlexibleFamilies(cfg.Arch)
	}

	// Resolve matching instance types
	instanceTypes := fleet.ResolveInstanceTypes(spec)
	if len(instanceTypes) == 0 {
		return errors.New("no instance types match the specified cpu/ram/family requirements")
	}

	// Store all matching types for spot diversification
	cfg.InstanceTypes = instanceTypes
	// Primary instance type is the smallest match
	cfg.InstanceType = instanceTypes[0]

	// Generate a synthetic runner spec for logging/display
	if cfg.Arch != "" {
		cfg.RunnerSpec = fmt.Sprintf("%dcpu-%s-%s", cfg.CPUMin, cfg.OS, cfg.Arch)
		if cfg.CPUMax > 0 {
			cfg.RunnerSpec = fmt.Sprintf("%d-%dcpu-%s-%s", cfg.CPUMin, cfg.CPUMax, cfg.OS, cfg.Arch)
		}
	} else {
		// Empty arch = arch doesn't matter
		cfg.RunnerSpec = fmt.Sprintf("%dcpu-%s", cfg.CPUMin, cfg.OS)
		if cfg.CPUMax > 0 {
			cfg.RunnerSpec = fmt.Sprintf("%d-%dcpu-%s", cfg.CPUMin, cfg.CPUMax, cfg.OS)
		}
	}

	return nil
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

// resolveSpotDiversificationTypes returns multiple instance types for spot capacity.
// When spot is enabled, EC2 Fleet selects from this pool for better availability.
func resolveSpotDiversificationTypes(runnerSpec string) []string {
	if types, ok := SpotDiversificationTypes[runnerSpec]; ok {
		return types
	}
	// Fallback to single instance type if no diversification defined
	return []string{resolveInstanceType(runnerSpec)}
}

// parseRunnerOSArch extracts OS and architecture from runner spec.
// Returns empty arch if not explicitly specified, which triggers ARM64 family
// defaults in DefaultFlexibleFamilies for backward compatibility with legacy template.
func parseRunnerOSArch(runnerSpec string) (os, arch string) {
	os = "linux"
	arch = "" // Empty = arch doesn't matter

	if strings.Contains(runnerSpec, "windows") {
		os = "windows"
	}
	if strings.Contains(runnerSpec, ArchX64) {
		arch = ArchX64
	} else if strings.Contains(runnerSpec, ArchARM64) {
		arch = ArchARM64
	}

	return os, arch
}

// IsWindowsRunner returns true if the runner spec is for Windows.
func IsWindowsRunner(runnerSpec string) bool {
	return strings.Contains(runnerSpec, "windows")
}
