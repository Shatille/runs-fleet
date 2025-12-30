package secrets

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

// SSMAPI defines SSM operations required by SSMStore.
type SSMAPI interface {
	PutParameter(ctx context.Context, params *ssm.PutParameterInput, optFns ...func(*ssm.Options)) (*ssm.PutParameterOutput, error)
	GetParameter(ctx context.Context, params *ssm.GetParameterInput, optFns ...func(*ssm.Options)) (*ssm.GetParameterOutput, error)
	DeleteParameter(ctx context.Context, params *ssm.DeleteParameterInput, optFns ...func(*ssm.Options)) (*ssm.DeleteParameterOutput, error)
	GetParametersByPath(ctx context.Context, params *ssm.GetParametersByPathInput, optFns ...func(*ssm.Options)) (*ssm.GetParametersByPathOutput, error)
}

// SSMStore implements Store using AWS SSM Parameter Store.
type SSMStore struct {
	client SSMAPI
	prefix string
}

// NewSSMStore creates an SSM-backed secrets store.
func NewSSMStore(awsCfg aws.Config, prefix string) *SSMStore {
	if prefix == "" {
		prefix = DefaultSSMPrefix
	}
	return &SSMStore{
		client: ssm.NewFromConfig(awsCfg),
		prefix: prefix,
	}
}

// NewSSMStoreWithClient creates an SSM store with a custom client (for testing).
func NewSSMStoreWithClient(client SSMAPI, prefix string) *SSMStore {
	if prefix == "" {
		prefix = DefaultSSMPrefix
	}
	return &SSMStore{
		client: client,
		prefix: prefix,
	}
}

// Put stores runner configuration in SSM Parameter Store as SecureString.
func (s *SSMStore) Put(ctx context.Context, runnerID string, config *RunnerConfig) error {
	configJSON, err := json.Marshal(config)
	if err != nil {
		return fmt.Errorf("failed to marshal runner config: %w", err)
	}

	paramPath := s.parameterPath(runnerID)

	_, err = s.client.PutParameter(ctx, &ssm.PutParameterInput{
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
				Value: aws.String(config.JobID),
			},
		},
	})
	if err != nil {
		return fmt.Errorf("failed to store runner config in SSM: %w", err)
	}

	return nil
}

// Get retrieves runner configuration from SSM Parameter Store.
func (s *SSMStore) Get(ctx context.Context, runnerID string) (*RunnerConfig, error) {
	paramPath := s.parameterPath(runnerID)

	output, err := s.client.GetParameter(ctx, &ssm.GetParameterInput{
		Name:           aws.String(paramPath),
		WithDecryption: aws.Bool(true),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get runner config from SSM: %w", err)
	}

	if output.Parameter == nil || output.Parameter.Value == nil {
		return nil, fmt.Errorf("parameter value is nil")
	}

	var config RunnerConfig
	if err := json.Unmarshal([]byte(*output.Parameter.Value), &config); err != nil {
		return nil, fmt.Errorf("failed to parse runner config: %w", err)
	}

	return &config, nil
}

// Delete removes runner configuration from SSM Parameter Store.
func (s *SSMStore) Delete(ctx context.Context, runnerID string) error {
	paramPath := s.parameterPath(runnerID)

	_, err := s.client.DeleteParameter(ctx, &ssm.DeleteParameterInput{
		Name: aws.String(paramPath),
	})
	if err != nil {
		if strings.Contains(err.Error(), "ParameterNotFound") {
			return nil
		}
		return fmt.Errorf("failed to delete runner config from SSM: %w", err)
	}

	return nil
}

// List returns all runner IDs with stored configuration.
func (s *SSMStore) List(ctx context.Context) ([]string, error) {
	var runnerIDs []string
	var nextToken *string

	path := s.prefix + "/"

	for {
		input := &ssm.GetParametersByPathInput{
			Path:      aws.String(path),
			Recursive: aws.Bool(true),
			NextToken: nextToken,
		}

		output, err := s.client.GetParametersByPath(ctx, input)
		if err != nil {
			return nil, fmt.Errorf("failed to list parameters: %w", err)
		}

		for _, param := range output.Parameters {
			if param.Name == nil {
				continue
			}

			runnerID := s.extractRunnerID(*param.Name)
			if runnerID != "" {
				runnerIDs = append(runnerIDs, runnerID)
			}
		}

		nextToken = output.NextToken
		if nextToken == nil {
			break
		}
	}

	return runnerIDs, nil
}

// parameterPath returns the full SSM parameter path for a runner ID.
func (s *SSMStore) parameterPath(runnerID string) string {
	return fmt.Sprintf("%s/%s/config", s.prefix, runnerID)
}

// extractRunnerID extracts the runner ID from a parameter path.
// Expected format: {prefix}/{runner-id}/config
func (s *SSMStore) extractRunnerID(paramPath string) string {
	trimmed := strings.TrimPrefix(paramPath, s.prefix+"/")
	parts := strings.Split(trimmed, "/")
	// Must have at least 2 parts: runner-id and config
	if len(parts) >= 2 && parts[len(parts)-1] == "config" {
		return parts[0]
	}
	return ""
}

// Ensure SSMStore implements Store.
var _ Store = (*SSMStore)(nil)
