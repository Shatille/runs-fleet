// Package github provides a GitHub App API client (auth, registration
// tokens, workflow-job status), webhook validation, and label parsing for
// GitHub Actions workflow jobs.
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

// normalizeArch maps an architecture token to runs-fleet's canonical form,
// accepting common synonyms (x64/x86_64 for amd64, aarch64 for arm64) so alias
// specs and labels written in other ecosystems' vocabulary resolve correctly.
// ok is false for unrecognized values, which the caller leaves unset.
func normalizeArch(value string) (arch string, ok bool) {
	switch value {
	case ArchARM64, "aarch64":
		return ArchARM64, true
	case ArchAMD64, "x64", "x86_64":
		return ArchAMD64, true
	default:
		return "", false
	}
}

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
	RunID        string // Optional; only set by the legacy runs-fleet=<run-id> form
	InstanceType string
	Pool         string
	Spot         bool
	Arch         string // Architecture (amd64, arm64)

	// Flexible instance selection
	InstanceTypes []string // Multiple instance types for spot diversification
	CPUMin        int      // Minimum vCPUs (from cpu= label)
	CPUMax        int      // Maximum vCPUs (from cpu= label, e.g., cpu=4+16)
	RAMMin        float64  // Minimum RAM in GB (from ram= label)
	RAMMax        float64  // Maximum RAM in GB (from ram= label, e.g., ram=8+32)
	Families      []string // Instance families (from family= label, e.g., family=c7g+m7g)
	Gen           int      // Instance generation (from gen= label, e.g., gen=8). 0 = any generation
	OriginalLabel string   // Original runs-fleet label from workflow (for GitHub matching)

	// AliasLabel is the externally-defined custom label that matched a configured
	// alias rule, or empty when the job used a native runs-fleet marker. It is the
	// observability discriminator (OriginalLabel alone can't distinguish an alias
	// from a marker, since it carries both) and records which alias was hit.
	AliasLabel string

	// Storage configuration
	StorageGiB int // Disk storage in GiB (from disk= label, e.g., disk=100)
}

// markerPrefix is the bare runs-fleet marker that identifies a runner label.
const markerPrefix = "runs-fleet"

// ParseLabels extracts runner configuration from runs-fleet workflow job labels.
//
// The label is recognized in three forms, all of which parse the spec
// (cpu/ram/family/gen/arch/pool/spot/disk) identically:
//
//	runs-fleet                                            (marker only, all defaults)
//	runs-fleet/cpu=4/arch=arm64/pool=default/spot=true    (marker + spec, no run-id)
//	runs-fleet=<run-id>/cpu=4/arch=arm64/pool=default     (legacy, marker carries run-id)
//
// run_id is optional in the label; the legacy form still populates RunID from
// the segment after "runs-fleet=", but downstream prefers the webhook's run_id.
//
// CPU and RAM ranges (min+max) enable spot diversification for better availability.
// Multiple instance families can be specified with + separator.
func ParseLabels(labels []string) (*JobConfig, error) {
	return ParseLabelsWithAliases(labels, nil)
}

// ParseLabelsWithAliases is ParseLabels with a configured alias resolver. The
// native runs-fleet marker always takes precedence; only when no marker is
// present is the resolver consulted, letting a job that targets an
// externally-defined custom label (e.g. an ARC scale-set label) be claimed and
// served by runs-fleet. The matched custom label is preserved as OriginalLabel
// so the booted runner registers under it and GitHub dispatches the job to it.
// A nil resolver disables aliasing, so behavior matches ParseLabels.
func ParseLabelsWithAliases(labels []string, resolver *AliasResolver) (*JobConfig, error) {
	cfg := &JobConfig{
		Spot: true,
		Arch: "", // Empty = arch doesn't matter, uses non-suffixed launch template
	}

	if runsFleetLabel, ok := findMarkerLabel(labels); ok {
		cfg.OriginalLabel = runsFleetLabel
		if err := parseLabelParts(cfg, labelSpecParts(cfg, runsFleetLabel)); err != nil {
			return nil, err
		}
	} else if aliasLabel, spec, ok := resolveAlias(labels, resolver); ok {
		cfg.OriginalLabel = aliasLabel
		cfg.AliasLabel = aliasLabel
		if err := parseLabelParts(cfg, strings.Split(spec, "/")); err != nil {
			return nil, fmt.Errorf("invalid alias spec for label %q: %w", aliasLabel, err)
		}
		// An aliased label doubles as its own warm pool so migrated workloads get
		// fast restarts (the ephemeral pool is auto-created downstream). Only
		// default it when the spec set no explicit pool and the label is a legal
		// pool name; otherwise the job simply cold-starts.
		if cfg.Pool == "" && isValidPoolName(aliasLabel) {
			cfg.Pool = aliasLabel
		}
	} else {
		return nil, errors.New("no runs-fleet label found")
	}

	if err := ResolveFlexibleSpec(cfg); err != nil {
		return nil, err
	}

	return cfg, nil
}

// resolveAlias returns the first runs-on label that matches a configured alias
// rule along with its resolved spec.
func resolveAlias(labels []string, resolver *AliasResolver) (label, spec string, ok bool) {
	if resolver == nil {
		return "", "", false
	}
	for _, l := range labels {
		if spec, matched := resolver.Resolve(l); matched {
			return l, spec, true
		}
	}
	return "", "", false
}

// findMarkerLabel returns the runs-fleet marker label from the label set. It
// matches the bare marker "runs-fleet", the new "runs-fleet/..." form, and the
// legacy "runs-fleet=..." form, while rejecting unrelated labels that merely
// share the prefix (e.g. "runs-fleet-foo", "runs-fleetish").
func findMarkerLabel(labels []string) (string, bool) {
	for _, label := range labels {
		if label == markerPrefix ||
			strings.HasPrefix(label, markerPrefix+"=") ||
			strings.HasPrefix(label, markerPrefix+"/") {
			return label, true
		}
	}
	return "", false
}

// labelSpecParts strips the marker (and any legacy run-id) from the label and
// returns the remaining "/"-separated spec segments. For the legacy
// "runs-fleet=<run-id>/..." form it also records RunID on cfg.
func labelSpecParts(cfg *JobConfig, label string) []string {
	if rest, ok := strings.CutPrefix(label, markerPrefix+"="); ok {
		// Legacy form: the first segment is the run-id, the rest is spec.
		parts := strings.Split(rest, "/")
		cfg.RunID = parts[0]
		return parts[1:]
	}

	rest := strings.TrimPrefix(label, markerPrefix)
	rest = strings.TrimPrefix(rest, "/")
	if rest == "" {
		return nil
	}
	return strings.Split(rest, "/")
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
			if arch, ok := normalizeArch(value); ok {
				cfg.Arch = arch
			}

		case "pool":
			cfg.Pool = value

		case "spot":
			cfg.Spot = value != "false"

		case "disk":
			diskGiB, err := parseBoundedInt(value, 1, 16384, "disk")
			if err != nil {
				return err
			}
			cfg.StorageGiB = diskGiB
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
