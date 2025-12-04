// Package config manages application configuration from environment variables.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// Backend constants for compute provider selection.
const (
	BackendEC2 = "ec2"
	BackendK8s = "k8s"
)

// Toleration represents a Kubernetes toleration for runner pods.
type Toleration struct {
	Key      string `json:"key"`
	Operator string `json:"operator,omitempty"`
	Value    string `json:"value,omitempty"`
	Effect   string `json:"effect,omitempty"`
}

// Config holds all application configuration loaded from environment variables.
type Config struct {
	AWSRegion string

	// Provider selection
	DefaultBackend string // "ec2" or "k8s" (default: "ec2")

	GitHubWebhookSecret string
	GitHubAppID         string
	GitHubAppPrivateKey string

	QueueURL             string
	QueueDLQURL          string
	PoolQueueURL         string
	EventsQueueURL       string
	TerminationQueueURL  string
	HousekeepingQueueURL string
	LocksTableName       string
	JobsTableName        string
	PoolsTableName       string
	CircuitBreakerTable  string
	CacheBucketName      string
	ConfigBucketName     string
	CostReportSNSTopic   string
	CostReportBucket     string

	// EC2-specific configuration
	VPCID              string
	PublicSubnetIDs    []string
	PrivateSubnetIDs   []string
	SecurityGroupID    string
	InstanceProfileARN string
	KeyName            string

	SpotEnabled        bool
	MaxRuntimeMinutes  int
	LogLevel           string
	LaunchTemplateName string
	RunnerImage        string // Container image for EC2 runners (ECR URL)

	CoordinatorEnabled bool
	InstanceID         string

	CacheSecret string
	CacheURL    string

	// K8s-specific configuration
	KubeConfig             string            // Path to kubeconfig (empty = in-cluster)
	KubeNamespace          string            // Default namespace for runners
	KubeServiceAccount     string            // ServiceAccount for runner pods
	KubeNodeSelector       map[string]string // Default node selector for runners
	KubeTolerations        []Toleration      // Tolerations for runner pods
	KubeRunnerImage        string            // Container image for runner pods
	KubeIdleTimeoutMinutes int               // Idle timeout for K8s pods (default: 10)
	KubeReleaseName        string            // Helm release name for deployment naming
	KubeStorageClass       string            // StorageClass for runner PVCs (empty = default)

	// DinD (Docker-in-Docker) configuration for K8s runners
	KubeDindImage           string // DinD sidecar image (default: docker:dind)
	KubeDaemonJSONConfigMap string // ConfigMap name for daemon.json (optional)
	KubeDockerWaitSeconds   int    // Seconds to wait for Docker daemon (default: 120, range: 10-300)
	KubeDockerGroupGID      int    // Docker socket group GID (default: 123)
	KubeRegistryMirror      string // Docker registry mirror URL (optional)

	// Valkey queue configuration (K8s mode only)
	ValkeyAddr     string // Valkey/Redis address (e.g., "valkey:6379")
	ValkeyPassword string // Optional password
	ValkeyDB       int    // Database number (default: 0)
}

// Load reads configuration from environment variables and validates required fields.
func Load() (*Config, error) {
	maxRuntimeMinutes, err := getEnvInt("RUNS_FLEET_MAX_RUNTIME_MINUTES", 360)
	if err != nil {
		return nil, fmt.Errorf("config error: %w", err)
	}

	kubeIdleTimeoutMinutes, err := getEnvInt("RUNS_FLEET_KUBE_IDLE_TIMEOUT_MINUTES", 10)
	if err != nil {
		return nil, fmt.Errorf("config error: %w", err)
	}

	kubeDockerWaitSeconds, err := getEnvInt("RUNS_FLEET_KUBE_DOCKER_WAIT_SECONDS", 120)
	if err != nil {
		return nil, fmt.Errorf("config error: %w", err)
	}

	kubeDockerGroupGID, err := getEnvInt("RUNS_FLEET_KUBE_DOCKER_GROUP_GID", 123)
	if err != nil {
		return nil, fmt.Errorf("config error: %w", err)
	}

	valkeyDB, err := getEnvInt("RUNS_FLEET_VALKEY_DB", 0)
	if err != nil {
		return nil, fmt.Errorf("config error: %w", err)
	}

	cfg := &Config{
		AWSRegion:      getEnv("AWS_REGION", "ap-northeast-1"),
		DefaultBackend: getEnv("RUNS_FLEET_MODE", BackendEC2),

		GitHubWebhookSecret:  getEnv("RUNS_FLEET_GITHUB_WEBHOOK_SECRET", ""),
		GitHubAppID:          getEnv("RUNS_FLEET_GITHUB_APP_ID", ""),
		GitHubAppPrivateKey:  getEnv("RUNS_FLEET_GITHUB_APP_PRIVATE_KEY", ""),
		QueueURL:             getEnv("RUNS_FLEET_QUEUE_URL", ""),
		QueueDLQURL:          getEnv("RUNS_FLEET_QUEUE_DLQ_URL", ""),
		PoolQueueURL:         getEnv("RUNS_FLEET_POOL_QUEUE_URL", ""),
		EventsQueueURL:       getEnv("RUNS_FLEET_EVENTS_QUEUE_URL", ""),
		TerminationQueueURL:  getEnv("RUNS_FLEET_TERMINATION_QUEUE_URL", ""),
		HousekeepingQueueURL: getEnv("RUNS_FLEET_HOUSEKEEPING_QUEUE_URL", ""),
		LocksTableName:       getEnv("RUNS_FLEET_LOCKS_TABLE", ""),
		JobsTableName:        getEnv("RUNS_FLEET_JOBS_TABLE", ""),
		PoolsTableName:       getEnv("RUNS_FLEET_POOLS_TABLE", ""),
		CircuitBreakerTable:  getEnv("RUNS_FLEET_CIRCUIT_BREAKER_TABLE", "runs-fleet-circuit-state"),
		CacheBucketName:      getEnv("RUNS_FLEET_CACHE_BUCKET", ""),
		ConfigBucketName:     getEnv("RUNS_FLEET_CONFIG_BUCKET", ""),
		CostReportSNSTopic:   getEnv("RUNS_FLEET_COST_REPORT_SNS_TOPIC", ""),
		CostReportBucket:     getEnv("RUNS_FLEET_COST_REPORT_BUCKET", ""),

		// EC2-specific
		VPCID:              getEnv("RUNS_FLEET_VPC_ID", ""),
		PublicSubnetIDs:    splitAndFilter(getEnv("RUNS_FLEET_PUBLIC_SUBNET_IDS", "")),
		PrivateSubnetIDs:   splitAndFilter(getEnv("RUNS_FLEET_PRIVATE_SUBNET_IDS", "")),
		SecurityGroupID:    getEnv("RUNS_FLEET_SECURITY_GROUP_ID", ""),
		InstanceProfileARN: getEnv("RUNS_FLEET_INSTANCE_PROFILE_ARN", ""),
		KeyName:            getEnv("RUNS_FLEET_KEY_NAME", ""),
		SpotEnabled:        getEnvBool("RUNS_FLEET_SPOT_ENABLED", true),
		MaxRuntimeMinutes:  maxRuntimeMinutes,
		LogLevel:           getEnv("RUNS_FLEET_LOG_LEVEL", "info"),
		LaunchTemplateName: getEnv("RUNS_FLEET_LAUNCH_TEMPLATE_NAME", "runs-fleet-runner"),
		RunnerImage:        getEnv("RUNS_FLEET_RUNNER_IMAGE", ""),

		CoordinatorEnabled: getEnvBool("RUNS_FLEET_COORDINATOR_ENABLED", false),
		InstanceID:         getEnv("RUNS_FLEET_INSTANCE_ID", ""),

		CacheSecret: getEnv("RUNS_FLEET_CACHE_SECRET", ""),
		CacheURL:    getEnv("RUNS_FLEET_CACHE_URL", ""),

		// K8s-specific
		KubeConfig:             getEnv("RUNS_FLEET_KUBE_CONFIG", ""),
		KubeNamespace:          getEnv("RUNS_FLEET_KUBE_NAMESPACE", ""),
		KubeServiceAccount:     getEnv("RUNS_FLEET_KUBE_SERVICE_ACCOUNT", "runs-fleet-runner"),
		KubeRunnerImage:        getEnv("RUNS_FLEET_KUBE_RUNNER_IMAGE", ""),
		KubeIdleTimeoutMinutes: kubeIdleTimeoutMinutes,
		KubeReleaseName:        getEnv("RUNS_FLEET_KUBE_RELEASE_NAME", "runs-fleet"),
		KubeStorageClass:       getEnv("RUNS_FLEET_KUBE_STORAGE_CLASS", ""),

		// K8s DinD configuration
		KubeDindImage:           getEnv("RUNS_FLEET_KUBE_DIND_IMAGE", "docker:dind"),
		KubeDaemonJSONConfigMap: getEnv("RUNS_FLEET_KUBE_DAEMON_JSON_CONFIGMAP", ""),
		KubeDockerWaitSeconds:   kubeDockerWaitSeconds,
		KubeDockerGroupGID:      kubeDockerGroupGID,
		KubeRegistryMirror:      getEnv("RUNS_FLEET_KUBE_REGISTRY_MIRROR", ""),

		// Valkey queue (K8s mode)
		ValkeyAddr:     getEnv("RUNS_FLEET_VALKEY_ADDR", "valkey:6379"),
		ValkeyPassword: getEnv("RUNS_FLEET_VALKEY_PASSWORD", ""),
		ValkeyDB:       valkeyDB,
	}

	// Parse node selector with validation (only for K8s backend)
	if cfg.IsK8sBackend() {
		nodeSelectorStr := getEnv("RUNS_FLEET_KUBE_NODE_SELECTOR", "")
		if nodeSelectorStr != "" {
			nodeSelector, err := parseNodeSelector(nodeSelectorStr)
			if err != nil {
				return nil, fmt.Errorf("config error: %w", err)
			}
			cfg.KubeNodeSelector = nodeSelector
		} else {
			cfg.KubeNodeSelector = make(map[string]string)
		}

		// Parse tolerations JSON
		tolerationsStr := getEnv("RUNS_FLEET_KUBE_TOLERATIONS", "")
		if tolerationsStr != "" {
			tolerations, err := parseTolerations(tolerationsStr)
			if err != nil {
				return nil, fmt.Errorf("config error: %w", err)
			}
			cfg.KubeTolerations = tolerations
		}
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("config validation failed: %w", err)
	}

	return cfg, nil
}

// IsEC2Backend returns true if using EC2 as the compute backend.
func (c *Config) IsEC2Backend() bool {
	return c.DefaultBackend == BackendEC2 || c.DefaultBackend == ""
}

// IsK8sBackend returns true if using Kubernetes as the compute backend.
func (c *Config) IsK8sBackend() bool {
	return c.DefaultBackend == BackendK8s
}

// Validate checks that all required configuration fields are present and valid.
func (c *Config) Validate() error {
	// Validate backend selection
	if c.DefaultBackend != "" && c.DefaultBackend != BackendEC2 && c.DefaultBackend != BackendK8s {
		return fmt.Errorf("RUNS_FLEET_MODE must be 'ec2' or 'k8s', got %q", c.DefaultBackend)
	}

	// Common validation
	if c.GitHubWebhookSecret == "" {
		return fmt.Errorf("RUNS_FLEET_GITHUB_WEBHOOK_SECRET is required")
	}
	if c.GitHubAppID == "" {
		return fmt.Errorf("RUNS_FLEET_GITHUB_APP_ID is required")
	}
	if c.GitHubAppPrivateKey == "" {
		return fmt.Errorf("RUNS_FLEET_GITHUB_APP_PRIVATE_KEY is required")
	}

	// Queue validation: SQS for EC2, Valkey for K8s
	if c.IsEC2Backend() && c.QueueURL == "" {
		return fmt.Errorf("RUNS_FLEET_QUEUE_URL is required for EC2 backend")
	}
	if c.IsK8sBackend() && c.ValkeyAddr == "" {
		return fmt.Errorf("RUNS_FLEET_VALKEY_ADDR is required for K8s backend")
	}
	if c.MaxRuntimeMinutes <= 0 {
		return fmt.Errorf("RUNS_FLEET_MAX_RUNTIME_MINUTES must be greater than 0, got %d", c.MaxRuntimeMinutes)
	}
	if c.MaxRuntimeMinutes > 1440 {
		return fmt.Errorf("RUNS_FLEET_MAX_RUNTIME_MINUTES must not exceed 1440 (24 hours), got %d", c.MaxRuntimeMinutes)
	}

	// Backend-specific validation
	if c.IsEC2Backend() {
		if err := c.validateEC2Config(); err != nil {
			return err
		}
	}
	if c.IsK8sBackend() {
		if err := c.validateK8sConfig(); err != nil {
			return err
		}
	}

	return nil
}

// validateEC2Config validates EC2-specific configuration.
func (c *Config) validateEC2Config() error {
	if c.VPCID == "" {
		return fmt.Errorf("RUNS_FLEET_VPC_ID is required for EC2 backend")
	}
	if c.SecurityGroupID == "" {
		return fmt.Errorf("RUNS_FLEET_SECURITY_GROUP_ID is required for EC2 backend")
	}
	if c.InstanceProfileARN == "" {
		return fmt.Errorf("RUNS_FLEET_INSTANCE_PROFILE_ARN is required for EC2 backend")
	}
	if len(c.PublicSubnetIDs) == 0 && len(c.PrivateSubnetIDs) == 0 {
		return fmt.Errorf("at least one of RUNS_FLEET_PUBLIC_SUBNET_IDS or RUNS_FLEET_PRIVATE_SUBNET_IDS is required for EC2 backend")
	}
	if c.RunnerImage == "" {
		return fmt.Errorf("RUNS_FLEET_RUNNER_IMAGE is required for EC2 backend")
	}
	if err := validateECRImageURL(c.RunnerImage); err != nil {
		return fmt.Errorf("RUNS_FLEET_RUNNER_IMAGE: %w", err)
	}
	return nil
}

// ecrImageURLPattern matches valid ECR image URLs:
// <account-id>.dkr.ecr.<region>.amazonaws.com/<repo>[:<tag>][@sha256:<digest>]
var ecrImageURLPattern = regexp.MustCompile(`^[0-9]{12}\.dkr\.ecr\.[a-z0-9-]+\.amazonaws\.com/[a-z0-9][a-z0-9._/-]*[a-z0-9](:[a-zA-Z0-9._-]+)?(@sha256:[a-f0-9]{64})?$`)

func validateECRImageURL(url string) error {
	if !strings.Contains(url, ".dkr.ecr.") || !strings.Contains(url, ".amazonaws.com/") {
		return fmt.Errorf("must be an ECR URL (e.g., 123456789012.dkr.ecr.us-east-1.amazonaws.com/repo:tag), got %q", url)
	}
	if !ecrImageURLPattern.MatchString(url) {
		return fmt.Errorf("invalid ECR URL format: %q", url)
	}
	return nil
}

// validateK8sConfig validates Kubernetes-specific configuration.
func (c *Config) validateK8sConfig() error {
	if c.KubeNamespace == "" {
		return fmt.Errorf("RUNS_FLEET_KUBE_NAMESPACE is required for K8s backend")
	}
	if c.KubeRunnerImage == "" {
		return fmt.Errorf("RUNS_FLEET_KUBE_RUNNER_IMAGE is required for K8s backend")
	}
	if c.KubeDockerWaitSeconds < 10 || c.KubeDockerWaitSeconds > 300 {
		return fmt.Errorf("RUNS_FLEET_KUBE_DOCKER_WAIT_SECONDS must be between 10 and 300, got %d", c.KubeDockerWaitSeconds)
	}
	if c.KubeDockerGroupGID < 1 || c.KubeDockerGroupGID > 65535 {
		return fmt.Errorf("RUNS_FLEET_KUBE_DOCKER_GROUP_GID must be between 1 and 65535, got %d", c.KubeDockerGroupGID)
	}
	return nil
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getEnvInt(key string, defaultValue int) (int, error) {
	if value := os.Getenv(key); value != "" {
		var result int
		if _, err := fmt.Sscanf(value, "%d", &result); err != nil {
			return 0, fmt.Errorf("invalid integer for %s=%q: %w", key, value, err)
		}
		return result, nil
	}
	return defaultValue, nil
}

func getEnvBool(key string, defaultValue bool) bool {
	value := os.Getenv(key)
	if value == "" {
		return defaultValue
	}
	if parsed, err := strconv.ParseBool(value); err == nil {
		return parsed
	}
	return defaultValue
}

func splitAndFilter(s string) []string {
	if s == "" {
		return []string{}
	}
	parts := strings.Split(s, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

// parseNodeSelector parses a comma-separated key=value string into a map.
// Example: "kubernetes.io/arch=arm64,node.kubernetes.io/instance-type=c7g.xlarge"
// Returns an error if any label key or value is invalid per Kubernetes constraints.
func parseNodeSelector(s string) (map[string]string, error) {
	result := make(map[string]string)
	if s == "" {
		return result, nil
	}
	for _, pair := range strings.Split(s, ",") {
		trimmed := strings.TrimSpace(pair)
		if trimmed == "" {
			continue
		}
		kv := strings.SplitN(trimmed, "=", 2)
		if len(kv) != 2 {
			return nil, fmt.Errorf("invalid node selector pair %q: missing '='", trimmed)
		}
		key, value := kv[0], kv[1]
		if key == "" {
			return nil, fmt.Errorf("invalid node selector: empty key in pair %q", trimmed)
		}
		if err := validateK8sLabelKey(key); err != nil {
			return nil, fmt.Errorf("invalid node selector key %q: %w", key, err)
		}
		if !isValidK8sLabelValue(value) {
			return nil, fmt.Errorf("invalid node selector value %q for key %q", value, key)
		}
		result[key] = value
	}
	return result, nil
}

// validateK8sLabelKey validates Kubernetes label key constraints.
// Keys have format: [prefix/]name
// - Prefix (optional): <= 253 chars, DNS subdomain (lowercase alphanumeric, -, .)
// - Name: <= 63 chars, alphanumeric + -_, must start/end with alphanumeric
func validateK8sLabelKey(key string) error {
	if key == "" {
		return fmt.Errorf("key cannot be empty")
	}

	var prefix, name string
	if idx := strings.LastIndex(key, "/"); idx != -1 {
		prefix = key[:idx]
		name = key[idx+1:]
	} else {
		name = key
	}

	if prefix != "" {
		if err := validateK8sLabelKeyPrefix(prefix); err != nil {
			return err
		}
	} else if strings.HasPrefix(key, "/") {
		return fmt.Errorf("key cannot start with '/'")
	}

	return validateK8sLabelKeyName(name)
}

// validateK8sLabelKeyPrefix validates the prefix portion of a K8s label key.
func validateK8sLabelKeyPrefix(prefix string) error {
	if len(prefix) > 253 {
		return fmt.Errorf("prefix exceeds 253 characters")
	}
	for i, r := range prefix {
		isLowerAlphaNum := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		isSpecial := r == '-' || r == '.'
		if i == 0 || i == len(prefix)-1 {
			if !isLowerAlphaNum {
				return fmt.Errorf("prefix must start and end with lowercase alphanumeric")
			}
		} else if !isLowerAlphaNum && !isSpecial {
			return fmt.Errorf("prefix contains invalid character %q", r)
		}
	}
	return nil
}

// validateK8sLabelKeyName validates the name portion of a K8s label key.
func validateK8sLabelKeyName(name string) error {
	if name == "" {
		return fmt.Errorf("name cannot be empty")
	}
	if len(name) > 63 {
		return fmt.Errorf("name exceeds 63 characters")
	}
	for i, r := range name {
		isAlphaNum := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
		isSpecial := r == '-' || r == '_' || r == '.'
		if i == 0 || i == len(name)-1 {
			if !isAlphaNum {
				return fmt.Errorf("name must start and end with alphanumeric")
			}
		} else if !isAlphaNum && !isSpecial {
			return fmt.Errorf("name contains invalid character %q", r)
		}
	}
	return nil
}

// isValidK8sLabelValue validates Kubernetes label value constraints:
// - Max 63 characters
// - Empty string is valid
// - Must start and end with alphanumeric (ASCII only)
// - Can contain alphanumeric, -, _, and .
func isValidK8sLabelValue(s string) bool {
	if s == "" {
		return true
	}
	if len(s) > 63 {
		return false
	}
	for i, r := range s {
		// Reject non-ASCII characters
		if r > 127 {
			return false
		}
		isAlphaNum := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
		isSpecial := r == '-' || r == '_' || r == '.'
		if i == 0 || i == len(s)-1 {
			if !isAlphaNum {
				return false
			}
		} else if !isAlphaNum && !isSpecial {
			return false
		}
	}
	return true
}

// parseTolerations parses a JSON array of tolerations.
// Example: [{"key":"dedicated","operator":"Equal","value":"github-actions","effect":"NoSchedule"}]
func parseTolerations(s string) ([]Toleration, error) {
	if s == "" {
		return nil, nil
	}

	var tolerations []Toleration
	if err := json.Unmarshal([]byte(s), &tolerations); err != nil {
		return nil, fmt.Errorf("invalid tolerations JSON: %w", err)
	}

	return tolerations, nil
}
