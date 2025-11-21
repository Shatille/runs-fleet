package config

import (
	"fmt"
	"os"
	"strings"
)

type Config struct {
	AWSRegion string

	GitHubOrg           string
	GitHubWebhookSecret string
	GitHubAppID         string
	GitHubAppPrivateKey string

	QueueURL         string
	PoolQueueURL     string
	EventsQueueURL   string
	LocksTableName   string
	JobsTableName    string
	CacheBucketName  string
	ConfigBucketName string

	VPCID               string
	PublicSubnetIDs     []string
	PrivateSubnetIDs    []string
	SecurityGroupID     string
	InstanceProfileARN  string
	KeyName             string

	SpotEnabled        bool
	MaxRuntimeMinutes  int
	LogLevel           string
}

func Load() (*Config, error) {
	cfg := &Config{
		AWSRegion:           getEnv("AWS_REGION", "ap-northeast-1"),
		GitHubOrg:           getEnv("RUNS_FLEET_GITHUB_ORG", ""),
		GitHubWebhookSecret: getEnv("RUNS_FLEET_GITHUB_WEBHOOK_SECRET", ""),
		GitHubAppID:         getEnv("RUNS_FLEET_GITHUB_APP_ID", ""),
		GitHubAppPrivateKey: getEnv("RUNS_FLEET_GITHUB_APP_PRIVATE_KEY", ""),
		QueueURL:            getEnv("RUNS_FLEET_QUEUE_URL", ""),
		PoolQueueURL:        getEnv("RUNS_FLEET_POOL_QUEUE_URL", ""),
		EventsQueueURL:      getEnv("RUNS_FLEET_EVENTS_QUEUE_URL", ""),
		LocksTableName:      getEnv("RUNS_FLEET_LOCKS_TABLE", ""),
		JobsTableName:       getEnv("RUNS_FLEET_JOBS_TABLE", ""),
		CacheBucketName:     getEnv("RUNS_FLEET_CACHE_BUCKET", ""),
		ConfigBucketName:    getEnv("RUNS_FLEET_CONFIG_BUCKET", ""),
		VPCID:               getEnv("RUNS_FLEET_VPC_ID", ""),
		PublicSubnetIDs:     strings.Split(getEnv("RUNS_FLEET_PUBLIC_SUBNET_IDS", ""), ","),
		PrivateSubnetIDs:    strings.Split(getEnv("RUNS_FLEET_PRIVATE_SUBNET_IDS", ""), ","),
		SecurityGroupID:     getEnv("RUNS_FLEET_SECURITY_GROUP_ID", ""),
		InstanceProfileARN:  getEnv("RUNS_FLEET_INSTANCE_PROFILE_ARN", ""),
		KeyName:             getEnv("RUNS_FLEET_KEY_NAME", ""),
		SpotEnabled:         getEnv("RUNS_FLEET_SPOT_ENABLED", "true") == "true",
		MaxRuntimeMinutes:   getEnvInt("RUNS_FLEET_MAX_RUNTIME_MINUTES", 360),
		LogLevel:            getEnv("RUNS_FLEET_LOG_LEVEL", "info"),
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("config validation failed: %w", err)
	}

	return cfg, nil
}

func (c *Config) Validate() error {
	if c.GitHubOrg == "" {
		return fmt.Errorf("RUNS_FLEET_GITHUB_ORG is required")
	}
	if c.QueueURL == "" {
		return fmt.Errorf("RUNS_FLEET_QUEUE_URL is required")
	}
	if c.VPCID == "" {
		return fmt.Errorf("RUNS_FLEET_VPC_ID is required")
	}
	return nil
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getEnvInt(key string, defaultValue int) int {
	if value := os.Getenv(key); value != "" {
		var result int
		if _, err := fmt.Sscanf(value, "%d", &result); err == nil {
			return result
		}
	}
	return defaultValue
}
