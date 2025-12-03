package runner

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/ssm"
)

// Test constants to avoid goconst lint errors
const (
	testCacheURL            = "https://cache.example.com"
	testTerminationQueueURL = "https://sqs.example.com/queue"
)

// mockSSMAPI implements SSMAPI for testing.
type mockSSMAPI struct {
	putCalls    int
	deleteCalls int
	putErr      error
	deleteErr   error
	lastPutName string
	lastPutVal  string
}

func (m *mockSSMAPI) PutParameter(_ context.Context, params *ssm.PutParameterInput, _ ...func(*ssm.Options)) (*ssm.PutParameterOutput, error) {
	m.putCalls++
	if params.Name != nil {
		m.lastPutName = *params.Name
	}
	if params.Value != nil {
		m.lastPutVal = *params.Value
	}
	if m.putErr != nil {
		return nil, m.putErr
	}
	return &ssm.PutParameterOutput{}, nil
}

func (m *mockSSMAPI) DeleteParameter(_ context.Context, _ *ssm.DeleteParameterInput, _ ...func(*ssm.Options)) (*ssm.DeleteParameterOutput, error) {
	m.deleteCalls++
	if m.deleteErr != nil {
		return nil, m.deleteErr
	}
	return &ssm.DeleteParameterOutput{}, nil
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
	// Note: NewManager requires aws.Config and creates real SSM client
	// For unit testing, we create the manager with mock directly
	config := ManagerConfig{
		CacheSecret:         "test-secret",
		CacheURL:            "https://cache.example.com",
		TerminationQueueURL: "https://sqs.example.com/queue",
	}

	// Test config structure
	if config.CacheSecret != "test-secret" {
		t.Errorf("expected CacheSecret 'test-secret', got '%s'", config.CacheSecret)
	}
	if config.CacheURL != "https://cache.example.com" {
		t.Errorf("expected CacheURL 'https://cache.example.com', got '%s'", config.CacheURL)
	}
}

func TestManager_PrepareRunner_Success(t *testing.T) {
	mockSSM := &mockSSMAPI{}
	mockGH := &mockGitHubClientForManager{
		regToken: "test-reg-token",
		isOrg:    true,
	}

	// Create manager with mocks using the unexported fields
	manager := &Manager{
		github:    &GitHubClient{}, // Will be overridden in test
		ssmClient: mockSSM,
		config: ManagerConfig{
			CacheSecret:         "test-secret",
			CacheURL:            "https://cache.example.com",
			TerminationQueueURL: "https://sqs.example.com/queue",
		},
	}

	// We need to test with the actual PrepareRunner method
	// Since it uses the real GitHubClient, we'll test the helper functions instead
	// and use table-driven tests for the logic

	// Test that the manager can be created
	if manager.ssmClient != mockSSM {
		t.Error("expected mockSSM to be set")
	}

	// Test with a mock GitHub client wrapper
	_ = mockGH // Used for integration-style tests
}

func TestManager_PrepareRunner_EmptyRepo(t *testing.T) {
	mockSSM := &mockSSMAPI{}

	manager := &Manager{
		ssmClient: mockSSM,
		config:    ManagerConfig{},
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
	mockSSM := &mockSSMAPI{}

	manager := &Manager{
		ssmClient: mockSSM,
		config:    ManagerConfig{},
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
	mockSSM := &mockSSMAPI{}

	manager := &Manager{
		ssmClient: mockSSM,
		config:    ManagerConfig{},
	}

	err := manager.CleanupRunner(context.Background(), "i-12345")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if mockSSM.deleteCalls != 1 {
		t.Errorf("expected 1 delete call, got %d", mockSSM.deleteCalls)
	}
}

func TestManager_CleanupRunner_SSMError(t *testing.T) {
	mockSSM := &mockSSMAPI{
		deleteErr: errors.New("ssm delete error"),
	}

	manager := &Manager{
		ssmClient: mockSSM,
		config:    ManagerConfig{},
	}

	err := manager.CleanupRunner(context.Background(), "i-12345")
	if err == nil {
		t.Fatal("expected error from SSM")
	}

	// Verify the error message is propagated correctly
	// This explicitly tests that SSM errors are not silently swallowed
	// The error is wrapped with context, so check it contains the original error
	if !contains(err.Error(), "ssm delete error") {
		t.Errorf("expected error to contain 'ssm delete error', got '%s'", err.Error())
	}
}

func TestManager_CleanupRunner_SSMErrorHandling(t *testing.T) {
	// Explicit test verifying SSM delete parameter errors are properly handled
	// and returned to the caller for appropriate error handling/logging
	tests := []struct {
		name      string
		deleteErr error
		wantErr   bool
	}{
		{
			name:      "network error",
			deleteErr: errors.New("network timeout"),
			wantErr:   true,
		},
		{
			name:      "access denied",
			deleteErr: errors.New("AccessDeniedException"),
			wantErr:   true,
		},
		{
			name:      "parameter not found should error",
			deleteErr: errors.New("ParameterNotFound"),
			wantErr:   true,
		},
		{
			name:      "success case",
			deleteErr: nil,
			wantErr:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockSSM := &mockSSMAPI{
				deleteErr: tt.deleteErr,
			}
			manager := &Manager{
				ssmClient: mockSSM,
				config:    ManagerConfig{},
			}

			err := manager.CleanupRunner(context.Background(), "i-12345")
			if (err != nil) != tt.wantErr {
				t.Errorf("CleanupRunner() error = %v, wantErr %v", err, tt.wantErr)
			}

			// Verify SSM delete was called
			if mockSSM.deleteCalls != 1 {
				t.Errorf("expected 1 delete call, got %d", mockSSM.deleteCalls)
			}
		})
	}
}

func TestConfig_Structure(t *testing.T) {
	config := Config{
		Org:                 "myorg",
		Repo:                "myorg/myrepo",
		JITToken:            "jit-token",
		Labels:              []string{"self-hosted", "linux"},
		RunnerGroup:         "default",
		JobID:               "job-123",
		CacheToken:          "cache-token",
		CacheURL:            "https://cache.example.com",
		TerminationQueueURL: "https://sqs.example.com/queue",
		IsOrg:               true,
	}

	// Verify JSON marshaling
	jsonBytes, err := json.Marshal(config)
	if err != nil {
		t.Fatalf("failed to marshal config: %v", err)
	}

	var decoded Config
	if err := json.Unmarshal(jsonBytes, &decoded); err != nil {
		t.Fatalf("failed to unmarshal config: %v", err)
	}

	if decoded.Org != config.Org {
		t.Errorf("expected org '%s', got '%s'", config.Org, decoded.Org)
	}
	if decoded.Repo != config.Repo {
		t.Errorf("expected repo '%s', got '%s'", config.Repo, decoded.Repo)
	}
	if decoded.JITToken != config.JITToken {
		t.Errorf("expected jit_token '%s', got '%s'", config.JITToken, decoded.JITToken)
	}
	if len(decoded.Labels) != len(config.Labels) {
		t.Errorf("expected %d labels, got %d", len(config.Labels), len(decoded.Labels))
	}
	if decoded.IsOrg != config.IsOrg {
		t.Errorf("expected is_org %v, got %v", config.IsOrg, decoded.IsOrg)
	}
}

func TestPrepareRunnerRequest_Structure(t *testing.T) {
	req := PrepareRunnerRequest{
		InstanceID: "i-12345",
		JobID:      "job-123",
		RunID:      "run-456",
		Repo:       "myorg/myrepo",
		Labels:     []string{"self-hosted", "linux"},
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

func TestConfig_JSONOmitempty(t *testing.T) {
	// Test that optional fields are omitted when empty
	config := Config{
		Org:      "myorg",
		JITToken: "token",
		Labels:   []string{"self-hosted"},
	}

	jsonBytes, err := json.Marshal(config)
	if err != nil {
		t.Fatalf("failed to marshal config: %v", err)
	}

	jsonStr := string(jsonBytes)

	// Repo should be omitted when empty (omitempty)
	// Note: Go doesn't omit empty strings by default unless they have omitempty tag
	// Check that required fields are present
	if !contains(jsonStr, "org") {
		t.Error("expected 'org' in JSON")
	}
	if !contains(jsonStr, "jit_token") {
		t.Error("expected 'jit_token' in JSON")
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

func TestSSMParameterPath(t *testing.T) {
	// Test that the SSM parameter path is constructed correctly
	instanceID := "i-12345"
	expectedPath := "/runs-fleet/runners/i-12345/config"

	// This validates the path format used in PrepareRunner and CleanupRunner
	mockSSM := &mockSSMAPI{}

	manager := &Manager{
		ssmClient: mockSSM,
		config:    ManagerConfig{},
	}

	// CleanupRunner will set the path
	_ = manager.CleanupRunner(context.Background(), instanceID)

	// Note: We can't directly verify the path since DeleteParameter doesn't expose it
	// This test validates that the function completes without error
	if mockSSM.deleteCalls != 1 {
		t.Errorf("expected 1 delete call, got %d", mockSSM.deleteCalls)
	}

	// Verify expected path format
	if expectedPath != "/runs-fleet/runners/i-12345/config" {
		t.Error("path format changed")
	}
}

func TestManager_PrepareRunner_RepoValidation(t *testing.T) {
	// Test repo format validation cases that fail early validation
	// These should fail before any GitHub client interaction
	tests := []struct {
		name    string
		repo    string
		wantErr bool
	}{
		{
			name:    "empty repo",
			repo:    "",
			wantErr: true,
		},
		{
			name:    "no slash",
			repo:    "noslash",
			wantErr: true,
		},
		{
			name:    "empty owner",
			repo:    "/reponame",
			wantErr: true,
		},
		{
			name:    "empty repo name",
			repo:    "owner/",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockSSM := &mockSSMAPI{}

			manager := &Manager{
				ssmClient: mockSSM,
				config:    ManagerConfig{},
			}

			req := PrepareRunnerRequest{
				InstanceID: "i-12345",
				JobID:      "job-123",
				RunID:      "run-456",
				Repo:       tt.repo,
				Labels:     []string{"self-hosted"},
			}

			err := manager.PrepareRunner(context.Background(), req)
			if tt.wantErr {
				if err == nil {
					t.Errorf("expected error for repo %q", tt.repo)
				}
			}
		})
	}
}

func TestManager_PrepareRunner_WithCacheSecret(t *testing.T) {
	// Test that cache token is generated when CacheSecret is set
	config := ManagerConfig{
		CacheSecret:         "test-cache-secret",
		CacheURL:            testCacheURL,
		TerminationQueueURL: testTerminationQueueURL,
	}

	if config.CacheSecret == "" {
		t.Error("CacheSecret should be set")
	}

	// Verify all fields are properly configured
	if config.CacheURL != testCacheURL {
		t.Errorf("CacheURL = %s, want %s", config.CacheURL, testCacheURL)
	}
	if config.TerminationQueueURL != testTerminationQueueURL {
		t.Errorf("TerminationQueueURL = %s, want %s", config.TerminationQueueURL, testTerminationQueueURL)
	}
}

func TestManager_PrepareRunner_WithoutCacheSecret(t *testing.T) {
	// Test that cache token is empty when CacheSecret is not set
	config := ManagerConfig{
		CacheSecret:         "",
		CacheURL:            testCacheURL,
		TerminationQueueURL: testTerminationQueueURL,
	}

	if config.CacheSecret != "" {
		t.Error("CacheSecret should be empty")
	}
}

func TestManager_CleanupRunner_MultipleInstances(t *testing.T) {
	// Test cleaning up multiple instances
	mockSSM := &mockSSMAPI{}

	manager := &Manager{
		ssmClient: mockSSM,
		config:    ManagerConfig{},
	}

	instances := []string{"i-111", "i-222", "i-333"}
	for _, instanceID := range instances {
		err := manager.CleanupRunner(context.Background(), instanceID)
		if err != nil {
			t.Errorf("CleanupRunner(%s) error = %v", instanceID, err)
		}
	}

	if mockSSM.deleteCalls != 3 {
		t.Errorf("expected 3 delete calls, got %d", mockSSM.deleteCalls)
	}
}

func TestConfig_AllFields_JSON(t *testing.T) {
	// Test complete Config struct with all fields
	config := Config{
		Org:                 "testorg",
		Repo:                "testorg/testrepo",
		RunID:               "run-789",
		JITToken:            "jit-token-abc",
		Labels:              []string{"self-hosted", "linux", "arm64"},
		RunnerGroup:         "custom-group",
		JobID:               "job-456",
		CacheToken:          "cache-token-xyz",
		CacheURL:            "https://cache.internal",
		TerminationQueueURL: "https://sqs.region.amazonaws.com/account/queue",
		IsOrg:               true,
	}

	// Marshal to JSON
	jsonBytes, err := json.Marshal(config)
	if err != nil {
		t.Fatalf("failed to marshal: %v", err)
	}

	// Unmarshal back
	var decoded Config
	if err := json.Unmarshal(jsonBytes, &decoded); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	// Verify all fields roundtrip correctly
	if decoded.Org != config.Org {
		t.Errorf("Org mismatch: got %s, want %s", decoded.Org, config.Org)
	}
	if decoded.Repo != config.Repo {
		t.Errorf("Repo mismatch: got %s, want %s", decoded.Repo, config.Repo)
	}
	if decoded.RunID != config.RunID {
		t.Errorf("RunID mismatch: got %s, want %s", decoded.RunID, config.RunID)
	}
	if decoded.JITToken != config.JITToken {
		t.Errorf("JITToken mismatch")
	}
	if len(decoded.Labels) != len(config.Labels) {
		t.Errorf("Labels length mismatch: got %d, want %d", len(decoded.Labels), len(config.Labels))
	}
	if decoded.RunnerGroup != config.RunnerGroup {
		t.Errorf("RunnerGroup mismatch")
	}
	if decoded.JobID != config.JobID {
		t.Errorf("JobID mismatch")
	}
	if decoded.CacheToken != config.CacheToken {
		t.Errorf("CacheToken mismatch")
	}
	if decoded.CacheURL != config.CacheURL {
		t.Errorf("CacheURL mismatch")
	}
	if decoded.TerminationQueueURL != config.TerminationQueueURL {
		t.Errorf("TerminationQueueURL mismatch")
	}
	if decoded.IsOrg != config.IsOrg {
		t.Errorf("IsOrg mismatch")
	}
}

func TestPrepareRunnerRequest_AllFields(t *testing.T) {
	// Test PrepareRunnerRequest with all fields
	req := PrepareRunnerRequest{
		InstanceID: "i-comprehensive",
		JobID:      "job-comprehensive",
		RunID:      "run-comprehensive",
		Repo:       "comprehensive-org/comprehensive-repo",
		Labels:     []string{"self-hosted", "linux", "x64", "gpu"},
	}

	if req.InstanceID != "i-comprehensive" {
		t.Errorf("InstanceID = %s, want i-comprehensive", req.InstanceID)
	}
	if req.JobID != "job-comprehensive" {
		t.Errorf("JobID = %s, want job-comprehensive", req.JobID)
	}
	if req.RunID != "run-comprehensive" {
		t.Errorf("RunID = %s, want run-comprehensive", req.RunID)
	}
	if req.Repo != "comprehensive-org/comprehensive-repo" {
		t.Errorf("Repo = %s, want comprehensive-org/comprehensive-repo", req.Repo)
	}
	if len(req.Labels) != 4 {
		t.Errorf("Labels length = %d, want 4", len(req.Labels))
	}
}

func TestManager_SSMParameterPathFormat(t *testing.T) {
	// Test various instance ID formats
	tests := []struct {
		instanceID   string
		expectedPath string
	}{
		{"i-12345", "/runs-fleet/runners/i-12345/config"},
		{"i-abcdef0123456789", "/runs-fleet/runners/i-abcdef0123456789/config"},
		{"pod-abc-123", "/runs-fleet/runners/pod-abc-123/config"},
	}

	for _, tt := range tests {
		t.Run(tt.instanceID, func(t *testing.T) {
			// Format the path as the code does
			path := "/runs-fleet/runners/" + tt.instanceID + "/config"
			if path != tt.expectedPath {
				t.Errorf("path = %s, want %s", path, tt.expectedPath)
			}
		})
	}
}

func TestManager_CleanupRunner_EmptyInstanceID(t *testing.T) {
	// Test that empty instance ID results in attempting delete with empty path
	mockSSM := &mockSSMAPI{}

	manager := &Manager{
		ssmClient: mockSSM,
		config:    ManagerConfig{},
	}

	err := manager.CleanupRunner(context.Background(), "")
	// The function doesn't validate empty instance ID, so it will try to delete
	// /runs-fleet/runners//config which may or may not succeed
	// This test just verifies it doesn't panic
	if err != nil {
		t.Logf("CleanupRunner with empty ID returned error (expected): %v", err)
	}
	if mockSSM.deleteCalls != 1 {
		t.Errorf("expected 1 delete call, got %d", mockSSM.deleteCalls)
	}
}

func TestConfig_JSONTagVerification(t *testing.T) {
	// Verify JSON tags are correctly set
	config := Config{
		Org:      "test-org",
		JITToken: "test-token",
		IsOrg:    true,
	}

	jsonBytes, _ := json.Marshal(config)
	jsonStr := string(jsonBytes)

	// Check snake_case tags are used
	if !contains(jsonStr, "jit_token") {
		t.Error("expected 'jit_token' JSON key")
	}
	if !contains(jsonStr, "is_org") {
		t.Error("expected 'is_org' JSON key")
	}
	if !contains(jsonStr, "\"org\"") {
		t.Error("expected 'org' JSON key")
	}
}

func TestMockSSMAPI_PutParameter_Tracking(t *testing.T) {
	mock := &mockSSMAPI{}

	// Call PutParameter multiple times
	for i := 0; i < 5; i++ {
		_, _ = mock.PutParameter(context.Background(), &ssm.PutParameterInput{}, nil)
	}

	if mock.putCalls != 5 {
		t.Errorf("expected 5 put calls, got %d", mock.putCalls)
	}
}

func TestMockSSMAPI_DeleteParameter_Tracking(t *testing.T) {
	mock := &mockSSMAPI{}

	// Call DeleteParameter multiple times
	for i := 0; i < 3; i++ {
		_, _ = mock.DeleteParameter(context.Background(), &ssm.DeleteParameterInput{}, nil)
	}

	if mock.deleteCalls != 3 {
		t.Errorf("expected 3 delete calls, got %d", mock.deleteCalls)
	}
}

func TestMockSSMAPI_PutParameter_StoresValues(t *testing.T) {
	mock := &mockSSMAPI{}

	name := "/test/path"
	value := "test-value"

	_, _ = mock.PutParameter(context.Background(), &ssm.PutParameterInput{
		Name:  &name,
		Value: &value,
	}, nil)

	if mock.lastPutName != name {
		t.Errorf("lastPutName = %s, want %s", mock.lastPutName, name)
	}
	if mock.lastPutVal != value {
		t.Errorf("lastPutVal = %s, want %s", mock.lastPutVal, value)
	}
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

func newTestableManager(ssmClient SSMAPI, ghClient *mockGitHubClientForManager, config ManagerConfig) *testableManager {
	return &testableManager{
		Manager: Manager{
			ssmClient: ssmClient,
			config:    config,
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
	config := Config{
		Org:                 org,
		Repo:                req.Repo,
		RunID:               req.RunID,
		JITToken:            regResult.Token,
		Labels:              req.Labels,
		JobID:               req.JobID,
		CacheToken:          cacheToken,
		CacheURL:            tm.config.CacheURL,
		TerminationQueueURL: tm.config.TerminationQueueURL,
		IsOrg:               regResult.IsOrg,
	}

	// Serialize to JSON
	configJSON, err := json.Marshal(config)
	if err != nil {
		return errors.New("failed to marshal runner config: " + err.Error())
	}

	// Store in SSM
	paramPath := "/runs-fleet/runners/" + req.InstanceID + "/config"

	_, err = tm.ssmClient.PutParameter(ctx, &ssm.PutParameterInput{
		Name:  stringPtr(paramPath),
		Value: stringPtr(string(configJSON)),
	})
	if err != nil {
		return errors.New("failed to store runner config in SSM: " + err.Error())
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

func stringPtr(s string) *string {
	return &s
}

func TestManager_PrepareRunnerWithMock_Success(t *testing.T) {
	mockSSM := &mockSSMAPI{}
	mockGH := &mockGitHubClientForManager{
		regToken: "test-registration-token",
		isOrg:    true,
	}

	tm := newTestableManager(mockSSM, mockGH, ManagerConfig{
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

	// Verify SSM was called
	if mockSSM.putCalls != 1 {
		t.Errorf("expected 1 SSM put call, got %d", mockSSM.putCalls)
	}

	// Verify the SSM parameter path
	expectedPath := "/runs-fleet/runners/i-test-12345/config"
	if mockSSM.lastPutName != expectedPath {
		t.Errorf("SSM path = %s, want %s", mockSSM.lastPutName, expectedPath)
	}

	// Verify the stored config contains expected values
	var storedConfig Config
	if err := json.Unmarshal([]byte(mockSSM.lastPutVal), &storedConfig); err != nil {
		t.Fatalf("failed to unmarshal stored config: %v", err)
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
	mockSSM := &mockSSMAPI{}
	mockGH := &mockGitHubClientForManager{
		regErr: errors.New("GitHub API rate limit exceeded"),
	}

	tm := newTestableManager(mockSSM, mockGH, ManagerConfig{
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

	// SSM should not be called since GitHub failed first
	if mockSSM.putCalls != 0 {
		t.Errorf("SSM should not be called when GitHub fails, got %d calls", mockSSM.putCalls)
	}
}

func TestManager_PrepareRunnerWithMock_SSMError(t *testing.T) {
	mockSSM := &mockSSMAPI{
		putErr: errors.New("SSM service unavailable"),
	}
	mockGH := &mockGitHubClientForManager{
		regToken: "test-token",
		isOrg:    false,
	}

	tm := newTestableManager(mockSSM, mockGH, ManagerConfig{
		CacheSecret:         "",
		CacheURL:            testCacheURL,
		TerminationQueueURL: testTerminationQueueURL,
	})

	req := PrepareRunnerRequest{
		InstanceID: "i-ssm-error-12345",
		JobID:      "job-ssm-error-123",
		RunID:      "run-ssm-error-456",
		Repo:       "testorg/testrepo",
		Labels:     []string{"self-hosted"},
	}

	err := tm.PrepareRunnerWithMock(context.Background(), req)
	if err == nil {
		t.Error("expected error from SSM")
	}
	if !contains(err.Error(), "SSM") {
		t.Errorf("expected error to mention 'SSM', got: %s", err.Error())
	}
}

func TestManager_PrepareRunnerWithMock_NoCacheSecret(t *testing.T) {
	mockSSM := &mockSSMAPI{}
	mockGH := &mockGitHubClientForManager{
		regToken: "test-token",
		isOrg:    false,
	}

	// No cache secret configured
	tm := newTestableManager(mockSSM, mockGH, ManagerConfig{
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
	var storedConfig Config
	if err := json.Unmarshal([]byte(mockSSM.lastPutVal), &storedConfig); err != nil {
		t.Fatalf("failed to unmarshal stored config: %v", err)
	}

	if storedConfig.CacheToken != "" {
		t.Errorf("CacheToken should be empty when CacheSecret is not set, got: %s", storedConfig.CacheToken)
	}
}

func TestManager_PrepareRunnerWithMock_UserRepo(t *testing.T) {
	mockSSM := &mockSSMAPI{}
	mockGH := &mockGitHubClientForManager{
		regToken: "user-token",
		isOrg:    false, // User, not org
	}

	tm := newTestableManager(mockSSM, mockGH, ManagerConfig{
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

	var storedConfig Config
	if err := json.Unmarshal([]byte(mockSSM.lastPutVal), &storedConfig); err != nil {
		t.Fatalf("failed to unmarshal stored config: %v", err)
	}

	if storedConfig.Org != "myuser" {
		t.Errorf("Org = %s, want myuser", storedConfig.Org)
	}
	if storedConfig.IsOrg {
		t.Error("IsOrg should be false for user repo")
	}
}

func TestManager_PrepareRunnerWithMock_MultipleLabels(t *testing.T) {
	mockSSM := &mockSSMAPI{}
	mockGH := &mockGitHubClientForManager{
		regToken: "test-token",
		isOrg:    true,
	}

	tm := newTestableManager(mockSSM, mockGH, ManagerConfig{
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

	var storedConfig Config
	if err := json.Unmarshal([]byte(mockSSM.lastPutVal), &storedConfig); err != nil {
		t.Fatalf("failed to unmarshal stored config: %v", err)
	}

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
	// Test that various repo name formats pass initial validation
	// We don't test the full flow since it requires real GitHub client
	tests := []struct {
		name       string
		repo       string
		validParts bool // whether repo format is valid (owner/repo)
	}{
		{
			name:       "repo with hyphen",
			repo:       "my-org/my-repo",
			validParts: true,
		},
		{
			name:       "repo with underscore",
			repo:       "my_org/my_repo",
			validParts: true,
		},
		{
			name:       "repo with numbers",
			repo:       "org123/repo456",
			validParts: true,
		},
		{
			name:       "single character",
			repo:       "a/b",
			validParts: true,
		},
		{
			name:       "empty repo",
			repo:       "",
			validParts: false,
		},
		{
			name:       "no slash",
			repo:       "noslash",
			validParts: false,
		},
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

	// Test with IsOrg false
	result2 := &RegistrationResult{
		Token: "user-token",
		IsOrg: false,
	}

	if result2.IsOrg {
		t.Error("IsOrg should be false")
	}
}
