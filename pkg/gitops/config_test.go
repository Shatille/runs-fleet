package gitops

import (
	"context"
	"errors"
	"testing"

	"github.com/Shavakan/runs-fleet/pkg/db"
)

// mockGitHubAPI implements GitHubAPI for testing.
type mockGitHubAPI struct {
	content []byte
	err     error
	calls   int
}

func (m *mockGitHubAPI) GetFileContent(ctx context.Context, owner, repo, path, ref string) ([]byte, error) {
	m.calls++
	if m.err != nil {
		return nil, m.err
	}
	return m.content, nil
}

// mockDBAPI implements DBAPI for testing.
type mockDBAPI struct {
	savedConfigs map[string]*db.PoolConfig
	getErr       error
	saveErr      error
	saveCalls    int
	getCalls     int
}

func newMockDBAPI() *mockDBAPI {
	return &mockDBAPI{
		savedConfigs: make(map[string]*db.PoolConfig),
	}
}

func (m *mockDBAPI) SavePoolConfig(ctx context.Context, config *db.PoolConfig) error {
	m.saveCalls++
	if m.saveErr != nil {
		return m.saveErr
	}
	m.savedConfigs[config.PoolName] = config
	return nil
}

func (m *mockDBAPI) GetPoolConfig(ctx context.Context, poolName string) (*db.PoolConfig, error) {
	m.getCalls++
	if m.getErr != nil {
		return nil, m.getErr
	}
	return m.savedConfigs[poolName], nil
}

func TestParseConfig_ValidConfig(t *testing.T) {
	yaml := `
version: "1"
pools:
  - name: default
    instance_type: m5.large
    desired_running: 2
    desired_stopped: 1
runners:
  - name: 4cpu-linux
    instance_type: m5.xlarge
    labels:
      - self-hosted
      - linux
`
	config, err := ParseConfig([]byte(yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if config.Version != "1" {
		t.Errorf("expected version '1', got '%s'", config.Version)
	}

	if len(config.Pools) != 1 {
		t.Fatalf("expected 1 pool, got %d", len(config.Pools))
	}

	pool := config.Pools[0]
	if pool.Name != "default" {
		t.Errorf("expected pool name 'default', got '%s'", pool.Name)
	}
	if pool.InstanceType != "m5.large" {
		t.Errorf("expected instance_type 'm5.large', got '%s'", pool.InstanceType)
	}
	if pool.DesiredRunning != 2 {
		t.Errorf("expected desired_running 2, got %d", pool.DesiredRunning)
	}

	if len(config.Runners) != 1 {
		t.Fatalf("expected 1 runner, got %d", len(config.Runners))
	}

	runner := config.Runners[0]
	if runner.Name != "4cpu-linux" {
		t.Errorf("expected runner name '4cpu-linux', got '%s'", runner.Name)
	}
	if len(runner.Labels) != 2 {
		t.Errorf("expected 2 labels, got %d", len(runner.Labels))
	}
}

func TestParseConfig_InvalidYAML(t *testing.T) {
	yaml := `
version: "1"
pools:
  - name: default
    instance_type: [invalid yaml
`
	_, err := ParseConfig([]byte(yaml))
	if err == nil {
		t.Fatal("expected error for invalid YAML")
	}
}

func TestParseConfig_EmptyConfig(t *testing.T) {
	yaml := ``
	config, err := ParseConfig([]byte(yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if config.Version != "" {
		t.Errorf("expected empty version, got '%s'", config.Version)
	}
}

func TestConfig_Validate_Valid(t *testing.T) {
	config := &Config{
		Version: "1",
		Pools: []PoolConfig{
			{
				Name:           "default",
				InstanceType:   "m5.large",
				DesiredRunning: 2,
				DesiredStopped: 1,
			},
		},
	}

	if err := config.Validate(); err != nil {
		t.Errorf("unexpected validation error: %v", err)
	}
}

func TestConfig_Validate_Version10(t *testing.T) {
	config := &Config{
		Version: "1.0",
		Pools: []PoolConfig{
			{
				Name:           "default",
				InstanceType:   "m5.large",
				DesiredRunning: 0,
				DesiredStopped: 0,
			},
		},
	}

	if err := config.Validate(); err != nil {
		t.Errorf("unexpected validation error for version 1.0: %v", err)
	}
}

func TestConfig_Validate_MissingVersion(t *testing.T) {
	config := &Config{
		Pools: []PoolConfig{
			{
				Name:         "default",
				InstanceType: "m5.large",
			},
		},
	}

	err := config.Validate()
	if err == nil {
		t.Fatal("expected validation error for missing version")
	}
	if err.Error() != "version is required" {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestConfig_Validate_UnsupportedVersion(t *testing.T) {
	config := &Config{
		Version: "2.0",
		Pools:   []PoolConfig{},
	}

	err := config.Validate()
	if err == nil {
		t.Fatal("expected validation error for unsupported version")
	}
}

func TestConfig_Validate_MissingPoolName(t *testing.T) {
	config := &Config{
		Version: "1",
		Pools: []PoolConfig{
			{
				InstanceType:   "m5.large",
				DesiredRunning: 2,
			},
		},
	}

	err := config.Validate()
	if err == nil {
		t.Fatal("expected validation error for missing pool name")
	}
}

func TestConfig_Validate_DuplicatePoolName(t *testing.T) {
	config := &Config{
		Version: "1",
		Pools: []PoolConfig{
			{
				Name:           "default",
				InstanceType:   "m5.large",
				DesiredRunning: 2,
			},
			{
				Name:           "default",
				InstanceType:   "m5.xlarge",
				DesiredRunning: 1,
			},
		},
	}

	err := config.Validate()
	if err == nil {
		t.Fatal("expected validation error for duplicate pool name")
	}
}

func TestConfig_Validate_MissingInstanceType(t *testing.T) {
	config := &Config{
		Version: "1",
		Pools: []PoolConfig{
			{
				Name:           "default",
				DesiredRunning: 2,
			},
		},
	}

	err := config.Validate()
	if err == nil {
		t.Fatal("expected validation error for missing instance_type")
	}
}

func TestConfig_Validate_NegativeDesiredRunning(t *testing.T) {
	config := &Config{
		Version: "1",
		Pools: []PoolConfig{
			{
				Name:           "default",
				InstanceType:   "m5.large",
				DesiredRunning: -1,
			},
		},
	}

	err := config.Validate()
	if err == nil {
		t.Fatal("expected validation error for negative desired_running")
	}
}

func TestConfig_Validate_NegativeDesiredStopped(t *testing.T) {
	config := &Config{
		Version: "1",
		Pools: []PoolConfig{
			{
				Name:           "default",
				InstanceType:   "m5.large",
				DesiredStopped: -1,
			},
		},
	}

	err := config.Validate()
	if err == nil {
		t.Fatal("expected validation error for negative desired_stopped")
	}
}

func TestConfig_Validate_InvalidEnvironment(t *testing.T) {
	config := &Config{
		Version: "1",
		Pools: []PoolConfig{
			{
				Name:         "default",
				InstanceType: "m5.large",
				Environment:  "invalid",
			},
		},
	}

	err := config.Validate()
	if err == nil {
		t.Fatal("expected validation error for invalid environment")
	}
}

func TestConfig_Validate_ValidEnvironments(t *testing.T) {
	environments := []string{"dev", "staging", "prod"}

	for _, env := range environments {
		config := &Config{
			Version: "1",
			Pools: []PoolConfig{
				{
					Name:         "default",
					InstanceType: "m5.large",
					Environment:  env,
				},
			},
		}

		if err := config.Validate(); err != nil {
			t.Errorf("unexpected validation error for environment '%s': %v", env, err)
		}
	}
}

func TestConfig_Validate_InvalidScheduleStartHour(t *testing.T) {
	config := &Config{
		Version: "1",
		Pools: []PoolConfig{
			{
				Name:         "default",
				InstanceType: "m5.large",
				Schedules: []PoolSchedule{
					{
						Name:      "peak",
						StartHour: 25, // Invalid
						EndHour:   18,
					},
				},
			},
		},
	}

	err := config.Validate()
	if err == nil {
		t.Fatal("expected validation error for invalid start_hour")
	}
}

func TestConfig_Validate_InvalidScheduleEndHour(t *testing.T) {
	config := &Config{
		Version: "1",
		Pools: []PoolConfig{
			{
				Name:         "default",
				InstanceType: "m5.large",
				Schedules: []PoolSchedule{
					{
						Name:      "peak",
						StartHour: 9,
						EndHour:   -1, // Invalid
					},
				},
			},
		},
	}

	err := config.Validate()
	if err == nil {
		t.Fatal("expected validation error for invalid end_hour")
	}
}

func TestConfig_Validate_InvalidScheduleDaysOfWeek(t *testing.T) {
	config := &Config{
		Version: "1",
		Pools: []PoolConfig{
			{
				Name:         "default",
				InstanceType: "m5.large",
				Schedules: []PoolSchedule{
					{
						Name:       "peak",
						StartHour:  9,
						EndHour:    18,
						DaysOfWeek: []int{1, 2, 8}, // 8 is invalid
					},
				},
			},
		},
	}

	err := config.Validate()
	if err == nil {
		t.Fatal("expected validation error for invalid days_of_week")
	}
}

func TestConfig_Validate_NegativeScheduleDesiredRunning(t *testing.T) {
	config := &Config{
		Version: "1",
		Pools: []PoolConfig{
			{
				Name:         "default",
				InstanceType: "m5.large",
				Schedules: []PoolSchedule{
					{
						Name:           "peak",
						StartHour:      9,
						EndHour:        18,
						DesiredRunning: -1, // Invalid
					},
				},
			},
		},
	}

	err := config.Validate()
	if err == nil {
		t.Fatal("expected validation error for negative schedule desired_running")
	}
}

func TestConfig_Validate_NegativeScheduleDesiredStopped(t *testing.T) {
	config := &Config{
		Version: "1",
		Pools: []PoolConfig{
			{
				Name:         "default",
				InstanceType: "m5.large",
				Schedules: []PoolSchedule{
					{
						Name:           "peak",
						StartHour:      9,
						EndHour:        18,
						DesiredStopped: -1, // Invalid
					},
				},
			},
		},
	}

	err := config.Validate()
	if err == nil {
		t.Fatal("expected validation error for negative schedule desired_stopped")
	}
}

func TestConfig_Validate_MissingRunnerName(t *testing.T) {
	config := &Config{
		Version: "1",
		Runners: []RunnerSpec{
			{
				InstanceType: "m5.large",
			},
		},
	}

	err := config.Validate()
	if err == nil {
		t.Fatal("expected validation error for missing runner name")
	}
}

func TestConfig_Validate_DuplicateRunnerName(t *testing.T) {
	config := &Config{
		Version: "1",
		Runners: []RunnerSpec{
			{
				Name:         "runner-1",
				InstanceType: "m5.large",
			},
			{
				Name:         "runner-1",
				InstanceType: "m5.xlarge",
			},
		},
	}

	err := config.Validate()
	if err == nil {
		t.Fatal("expected validation error for duplicate runner name")
	}
}

func TestConfig_Validate_MissingRunnerInstanceType(t *testing.T) {
	config := &Config{
		Version: "1",
		Runners: []RunnerSpec{
			{
				Name: "runner-1",
			},
		},
	}

	err := config.Validate()
	if err == nil {
		t.Fatal("expected validation error for missing runner instance_type")
	}
}

func TestNewWatcher(t *testing.T) {
	gh := &mockGitHubAPI{}
	db := newMockDBAPI()

	watcher := NewWatcher(gh, db, "myorg", "myrepo", "")

	if watcher.configOwner != "myorg" {
		t.Errorf("expected configOwner 'myorg', got '%s'", watcher.configOwner)
	}
	if watcher.configRepo != "myrepo" {
		t.Errorf("expected configRepo 'myrepo', got '%s'", watcher.configRepo)
	}
	if watcher.configPath != ".github/runs-fleet.yml" {
		t.Errorf("expected default configPath, got '%s'", watcher.configPath)
	}
}

func TestNewWatcher_CustomPath(t *testing.T) {
	gh := &mockGitHubAPI{}
	db := newMockDBAPI()

	watcher := NewWatcher(gh, db, "myorg", "myrepo", "custom/path.yml")

	if watcher.configPath != "custom/path.yml" {
		t.Errorf("expected configPath 'custom/path.yml', got '%s'", watcher.configPath)
	}
}

func TestWatcher_HandlePushEvent_NotConfigRepo(t *testing.T) {
	gh := &mockGitHubAPI{}
	db := newMockDBAPI()
	watcher := NewWatcher(gh, db, "myorg", "myrepo", "")

	err := watcher.HandlePushEvent(context.Background(), "otherorg", "otherrepo", "main", []string{".github/runs-fleet.yml"})
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	// Should not have called GitHub API
	if gh.calls > 0 {
		t.Errorf("expected no GitHub API calls for non-config repo")
	}
}

func TestWatcher_HandlePushEvent_ConfigNotModified(t *testing.T) {
	gh := &mockGitHubAPI{}
	db := newMockDBAPI()
	watcher := NewWatcher(gh, db, "myorg", "myrepo", "")

	err := watcher.HandlePushEvent(context.Background(), "myorg", "myrepo", "main", []string{"README.md", "src/main.go"})
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	// Should not have called GitHub API
	if gh.calls > 0 {
		t.Errorf("expected no GitHub API calls when config not modified")
	}
}

func TestWatcher_HandlePushEvent_ConfigModified(t *testing.T) {
	yaml := `
version: "1"
pools:
  - name: default
    instance_type: m5.large
    desired_running: 2
`
	gh := &mockGitHubAPI{content: []byte(yaml)}
	db := newMockDBAPI()
	watcher := NewWatcher(gh, db, "myorg", "myrepo", "")

	err := watcher.HandlePushEvent(context.Background(), "myorg", "myrepo", "main", []string{".github/runs-fleet.yml"})
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	// Should have called GitHub API
	if gh.calls != 1 {
		t.Errorf("expected 1 GitHub API call, got %d", gh.calls)
	}

	// Should have saved config
	if db.saveCalls != 1 {
		t.Errorf("expected 1 DB save call, got %d", db.saveCalls)
	}
}

func TestWatcher_HandlePushEvent_ConfigModifiedWithSuffix(t *testing.T) {
	yaml := `
version: "1"
pools:
  - name: default
    instance_type: m5.large
    desired_running: 2
`
	gh := &mockGitHubAPI{content: []byte(yaml)}
	db := newMockDBAPI()
	watcher := NewWatcher(gh, db, "myorg", "myrepo", "")

	err := watcher.HandlePushEvent(context.Background(), "myorg", "myrepo", "main", []string{"some/path/runs-fleet.yml"})
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	// Should have called GitHub API (detected by suffix)
	if gh.calls != 1 {
		t.Errorf("expected 1 GitHub API call, got %d", gh.calls)
	}
}

func TestWatcher_ReloadConfig(t *testing.T) {
	yaml := `
version: "1"
pools:
  - name: default
    instance_type: m5.large
    desired_running: 2
    desired_stopped: 1
    idle_timeout_minutes: 30
    environment: prod
    region: us-west-2
    schedules:
      - name: peak
        start_hour: 9
        end_hour: 18
        days_of_week: [1, 2, 3, 4, 5]
        desired_running: 5
        desired_stopped: 2
runners:
  - name: 4cpu-linux
    instance_type: m5.xlarge
    labels:
      - self-hosted
`
	gh := &mockGitHubAPI{content: []byte(yaml)}
	db := newMockDBAPI()
	watcher := NewWatcher(gh, db, "myorg", "myrepo", "")

	err := watcher.ReloadConfig(context.Background(), "main")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify pool config was saved
	savedConfig := db.savedConfigs["default"]
	if savedConfig == nil {
		t.Fatal("expected pool config to be saved")
	}

	if savedConfig.InstanceType != "m5.large" {
		t.Errorf("expected instance_type 'm5.large', got '%s'", savedConfig.InstanceType)
	}
	if savedConfig.DesiredRunning != 2 {
		t.Errorf("expected desired_running 2, got %d", savedConfig.DesiredRunning)
	}
	if savedConfig.IdleTimeoutMinutes != 30 {
		t.Errorf("expected idle_timeout_minutes 30, got %d", savedConfig.IdleTimeoutMinutes)
	}
	if savedConfig.Environment != "prod" {
		t.Errorf("expected environment 'prod', got '%s'", savedConfig.Environment)
	}
	if savedConfig.Region != "us-west-2" {
		t.Errorf("expected region 'us-west-2', got '%s'", savedConfig.Region)
	}
	if len(savedConfig.Schedules) != 1 {
		t.Fatalf("expected 1 schedule, got %d", len(savedConfig.Schedules))
	}

	// Verify runner spec was cached
	spec, ok := watcher.GetRunnerSpec("4cpu-linux")
	if !ok {
		t.Fatal("expected runner spec to be cached")
	}
	if spec.InstanceType != "m5.xlarge" {
		t.Errorf("expected runner instance_type 'm5.xlarge', got '%s'", spec.InstanceType)
	}
}

func TestWatcher_ReloadConfig_FetchError(t *testing.T) {
	gh := &mockGitHubAPI{err: errors.New("fetch error")}
	db := newMockDBAPI()
	watcher := NewWatcher(gh, db, "myorg", "myrepo", "")

	err := watcher.ReloadConfig(context.Background(), "main")
	if err == nil {
		t.Fatal("expected error from fetch failure")
	}
}

func TestWatcher_ReloadConfig_ParseError(t *testing.T) {
	gh := &mockGitHubAPI{content: []byte("invalid: [yaml")}
	db := newMockDBAPI()
	watcher := NewWatcher(gh, db, "myorg", "myrepo", "")

	err := watcher.ReloadConfig(context.Background(), "main")
	if err == nil {
		t.Fatal("expected error from parse failure")
	}
}

func TestWatcher_ReloadConfig_ValidationError(t *testing.T) {
	yaml := `
pools:
  - name: default
    instance_type: m5.large
`
	gh := &mockGitHubAPI{content: []byte(yaml)}
	db := newMockDBAPI()
	watcher := NewWatcher(gh, db, "myorg", "myrepo", "")

	err := watcher.ReloadConfig(context.Background(), "main")
	if err == nil {
		t.Fatal("expected error from validation failure (missing version)")
	}
}

func TestWatcher_ReloadConfig_SaveError(t *testing.T) {
	yaml := `
version: "1"
pools:
  - name: default
    instance_type: m5.large
`
	gh := &mockGitHubAPI{content: []byte(yaml)}
	db := newMockDBAPI()
	db.saveErr = errors.New("save error")
	watcher := NewWatcher(gh, db, "myorg", "myrepo", "")

	err := watcher.ReloadConfig(context.Background(), "main")
	if err == nil {
		t.Fatal("expected error from save failure")
	}
}

func TestWatcher_GetRunnerSpec_NotFound(t *testing.T) {
	gh := &mockGitHubAPI{}
	db := newMockDBAPI()
	watcher := NewWatcher(gh, db, "myorg", "myrepo", "")

	_, ok := watcher.GetRunnerSpec("nonexistent")
	if ok {
		t.Error("expected runner spec not found")
	}
}

func TestDecodeBase64Content(t *testing.T) {
	tests := []struct {
		name     string
		encoded  string
		expected string
		wantErr  bool
	}{
		{
			name:     "simple encoding",
			encoded:  "SGVsbG8gV29ybGQ=",
			expected: "Hello World",
			wantErr:  false,
		},
		{
			name:     "encoding with newlines",
			encoded:  "SGVs\nbG8g\nV29y\nbGQ=",
			expected: "Hello World",
			wantErr:  false,
		},
		{
			name:     "encoding with spaces",
			encoded:  "SGVs bG8g V29y bGQ=",
			expected: "Hello World",
			wantErr:  false,
		},
		{
			name:     "invalid base64",
			encoded:  "invalid!!!",
			expected: "",
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			decoded, err := DecodeBase64Content(tt.encoded)
			if (err != nil) != tt.wantErr {
				t.Errorf("DecodeBase64Content() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && string(decoded) != tt.expected {
				t.Errorf("DecodeBase64Content() = %v, want %v", string(decoded), tt.expected)
			}
		})
	}
}

func TestWatcher_ApplyConfig_MultiplePools(t *testing.T) {
	gh := &mockGitHubAPI{}
	db := newMockDBAPI()
	watcher := NewWatcher(gh, db, "myorg", "myrepo", "")

	config := &Config{
		Version: "1",
		Pools: []PoolConfig{
			{
				Name:           "pool-1",
				InstanceType:   "m5.large",
				DesiredRunning: 2,
			},
			{
				Name:           "pool-2",
				InstanceType:   "m5.xlarge",
				DesiredRunning: 3,
			},
		},
		Runners: []RunnerSpec{
			{Name: "runner-1", InstanceType: "c5.large"},
			{Name: "runner-2", InstanceType: "c5.xlarge"},
		},
	}

	err := watcher.ApplyConfig(context.Background(), config)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if db.saveCalls != 2 {
		t.Errorf("expected 2 save calls, got %d", db.saveCalls)
	}

	// Verify both runner specs cached
	_, ok1 := watcher.GetRunnerSpec("runner-1")
	_, ok2 := watcher.GetRunnerSpec("runner-2")
	if !ok1 || !ok2 {
		t.Error("expected both runner specs to be cached")
	}
}
