package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

const (
	testRepoOwnerRepo = "owner/repo"
)

func TestNewFileConfigFetcher(t *testing.T) {
	logger := &mockLogger{}
	fetcher := NewFileConfigFetcher(logger)

	if fetcher == nil {
		t.Fatal("NewFileConfigFetcher() returned nil")
		return
	}
	if fetcher.logger == nil {
		t.Error("NewFileConfigFetcher() did not set logger")
	}
}

func TestFileConfigFetcher_FetchConfig(t *testing.T) {
	logger := &mockLogger{}
	fetcher := NewFileConfigFetcher(logger)

	t.Run("valid config file", func(t *testing.T) {
		tmpDir := t.TempDir()
		configPath := filepath.Join(tmpDir, "config.json")

		configContent := `{
			"repo": "owner/repo",
			"org": "myorg",
			"jit_token": "test-token",
			"runner_group": "default",
			"job_id": "job-123",
			"labels": ["self-hosted", "linux"],
			"cache_token": "cache-abc"
		}`

		if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
			t.Fatalf("failed to write test config: %v", err)
		}

		config, err := fetcher.FetchConfig(context.Background(), configPath)
		if err != nil {
			t.Fatalf("FetchConfig() error = %v", err)
		}

		if config.Repo != testRepoOwnerRepo {
			t.Errorf("expected Repo '%s', got '%s'", testRepoOwnerRepo, config.Repo)
		}
		if config.Org != "myorg" {
			t.Errorf("expected Org 'myorg', got '%s'", config.Org)
		}
		if config.JITToken != "test-token" {
			t.Errorf("expected JITToken 'test-token', got '%s'", config.JITToken)
		}
		if config.RunnerGroup != "default" {
			t.Errorf("expected RunnerGroup 'default', got '%s'", config.RunnerGroup)
		}
		if config.JobID != "job-123" {
			t.Errorf("expected JobID 'job-123', got '%s'", config.JobID)
		}
		if len(config.Labels) != 2 || config.Labels[0] != "self-hosted" {
			t.Errorf("expected Labels ['self-hosted', 'linux'], got %v", config.Labels)
		}
		if config.CacheToken != "cache-abc" {
			t.Errorf("expected CacheToken 'cache-abc', got '%s'", config.CacheToken)
		}
	})

	t.Run("file not found", func(t *testing.T) {
		_, err := fetcher.FetchConfig(context.Background(), "/nonexistent/path/config.json")
		if err == nil {
			t.Error("FetchConfig() expected error for non-existent file")
		}
	})

	t.Run("invalid JSON", func(t *testing.T) {
		tmpDir := t.TempDir()
		configPath := filepath.Join(tmpDir, "invalid.json")

		if err := os.WriteFile(configPath, []byte("not valid json"), 0644); err != nil {
			t.Fatalf("failed to write test config: %v", err)
		}

		_, err := fetcher.FetchConfig(context.Background(), configPath)
		if err == nil {
			t.Error("FetchConfig() expected error for invalid JSON")
		}
	})

	t.Run("empty config file", func(t *testing.T) {
		tmpDir := t.TempDir()
		configPath := filepath.Join(tmpDir, "empty.json")

		if err := os.WriteFile(configPath, []byte("{}"), 0644); err != nil {
			t.Fatalf("failed to write test config: %v", err)
		}

		config, err := fetcher.FetchConfig(context.Background(), configPath)
		if err != nil {
			t.Fatalf("FetchConfig() error = %v", err)
		}

		if config.Repo != "" {
			t.Errorf("expected empty Repo, got '%s'", config.Repo)
		}
	})
}

func TestDefaultK8sConfigPaths(t *testing.T) {
	paths := DefaultK8sConfigPaths()

	if paths.ConfigDir != "/etc/runs-fleet/config" {
		t.Errorf("expected ConfigDir '/etc/runs-fleet/config', got '%s'", paths.ConfigDir)
	}
	if paths.SecretDir != "/etc/runs-fleet/secrets" {
		t.Errorf("expected SecretDir '/etc/runs-fleet/secrets', got '%s'", paths.SecretDir)
	}
}

func TestFetchK8sConfig(t *testing.T) {
	logger := &mockLogger{}

	t.Run("complete config", func(t *testing.T) {
		tmpDir := t.TempDir()
		configDir := filepath.Join(tmpDir, "config")
		secretDir := filepath.Join(tmpDir, "secrets")

		if err := os.MkdirAll(configDir, 0755); err != nil {
			t.Fatalf("failed to create config dir: %v", err)
		}
		if err := os.MkdirAll(secretDir, 0755); err != nil {
			t.Fatalf("failed to create secret dir: %v", err)
		}

		// Write config files
		writeFile(t, filepath.Join(configDir, "repo"), testRepoOwnerRepo)
		writeFile(t, filepath.Join(configDir, "org"), "myorg")
		writeFile(t, filepath.Join(configDir, "runner_group"), "default")
		writeFile(t, filepath.Join(configDir, "job_id"), "job-123")
		writeFile(t, filepath.Join(configDir, "labels"), "self-hosted,linux,arm64")

		// Write secret files
		writeFile(t, filepath.Join(secretDir, "jit_token"), "test-jit-token")
		writeFile(t, filepath.Join(secretDir, "cache_token"), "test-cache-token")

		paths := K8sConfigPaths{
			ConfigDir: configDir,
			SecretDir: secretDir,
		}

		config, err := FetchK8sConfig(context.Background(), paths, logger)
		if err != nil {
			t.Fatalf("FetchK8sConfig() error = %v", err)
		}

		if config.Repo != testRepoOwnerRepo {
			t.Errorf("expected Repo '%s', got '%s'", testRepoOwnerRepo, config.Repo)
		}
		if config.Org != "myorg" {
			t.Errorf("expected Org 'myorg', got '%s'", config.Org)
		}
		if config.RunnerGroup != "default" {
			t.Errorf("expected RunnerGroup 'default', got '%s'", config.RunnerGroup)
		}
		if config.JobID != "job-123" {
			t.Errorf("expected JobID 'job-123', got '%s'", config.JobID)
		}
		if config.JITToken != "test-jit-token" {
			t.Errorf("expected JITToken 'test-jit-token', got '%s'", config.JITToken)
		}
		if config.CacheToken != "test-cache-token" {
			t.Errorf("expected CacheToken 'test-cache-token', got '%s'", config.CacheToken)
		}
		if len(config.Labels) != 3 {
			t.Errorf("expected 3 labels, got %d: %v", len(config.Labels), config.Labels)
		}
	})

	t.Run("labels as JSON array", func(t *testing.T) {
		tmpDir := t.TempDir()
		configDir := filepath.Join(tmpDir, "config")
		secretDir := filepath.Join(tmpDir, "secrets")

		if err := os.MkdirAll(configDir, 0755); err != nil {
			t.Fatalf("failed to create config dir: %v", err)
		}
		if err := os.MkdirAll(secretDir, 0755); err != nil {
			t.Fatalf("failed to create secret dir: %v", err)
		}

		writeFile(t, filepath.Join(configDir, "repo"), testRepoOwnerRepo)
		writeFile(t, filepath.Join(configDir, "labels"), `["self-hosted", "linux"]`)
		writeFile(t, filepath.Join(secretDir, "jit_token"), "test-token")

		paths := K8sConfigPaths{
			ConfigDir: configDir,
			SecretDir: secretDir,
		}

		config, err := FetchK8sConfig(context.Background(), paths, logger)
		if err != nil {
			t.Fatalf("FetchK8sConfig() error = %v", err)
		}

		if len(config.Labels) != 2 || config.Labels[0] != "self-hosted" || config.Labels[1] != "linux" {
			t.Errorf("expected Labels ['self-hosted', 'linux'], got %v", config.Labels)
		}
	})

	t.Run("missing jit_token", func(t *testing.T) {
		tmpDir := t.TempDir()
		configDir := filepath.Join(tmpDir, "config")
		secretDir := filepath.Join(tmpDir, "secrets")

		if err := os.MkdirAll(configDir, 0755); err != nil {
			t.Fatalf("failed to create config dir: %v", err)
		}
		if err := os.MkdirAll(secretDir, 0755); err != nil {
			t.Fatalf("failed to create secret dir: %v", err)
		}

		writeFile(t, filepath.Join(configDir, "repo"), testRepoOwnerRepo)
		// jit_token not provided

		paths := K8sConfigPaths{
			ConfigDir: configDir,
			SecretDir: secretDir,
		}

		_, err := FetchK8sConfig(context.Background(), paths, logger)
		if err == nil {
			t.Error("FetchK8sConfig() expected error when jit_token is missing")
		}
	})

	t.Run("missing repo", func(t *testing.T) {
		tmpDir := t.TempDir()
		configDir := filepath.Join(tmpDir, "config")
		secretDir := filepath.Join(tmpDir, "secrets")

		if err := os.MkdirAll(configDir, 0755); err != nil {
			t.Fatalf("failed to create config dir: %v", err)
		}
		if err := os.MkdirAll(secretDir, 0755); err != nil {
			t.Fatalf("failed to create secret dir: %v", err)
		}

		// repo not provided
		writeFile(t, filepath.Join(secretDir, "jit_token"), "test-token")

		paths := K8sConfigPaths{
			ConfigDir: configDir,
			SecretDir: secretDir,
		}

		_, err := FetchK8sConfig(context.Background(), paths, logger)
		if err == nil {
			t.Error("FetchK8sConfig() expected error when repo is missing")
		}
	})

	t.Run("whitespace trimmed", func(t *testing.T) {
		tmpDir := t.TempDir()
		configDir := filepath.Join(tmpDir, "config")
		secretDir := filepath.Join(tmpDir, "secrets")

		if err := os.MkdirAll(configDir, 0755); err != nil {
			t.Fatalf("failed to create config dir: %v", err)
		}
		if err := os.MkdirAll(secretDir, 0755); err != nil {
			t.Fatalf("failed to create secret dir: %v", err)
		}

		writeFile(t, filepath.Join(configDir, "repo"), "  owner/repo  \n")
		writeFile(t, filepath.Join(secretDir, "jit_token"), "  test-token  \n")

		paths := K8sConfigPaths{
			ConfigDir: configDir,
			SecretDir: secretDir,
		}

		config, err := FetchK8sConfig(context.Background(), paths, logger)
		if err != nil {
			t.Fatalf("FetchK8sConfig() error = %v", err)
		}

		if config.Repo != testRepoOwnerRepo {
			t.Errorf("expected Repo '%s' (trimmed), got '%s'", testRepoOwnerRepo, config.Repo)
		}
		if config.JITToken != "test-token" {
			t.Errorf("expected JITToken 'test-token' (trimmed), got '%s'", config.JITToken)
		}
	})
}

func TestSplitLabels(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected []string
	}{
		{
			name:     "empty string",
			input:    "",
			expected: nil,
		},
		{
			name:     "single label",
			input:    "self-hosted",
			expected: []string{"self-hosted"},
		},
		{
			name:     "multiple labels",
			input:    "self-hosted,linux,arm64",
			expected: []string{"self-hosted", "linux", "arm64"},
		},
		{
			name:     "labels with spaces",
			input:    " self-hosted , linux , arm64 ",
			expected: []string{"self-hosted", "linux", "arm64"},
		},
		{
			name:     "labels with tabs",
			input:    "self-hosted\t,\tlinux",
			expected: []string{"self-hosted", "linux"},
		},
		{
			name:     "trailing comma",
			input:    "self-hosted,linux,",
			expected: []string{"self-hosted", "linux"},
		},
		{
			name:     "empty entries",
			input:    "self-hosted,,linux",
			expected: []string{"self-hosted", "linux"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := splitLabels(tt.input)

			if len(result) != len(tt.expected) {
				t.Errorf("splitLabels(%q) = %v, want %v", tt.input, result, tt.expected)
				return
			}

			for i := range result {
				if result[i] != tt.expected[i] {
					t.Errorf("splitLabels(%q)[%d] = %q, want %q", tt.input, i, result[i], tt.expected[i])
				}
			}
		})
	}
}

func TestTrimSpace(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "no whitespace",
			input:    "hello",
			expected: "hello",
		},
		{
			name:     "leading spaces",
			input:    "   hello",
			expected: "hello",
		},
		{
			name:     "trailing spaces",
			input:    "hello   ",
			expected: "hello",
		},
		{
			name:     "both sides",
			input:    "   hello   ",
			expected: "hello",
		},
		{
			name:     "tabs",
			input:    "\t\thello\t\t",
			expected: "hello",
		},
		{
			name:     "mixed whitespace",
			input:    " \t hello \t ",
			expected: "hello",
		},
		{
			name:     "empty string",
			input:    "",
			expected: "",
		},
		{
			name:     "only whitespace",
			input:    "   \t   ",
			expected: "",
		},
		{
			name:     "internal spaces preserved",
			input:    "  hello world  ",
			expected: "hello world",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := trimSpace(tt.input)
			if result != tt.expected {
				t.Errorf("trimSpace(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestIsK8sEnvironment(t *testing.T) {
	t.Run("KUBERNETES_SERVICE_HOST set", func(t *testing.T) {
		// Save original value
		original := os.Getenv("KUBERNETES_SERVICE_HOST")
		defer func() { _ = os.Setenv("KUBERNETES_SERVICE_HOST", original) }()

		_ = os.Setenv("KUBERNETES_SERVICE_HOST", "10.0.0.1")

		if !IsK8sEnvironment() {
			t.Error("IsK8sEnvironment() should return true when KUBERNETES_SERVICE_HOST is set")
		}
	})

	t.Run("no K8s indicators", func(_ *testing.T) {
		// Save original value
		original := os.Getenv("KUBERNETES_SERVICE_HOST")
		defer func() { _ = os.Setenv("KUBERNETES_SERVICE_HOST", original) }()

		_ = os.Unsetenv("KUBERNETES_SERVICE_HOST")

		// This test may still return true if running in K8s due to service account check
		// We can't easily mock the file system check, so we just verify it doesn't panic
		_ = IsK8sEnvironment()
	})
}

// writeFile is a test helper that writes content to a file.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write file %s: %v", path, err)
	}
}
