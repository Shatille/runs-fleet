package runner

import (
	"context"
	"errors"
	"testing"

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

// mockGitHubClientForManager implements the GitHub client interface for manager testing.
type mockGitHubClientForManager struct {
	regToken string
	regErr   error
	isOrg    bool
}

func (m *mockGitHubClientForManager) GetRegistrationToken(_ context.Context, _ string) (*RegistrationResult, error) {
	if m.regErr != nil {
		return nil, m.regErr
	}
	return &RegistrationResult{Token: m.regToken, IsOrg: m.isOrg}, nil
}

func TestNewManager(t *testing.T) {
	mockStore := &mockSecretsStore{}
	config := ManagerConfig{
		CacheSecret:         "test-secret",
		CacheURL:            "https://cache.example.com",
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

func TestManager_CleanupRunner_Success(t *testing.T) {
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
	req := PrepareRunnerRequest{
		InstanceID: "i-12345",
		JobID:      "job-123",
		RunID:      "run-456",
		Repo:       "myorg/myrepo",
		Labels:     []string{"self-hosted", "linux"},
		Pool:       "default",
		Arch:       "arm64",
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
	if req.Arch != "arm64" {
		t.Errorf("expected Arch 'arm64', got '%s'", req.Arch)
	}
}

func TestManagerConfig_Structure(t *testing.T) {
	config := ManagerConfig{
		CacheSecret:         "my-secret",
		CacheURL:            "https://cache.example.com",
		TerminationQueueURL: "https://sqs.example.com/queue",
	}

	if config.CacheSecret != "my-secret" {
		t.Errorf("expected CacheSecret 'my-secret', got '%s'", config.CacheSecret)
	}
	if config.CacheURL != "https://cache.example.com" {
		t.Errorf("expected CacheURL 'https://cache.example.com', got '%s'", config.CacheURL)
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

func TestMockGitHubClientForManager(t *testing.T) {
	mock := &mockGitHubClientForManager{
		regToken: "mock-token",
		isOrg:    true,
	}

	result, err := mock.GetRegistrationToken(context.Background(), "org/repo")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.Token != "mock-token" {
		t.Errorf("Token = %s, want mock-token", result.Token)
	}
	if !result.IsOrg {
		t.Error("IsOrg should be true")
	}
}

func TestMockGitHubClientForManager_Error(t *testing.T) {
	mock := &mockGitHubClientForManager{
		regErr: errors.New("mock error"),
	}

	_, err := mock.GetRegistrationToken(context.Background(), "org/repo")
	if err == nil {
		t.Error("expected error from mock")
	}
}

// testableManager wraps Manager to allow direct field access for testing.
type testableManager struct {
	Manager
	ghClient *mockGitHubClientForManager
}

func newTestableManager(store secrets.Store, ghClient *mockGitHubClientForManager, config ManagerConfig) *testableManager {
	return &testableManager{
		Manager: Manager{
			secretsStore: store,
			config:       config,
		},
		ghClient: ghClient,
	}
}

// PrepareRunnerWithMock is a test helper that mimics PrepareRunner but uses mock GitHub client.
func (tm *testableManager) PrepareRunnerWithMock(ctx context.Context, req PrepareRunnerRequest) error {
	// Extract org from repo string (owner/repo format, required)
	if req.Repo == "" {
		return errors.New("repo is required (owner/repo format)")
	}
	parts := splitRepo(req.Repo)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return errors.New("invalid repo format, expected owner/repo: " + req.Repo)
	}
	org := parts[0]
	repoName := parts[1]

	// Build descriptive runner name
	runnerName := buildRunnerName(req.Pool, repoName, req.Arch, req.JobID)

	// Get registration token from mock GitHub client
	regResult, err := tm.ghClient.GetRegistrationToken(ctx, req.Repo)
	if err != nil {
		return errors.New("failed to get registration token: " + err.Error())
	}

	// Generate cache token with repository scope for cache isolation
	cacheToken := ""
	if tm.config.CacheSecret != "" {
		cacheToken = "generated-cache-token-" + req.JobID
	}

	// Build runner config with dynamic org from repo
	config := &secrets.RunnerConfig{
		Org:                 org,
		Repo:                req.Repo,
		RunID:               req.RunID,
		JITToken:            regResult.Token,
		Labels:              req.Labels,
		RunnerName:          runnerName,
		JobID:               req.JobID,
		CacheToken:          cacheToken,
		CacheURL:            tm.config.CacheURL,
		TerminationQueueURL: tm.config.TerminationQueueURL,
		IsOrg:               regResult.IsOrg,
	}

	// Store in secrets backend
	if err := tm.secretsStore.Put(ctx, req.InstanceID, config); err != nil {
		return errors.New("failed to store runner config: " + err.Error())
	}

	return nil
}

func splitRepo(repo string) []string {
	for i := 0; i < len(repo); i++ {
		if repo[i] == '/' {
			return []string{repo[:i], repo[i+1:]}
		}
	}
	return []string{repo}
}

func TestManager_PrepareRunnerWithMock_Success(t *testing.T) {
	mockStore := &mockSecretsStore{}
	mockGH := &mockGitHubClientForManager{
		regToken: "test-registration-token",
		isOrg:    true,
	}

	tm := newTestableManager(mockStore, mockGH, ManagerConfig{
		CacheSecret:         "test-secret",
		CacheURL:            testCacheURL,
		TerminationQueueURL: testTerminationQueueURL,
	})

	req := PrepareRunnerRequest{
		InstanceID: "i-test-12345",
		JobID:      "job-success-123",
		RunID:      "run-success-456",
		Repo:       "testorg/testrepo",
		Labels:     []string{"self-hosted", "linux"},
	}

	err := tm.PrepareRunnerWithMock(context.Background(), req)
	if err != nil {
		t.Fatalf("PrepareRunnerWithMock() error = %v", err)
	}

	// Verify secrets store was called
	if mockStore.putCalls != 1 {
		t.Errorf("expected 1 put call, got %d", mockStore.putCalls)
	}

	// Verify the runner ID
	if mockStore.lastPutID != "i-test-12345" {
		t.Errorf("runner ID = %s, want i-test-12345", mockStore.lastPutID)
	}

	// Verify the stored config contains expected values
	storedConfig := mockStore.lastPutCfg
	if storedConfig == nil {
		t.Fatal("stored config is nil")
		return
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
	if !storedConfig.IsOrg {
		t.Error("IsOrg should be true")
	}
}

func TestManager_PrepareRunnerWithMock_GitHubError(t *testing.T) {
	mockStore := &mockSecretsStore{}
	mockGH := &mockGitHubClientForManager{
		regErr: errors.New("GitHub API rate limit exceeded"),
	}

	tm := newTestableManager(mockStore, mockGH, ManagerConfig{
		CacheSecret:         "test-secret",
		CacheURL:            testCacheURL,
		TerminationQueueURL: testTerminationQueueURL,
	})

	req := PrepareRunnerRequest{
		InstanceID: "i-gh-error-12345",
		JobID:      "job-gh-error-123",
		RunID:      "run-gh-error-456",
		Repo:       "testorg/testrepo",
		Labels:     []string{"self-hosted"},
	}

	err := tm.PrepareRunnerWithMock(context.Background(), req)
	if err == nil {
		t.Error("expected error from GitHub client")
	}
	if !contains(err.Error(), "registration token") {
		t.Errorf("expected error to mention 'registration token', got: %s", err.Error())
	}

	// Secrets store should not be called since GitHub failed first
	if mockStore.putCalls != 0 {
		t.Errorf("secrets store should not be called when GitHub fails, got %d calls", mockStore.putCalls)
	}
}

func TestManager_PrepareRunnerWithMock_StoreError(t *testing.T) {
	mockStore := &mockSecretsStore{
		putFunc: func(_ context.Context, _ string, _ *secrets.RunnerConfig) error {
			return errors.New("secrets store unavailable")
		},
	}
	mockGH := &mockGitHubClientForManager{
		regToken: "test-token",
		isOrg:    false,
	}

	tm := newTestableManager(mockStore, mockGH, ManagerConfig{
		CacheSecret:         "",
		CacheURL:            testCacheURL,
		TerminationQueueURL: testTerminationQueueURL,
	})

	req := PrepareRunnerRequest{
		InstanceID: "i-store-error-12345",
		JobID:      "job-store-error-123",
		RunID:      "run-store-error-456",
		Repo:       "testorg/testrepo",
		Labels:     []string{"self-hosted"},
	}

	err := tm.PrepareRunnerWithMock(context.Background(), req)
	if err == nil {
		t.Error("expected error from secrets store")
	}
	if !contains(err.Error(), "store") {
		t.Errorf("expected error to mention 'store', got: %s", err.Error())
	}
}

func TestManager_PrepareRunnerWithMock_NoCacheSecret(t *testing.T) {
	mockStore := &mockSecretsStore{}
	mockGH := &mockGitHubClientForManager{
		regToken: "test-token",
		isOrg:    false,
	}

	// No cache secret configured
	tm := newTestableManager(mockStore, mockGH, ManagerConfig{
		CacheSecret:         "",
		CacheURL:            testCacheURL,
		TerminationQueueURL: testTerminationQueueURL,
	})

	req := PrepareRunnerRequest{
		InstanceID: "i-no-cache-12345",
		JobID:      "job-no-cache-123",
		RunID:      "run-no-cache-456",
		Repo:       "testorg/testrepo",
		Labels:     []string{"self-hosted"},
	}

	err := tm.PrepareRunnerWithMock(context.Background(), req)
	if err != nil {
		t.Fatalf("PrepareRunnerWithMock() error = %v", err)
	}

	// Verify stored config has empty cache token
	storedConfig := mockStore.lastPutCfg
	if storedConfig.CacheToken != "" {
		t.Errorf("CacheToken should be empty when CacheSecret is not set, got: %s", storedConfig.CacheToken)
	}
}

func TestManager_PrepareRunnerWithMock_UserRepo(t *testing.T) {
	mockStore := &mockSecretsStore{}
	mockGH := &mockGitHubClientForManager{
		regToken: "user-token",
		isOrg:    false, // User, not org
	}

	tm := newTestableManager(mockStore, mockGH, ManagerConfig{
		CacheSecret: "secret",
		CacheURL:    testCacheURL,
	})

	req := PrepareRunnerRequest{
		InstanceID: "i-user-12345",
		JobID:      "job-user-123",
		RunID:      "run-user-456",
		Repo:       "myuser/myrepo",
		Labels:     []string{"self-hosted", "linux", "x64"},
	}

	err := tm.PrepareRunnerWithMock(context.Background(), req)
	if err != nil {
		t.Fatalf("PrepareRunnerWithMock() error = %v", err)
	}

	storedConfig := mockStore.lastPutCfg

	if storedConfig.Org != "myuser" {
		t.Errorf("Org = %s, want myuser", storedConfig.Org)
	}
	if storedConfig.IsOrg {
		t.Error("IsOrg should be false for user repo")
	}
}

func TestManager_PrepareRunnerWithMock_MultipleLabels(t *testing.T) {
	mockStore := &mockSecretsStore{}
	mockGH := &mockGitHubClientForManager{
		regToken: "test-token",
		isOrg:    true,
	}

	tm := newTestableManager(mockStore, mockGH, ManagerConfig{
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

	err := tm.PrepareRunnerWithMock(context.Background(), req)
	if err != nil {
		t.Fatalf("PrepareRunnerWithMock() error = %v", err)
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

func TestManager_PrepareRunner_SpecialRepoNames(t *testing.T) {
	tests := []struct {
		name       string
		repo       string
		validParts bool
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
			parts := splitRepo(tt.repo)
			hasValidFormat := len(parts) == 2 && parts[0] != "" && parts[1] != ""
			if hasValidFormat != tt.validParts {
				t.Errorf("repo %q: validParts = %v, want %v", tt.repo, hasValidFormat, tt.validParts)
			}
		})
	}
}

func TestSplitRepo(t *testing.T) {
	tests := []struct {
		input    string
		expected []string
	}{
		{"owner/repo", []string{"owner", "repo"}},
		{"org/project", []string{"org", "project"}},
		{"a/b", []string{"a", "b"}},
		{"noslash", []string{"noslash"}},
		{"", []string{""}},
		{"multiple/slashes/here", []string{"multiple", "slashes/here"}},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := splitRepo(tt.input)
			if len(result) != len(tt.expected) {
				t.Errorf("splitRepo(%q) length = %d, want %d", tt.input, len(result), len(tt.expected))
				return
			}
			for i := range result {
				if result[i] != tt.expected[i] {
					t.Errorf("splitRepo(%q)[%d] = %q, want %q", tt.input, i, result[i], tt.expected[i])
				}
			}
		})
	}
}

func TestBuildRunnerName(t *testing.T) {
	tests := []struct {
		name     string
		pool     string
		repoName string
		arch     string
		jobID    string
		want     string
	}{
		{
			name:     "pool job",
			pool:     "default",
			repoName: "myapp",
			arch:     "arm64",
			jobID:    "12345678",
			want:     "runs-fleet-runner-default-myapp-arm64-12345678",
		},
		{
			name:     "cold-start job",
			pool:     "",
			repoName: "myapp",
			arch:     "amd64",
			jobID:    "98765432",
			want:     "runs-fleet-runner-myapp-amd64-98765432",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildRunnerName(tt.pool, tt.repoName, tt.arch, tt.jobID)
			if got != tt.want {
				t.Errorf("buildRunnerName() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestManager_PrepareRunnerWithMock_RunnerName(t *testing.T) {
	mockStore := &mockSecretsStore{}
	mockGH := &mockGitHubClientForManager{
		regToken: "test-token",
		isOrg:    true,
	}

	tm := newTestableManager(mockStore, mockGH, ManagerConfig{
		CacheSecret: "secret",
	})

	req := PrepareRunnerRequest{
		InstanceID: "i-12345",
		JobID:      "99999",
		RunID:      "run-456",
		Repo:       "org/myapp",
		Labels:     []string{"self-hosted"},
		Pool:       "default",
		Arch:       "arm64",
	}

	err := tm.PrepareRunnerWithMock(context.Background(), req)
	if err != nil {
		t.Fatalf("PrepareRunnerWithMock() error = %v", err)
	}

	storedConfig := mockStore.lastPutCfg
	if storedConfig.RunnerName != "runs-fleet-runner-default-myapp-arm64-99999" {
		t.Errorf("RunnerName = %q, want %q", storedConfig.RunnerName, "runs-fleet-runner-default-myapp-arm64-99999")
	}
}

func TestManager_PrepareRunnerWithMock_RunnerName_ColdStart(t *testing.T) {
	mockStore := &mockSecretsStore{}
	mockGH := &mockGitHubClientForManager{
		regToken: "test-token",
		isOrg:    true,
	}

	tm := newTestableManager(mockStore, mockGH, ManagerConfig{})

	req := PrepareRunnerRequest{
		InstanceID: "i-12345",
		JobID:      "88888",
		RunID:      "run-456",
		Repo:       "org/myapp",
		Labels:     []string{"self-hosted"},
		Pool:       "",
		Arch:       "amd64",
	}

	err := tm.PrepareRunnerWithMock(context.Background(), req)
	if err != nil {
		t.Fatalf("PrepareRunnerWithMock() error = %v", err)
	}

	storedConfig := mockStore.lastPutCfg
	if storedConfig.RunnerName != "runs-fleet-runner-myapp-amd64-88888" {
		t.Errorf("RunnerName = %q, want %q", storedConfig.RunnerName, "runs-fleet-runner-myapp-amd64-88888")
	}
}

func TestRegistrationResult_Fields(t *testing.T) {
	result := &RegistrationResult{
		Token: "test-token-12345",
		IsOrg: true,
	}

	if result.Token != "test-token-12345" {
		t.Errorf("Token = %s, want test-token-12345", result.Token)
	}
	if !result.IsOrg {
		t.Error("IsOrg should be true")
	}

	result2 := &RegistrationResult{
		Token: "user-token",
		IsOrg: false,
	}

	if result2.IsOrg {
		t.Error("IsOrg should be false")
	}
}
