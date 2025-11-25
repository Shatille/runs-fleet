// Package gitops provides configuration-as-code support for runs-fleet.
// It watches for configuration changes in GitHub repositories and applies them to DynamoDB.
package gitops

import (
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"strings"

	"github.com/Shavakan/runs-fleet/pkg/db"
	"gopkg.in/yaml.v3"
)

// Config represents the runs-fleet configuration file schema.
// This maps to .github-private/.github/runs-fleet.yml
type Config struct {
	Version string       `yaml:"version"`
	Pools   []PoolConfig `yaml:"pools"`
	Runners []RunnerSpec `yaml:"runners,omitempty"`
}

// PoolConfig defines a warm pool configuration.
type PoolConfig struct {
	Name               string         `yaml:"name"`
	InstanceType       string         `yaml:"instance_type"`
	DesiredRunning     int            `yaml:"desired_running"`
	DesiredStopped     int            `yaml:"desired_stopped"`
	IdleTimeoutMinutes int            `yaml:"idle_timeout_minutes,omitempty"`
	Schedules          []PoolSchedule `yaml:"schedules,omitempty"`
	Environment        string         `yaml:"environment,omitempty"`
	Region             string         `yaml:"region,omitempty"`
}

// PoolSchedule defines time-based pool sizing.
type PoolSchedule struct {
	Name           string `yaml:"name"`
	StartHour      int    `yaml:"start_hour"`      // 0-23
	EndHour        int    `yaml:"end_hour"`        // 0-23
	DaysOfWeek     []int  `yaml:"days_of_week"`    // 0=Sunday, 1=Monday, etc.
	DesiredRunning int    `yaml:"desired_running"`
	DesiredStopped int    `yaml:"desired_stopped"`
}

// RunnerSpec defines a custom runner specification.
type RunnerSpec struct {
	Name         string   `yaml:"name"`         // e.g., "4cpu-linux-arm64"
	InstanceType string   `yaml:"instance_type"` // e.g., "c7g.xlarge"
	Labels       []string `yaml:"labels,omitempty"`
}

// GitHubAPI defines GitHub operations for fetching configuration.
type GitHubAPI interface {
	GetFileContent(ctx context.Context, owner, repo, path, ref string) ([]byte, error)
}

// DBAPI defines database operations for applying configuration.
type DBAPI interface {
	SavePoolConfig(ctx context.Context, config *db.PoolConfig) error
	GetPoolConfig(ctx context.Context, poolName string) (*db.PoolConfig, error)
}

// Watcher monitors GitHub repositories for configuration changes.
type Watcher struct {
	githubClient  GitHubAPI
	dbClient      DBAPI
	configPath    string
	configRepo    string
	configOwner   string
	runnerSpecs   map[string]RunnerSpec // Cache of runner specs
}

// NewWatcher creates a new configuration watcher.
func NewWatcher(gh GitHubAPI, db DBAPI, owner, repo, path string) *Watcher {
	if path == "" {
		path = ".github/runs-fleet.yml"
	}
	return &Watcher{
		githubClient: gh,
		dbClient:     db,
		configPath:   path,
		configRepo:   repo,
		configOwner:  owner,
		runnerSpecs:  make(map[string]RunnerSpec),
	}
}

// ParseConfig parses a runs-fleet configuration from YAML bytes.
func ParseConfig(data []byte) (*Config, error) {
	var config Config
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("failed to parse YAML: %w", err)
	}
	return &config, nil
}

// Validate checks the configuration for errors.
func (c *Config) Validate() error {
	if c.Version == "" {
		return fmt.Errorf("version is required")
	}
	if c.Version != "1" && c.Version != "1.0" {
		return fmt.Errorf("unsupported config version: %s (supported: 1, 1.0)", c.Version)
	}

	poolNames := make(map[string]bool)
	for i, pool := range c.Pools {
		if pool.Name == "" {
			return fmt.Errorf("pool[%d]: name is required", i)
		}
		if poolNames[pool.Name] {
			return fmt.Errorf("pool[%d]: duplicate pool name: %s", i, pool.Name)
		}
		poolNames[pool.Name] = true

		if pool.InstanceType == "" {
			return fmt.Errorf("pool[%d] %s: instance_type is required", i, pool.Name)
		}
		if pool.DesiredRunning < 0 {
			return fmt.Errorf("pool[%d] %s: desired_running must be non-negative", i, pool.Name)
		}
		if pool.DesiredStopped < 0 {
			return fmt.Errorf("pool[%d] %s: desired_stopped must be non-negative", i, pool.Name)
		}

		for j, sched := range pool.Schedules {
			if err := validateSchedule(sched, i, j, pool.Name); err != nil {
				return err
			}
		}

		if pool.Environment != "" {
			if pool.Environment != "dev" && pool.Environment != "staging" && pool.Environment != "prod" {
				return fmt.Errorf("pool[%d] %s: environment must be dev, staging, or prod", i, pool.Name)
			}
		}
	}

	runnerNames := make(map[string]bool)
	for i, runner := range c.Runners {
		if runner.Name == "" {
			return fmt.Errorf("runner[%d]: name is required", i)
		}
		if runnerNames[runner.Name] {
			return fmt.Errorf("runner[%d]: duplicate runner name: %s", i, runner.Name)
		}
		runnerNames[runner.Name] = true

		if runner.InstanceType == "" {
			return fmt.Errorf("runner[%d] %s: instance_type is required", i, runner.Name)
		}
	}

	return nil
}

func validateSchedule(sched PoolSchedule, poolIdx, schedIdx int, poolName string) error {
	if sched.StartHour < 0 || sched.StartHour > 23 {
		return fmt.Errorf("pool[%d] %s: schedule[%d]: start_hour must be 0-23", poolIdx, poolName, schedIdx)
	}
	if sched.EndHour < 0 || sched.EndHour > 23 {
		return fmt.Errorf("pool[%d] %s: schedule[%d]: end_hour must be 0-23", poolIdx, poolName, schedIdx)
	}
	for _, day := range sched.DaysOfWeek {
		if day < 0 || day > 6 {
			return fmt.Errorf("pool[%d] %s: schedule[%d]: days_of_week values must be 0-6", poolIdx, poolName, schedIdx)
		}
	}
	if sched.DesiredRunning < 0 {
		return fmt.Errorf("pool[%d] %s: schedule[%d]: desired_running must be non-negative", poolIdx, poolName, schedIdx)
	}
	if sched.DesiredStopped < 0 {
		return fmt.Errorf("pool[%d] %s: schedule[%d]: desired_stopped must be non-negative", poolIdx, poolName, schedIdx)
	}
	return nil
}

// HandlePushEvent processes a GitHub push event to check for configuration changes.
func (w *Watcher) HandlePushEvent(ctx context.Context, owner, repo, ref string, modifiedFiles []string) error {
	// Check if this is the config repository
	if owner != w.configOwner || repo != w.configRepo {
		return nil // Not our config repo
	}

	// Check if the config file was modified
	configModified := false
	for _, file := range modifiedFiles {
		if file == w.configPath || strings.HasSuffix(file, "/runs-fleet.yml") {
			configModified = true
			break
		}
	}

	if !configModified {
		return nil
	}

	log.Printf("Configuration file modified in %s/%s, reloading...", owner, repo)

	// Fetch and apply the new configuration
	return w.ReloadConfig(ctx, ref)
}

// ReloadConfig fetches the configuration from GitHub and applies it.
func (w *Watcher) ReloadConfig(ctx context.Context, ref string) error {
	content, err := w.githubClient.GetFileContent(ctx, w.configOwner, w.configRepo, w.configPath, ref)
	if err != nil {
		return fmt.Errorf("failed to fetch config: %w", err)
	}

	config, err := ParseConfig(content)
	if err != nil {
		return fmt.Errorf("failed to parse config: %w", err)
	}

	if err := config.Validate(); err != nil {
		return fmt.Errorf("invalid config: %w", err)
	}

	return w.ApplyConfig(ctx, config)
}

// ApplyConfig applies the configuration to DynamoDB.
func (w *Watcher) ApplyConfig(ctx context.Context, config *Config) error {
	// Apply pool configurations
	for _, pool := range config.Pools {
		dbConfig := &db.PoolConfig{
			PoolName:           pool.Name,
			InstanceType:       pool.InstanceType,
			DesiredRunning:     pool.DesiredRunning,
			DesiredStopped:     pool.DesiredStopped,
			IdleTimeoutMinutes: pool.IdleTimeoutMinutes,
			Environment:        pool.Environment,
			Region:             pool.Region,
		}

		// Convert schedules
		for _, sched := range pool.Schedules {
			dbConfig.Schedules = append(dbConfig.Schedules, db.PoolSchedule{
				Name:           sched.Name,
				StartHour:      sched.StartHour,
				EndHour:        sched.EndHour,
				DaysOfWeek:     sched.DaysOfWeek,
				DesiredRunning: sched.DesiredRunning,
				DesiredStopped: sched.DesiredStopped,
			})
		}

		if err := w.dbClient.SavePoolConfig(ctx, dbConfig); err != nil {
			return fmt.Errorf("failed to save pool config %s: %w", pool.Name, err)
		}
		log.Printf("Applied pool configuration: %s", pool.Name)
	}

	// Cache runner specs for label parsing
	for _, runner := range config.Runners {
		w.runnerSpecs[runner.Name] = runner
	}
	log.Printf("Loaded %d runner specs", len(config.Runners))

	return nil
}

// GetRunnerSpec returns the runner specification for a given name.
func (w *Watcher) GetRunnerSpec(name string) (RunnerSpec, bool) {
	spec, ok := w.runnerSpecs[name]
	return spec, ok
}

// DecodeBase64Content decodes base64-encoded content from GitHub API.
func DecodeBase64Content(encoded string) ([]byte, error) {
	// Remove any whitespace/newlines that GitHub might add
	encoded = strings.ReplaceAll(encoded, "\n", "")
	encoded = strings.ReplaceAll(encoded, " ", "")

	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("failed to decode base64: %w", err)
	}
	return decoded, nil
}
