package ec2

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Shavakan/runs-fleet/pkg/provider"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

// SSMAPI defines SSM operations for runner configuration.
type SSMAPI interface {
	PutParameter(ctx context.Context, params *ssm.PutParameterInput, optFns ...func(*ssm.Options)) (*ssm.PutParameterOutput, error)
	DeleteParameter(ctx context.Context, params *ssm.DeleteParameterInput, optFns ...func(*ssm.Options)) (*ssm.DeleteParameterOutput, error)
}

// ConfigStore implements provider.ConfigStore using SSM Parameter Store.
type ConfigStore struct {
	ssmClient           SSMAPI
	terminationQueueURL string
}

// NewConfigStore creates an SSM-backed config store.
func NewConfigStore(awsCfg aws.Config, terminationQueueURL string) *ConfigStore {
	return &ConfigStore{
		ssmClient:           ssm.NewFromConfig(awsCfg),
		terminationQueueURL: terminationQueueURL,
	}
}

// NewConfigStoreWithClient creates a config store with a custom SSM client (for testing).
func NewConfigStoreWithClient(client SSMAPI, terminationQueueURL string) *ConfigStore {
	return &ConfigStore{
		ssmClient:           client,
		terminationQueueURL: terminationQueueURL,
	}
}

// runnerConfig represents the configuration stored in SSM for a runner.
type runnerConfig struct {
	Org                 string   `json:"org"`
	Repo                string   `json:"repo,omitempty"`
	RunID               string   `json:"run_id"`
	JITToken            string   `json:"jit_token"`
	Labels              []string `json:"labels"`
	RunnerGroup         string   `json:"runner_group,omitempty"`
	JobID               string   `json:"job_id,omitempty"`
	CacheToken          string   `json:"cache_token,omitempty"`
	CacheURL            string   `json:"cache_url,omitempty"`
	TerminationQueueURL string   `json:"termination_queue_url,omitempty"`
	IsOrg               bool     `json:"is_org"`
}

// StoreRunnerConfig stores runner configuration in SSM Parameter Store.
func (c *ConfigStore) StoreRunnerConfig(ctx context.Context, req *provider.StoreConfigRequest) error {
	// Extract org from repo string (owner/repo format)
	org, err := extractOrg(req.Repo)
	if err != nil {
		return fmt.Errorf("invalid repo format: %w", err)
	}

	config := runnerConfig{
		Org:                 org,
		Repo:                req.Repo,
		RunID:               req.RunID,
		JITToken:            req.JITToken,
		Labels:              req.Labels,
		JobID:               req.JobID,
		CacheToken:          req.CacheToken,
		CacheURL:            req.CacheURL,
		TerminationQueueURL: c.terminationQueueURL,
		IsOrg:               false,
	}

	configJSON, err := json.Marshal(config)
	if err != nil {
		return fmt.Errorf("failed to marshal runner config: %w", err)
	}

	paramPath := fmt.Sprintf("/runs-fleet/runners/%s/config", req.RunnerID)

	_, err = c.ssmClient.PutParameter(ctx, &ssm.PutParameterInput{
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

	return nil
}

// DeleteRunnerConfig deletes runner configuration from SSM Parameter Store.
func (c *ConfigStore) DeleteRunnerConfig(ctx context.Context, runnerID string) error {
	paramPath := fmt.Sprintf("/runs-fleet/runners/%s/config", runnerID)

	_, err := c.ssmClient.DeleteParameter(ctx, &ssm.DeleteParameterInput{
		Name: aws.String(paramPath),
	})
	if err != nil {
		return fmt.Errorf("failed to delete runner config from SSM: %w", err)
	}

	return nil
}

// extractOrg extracts the organization from a repo string (owner/repo format).
func extractOrg(repo string) (string, error) {
	parts := strings.SplitN(repo, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", fmt.Errorf("expected owner/repo format, got %q", repo)
	}
	return parts[0], nil
}

// Ensure ConfigStore implements provider.ConfigStore.
var _ provider.ConfigStore = (*ConfigStore)(nil)
