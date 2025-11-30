package ec2

import (
	"context"
	"errors"
	"testing"

	"github.com/Shavakan/runs-fleet/pkg/provider"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
)

type mockSSMAPI struct {
	putParameterFunc    func(ctx context.Context, params *ssm.PutParameterInput, optFns ...func(*ssm.Options)) (*ssm.PutParameterOutput, error)
	deleteParameterFunc func(ctx context.Context, params *ssm.DeleteParameterInput, optFns ...func(*ssm.Options)) (*ssm.DeleteParameterOutput, error)
}

func (m *mockSSMAPI) PutParameter(ctx context.Context, params *ssm.PutParameterInput, optFns ...func(*ssm.Options)) (*ssm.PutParameterOutput, error) {
	if m.putParameterFunc != nil {
		return m.putParameterFunc(ctx, params, optFns...)
	}
	return &ssm.PutParameterOutput{}, nil
}

func (m *mockSSMAPI) DeleteParameter(ctx context.Context, params *ssm.DeleteParameterInput, optFns ...func(*ssm.Options)) (*ssm.DeleteParameterOutput, error) {
	if m.deleteParameterFunc != nil {
		return m.deleteParameterFunc(ctx, params, optFns...)
	}
	return &ssm.DeleteParameterOutput{}, nil
}

func TestConfigStore_StoreRunnerConfig(t *testing.T) {
	tests := []struct {
		name    string
		req     *provider.StoreConfigRequest
		wantErr bool
		errMsg  string
	}{
		{
			name: "valid request",
			req: &provider.StoreConfigRequest{
				RunnerID: "i-123",
				JobID:    "job-456",
				Repo:     "owner/repo",
				Labels:   []string{"self-hosted"},
				JITToken: "token123",
			},
			wantErr: false,
		},
		{
			name: "invalid repo format - no slash",
			req: &provider.StoreConfigRequest{
				RunnerID: "i-123",
				Repo:     "invalid",
			},
			wantErr: true,
			errMsg:  "invalid repo format",
		},
		{
			name: "invalid repo format - empty owner",
			req: &provider.StoreConfigRequest{
				RunnerID: "i-123",
				Repo:     "/repo",
			},
			wantErr: true,
			errMsg:  "invalid repo format",
		},
		{
			name: "invalid repo format - empty repo",
			req: &provider.StoreConfigRequest{
				RunnerID: "i-123",
				Repo:     "owner/",
			},
			wantErr: true,
			errMsg:  "invalid repo format",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &mockSSMAPI{
				putParameterFunc: func(_ context.Context, _ *ssm.PutParameterInput, _ ...func(*ssm.Options)) (*ssm.PutParameterOutput, error) {
					return &ssm.PutParameterOutput{}, nil
				},
			}

			cs := NewConfigStoreWithClient(mock, "https://termination-queue")

			err := cs.StoreRunnerConfig(context.Background(), tt.req)
			if (err != nil) != tt.wantErr {
				t.Errorf("StoreRunnerConfig() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr && err != nil && tt.errMsg != "" {
				if !contains(err.Error(), tt.errMsg) {
					t.Errorf("StoreRunnerConfig() error = %v, want error containing %q", err, tt.errMsg)
				}
			}
		})
	}
}

func TestConfigStore_StoreRunnerConfig_SSMError(t *testing.T) {
	mock := &mockSSMAPI{
		putParameterFunc: func(_ context.Context, _ *ssm.PutParameterInput, _ ...func(*ssm.Options)) (*ssm.PutParameterOutput, error) {
			return nil, errors.New("SSM error")
		},
	}

	cs := NewConfigStoreWithClient(mock, "")

	err := cs.StoreRunnerConfig(context.Background(), &provider.StoreConfigRequest{
		RunnerID: "i-123",
		Repo:     "owner/repo",
	})
	if err == nil {
		t.Error("StoreRunnerConfig() should return error on SSM failure")
	}
}

func TestConfigStore_DeleteRunnerConfig(t *testing.T) {
	called := false
	mock := &mockSSMAPI{
		deleteParameterFunc: func(_ context.Context, params *ssm.DeleteParameterInput, _ ...func(*ssm.Options)) (*ssm.DeleteParameterOutput, error) {
			called = true
			if *params.Name != "/runs-fleet/runners/i-123/config" {
				t.Errorf("DeleteParameter got name %v, want /runs-fleet/runners/i-123/config", *params.Name)
			}
			return &ssm.DeleteParameterOutput{}, nil
		},
	}

	cs := NewConfigStoreWithClient(mock, "")

	err := cs.DeleteRunnerConfig(context.Background(), "i-123")
	if err != nil {
		t.Errorf("DeleteRunnerConfig() error = %v", err)
	}
	if !called {
		t.Error("DeleteParameter was not called")
	}
}

func TestExtractOrg(t *testing.T) {
	tests := []struct {
		repo    string
		want    string
		wantErr bool
	}{
		{"owner/repo", "owner", false},
		{"my-org/my-repo", "my-org", false},
		{"owner/repo/extra", "owner", false},
		{"invalid", "", true},
		{"/repo", "", true},
		{"owner/", "", true},
		{"", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.repo, func(t *testing.T) {
			got, err := extractOrg(tt.repo)
			if (err != nil) != tt.wantErr {
				t.Errorf("extractOrg(%q) error = %v, wantErr %v", tt.repo, err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("extractOrg(%q) = %v, want %v", tt.repo, got, tt.want)
			}
		})
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsHelper(s, substr))
}

func containsHelper(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

var _ provider.ConfigStore = (*ConfigStore)(nil)
