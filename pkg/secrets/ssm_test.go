package secrets

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

const testExpectedPath = "/runs-fleet/runners/i-123456/config"

type mockSSMClient struct {
	putFunc              func(ctx context.Context, params *ssm.PutParameterInput, optFns ...func(*ssm.Options)) (*ssm.PutParameterOutput, error)
	getFunc              func(ctx context.Context, params *ssm.GetParameterInput, optFns ...func(*ssm.Options)) (*ssm.GetParameterOutput, error)
	deleteFunc           func(ctx context.Context, params *ssm.DeleteParameterInput, optFns ...func(*ssm.Options)) (*ssm.DeleteParameterOutput, error)
	getParametersByPath  func(ctx context.Context, params *ssm.GetParametersByPathInput, optFns ...func(*ssm.Options)) (*ssm.GetParametersByPathOutput, error)
}

func (m *mockSSMClient) PutParameter(ctx context.Context, params *ssm.PutParameterInput, optFns ...func(*ssm.Options)) (*ssm.PutParameterOutput, error) {
	if m.putFunc != nil {
		return m.putFunc(ctx, params, optFns...)
	}
	return &ssm.PutParameterOutput{}, nil
}

func (m *mockSSMClient) GetParameter(ctx context.Context, params *ssm.GetParameterInput, optFns ...func(*ssm.Options)) (*ssm.GetParameterOutput, error) {
	if m.getFunc != nil {
		return m.getFunc(ctx, params, optFns...)
	}
	return nil, errors.New("not implemented")
}

func (m *mockSSMClient) DeleteParameter(ctx context.Context, params *ssm.DeleteParameterInput, optFns ...func(*ssm.Options)) (*ssm.DeleteParameterOutput, error) {
	if m.deleteFunc != nil {
		return m.deleteFunc(ctx, params, optFns...)
	}
	return &ssm.DeleteParameterOutput{}, nil
}

func (m *mockSSMClient) GetParametersByPath(ctx context.Context, params *ssm.GetParametersByPathInput, optFns ...func(*ssm.Options)) (*ssm.GetParametersByPathOutput, error) {
	if m.getParametersByPath != nil {
		return m.getParametersByPath(ctx, params, optFns...)
	}
	return &ssm.GetParametersByPathOutput{}, nil
}

func TestSSMStore_Put(t *testing.T) {
	tests := []struct {
		name      string
		runnerID  string
		config    *RunnerConfig
		putFunc   func(ctx context.Context, params *ssm.PutParameterInput, optFns ...func(*ssm.Options)) (*ssm.PutParameterOutput, error)
		wantErr   bool
	}{
		{
			name:     "successful put",
			runnerID: "i-123456",
			config: &RunnerConfig{
				Org:      "testorg",
				Repo:     "testorg/testrepo",
				JITToken: "token123",
				JobID:    "job-1",
			},
			putFunc: func(_ context.Context, params *ssm.PutParameterInput, _ ...func(*ssm.Options)) (*ssm.PutParameterOutput, error) {
				if *params.Name != testExpectedPath {
					t.Errorf("unexpected parameter path: %s", *params.Name)
				}
				if params.Type != types.ParameterTypeSecureString {
					t.Errorf("expected SecureString type")
				}
				return &ssm.PutParameterOutput{}, nil
			},
			wantErr: false,
		},
		{
			name:     "put error",
			runnerID: "i-123456",
			config:   &RunnerConfig{Org: "testorg"},
			putFunc: func(_ context.Context, _ *ssm.PutParameterInput, _ ...func(*ssm.Options)) (*ssm.PutParameterOutput, error) {
				return nil, errors.New("SSM error")
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &mockSSMClient{putFunc: tt.putFunc}
			store := NewSSMStoreWithClient(mock, "")

			err := store.Put(context.Background(), tt.runnerID, tt.config)
			if (err != nil) != tt.wantErr {
				t.Errorf("Put() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestSSMStore_Get(t *testing.T) {
	expectedConfig := &RunnerConfig{
		Org:      "testorg",
		Repo:     "testorg/testrepo",
		JITToken: "token123",
		Labels:   []string{"self-hosted", "linux"},
	}
	configJSON, _ := json.Marshal(expectedConfig)

	tests := []struct {
		name     string
		runnerID string
		getFunc  func(ctx context.Context, params *ssm.GetParameterInput, optFns ...func(*ssm.Options)) (*ssm.GetParameterOutput, error)
		wantErr  bool
	}{
		{
			name:     "successful get",
			runnerID: "i-123456",
			getFunc: func(_ context.Context, params *ssm.GetParameterInput, _ ...func(*ssm.Options)) (*ssm.GetParameterOutput, error) {
				if *params.Name != testExpectedPath {
					t.Errorf("unexpected parameter path: %s", *params.Name)
				}
				if !*params.WithDecryption {
					t.Errorf("expected WithDecryption to be true")
				}
				return &ssm.GetParameterOutput{
					Parameter: &types.Parameter{
						Value: aws.String(string(configJSON)),
					},
				}, nil
			},
			wantErr: false,
		},
		{
			name:     "get error",
			runnerID: "i-123456",
			getFunc: func(_ context.Context, _ *ssm.GetParameterInput, _ ...func(*ssm.Options)) (*ssm.GetParameterOutput, error) {
				return nil, errors.New("parameter not found")
			},
			wantErr: true,
		},
		{
			name:     "nil parameter value",
			runnerID: "i-123456",
			getFunc: func(_ context.Context, _ *ssm.GetParameterInput, _ ...func(*ssm.Options)) (*ssm.GetParameterOutput, error) {
				return &ssm.GetParameterOutput{
					Parameter: &types.Parameter{Value: nil},
				}, nil
			},
			wantErr: true,
		},
		{
			name:     "invalid JSON",
			runnerID: "i-123456",
			getFunc: func(_ context.Context, _ *ssm.GetParameterInput, _ ...func(*ssm.Options)) (*ssm.GetParameterOutput, error) {
				return &ssm.GetParameterOutput{
					Parameter: &types.Parameter{
						Value: aws.String("not valid json"),
					},
				}, nil
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &mockSSMClient{getFunc: tt.getFunc}
			store := NewSSMStoreWithClient(mock, "")

			config, err := store.Get(context.Background(), tt.runnerID)
			if (err != nil) != tt.wantErr {
				t.Errorf("Get() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && config.Org != expectedConfig.Org {
				t.Errorf("Get() config.Org = %v, want %v", config.Org, expectedConfig.Org)
			}
		})
	}
}

func TestSSMStore_Delete(t *testing.T) {
	tests := []struct {
		name       string
		runnerID   string
		deleteFunc func(ctx context.Context, params *ssm.DeleteParameterInput, optFns ...func(*ssm.Options)) (*ssm.DeleteParameterOutput, error)
		wantErr    bool
	}{
		{
			name:     "successful delete",
			runnerID: "i-123456",
			deleteFunc: func(_ context.Context, params *ssm.DeleteParameterInput, _ ...func(*ssm.Options)) (*ssm.DeleteParameterOutput, error) {
				if *params.Name != testExpectedPath {
					t.Errorf("unexpected parameter path: %s", *params.Name)
				}
				return &ssm.DeleteParameterOutput{}, nil
			},
			wantErr: false,
		},
		{
			name:     "parameter not found is not an error",
			runnerID: "i-123456",
			deleteFunc: func(_ context.Context, _ *ssm.DeleteParameterInput, _ ...func(*ssm.Options)) (*ssm.DeleteParameterOutput, error) {
				return nil, errors.New("ParameterNotFound: parameter does not exist")
			},
			wantErr: false,
		},
		{
			name:     "other errors propagate",
			runnerID: "i-123456",
			deleteFunc: func(_ context.Context, _ *ssm.DeleteParameterInput, _ ...func(*ssm.Options)) (*ssm.DeleteParameterOutput, error) {
				return nil, errors.New("access denied")
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &mockSSMClient{deleteFunc: tt.deleteFunc}
			store := NewSSMStoreWithClient(mock, "")

			err := store.Delete(context.Background(), tt.runnerID)
			if (err != nil) != tt.wantErr {
				t.Errorf("Delete() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestSSMStore_List(t *testing.T) {
	tests := []struct {
		name                string
		getParametersByPath func(ctx context.Context, params *ssm.GetParametersByPathInput, optFns ...func(*ssm.Options)) (*ssm.GetParametersByPathOutput, error)
		wantIDs             []string
		wantErr             bool
	}{
		{
			name: "list multiple runners",
			getParametersByPath: func(_ context.Context, _ *ssm.GetParametersByPathInput, _ ...func(*ssm.Options)) (*ssm.GetParametersByPathOutput, error) {
				return &ssm.GetParametersByPathOutput{
					Parameters: []types.Parameter{
						{Name: aws.String("/runs-fleet/runners/i-111/config")},
						{Name: aws.String("/runs-fleet/runners/i-222/config")},
						{Name: aws.String("/runs-fleet/runners/i-333/config")},
					},
				}, nil
			},
			wantIDs: []string{"i-111", "i-222", "i-333"},
			wantErr: false,
		},
		{
			name: "empty list",
			getParametersByPath: func(_ context.Context, _ *ssm.GetParametersByPathInput, _ ...func(*ssm.Options)) (*ssm.GetParametersByPathOutput, error) {
				return &ssm.GetParametersByPathOutput{
					Parameters: []types.Parameter{},
				}, nil
			},
			wantIDs: nil,
			wantErr: false,
		},
		{
			name: "pagination",
			getParametersByPath: func() func(context.Context, *ssm.GetParametersByPathInput, ...func(*ssm.Options)) (*ssm.GetParametersByPathOutput, error) {
				callCount := 0
				return func(_ context.Context, _ *ssm.GetParametersByPathInput, _ ...func(*ssm.Options)) (*ssm.GetParametersByPathOutput, error) {
					callCount++
					if callCount == 1 {
						return &ssm.GetParametersByPathOutput{
							Parameters: []types.Parameter{
								{Name: aws.String("/runs-fleet/runners/i-111/config")},
							},
							NextToken: aws.String("token1"),
						}, nil
					}
					return &ssm.GetParametersByPathOutput{
						Parameters: []types.Parameter{
							{Name: aws.String("/runs-fleet/runners/i-222/config")},
						},
					}, nil
				}
			}(),
			wantIDs: []string{"i-111", "i-222"},
			wantErr: false,
		},
		{
			name: "API error",
			getParametersByPath: func(_ context.Context, _ *ssm.GetParametersByPathInput, _ ...func(*ssm.Options)) (*ssm.GetParametersByPathOutput, error) {
				return nil, errors.New("access denied")
			},
			wantIDs: nil,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &mockSSMClient{getParametersByPath: tt.getParametersByPath}
			store := NewSSMStoreWithClient(mock, "")

			ids, err := store.List(context.Background())
			if (err != nil) != tt.wantErr {
				t.Errorf("List() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && len(ids) != len(tt.wantIDs) {
				t.Errorf("List() got %v, want %v", ids, tt.wantIDs)
			}
		})
	}
}

func TestSSMStore_extractRunnerID(t *testing.T) {
	store := NewSSMStoreWithClient(nil, "/runs-fleet/runners")

	tests := []struct {
		path   string
		wantID string
	}{
		{"/runs-fleet/runners/i-123456/config", "i-123456"},
		{"/runs-fleet/runners/abc-def-ghi/config", "abc-def-ghi"},
		{"/runs-fleet/runners/config", ""},
		{"/other/path/config", ""},
		{"", ""},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			id := store.extractRunnerID(tt.path)
			if id != tt.wantID {
				t.Errorf("extractRunnerID(%s) = %s, want %s", tt.path, id, tt.wantID)
			}
		})
	}
}

func TestSSMStore_CustomPrefix(t *testing.T) {
	mock := &mockSSMClient{
		putFunc: func(_ context.Context, params *ssm.PutParameterInput, _ ...func(*ssm.Options)) (*ssm.PutParameterOutput, error) {
			expected := "/custom/prefix/i-123/config"
			if *params.Name != expected {
				t.Errorf("expected path %s, got %s", expected, *params.Name)
			}
			return &ssm.PutParameterOutput{}, nil
		},
	}
	store := NewSSMStoreWithClient(mock, "/custom/prefix")

	err := store.Put(context.Background(), "i-123", &RunnerConfig{})
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}
