package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ConfigFetcher defines the interface for fetching runner configuration.
type ConfigFetcher interface {
	FetchConfig(ctx context.Context, source string) (*RunnerConfig, error)
}

// FileConfigFetcher reads runner configuration from mounted files (K8s ConfigMap/Secret).
type FileConfigFetcher struct {
	logger Logger
}

// NewFileConfigFetcher creates a new file-based config fetcher.
func NewFileConfigFetcher(logger Logger) *FileConfigFetcher {
	return &FileConfigFetcher{
		logger: logger,
	}
}

// FetchConfig reads runner configuration from a JSON file.
// source is the path to the configuration file.
func (f *FileConfigFetcher) FetchConfig(_ context.Context, source string) (*RunnerConfig, error) {
	f.logger.Printf("Reading config from file: %s", source)

	data, err := os.ReadFile(source)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var config RunnerConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("failed to parse config JSON: %w", err)
	}

	return &config, nil
}

// K8sConfigPaths holds the paths to K8s-mounted configuration files.
type K8sConfigPaths struct {
	ConfigDir string // Base directory where ConfigMap is mounted
	SecretDir string // Base directory where Secret is mounted
}

// DefaultK8sConfigPaths returns the default paths for K8s configuration.
func DefaultK8sConfigPaths() K8sConfigPaths {
	return K8sConfigPaths{
		ConfigDir: "/etc/runs-fleet/config",
		SecretDir: "/etc/runs-fleet/secrets",
	}
}

// FetchK8sConfig assembles RunnerConfig from K8s mounted volumes.
// ConfigMap contains: repo, labels, runner_group
// Secret contains: jit_token, cache_token
func FetchK8sConfig(_ context.Context, paths K8sConfigPaths, logger Logger) (*RunnerConfig, error) {
	config := &RunnerConfig{}

	// Read from ConfigMap (required: repo; optional: org, runner_group, job_id, labels)
	if repo, err := readFileContent(filepath.Join(paths.ConfigDir, "repo")); err == nil {
		config.Repo = repo
	} else if !os.IsNotExist(err) {
		logger.Printf("Warning: failed to read repo config: %v", err)
	}
	if org, err := readFileContent(filepath.Join(paths.ConfigDir, "org")); err == nil {
		config.Org = org
	} else if !os.IsNotExist(err) {
		logger.Printf("Warning: failed to read org config: %v", err)
	}
	if runnerGroup, err := readFileContent(filepath.Join(paths.ConfigDir, "runner_group")); err == nil {
		config.RunnerGroup = runnerGroup
	} else if !os.IsNotExist(err) {
		logger.Printf("Warning: failed to read runner_group config: %v", err)
	}
	if jobID, err := readFileContent(filepath.Join(paths.ConfigDir, "job_id")); err == nil {
		config.JobID = jobID
	} else if !os.IsNotExist(err) {
		logger.Printf("Warning: failed to read job_id config: %v", err)
	}

	// Read labels from file (comma-separated or JSON array)
	if labelsStr, err := readFileContent(filepath.Join(paths.ConfigDir, "labels")); err == nil {
		var labels []string
		if unmarshalErr := json.Unmarshal([]byte(labelsStr), &labels); unmarshalErr != nil {
			// Try comma-separated format
			labels = splitLabels(labelsStr)
		}
		config.Labels = labels
	} else if !os.IsNotExist(err) {
		logger.Printf("Warning: failed to read labels config: %v", err)
	}

	// Read from Secret (required: jit_token; optional: cache_token)
	if jitToken, err := readFileContent(filepath.Join(paths.SecretDir, "jit_token")); err == nil {
		config.JITToken = jitToken
	} else {
		return nil, fmt.Errorf("jit_token is required: %w", err)
	}

	if cacheToken, err := readFileContent(filepath.Join(paths.SecretDir, "cache_token")); err == nil {
		config.CacheToken = cacheToken
	} else if !os.IsNotExist(err) {
		logger.Printf("Warning: failed to read cache_token: %v", err)
	}

	if config.Repo == "" {
		return nil, fmt.Errorf("repo is required in config")
	}

	logger.Printf("Loaded K8s config for repo: %s", config.Repo)
	return config, nil
}

// readFileContent reads and trims a file's content.
func readFileContent(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// splitLabels splits comma-separated labels.
func splitLabels(s string) []string {
	if s == "" {
		return nil
	}
	var labels []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == ',' {
			if label := trimSpace(s[start:i]); label != "" {
				labels = append(labels, label)
			}
			start = i + 1
		}
	}
	if label := trimSpace(s[start:]); label != "" {
		labels = append(labels, label)
	}
	return labels
}

// trimSpace removes leading and trailing whitespace.
func trimSpace(s string) string {
	start := 0
	end := len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t') {
		end--
	}
	return s[start:end]
}

// IsK8sEnvironment detects if running in Kubernetes.
func IsK8sEnvironment() bool {
	// Standard K8s environment variable set by kubelet
	if os.Getenv("KUBERNETES_SERVICE_HOST") != "" {
		return true
	}
	// Also check for service account token
	if _, err := os.Stat("/var/run/secrets/kubernetes.io/serviceaccount/token"); err == nil {
		return true
	}
	return false
}
