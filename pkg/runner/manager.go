package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"github.com/Shavakan/runs-fleet/pkg/cache"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

// SSMAPI defines SSM operations for runner configuration.
type SSMAPI interface {
	PutParameter(ctx context.Context, params *ssm.PutParameterInput, optFns ...func(*ssm.Options)) (*ssm.PutParameterOutput, error)
	DeleteParameter(ctx context.Context, params *ssm.DeleteParameterInput, optFns ...func(*ssm.Options)) (*ssm.DeleteParameterOutput, error)
}

// ManagerConfig holds configuration for the runner manager.
type ManagerConfig struct {
	Org         string
	CacheSecret string
	CacheURL    string
}

// Manager handles runner registration and SSM configuration.
type Manager struct {
	github    *GitHubClient
	ssmClient SSMAPI
	config    ManagerConfig
}

// NewManager creates a new runner manager.
func NewManager(awsCfg aws.Config, githubClient *GitHubClient, config ManagerConfig) *Manager {
	return &Manager{
		github:    githubClient,
		ssmClient: ssm.NewFromConfig(awsCfg),
		config:    config,
	}
}

// Config represents the configuration stored in SSM for a runner.
type Config struct {
	Org         string   `json:"org"`
	Repo        string   `json:"repo,omitempty"`
	JITToken    string   `json:"jit_token"`
	Labels      []string `json:"labels"`
	RunnerGroup string   `json:"runner_group,omitempty"`
	JobID       string   `json:"job_id,omitempty"`
	CacheToken  string   `json:"cache_token,omitempty"`
	CacheURL    string   `json:"cache_url,omitempty"`
}

// PrepareRunnerRequest contains parameters for preparing a runner.
type PrepareRunnerRequest struct {
	InstanceID string
	JobID      string
	RunID      string
	Repo       string // owner/repo format for repo-level registration
	Labels     []string
}

// PrepareRunner creates the SSM parameter with runner configuration.
// This should be called after the EC2 instance is created but before it boots.
func (m *Manager) PrepareRunner(ctx context.Context, req PrepareRunnerRequest) error {
	// Generate runner name
	runnerName := fmt.Sprintf("runs-fleet-%s", req.InstanceID)

	// Get JIT token from GitHub
	log.Printf("Getting JIT token for runner %s", runnerName)
	jitToken, err := m.github.GetRegistrationToken(ctx, req.Repo)
	if err != nil {
		return fmt.Errorf("failed to get JIT token: %w", err)
	}

	// Generate cache token
	cacheToken := ""
	if m.config.CacheSecret != "" {
		cacheToken = cache.GenerateCacheToken(m.config.CacheSecret, req.JobID, req.InstanceID)
	}

	// Build runner config
	config := Config{
		Org:        m.config.Org,
		JITToken:   jitToken,
		Labels:     req.Labels,
		JobID:      req.JobID,
		CacheToken: cacheToken,
		CacheURL:   m.config.CacheURL,
	}

	// Serialize to JSON
	configJSON, err := json.Marshal(config)
	if err != nil {
		return fmt.Errorf("failed to marshal runner config: %w", err)
	}

	// Store in SSM
	paramPath := fmt.Sprintf("/runs-fleet/runners/%s/config", req.InstanceID)
	log.Printf("Storing runner config in SSM: %s", paramPath)

	_, err = m.ssmClient.PutParameter(ctx, &ssm.PutParameterInput{
		Name:      aws.String(paramPath),
		Value:     aws.String(string(configJSON)),
		Type:      types.ParameterTypeSecureString,
		Overwrite: aws.Bool(true),
		Tags: []types.Tag{
			{
				Key:   aws.String("runs-fleet:managed"),
				Value: aws.String("true"),
			},
			{
				Key:   aws.String("runs-fleet:job-id"),
				Value: aws.String(req.JobID),
			},
		},
	})
	if err != nil {
		return fmt.Errorf("failed to store runner config in SSM: %w", err)
	}

	log.Printf("Runner config stored successfully for instance %s", req.InstanceID)
	return nil
}

// CleanupRunner deletes the SSM parameter for a runner.
func (m *Manager) CleanupRunner(ctx context.Context, instanceID string) error {
	paramPath := fmt.Sprintf("/runs-fleet/runners/%s/config", instanceID)

	_, err := m.ssmClient.DeleteParameter(ctx, &ssm.DeleteParameterInput{
		Name: aws.String(paramPath),
	})
	if err != nil {
		return fmt.Errorf("failed to delete runner config from SSM: %w", err)
	}

	return nil
}
