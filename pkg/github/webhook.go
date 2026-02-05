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
	ArchAMD64 = "amd64"
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
	Spot         bool
	Region       string // Multi-region support (Phase 3)
	Environment  string // Per-stack environment support (Phase 6)
	OS           string // Operating system (linux, windows)
	Arch         string // Architecture (amd64, arm64)
	Backend      string // Compute backend: "ec2" or "k8s" (empty = use default)

	// Flexible instance selection
	InstanceTypes []string // Multiple instance types for spot diversification
	CPUMin        int      // Minimum vCPUs (from cpu= label)
	CPUMax        int      // Maximum vCPUs (from cpu= label, e.g., cpu=4+16)
	RAMMin        float64  // Minimum RAM in GB (from ram= label)
	RAMMax        float64  // Maximum RAM in GB (from ram= label, e.g., ram=8+32)
	Families      []string // Instance families (from family= label, e.g., family=c7g+m7g)
	Gen           int      // Instance generation (from gen= label, e.g., gen=8). 0 = any generation
	OriginalLabel string   // Original runs-fleet label from workflow (for GitHub matching)

	// Storage configuration
	StorageGiB int // Disk storage in GiB (from disk= label, e.g., disk=100)

	// Network configuration
	PublicIP bool // Request public IP (uses public subnet). Default: false (private subnet preferred)
}


// ParseLabels extracts runner configuration from runs-fleet= workflow job labels.
//
// Format:
//
//	runs-fleet=<run-id>/cpu=4/arch=arm64/pool=default/spot=true
//	runs-fleet=<run-id>/cpu=4+16/ram=8+32/family=c7g+m7g/arch=arm64
//
// CPU and RAM ranges (min+max) enable spot diversification for better availability.
// Multiple instance families can be specified with + separator.
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

	cfg.OriginalLabel = runsFleetLabel

	parts := strings.Split(strings.TrimPrefix(runsFleetLabel, "runs-fleet="), "/")
	if len(parts) == 0 {
		return nil, errors.New("invalid runs-fleet label format")
	}

	cfg.RunID = parts[0]
	if cfg.RunID == "" {
		return nil, errors.New("empty run-id in label")
	}

	if err := parseLabelParts(cfg, parts[1:]); err != nil {
		return nil, err
	}

	if err := ResolveFlexibleSpec(cfg); err != nil {
		return nil, err
	}

	// Validate Windows only supports amd64 architecture
	if cfg.OS == "windows" && cfg.Arch != "" && cfg.Arch != ArchAMD64 {
		return nil, fmt.Errorf("windows runners only support amd64 architecture, got %q", cfg.Arch)
	}

	return cfg, nil
}

// parseLabelParts parses key=value pairs from the runs-fleet label and populates the JobConfig.
func parseLabelParts(cfg *JobConfig, parts []string) error {
	for _, part := range parts {
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			continue
		}
		key, value := kv[0], kv[1]

		switch key {
		case "cpu":
			cpuMin, cpuMax, err := parseRange(value)
			if err != nil {
				return fmt.Errorf("invalid cpu label: %w", err)
			}
			cfg.CPUMin = cpuMin
			cfg.CPUMax = cpuMax

		case "ram":
			ramMin, ramMax, err := parseRangeFloat(value)
			if err != nil {
				return fmt.Errorf("invalid ram label: %w", err)
			}
			cfg.RAMMin = ramMin
			cfg.RAMMax = ramMax

		case "family":
			cfg.Families = strings.Split(value, "+")

		case "gen":
			gen, err := parseBoundedInt(value, 1, 10, "gen")
			if err != nil {
				return err
			}
			cfg.Gen = gen

		case "arch":
			if value == ArchARM64 || value == ArchAMD64 {
				cfg.Arch = value
			}

		case "pool":
			cfg.Pool = value

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
				return fmt.Errorf("invalid backend label: must be 'ec2' or 'k8s', got %q", value)
			}

		case "disk":
			diskGiB, err := parseBoundedInt(value, 1, 16384, "disk")
			if err != nil {
				return err
			}
			cfg.StorageGiB = diskGiB

		case "public":
			var err error
			cfg.PublicIP, err = strconv.ParseBool(value)
			if err != nil {
				return fmt.Errorf("invalid public value %q: %w", value, err)
			}
		}
	}

	return nil
}

// parseBoundedInt parses a string to int and validates it's within [min, max] range.
func parseBoundedInt(s string, minBound, maxBound int, label string) (int, error) {
	val, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("invalid %s label: %w", label, err)
	}
	if val < minBound || val > maxBound {
		return 0, fmt.Errorf("%s must be between %d and %d, got %d", label, minBound, maxBound, val)
	}
	return val, nil
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

// ResolveFlexibleSpec resolves instance types from flexible CPU/RAM/family specifications.
func ResolveFlexibleSpec(cfg *JobConfig) error {
	// Set defaults for CPU if not specified
	if cfg.CPUMin == 0 {
		cfg.CPUMin = 2 // Default minimum 2 vCPUs
	}

	// Default CPUMax to 2x CPUMin for bounded spot diversification.
	// Without this cap, cpu=4 would match all instances with 4+ vCPUs,
	// and price-capacity-optimized could select a much larger instance.
	if cfg.CPUMax == 0 {
		cfg.CPUMax = cfg.CPUMin * 2
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
		Gen:      cfg.Gen,
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

	return nil
}

