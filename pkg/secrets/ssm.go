package secrets

import (
	"context"
	"encoding/json"
	"errors"
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
//
// The config is written as two parameters: the plaintext fields under
// {prefix}/{id}/config, and the registration credential — a GitHub JIT config or
// registration token — under {prefix}/{id}/credential. A JIT config embeds a
// 2048-bit RSA private key and alone exceeds the 4096-character Standard-tier
// ceiling; combining both halves in one parameter forces the whole store onto the
// paid Advanced tier. Split, and with the credential stored decoded-and-gzipped
// (stripping a base64 layer GitHub applied before compressing), both halves stay
// under the ceiling and the store stays free.
func (s *SSMStore) Put(ctx context.Context, runnerID string, config *RunnerConfig) error {
	configJSON, err := marshalConfigHalf(config)
	if err != nil {
		return err
	}

	credential, err := packCredential(config)
	if err != nil {
		return fmt.Errorf("failed to pack runner credential: %w", err)
	}

	// Credential first: a config parameter visible without its credential is a
	// runner that boots and hangs, whereas an orphaned credential registers
	// nothing on its own. List reports either half, so the housekeeping sweep can
	// still see one left behind by a crash between these two writes.
	if putErr := s.putCredential(ctx, runnerID, credential); putErr != nil {
		return putErr
	}

	_, err = s.client.PutParameter(ctx, &ssm.PutParameterInput{
		Name:      aws.String(s.parameterPath(runnerID)),
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
				Value: aws.String(config.JobID),
			},
		},
	})
	if err != nil {
		return fmt.Errorf("failed to store runner config in SSM: %w", err)
	}

	return nil
}

// putCredential writes the packed credential. It carries no tags: a tag value is
// readable by anyone with DescribeTags, and this parameter registers a runner.
func (s *SSMStore) putCredential(ctx context.Context, runnerID, credential string) error {
	_, err := s.client.PutParameter(ctx, &ssm.PutParameterInput{
		Name:      aws.String(s.credentialPath(runnerID)),
		Value:     aws.String(credential),
		Type:      types.ParameterTypeSecureString,
		Overwrite: aws.Bool(true),
	})
	if err != nil {
		return fmt.Errorf("failed to store runner credential in SSM: %w", err)
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
		var notFound *types.ParameterNotFound
		if errors.As(err, &notFound) {
			return nil, fmt.Errorf("%s: %w", paramPath, ErrConfigNotFound)
		}
		return nil, fmt.Errorf("failed to get runner config from SSM: %w", err)
	}

	if output.Parameter == nil || output.Parameter.Value == nil {
		return nil, fmt.Errorf("parameter value is nil")
	}

	var config RunnerConfig
	if err := json.Unmarshal([]byte(*output.Parameter.Value), &config); err != nil {
		return nil, fmt.Errorf("failed to parse runner config: %w", err)
	}

	if err := s.loadCredential(ctx, runnerID, &config); err != nil {
		return nil, err
	}

	return &config, nil
}

// loadCredential reads the credential parameter and restores it onto config.
//
// An absent credential parameter is not an error: it is the legacy layout, where
// the credential rode inside the config parameter itself. Agents already booted
// from an older AMI read configs written by a newer orchestrator and vice versa,
// so both layouts must resolve for the length of an AMI rollout. config already
// carries the legacy credential in that case, having been unmarshalled from the
// same JSON — there is nothing further to restore.
//
// A config with neither a credential parameter nor an inline credential is a real
// failure: the agent would boot, find nothing to register with, and hang until its
// watchdog fires.
func (s *SSMStore) loadCredential(ctx context.Context, runnerID string, config *RunnerConfig) error {
	credPath := s.credentialPath(runnerID)

	output, err := s.client.GetParameter(ctx, &ssm.GetParameterInput{
		Name:           aws.String(credPath),
		WithDecryption: aws.Bool(true),
	})
	if err != nil {
		var notFound *types.ParameterNotFound
		if errors.As(err, &notFound) || strings.Contains(err.Error(), "ParameterNotFound") {
			if config.JITConfig == "" && config.RegistrationToken == "" {
				return fmt.Errorf("%s: %w", credPath, ErrConfigNotFound)
			}
			return nil
		}
		return fmt.Errorf("failed to get runner credential from SSM: %w", err)
	}

	if output.Parameter == nil || output.Parameter.Value == nil {
		return fmt.Errorf("credential parameter value is nil")
	}

	return unpackCredential(*output.Parameter.Value, config)
}

// Delete removes runner configuration from SSM Parameter Store.
func (s *SSMStore) Delete(ctx context.Context, runnerID string) error {
	// Both halves are deleted even if the first fails, so a transient error on
	// one cannot strand the other: a surviving credential is a live registration
	// credential outliving the instance it was minted for.
	//
	// Config first, mirroring Put's reverse order, so either crash point leaves at
	// most a lone credential rather than a config that cannot be read. List
	// reports both halves, so a leftover is still enumerated; the sweep deletes it
	// outright once the instance is gone, which is the state a leftover outlives.
	configErr := s.deleteParameter(ctx, s.parameterPath(runnerID))
	credErr := s.deleteParameter(ctx, s.credentialPath(runnerID))

	if configErr != nil {
		return fmt.Errorf("failed to delete runner config from SSM: %w", configErr)
	}
	if credErr != nil {
		return fmt.Errorf("failed to delete runner credential from SSM: %w", credErr)
	}

	return nil
}

// deleteParameter removes one parameter, treating an absent one as success.
func (s *SSMStore) deleteParameter(ctx context.Context, path string) error {
	_, err := s.client.DeleteParameter(ctx, &ssm.DeleteParameterInput{
		Name: aws.String(path),
	})
	if err != nil {
		var notFound *types.ParameterNotFound
		if errors.As(err, &notFound) || strings.Contains(err.Error(), "ParameterNotFound") {
			return nil
		}
		return err
	}

	return nil
}

// List returns all runner IDs with stored configuration.
func (s *SSMStore) List(ctx context.Context) ([]string, error) {
	var runnerIDs []string
	var nextToken *string
	// A runner holding both halves appears under two paths; without this the
	// housekeeping sweep would act on each runner twice.
	seen := map[string]bool{}

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
			if runnerID == "" || seen[runnerID] {
				continue
			}
			seen[runnerID] = true
			runnerIDs = append(runnerIDs, runnerID)
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

// credentialPath returns the path holding the runner's registration credential.
func (s *SSMStore) credentialPath(runnerID string) string {
	return fmt.Sprintf("%s/%s/credential", s.prefix, runnerID)
}

// extractRunnerID extracts the runner ID from a parameter path.
// Expected format: {prefix}/{runner-id}/{config,credential}
//
// Both halves yield the runner ID so that List reports a runner holding either
// one. A crash between Put's two writes leaves a credential with no config; were
// that path unmatched here, the housekeeping sweep — List is its only source of
// runner IDs — could never see it, and a live registration credential would sit
// in the store forever.
func (s *SSMStore) extractRunnerID(paramPath string) string {
	trimmed := strings.TrimPrefix(paramPath, s.prefix+"/")
	parts := strings.Split(trimmed, "/")
	if len(parts) < 2 {
		return ""
	}
	switch parts[len(parts)-1] {
	case "config", "credential":
		return parts[0]
	}
	return ""
}

// Ensure SSMStore implements Store.
var _ Store = (*SSMStore)(nil)
