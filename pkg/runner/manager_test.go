package runner

import (
	"context"
	"errors"
	"testing"

	"github.com/Shavakan/runs-fleet/pkg/github"
	"github.com/Shavakan/runs-fleet/pkg/secrets"
)

// Test constants to avoid goconst lint errors
const (
	testCacheURL            = "https://cache.example.com"
	testTerminationQueueURL = "https://sqs.example.com/queue"
)

// mockSecretsStore implements secrets.Store for testing.
type mockSecretsStore struct {
	putFunc    func(ctx context.Context, runnerID string, config *secrets.RunnerConfig) error
	getFunc    func(ctx context.Context, runnerID string) (*secrets.RunnerConfig, error)
	deleteFunc func(ctx context.Context, runnerID string) error
	listFunc   func(ctx context.Context) ([]string, error)

	putCalls    int
	deleteCalls int
	lastPutID   string
	lastPutCfg  *secrets.RunnerConfig
}

func (m *mockSecretsStore) Put(ctx context.Context, runnerID string, config *secrets.RunnerConfig) error {
	m.putCalls++
	m.lastPutID = runnerID
	m.lastPutCfg = config
	if m.putFunc != nil {
		return m.putFunc(ctx, runnerID, config)
	}
	return nil
}

func (m *mockSecretsStore) Get(ctx context.Context, runnerID string) (*secrets.RunnerConfig, error) {
	if m.getFunc != nil {
		return m.getFunc(ctx, runnerID)
	}
	return nil, errors.New("not implemented")
}

func (m *mockSecretsStore) Delete(ctx context.Context, runnerID string) error {
	m.deleteCalls++
	if m.deleteFunc != nil {
		return m.deleteFunc(ctx, runnerID)
	}
	return nil
}

func (m *mockSecretsStore) List(ctx context.Context) ([]string, error) {
	if m.listFunc != nil {
		return m.listFunc(ctx)
	}
	return nil, nil
}

// mockGitHubClient implements registrationTokenGetter for manager testing.
type mockGitHubClient struct {
	regToken string
	regErr   error
	isOrg    bool
}

func (m *mockGitHubClient) GetRegistrationToken(_ context.Context, _ string) (*github.RegistrationResult, error) {
	if m.regErr != nil {
		return nil, m.regErr
	}
	return &github.RegistrationResult{Token: m.regToken, IsOrg: m.isOrg}, nil
}

func TestNewManager(t *testing.T) {
	t.Parallel()

	mockStore := &mockSecretsStore{}
	config := ManagerConfig{
		CacheSecret:         "test-secret",
		BaseURL:             "https://cache.example.com",
		TerminationQueueURL: "https://sqs.example.com/queue",
	}

	manager := NewManager(nil, mockStore, config)

	if manager.secretsStore != mockStore {
		t.Error("expected secrets store to be set")
	}
	if manager.config.CacheSecret != "test-secret" {
		t.Errorf("expected CacheSecret 'test-secret', got '%s'", manager.config.CacheSecret)
	}
}

func TestManager_PrepareRunner_EmptyRepo(t *testing.T) {
	t.Parallel()

	mockStore := &mockSecretsStore{}

	manager := &Manager{
		secretsStore: mockStore,
		config:       ManagerConfig{},
	}

	req := PrepareRunnerRequest{
		InstanceID: "i-12345",
		JobID:      "job-123",
		RunID:      "run-456",
		Repo:       "", // Empty repo
		Labels:     []string{"self-hosted"},
	}

	err := manager.PrepareRunner(context.Background(), req)
	if err == nil {
		t.Error("expected error for empty repo")
	}
}

func TestManager_PrepareRunner_InvalidRepoFormat(t *testing.T) {
	t.Parallel()

	mockStore := &mockSecretsStore{}

	manager := &Manager{
		secretsStore: mockStore,
		config:       ManagerConfig{},
	}

	tests := []struct {
		name string
		repo string
	}{
		{"no slash", "invalid"},
		{"empty owner", "/repo"},
		{"empty repo name", "owner/"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			req := PrepareRunnerRequest{
				InstanceID: "i-12345",
				JobID:      "job-123",
				Repo:       tt.repo,
			}

			err := manager.PrepareRunner(context.Background(), req)
			if err == nil {
				t.Errorf("expected error for repo '%s'", tt.repo)
			}
		})
	}
}

func TestManager_PrepareRunner_SpecialRepoNames(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		repo  string
		valid bool
	}{
		{"repo with hyphen", "my-org/my-repo", true},
		{"repo with underscore", "my_org/my_repo", true},
		{"repo with numbers", "org123/repo456", true},
		{"single character", "a/b", true},
		{"empty repo", "", false},
		{"no slash", "noslash", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			mockStore := &mockSecretsStore{}
			manager := NewManager(&mockGitHubClient{regToken: "tok"}, mockStore, ManagerConfig{})

			req := PrepareRunnerRequest{
				InstanceID: "i-12345",
				JobID:      "job-123",
				Repo:       tt.repo,
				Labels:     []string{"self-hosted"},
			}

			err := manager.PrepareRunner(context.Background(), req)
			if tt.valid && err != nil {
				t.Errorf("repo %q: unexpected error: %v", tt.repo, err)
			}
			if !tt.valid && err == nil {
				t.Errorf("repo %q: expected error", tt.repo)
			}
		})
	}
}

func TestManager_PrepareRunner_Success(t *testing.T) {
	t.Parallel()

	mockStore := &mockSecretsStore{}
	mockGH := &mockGitHubClient{
		regToken: "test-registration-token",
		isOrg:    true,
	}

	manager := NewManager(mockGH, mockStore, ManagerConfig{
		CacheSecret:         "test-secret",
		BaseURL:             testCacheURL,
		TerminationQueueURL: testTerminationQueueURL,
	})

	req := PrepareRunnerRequest{
		InstanceID: "i-test-12345",
		JobID:      "job-success-123",
		RunID:      "run-success-456",
		Repo:       "testorg/testrepo",
		Labels:     []string{"self-hosted", "linux"},
	}

	err := manager.PrepareRunner(context.Background(), req)
	if err != nil {
		t.Fatalf("PrepareRunner() error = %v", err)
	}

	if mockStore.putCalls != 1 {
		t.Errorf("expected 1 put call, got %d", mockStore.putCalls)
	}
	if mockStore.lastPutID != "i-test-12345" {
		t.Errorf("runner ID = %s, want i-test-12345", mockStore.lastPutID)
	}

	storedConfig := mockStore.lastPutCfg
	if storedConfig == nil {
		t.Fatal("stored config is nil")
	}

	if storedConfig.Org != "testorg" {
		t.Errorf("Org = %s, want testorg", storedConfig.Org)
	}
	if storedConfig.Repo != "testorg/testrepo" {
		t.Errorf("Repo = %s, want testorg/testrepo", storedConfig.Repo)
	}
	if storedConfig.JITToken != "test-registration-token" {
		t.Errorf("JITToken = %s, want test-registration-token", storedConfig.JITToken)
	}
	if storedConfig.JobID != "job-success-123" {
		t.Errorf("JobID = %s, want job-success-123", storedConfig.JobID)
	}
	if storedConfig.CacheURL != testCacheURL {
		t.Errorf("CacheURL = %s, want %s", storedConfig.CacheURL, testCacheURL)
	}
	if storedConfig.CacheToken == "" {
		t.Error("expected a non-empty cache token when CacheSecret is set")
	}
	if !storedConfig.IsOrg {
		t.Error("IsOrg should be true")
	}
}

func TestManager_PrepareRunner_GitHubError(t *testing.T) {
	t.Parallel()

	mockStore := &mockSecretsStore{}
	mockGH := &mockGitHubClient{
		regErr: errors.New("GitHub API rate limit exceeded"),
	}

	manager := NewManager(mockGH, mockStore, ManagerConfig{
		CacheSecret:         "test-secret",
		BaseURL:             testCacheURL,
		TerminationQueueURL: testTerminationQueueURL,
	})

	req := PrepareRunnerRequest{
		InstanceID: "i-gh-error-12345",
		JobID:      "job-gh-error-123",
		RunID:      "run-gh-error-456",
		Repo:       "testorg/testrepo",
		Labels:     []string{"self-hosted"},
	}

	err := manager.PrepareRunner(context.Background(), req)
	if err == nil {
		t.Error("expected error from GitHub client")
	}
	if !contains(err.Error(), "registration token") {
		t.Errorf("expected error to mention 'registration token', got: %s", err.Error())
	}

	if mockStore.putCalls != 0 {
		t.Errorf("secrets store should not be called when GitHub fails, got %d calls", mockStore.putCalls)
	}
}

func TestManager_PrepareRunner_StoreError(t *testing.T) {
	t.Parallel()

	mockStore := &mockSecretsStore{
		putFunc: func(_ context.Context, _ string, _ *secrets.RunnerConfig) error {
			return errors.New("secrets store unavailable")
		},
	}
	mockGH := &mockGitHubClient{
		regToken: "test-token",
		isOrg:    false,
	}

	manager := NewManager(mockGH, mockStore, ManagerConfig{
		CacheSecret:         "",
		BaseURL:             testCacheURL,
		TerminationQueueURL: testTerminationQueueURL,
	})

	req := PrepareRunnerRequest{
		InstanceID: "i-store-error-12345",
		JobID:      "job-store-error-123",
		RunID:      "run-store-error-456",
		Repo:       "testorg/testrepo",
		Labels:     []string{"self-hosted"},
	}

	err := manager.PrepareRunner(context.Background(), req)
	if err == nil {
		t.Error("expected error from secrets store")
	}
	if !contains(err.Error(), "store") {
		t.Errorf("expected error to mention 'store', got: %s", err.Error())
	}
}

func TestManager_PrepareRunner_NoCacheSecret(t *testing.T) {
	t.Parallel()

	mockStore := &mockSecretsStore{}
	mockGH := &mockGitHubClient{
		regToken: "test-token",
		isOrg:    false,
	}

	// No cache secret configured
	manager := NewManager(mockGH, mockStore, ManagerConfig{
		CacheSecret:         "",
		BaseURL:             testCacheURL,
		TerminationQueueURL: testTerminationQueueURL,
	})

	req := PrepareRunnerRequest{
		InstanceID: "i-no-cache-12345",
		JobID:      "job-no-cache-123",
		RunID:      "run-no-cache-456",
		Repo:       "testorg/testrepo",
		Labels:     []string{"self-hosted"},
	}

	err := manager.PrepareRunner(context.Background(), req)
	if err != nil {
		t.Fatalf("PrepareRunner() error = %v", err)
	}

	storedConfig := mockStore.lastPutCfg
	if storedConfig.CacheToken != "" {
		t.Errorf("CacheToken should be empty when CacheSecret is not set, got: %s", storedConfig.CacheToken)
	}
}

func TestManager_PrepareRunner_UserRepo(t *testing.T) {
	t.Parallel()

	mockStore := &mockSecretsStore{}
	mockGH := &mockGitHubClient{
		regToken: "user-token",
		isOrg:    false, // User, not org
	}

	manager := NewManager(mockGH, mockStore, ManagerConfig{
		CacheSecret: "secret",
		BaseURL:     testCacheURL,
	})

	req := PrepareRunnerRequest{
		InstanceID: "i-user-12345",
		JobID:      "job-user-123",
		RunID:      "run-user-456",
		Repo:       "myuser/myrepo",
		Labels:     []string{"self-hosted", "linux", "x64"},
	}

	err := manager.PrepareRunner(context.Background(), req)
	if err != nil {
		t.Fatalf("PrepareRunner() error = %v", err)
	}

	storedConfig := mockStore.lastPutCfg

	if storedConfig.Org != "myuser" {
		t.Errorf("Org = %s, want myuser", storedConfig.Org)
	}
	if storedConfig.IsOrg {
		t.Error("IsOrg should be false for user repo")
	}
}

func TestManager_PrepareRunner_MultipleLabels(t *testing.T) {
	t.Parallel()

	mockStore := &mockSecretsStore{}
	mockGH := &mockGitHubClient{
		regToken: "test-token",
		isOrg:    true,
	}

	manager := NewManager(mockGH, mockStore, ManagerConfig{
		CacheSecret: "secret",
	})

	labels := []string{"self-hosted", "linux", "arm64", "gpu", "custom-label"}
	req := PrepareRunnerRequest{
		InstanceID: "i-labels-12345",
		JobID:      "job-labels-123",
		RunID:      "run-labels-456",
		Repo:       "org/repo",
		Labels:     labels,
	}

	err := manager.PrepareRunner(context.Background(), req)
	if err != nil {
		t.Fatalf("PrepareRunner() error = %v", err)
	}

	storedConfig := mockStore.lastPutCfg

	if len(storedConfig.Labels) != len(labels) {
		t.Errorf("Labels length = %d, want %d", len(storedConfig.Labels), len(labels))
	}
	for i, label := range labels {
		if storedConfig.Labels[i] != label {
			t.Errorf("Labels[%d] = %s, want %s", i, storedConfig.Labels[i], label)
		}
	}
}

func TestManager_PrepareRunner_RunnerName(t *testing.T) {
	t.Parallel()

	mockStore := &mockSecretsStore{}
	mockGH := &mockGitHubClient{
		regToken: "test-token",
		isOrg:    true,
	}

	manager := NewManager(mockGH, mockStore, ManagerConfig{
		CacheSecret: "secret",
	})

	req := PrepareRunnerRequest{
		InstanceID: "i-12345",
		JobID:      "99999",
		RunID:      "run-456",
		Repo:       "org/myapp",
		Labels:     []string{"self-hosted"},
		Pool:       "default",
		Conditions: "arm64-cpu4",
	}

	err := manager.PrepareRunner(context.Background(), req)
	if err != nil {
		t.Fatalf("PrepareRunner() error = %v", err)
	}

	storedConfig := mockStore.lastPutCfg
	if storedConfig.RunnerName != "runs-fleet-runner-default-99999" {
		t.Errorf("RunnerName = %q, want %q", storedConfig.RunnerName, "runs-fleet-runner-default-99999")
	}
}

func TestManager_PrepareRunner_RunnerName_ColdStart(t *testing.T) {
	t.Parallel()

	mockStore := &mockSecretsStore{}
	mockGH := &mockGitHubClient{
		regToken: "test-token",
		isOrg:    true,
	}

	manager := NewManager(mockGH, mockStore, ManagerConfig{})

	req := PrepareRunnerRequest{
		InstanceID: "i-12345",
		JobID:      "88888",
		RunID:      "run-456",
		Repo:       "org/myapp",
		Labels:     []string{"self-hosted"},
		Pool:       "",
		Conditions: "amd64-cpu8",
	}

	err := manager.PrepareRunner(context.Background(), req)
	if err != nil {
		t.Fatalf("PrepareRunner() error = %v", err)
	}

	storedConfig := mockStore.lastPutCfg
	if storedConfig.RunnerName != "runs-fleet-runner-myapp-amd64-cpu8-88888" {
		t.Errorf("RunnerName = %q, want %q", storedConfig.RunnerName, "runs-fleet-runner-myapp-amd64-cpu8-88888")
	}
}

func TestManager_CleanupRunner_Success(t *testing.T) {
	t.Parallel()

	mockStore := &mockSecretsStore{}

	manager := &Manager{
		secretsStore: mockStore,
		config:       ManagerConfig{},
	}

	err := manager.CleanupRunner(context.Background(), "i-12345")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if mockStore.deleteCalls != 1 {
		t.Errorf("expected 1 delete call, got %d", mockStore.deleteCalls)
	}
}

func TestManager_CleanupRunner_Error(t *testing.T) {
	t.Parallel()

	mockStore := &mockSecretsStore{
		deleteFunc: func(_ context.Context, _ string) error {
			return errors.New("delete error")
		},
	}

	manager := &Manager{
		secretsStore: mockStore,
		config:       ManagerConfig{},
	}

	err := manager.CleanupRunner(context.Background(), "i-12345")
	if err == nil {
		t.Fatal("expected error from secrets store")
	}

	if !contains(err.Error(), "delete error") {
		t.Errorf("expected error to contain 'delete error', got '%s'", err.Error())
	}
}

func TestManager_CleanupRunner_MultipleInstances(t *testing.T) {
	t.Parallel()

	mockStore := &mockSecretsStore{}

	manager := &Manager{
		secretsStore: mockStore,
		config:       ManagerConfig{},
	}

	instances := []string{"i-111", "i-222", "i-333"}
	for _, instanceID := range instances {
		err := manager.CleanupRunner(context.Background(), instanceID)
		if err != nil {
			t.Errorf("CleanupRunner(%s) error = %v", instanceID, err)
		}
	}

	if mockStore.deleteCalls != 3 {
		t.Errorf("expected 3 delete calls, got %d", mockStore.deleteCalls)
	}
}

func TestPrepareRunnerRequest_Structure(t *testing.T) {
	t.Parallel()

	req := PrepareRunnerRequest{
		InstanceID: "i-12345",
		JobID:      "job-123",
		RunID:      "run-456",
		Repo:       "myorg/myrepo",
		Labels:     []string{"self-hosted", "linux"},
		Pool:       "default",
		Conditions: "arm64",
	}

	if req.InstanceID != "i-12345" {
		t.Errorf("expected InstanceID 'i-12345', got '%s'", req.InstanceID)
	}
	if req.JobID != "job-123" {
		t.Errorf("expected JobID 'job-123', got '%s'", req.JobID)
	}
	if req.RunID != "run-456" {
		t.Errorf("expected RunID 'run-456', got '%s'", req.RunID)
	}
	if req.Repo != "myorg/myrepo" {
		t.Errorf("expected Repo 'myorg/myrepo', got '%s'", req.Repo)
	}
	if len(req.Labels) != 2 {
		t.Errorf("expected 2 labels, got %d", len(req.Labels))
	}
	if req.Pool != "default" {
		t.Errorf("expected Pool 'default', got '%s'", req.Pool)
	}
	if req.Conditions != "arm64" {
		t.Errorf("expected Conditions 'arm64', got '%s'", req.Conditions)
	}
}

func TestManagerConfig_Structure(t *testing.T) {
	t.Parallel()

	config := ManagerConfig{
		CacheSecret:         "my-secret",
		BaseURL:             "https://cache.example.com",
		TerminationQueueURL: "https://sqs.example.com/queue",
	}

	if config.CacheSecret != "my-secret" {
		t.Errorf("expected CacheSecret 'my-secret', got '%s'", config.CacheSecret)
	}
	if config.BaseURL != "https://cache.example.com" {
		t.Errorf("expected BaseURL 'https://cache.example.com', got '%s'", config.BaseURL)
	}
	if config.TerminationQueueURL != "https://sqs.example.com/queue" {
		t.Errorf("expected TerminationQueueURL 'https://sqs.example.com/queue', got '%s'", config.TerminationQueueURL)
	}
}

func contains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func TestBuildRunnerName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		pool       string
		repoName   string
		conditions string
		jobID      string
		want       string
	}{
		{
			name:       "warm pool with job ID suffix",
			pool:       "default",
			repoName:   "myapp",
			conditions: "arm64-cpu4",
			jobID:      "65558617323",
			want:       "runs-fleet-runner-default-617323",
		},
		{
			name:       "cold-start with all conditions",
			pool:       "",
			repoName:   "myapp",
			conditions: "amd64-cpu8-ram16",
			jobID:      "12345",
			want:       "runs-fleet-runner-myapp-amd64-cpu8-ram16-12345",
		},
		{
			name:       "cold-start arch only",
			pool:       "",
			repoName:   "myapp",
			conditions: "arm64",
			jobID:      "99999",
			want:       "runs-fleet-runner-myapp-arm64-99999",
		},
		{
			name:       "cold-start without conditions",
			pool:       "",
			repoName:   "myapp",
			conditions: "",
			jobID:      "88888",
			want:       "runs-fleet-runner-myapp-88888",
		},
		{
			name:       "no pool no repo falls back",
			pool:       "",
			repoName:   "",
			conditions: "",
			jobID:      "77777",
			want:       "runs-fleet-runner-77777",
		},
		{
			name:       "empty job ID omits suffix",
			pool:       "default",
			repoName:   "myapp",
			conditions: "",
			jobID:      "",
			want:       "runs-fleet-runner-default",
		},
		{
			name:       "short job ID used as-is",
			pool:       "",
			repoName:   "myapp",
			conditions: "arm64",
			jobID:      "123",
			want:       "runs-fleet-runner-myapp-arm64-123",
		},
		{
			name:       "truncates to 64 chars",
			pool:       "",
			repoName:   "my-repository",
			conditions: "arm64-cpu4-ram16-disk200-c7g-m7g-r7g-gen8",
			jobID:      "65558617323",
			want:       "runs-fleet-runner-my-repository-arm64-cpu4-ram16-disk200-c7g-m7g",
		},
		{
			name:       "different job IDs produce different names",
			pool:       "",
			repoName:   "cygnus",
			conditions: "cpu4",
			jobID:      "65558617323",
			want:       "runs-fleet-runner-cygnus-cpu4-617323",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := buildRunnerName(tt.pool, tt.repoName, tt.conditions, tt.jobID)
			if got != tt.want {
				t.Errorf("buildRunnerName() = %q, want %q", got, tt.want)
			}
			if len(got) > 64 {
				t.Errorf("buildRunnerName() length = %d, exceeds 64 chars", len(got))
			}
		})
	}

	// Verify uniqueness: same spec but different job IDs produce different names
	name1 := buildRunnerName("", "cygnus", "cpu4", "65558617323")
	name2 := buildRunnerName("", "cygnus", "cpu4", "65558617355")
	if name1 == name2 {
		t.Errorf("different job IDs should produce different names: %q == %q", name1, name2)
	}
}
