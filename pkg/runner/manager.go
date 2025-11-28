package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"

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
	IsOrg       bool     `json:"is_org"`
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
	// Extract org from repo string (owner/repo format, required)
	if req.Repo == "" {
		return fmt.Errorf("repo is required (owner/repo format)")
	}
	parts := strings.SplitN(req.Repo, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return fmt.Errorf("invalid repo format, expected owner/repo: %s", req.Repo)
	}
	org := parts[0]

	// Generate runner name
	runnerName := fmt.Sprintf("runs-fleet-%s", req.InstanceID)

	// Get registration token from GitHub (returns token and whether owner is an org)
	log.Printf("Getting registration token for runner %s (org=%s, repo=%s)", runnerName, org, req.Repo)
	regResult, err := m.github.GetRegistrationToken(ctx, req.Repo)
	if err != nil {
		return fmt.Errorf("failed to get registration token: %w", err)
	}

	// Generate cache token
	cacheToken := ""
	if m.config.CacheSecret != "" {
		cacheToken = cache.GenerateCacheToken(m.config.CacheSecret, req.JobID, req.InstanceID)
	}

	// Build runner config with dynamic org from repo
	config := Config{
		Org:        org,
		Repo:       req.Repo,
		JITToken:   regResult.Token,
		Labels:     req.Labels,
		JobID:      req.JobID,
		CacheToken: cacheToken,
		CacheURL:   m.config.CacheURL,
		IsOrg:      regResult.IsOrg,
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
		Name:  aws.String(paramPath),
		Value: aws.String(string(configJSON)),
		Type:  types.ParameterTypeSecureString,
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
