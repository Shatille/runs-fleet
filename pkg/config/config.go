// Package config manages application configuration from environment variables.
package config

import (
	"fmt"
	"os"
	"strings"
)

// Config holds all application configuration loaded from environment variables.
type Config struct {
	AWSRegion string

	GitHubOrg           string
	GitHubWebhookSecret string
	GitHubAppID         string
	GitHubAppPrivateKey string

	QueueURL            string
	PoolQueueURL        string
	EventsQueueURL      string
	TerminationQueueURL string
	LocksTableName   string
	JobsTableName    string
	PoolsTableName   string
	CacheBucketName  string
	ConfigBucketName string

	VPCID              string
	PublicSubnetIDs    []string
	PrivateSubnetIDs   []string
	SecurityGroupID    string
	InstanceProfileARN string
	KeyName            string

	SpotEnabled        bool
	MaxRuntimeMinutes  int
	LogLevel           string
	LaunchTemplateName string
}

// Load reads configuration from environment variables and validates required fields.
func Load() (*Config, error) {
	maxRuntimeMinutes, err := getEnvInt("RUNS_FLEET_MAX_RUNTIME_MINUTES", 360)
	if err != nil {
		return nil, fmt.Errorf("config error: %w", err)
	}

	cfg := &Config{
		AWSRegion:           getEnv("AWS_REGION", "ap-northeast-1"),
		GitHubOrg:           getEnv("RUNS_FLEET_GITHUB_ORG", ""),
		GitHubWebhookSecret: getEnv("RUNS_FLEET_GITHUB_WEBHOOK_SECRET", ""),
		GitHubAppID:         getEnv("RUNS_FLEET_GITHUB_APP_ID", ""),
		GitHubAppPrivateKey: getEnv("RUNS_FLEET_GITHUB_APP_PRIVATE_KEY", ""),
		QueueURL:            getEnv("RUNS_FLEET_QUEUE_URL", ""),
		PoolQueueURL:        getEnv("RUNS_FLEET_POOL_QUEUE_URL", ""),
		EventsQueueURL:      getEnv("RUNS_FLEET_EVENTS_QUEUE_URL", ""),
		TerminationQueueURL: getEnv("RUNS_FLEET_TERMINATION_QUEUE_URL", ""),
		LocksTableName:      getEnv("RUNS_FLEET_LOCKS_TABLE", ""),
		JobsTableName:       getEnv("RUNS_FLEET_JOBS_TABLE", ""),
		PoolsTableName:      getEnv("RUNS_FLEET_POOLS_TABLE", ""),
		CacheBucketName:     getEnv("RUNS_FLEET_CACHE_BUCKET", ""),
		ConfigBucketName:    getEnv("RUNS_FLEET_CONFIG_BUCKET", ""),
		VPCID:               getEnv("RUNS_FLEET_VPC_ID", ""),
		PublicSubnetIDs:     splitAndFilter(getEnv("RUNS_FLEET_PUBLIC_SUBNET_IDS", "")),
		PrivateSubnetIDs:    splitAndFilter(getEnv("RUNS_FLEET_PRIVATE_SUBNET_IDS", "")),
		SecurityGroupID:     getEnv("RUNS_FLEET_SECURITY_GROUP_ID", ""),
		InstanceProfileARN:  getEnv("RUNS_FLEET_INSTANCE_PROFILE_ARN", ""),
		KeyName:             getEnv("RUNS_FLEET_KEY_NAME", ""),
		SpotEnabled:         getEnv("RUNS_FLEET_SPOT_ENABLED", "true") == "true",
		MaxRuntimeMinutes:   maxRuntimeMinutes,
		LogLevel:            getEnv("RUNS_FLEET_LOG_LEVEL", "info"),
		LaunchTemplateName:  getEnv("RUNS_FLEET_LAUNCH_TEMPLATE_NAME", "runs-fleet-runner"),
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("config validation failed: %w", err)
	}

	return cfg, nil
}

// Validate checks that all required configuration fields are present and valid.
func (c *Config) Validate() error {
	if c.GitHubOrg == "" {
		return fmt.Errorf("RUNS_FLEET_GITHUB_ORG is required")
	}
	if c.GitHubWebhookSecret == "" {
		return fmt.Errorf("RUNS_FLEET_GITHUB_WEBHOOK_SECRET is required")
	}
	if c.GitHubAppID == "" {
		return fmt.Errorf("RUNS_FLEET_GITHUB_APP_ID is required")
	}
	if c.GitHubAppPrivateKey == "" {
		return fmt.Errorf("RUNS_FLEET_GITHUB_APP_PRIVATE_KEY is required")
	}
	if c.QueueURL == "" {
		return fmt.Errorf("RUNS_FLEET_QUEUE_URL is required")
	}
	if c.VPCID == "" {
		return fmt.Errorf("RUNS_FLEET_VPC_ID is required")
	}
	if c.SecurityGroupID == "" {
		return fmt.Errorf("RUNS_FLEET_SECURITY_GROUP_ID is required")
	}
	if c.InstanceProfileARN == "" {
		return fmt.Errorf("RUNS_FLEET_INSTANCE_PROFILE_ARN is required")
	}
	if len(c.PublicSubnetIDs) == 0 && len(c.PrivateSubnetIDs) == 0 {
		return fmt.Errorf("at least one of RUNS_FLEET_PUBLIC_SUBNET_IDS or RUNS_FLEET_PRIVATE_SUBNET_IDS is required")
	}
	if c.MaxRuntimeMinutes <= 0 {
		return fmt.Errorf("RUNS_FLEET_MAX_RUNTIME_MINUTES must be greater than 0, got %d", c.MaxRuntimeMinutes)
	}
	if c.MaxRuntimeMinutes > 1440 {
		return fmt.Errorf("RUNS_FLEET_MAX_RUNTIME_MINUTES must not exceed 1440 (24 hours), got %d", c.MaxRuntimeMinutes)
	}
	return nil
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getEnvInt(key string, defaultValue int) (int, error) {
	if value := os.Getenv(key); value != "" {
		var result int
		if _, err := fmt.Sscanf(value, "%d", &result); err != nil {
			return 0, fmt.Errorf("invalid integer for %s=%q: %w", key, value, err)
		}
		return result, nil
	}
	return defaultValue, nil
}

func splitAndFilter(s string) []string {
	if s == "" {
		return []string{}
	}
	parts := strings.Split(s, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}
