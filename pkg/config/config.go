// Package config manages application configuration from environment variables.
package config

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Shavakan/runs-fleet/internal/validation"
	"github.com/Shavakan/runs-fleet/pkg/tracing"
)

// Config holds all application configuration loaded from environment variables.
type Config struct {
	AWSRegion string

	GitHubWebhookSecret string
	GitHubAppID         string
	GitHubAppPrivateKey string

	// LabelAliasesJSON is a JSON array of label alias rules mapping custom
	// runs-on labels to runs-fleet specs. Parsed and validated at startup.
	LabelAliasesJSON string

	QueueURL             string
	QueueDLQURL          string
	PoolQueueURL         string
	EventsQueueURL       string
	TerminationQueueURL  string
	HousekeepingQueueURL string
	JobsTableName        string
	JobsPoolStatusGSI    string
	JobsInstanceIDGSI    string
	PoolsTableName       string
	AuditTableName       string
	CircuitBreakerTable  string
	CacheBucketName      string
	CostReportSNSTopic   string
	CostReportBucket     string

	// EC2 fleet configuration
	VPCID              string
	SubnetIDs          []string
	SecurityGroupID    string
	InstanceProfileARN string
	KeyName            string

	SpotEnabled        bool
	MaxRuntimeMinutes  int
	LaunchTemplateName string
	RunnerImage        string            // Container image for EC2 runners (ECR URL)
	Tags               map[string]string // Custom tags for EC2 resources

	// Cost-attribution tag keys and values. Defaults are "Application"="runs-fleet"
	// and "Service"="runner"; both the key names and the values are independently
	// configurable so a fork with a different tag policy can remap either
	// (e.g. key "cost:application", value "my-org-infra").
	TagKeyApplication   string // Tag key for the application value (default: "Application")
	TagValueApplication string // Tag value for the application key (default: "runs-fleet")
	TagKeyService       string // Tag key for the service value (default: "Service")
	TagValueService     string // Tag value for the service key (default: "runner")

	CacheSecret    string
	BaseURL        string
	AdminRateLimit int
	TraceUIURL     string

	// ShutdownDrainDelay is how long the server keeps serving HTTP after
	// SIGTERM (readiness already flipped to 503) before closing the listener,
	// so the load balancer deregisters this task before it stops accepting
	// webhooks — otherwise requests routed during the deregistration window hit
	// a closed listener and 502/500, stranding jobs. Bounded by
	// MaxShutdownDrainDelay so it fits inside the deploy's shutdown budget:
	// ShutdownDrainDelay + worker drain + telemetry flush must stay below
	// terminationGracePeriodSeconds/stopTimeout (see cmd/server, timeouts.go).
	ShutdownDrainDelay time.Duration

	// Admin OIDC authentication. Auth is required when OIDCIssuerURL is set;
	// left entirely empty, the admin API runs unauthenticated (local dev).
	OIDCIssuerURL          string
	OIDCClientID           string
	OIDCClientSecret       string
	OIDCRedirectURL        string // defaults to {BaseURL}/api/auth/callback if unset
	OIDCScopes             []string
	OIDCGroupsClaim        string
	AdminSessionSecret     string
	AdminSessionTTLMinutes int

	// Metrics configuration
	MetricsCloudWatchEnabled        bool     // Enable CloudWatch metrics (default: true)
	MetricsPrometheusEnabled        bool     // Enable Prometheus /metrics endpoint
	MetricsPrometheusPath           string   // HTTP path for Prometheus /metrics endpoint (default: "/metrics")
	MetricsDatadogEnabled           bool     // Enable Datadog DogStatsD metrics
	MetricsDatadogAddr              string   // DogStatsD address (default: "127.0.0.1:8125")
	MetricsDatadogTags              []string // Global Datadog tags
	MetricsDatadogSampleRate        float64  // Sample rate for high-frequency metrics (default: 1.0)
	MetricsDatadogBufferPoolSize    int      // Buffer pool size (default: 0 = use library default)
	MetricsDatadogWorkersCount      int      // Workers count for parallel processing (default: 0 = use library default)
	MetricsDatadogMaxMsgsPerPayload int      // Max messages per UDP payload (default: 0 = unlimited)

	// Tracing configuration
	Tracing tracing.Config

	// Secrets backend configuration
	SecretsBackend    string // "ssm" or "vault" (default: "ssm")
	SecretsPathPrefix string // Path prefix for secrets (default: "/runs-fleet/runners")
	VaultAddr         string // Vault server address (required if vault backend)
	VaultKVMount      string // Vault KV mount path (default: "secret")
	VaultKVVersion    int    // Vault KV version: 0=auto-detect, 1, 2 (default: 0)
	VaultBasePath     string // Vault KV path prefix (default: "runs-fleet/runners")
	VaultAuthMethod   string // Vault auth method: "aws", "kubernetes", "approle", "token" (default: "aws")
	VaultAWSRole      string // Vault AWS auth role (default: "runs-fleet")
	VaultK8sAuthMount string // Vault Kubernetes auth mount path (default: "kubernetes")
	VaultK8sRole      string // Vault Kubernetes auth role
	VaultK8sJWTPath   string // Path to Kubernetes service account token (default: "/var/run/secrets/kubernetes.io/serviceaccount/token")
}

// Load reads configuration from environment variables and validates required fields.
func Load() (*Config, error) {
	if v := os.Getenv("RUNS_FLEET_MODE"); v != "" {
		slog.Warn("RUNS_FLEET_MODE is deprecated and ignored; the K8s runner backend was removed",
			slog.String("value", v))
	}

	maxRuntimeMinutes, err := getEnvInt("RUNS_FLEET_MAX_RUNTIME_MINUTES", 360)
	if err != nil {
		return nil, fmt.Errorf("config error: %w", err)
	}

	vaultKVVersion, err := getEnvInt("VAULT_KV_VERSION", 0)
	if err != nil {
		return nil, fmt.Errorf("config error: %w", err)
	}
	if vaultKVVersion < 0 || vaultKVVersion > 2 {
		return nil, fmt.Errorf("config error: VAULT_KV_VERSION must be 0 (auto), 1, or 2, got %d", vaultKVVersion)
	}

	cfg := &Config{
		AWSRegion: getEnv("AWS_REGION", "ap-northeast-1"),

		GitHubWebhookSecret:  getEnv("RUNS_FLEET_GITHUB_WEBHOOK_SECRET", ""),
		GitHubAppID:          getEnv("RUNS_FLEET_GITHUB_APP_ID", ""),
		GitHubAppPrivateKey:  getEnv("RUNS_FLEET_GITHUB_APP_PRIVATE_KEY", ""),
		LabelAliasesJSON:     getEnv("RUNS_FLEET_LABEL_ALIASES", ""),
		QueueURL:             getEnv("RUNS_FLEET_QUEUE_URL", ""),
		QueueDLQURL:          getEnv("RUNS_FLEET_QUEUE_DLQ_URL", ""),
		PoolQueueURL:         getEnv("RUNS_FLEET_POOL_QUEUE_URL", ""),
		EventsQueueURL:       getEnv("RUNS_FLEET_EVENTS_QUEUE_URL", ""),
		TerminationQueueURL:  getEnv("RUNS_FLEET_TERMINATION_QUEUE_URL", ""),
		HousekeepingQueueURL: getEnv("RUNS_FLEET_HOUSEKEEPING_QUEUE_URL", ""),
		JobsTableName:        getEnv("RUNS_FLEET_JOBS_TABLE", ""),
		JobsPoolStatusGSI:    getEnv("RUNS_FLEET_JOBS_POOL_STATUS_GSI", ""),
		JobsInstanceIDGSI:    getEnv("RUNS_FLEET_JOBS_INSTANCE_ID_GSI", ""),
		PoolsTableName:       getEnv("RUNS_FLEET_POOLS_TABLE", ""),
		AuditTableName:       getEnv("RUNS_FLEET_AUDIT_TABLE", ""),
		CircuitBreakerTable:  getEnv("RUNS_FLEET_CIRCUIT_BREAKER_TABLE", "runs-fleet-circuit-state"),
		CacheBucketName:      getEnv("RUNS_FLEET_CACHE_BUCKET", ""),
		CostReportSNSTopic:   getEnv("RUNS_FLEET_COST_REPORT_SNS_TOPIC", ""),
		CostReportBucket:     getEnv("RUNS_FLEET_COST_REPORT_BUCKET", ""),

		// EC2-specific
		VPCID:               getEnv("RUNS_FLEET_VPC_ID", ""),
		SubnetIDs:           splitAndFilter(getEnv("RUNS_FLEET_SUBNET_IDS", "")),
		SecurityGroupID:     getEnv("RUNS_FLEET_SECURITY_GROUP_ID", ""),
		InstanceProfileARN:  getEnv("RUNS_FLEET_INSTANCE_PROFILE_ARN", ""),
		KeyName:             getEnv("RUNS_FLEET_KEY_NAME", ""),
		SpotEnabled:         getEnvBool("RUNS_FLEET_SPOT_ENABLED", true),
		MaxRuntimeMinutes:   maxRuntimeMinutes,
		LaunchTemplateName:  getEnv("RUNS_FLEET_LAUNCH_TEMPLATE_NAME", "runs-fleet-runner"),
		RunnerImage:         getEnv("RUNS_FLEET_RUNNER_IMAGE", ""),
		Tags:                make(map[string]string),
		TagKeyApplication:   getEnv("RUNS_FLEET_TAG_KEY_APPLICATION", "Application"),
		TagValueApplication: getEnv("RUNS_FLEET_TAG_VALUE_APPLICATION", "runs-fleet"),
		TagKeyService:       getEnv("RUNS_FLEET_TAG_KEY_SERVICE", "Service"),
		TagValueService:     getEnv("RUNS_FLEET_TAG_VALUE_SERVICE", "runner"),

		CacheSecret:    getEnv("RUNS_FLEET_CACHE_SECRET", ""),
		BaseURL:        getEnv("RUNS_FLEET_BASE_URL", ""),
		AdminRateLimit: getEnvIntDefault("RUNS_FLEET_ADMIN_RATE_LIMIT", 60),
		TraceUIURL:     getEnv("RUNS_FLEET_TRACE_UI_URL", ""),

		ShutdownDrainDelay: time.Duration(getEnvIntDefault("RUNS_FLEET_SHUTDOWN_DRAIN_DELAY_SECONDS", 5)) * time.Second,

		OIDCIssuerURL:          getEnv("RUNS_FLEET_ADMIN_OIDC_ISSUER_URL", ""),
		OIDCClientID:           getEnv("RUNS_FLEET_ADMIN_OIDC_CLIENT_ID", ""),
		OIDCClientSecret:       getEnv("RUNS_FLEET_ADMIN_OIDC_CLIENT_SECRET", ""),
		OIDCRedirectURL:        getEnv("RUNS_FLEET_ADMIN_OIDC_REDIRECT_URL", ""),
		OIDCScopes:             splitAndFilter(getEnv("RUNS_FLEET_ADMIN_OIDC_SCOPES", "openid,profile,email")),
		OIDCGroupsClaim:        getEnv("RUNS_FLEET_ADMIN_OIDC_GROUPS_CLAIM", "groups"),
		AdminSessionSecret:     getEnv("RUNS_FLEET_ADMIN_SESSION_SECRET", ""),
		AdminSessionTTLMinutes: getEnvIntDefault("RUNS_FLEET_ADMIN_SESSION_TTL_MINUTES", 480),

		// Metrics
		MetricsCloudWatchEnabled:        getEnvBool("RUNS_FLEET_METRICS_CLOUDWATCH_ENABLED", true),
		MetricsPrometheusEnabled:        getEnvBool("RUNS_FLEET_METRICS_PROMETHEUS_ENABLED", false),
		MetricsPrometheusPath:           getEnv("RUNS_FLEET_METRICS_PROMETHEUS_PATH", "/metrics"),
		MetricsDatadogEnabled:           getEnvBool("RUNS_FLEET_METRICS_DATADOG_ENABLED", false),
		MetricsDatadogAddr:              getEnv("RUNS_FLEET_METRICS_DATADOG_ADDR", "127.0.0.1:8125"),
		MetricsDatadogTags:              splitAndFilter(getEnv("RUNS_FLEET_METRICS_DATADOG_TAGS", "")),
		MetricsDatadogSampleRate:        getEnvFloat("RUNS_FLEET_METRICS_DATADOG_SAMPLE_RATE", 1.0),
		MetricsDatadogBufferPoolSize:    getEnvIntDefault("RUNS_FLEET_METRICS_DATADOG_BUFFER_POOL_SIZE", 0),
		MetricsDatadogWorkersCount:      getEnvIntDefault("RUNS_FLEET_METRICS_DATADOG_WORKERS_COUNT", 0),
		MetricsDatadogMaxMsgsPerPayload: getEnvIntDefault("RUNS_FLEET_METRICS_DATADOG_MAX_MSGS_PER_PAYLOAD", 0),

		// Tracing
		Tracing: tracing.ParseConfig(),

		// Secrets backend
		SecretsBackend:    getEnv("RUNS_FLEET_SECRETS_BACKEND", "ssm"),
		SecretsPathPrefix: getEnv("RUNS_FLEET_SECRETS_PATH_PREFIX", "/runs-fleet/runners"),
		VaultAddr:         getEnv("VAULT_ADDR", ""),
		VaultKVMount:      getEnv("VAULT_KV_MOUNT", "secret"),
		VaultKVVersion:    vaultKVVersion,
		VaultBasePath:     getEnv("VAULT_BASE_PATH", "runs-fleet/runners"),
		VaultAuthMethod:   getEnv("VAULT_AUTH_METHOD", "aws"),
		VaultAWSRole:      getEnv("VAULT_AWS_ROLE", "runs-fleet"),
		VaultK8sAuthMount: getEnv("VAULT_K8S_AUTH_MOUNT", "kubernetes"),
		VaultK8sRole:      getEnv("VAULT_K8S_ROLE", ""),
		VaultK8sJWTPath:   getEnv("VAULT_K8S_JWT_PATH", "/var/run/secrets/kubernetes.io/serviceaccount/token"),
	}

	tagsStr := getEnv("RUNS_FLEET_TAGS", "")
	if tagsStr != "" {
		tags, err := parseTags(tagsStr)
		if err != nil {
			return nil, fmt.Errorf("config error: %w", err)
		}
		cfg.Tags = tags
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("config validation failed: %w", err)
	}

	return cfg, nil
}

// Validate checks that all required configuration fields are present and valid.
func (c *Config) Validate() error {
	if c.GitHubWebhookSecret == "" {
		return fmt.Errorf("RUNS_FLEET_GITHUB_WEBHOOK_SECRET is required")
	}
	if c.GitHubAppID == "" {
		return fmt.Errorf("RUNS_FLEET_GITHUB_APP_ID is required")
	}
	if c.GitHubAppPrivateKey == "" {
		return fmt.Errorf("RUNS_FLEET_GITHUB_APP_PRIVATE_KEY is required")
	}
	if c.QueueURL == "" {
		return fmt.Errorf("RUNS_FLEET_QUEUE_URL is required")
	}
	if c.BaseURL == "" {
		return fmt.Errorf("RUNS_FLEET_BASE_URL is required")
	}
	if err := validateBaseURL(c.BaseURL); err != nil {
		return fmt.Errorf("RUNS_FLEET_BASE_URL: %w", err)
	}
	if c.MaxRuntimeMinutes <= 0 {
		return fmt.Errorf("RUNS_FLEET_MAX_RUNTIME_MINUTES must be greater than 0, got %d", c.MaxRuntimeMinutes)
	}
	if c.MaxRuntimeMinutes > 1440 {
		return fmt.Errorf("RUNS_FLEET_MAX_RUNTIME_MINUTES must not exceed 1440 (24 hours), got %d", c.MaxRuntimeMinutes)
	}
	if c.ShutdownDrainDelay < 0 {
		return fmt.Errorf("RUNS_FLEET_SHUTDOWN_DRAIN_DELAY_SECONDS must not be negative, got %s", c.ShutdownDrainDelay)
	}
	if c.ShutdownDrainDelay > MaxShutdownDrainDelay {
		return fmt.Errorf("RUNS_FLEET_SHUTDOWN_DRAIN_DELAY_SECONDS must not exceed %s, got %s", MaxShutdownDrainDelay, c.ShutdownDrainDelay)
	}

	if err := c.validateEC2Config(); err != nil {
		return err
	}
	if err := c.validateSecretsConfig(); err != nil {
		return err
	}

	if err := c.validateMetricsConfig(); err != nil {
		return err
	}

	if err := c.validateOIDCConfig(); err != nil {
		return err
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
	if len(c.SubnetIDs) == 0 {
		return fmt.Errorf("RUNS_FLEET_SUBNET_IDS is required for EC2 backend")
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

// validateSecretsConfig validates secrets backend configuration.
func (c *Config) validateSecretsConfig() error {
	switch c.SecretsBackend {
	case "ssm", "":
		// SSM is the default, no additional validation needed
	case "vault":
		if c.VaultAddr == "" {
			return fmt.Errorf("VAULT_ADDR is required when RUNS_FLEET_SECRETS_BACKEND=vault")
		}
		if err := c.validateVaultAuthConfig(); err != nil {
			return err
		}
	default:
		return fmt.Errorf("RUNS_FLEET_SECRETS_BACKEND must be 'ssm' or 'vault', got %q", c.SecretsBackend)
	}
	return nil
}

// validateVaultAuthConfig validates Vault authentication configuration.
func (c *Config) validateVaultAuthConfig() error {
	switch c.VaultAuthMethod {
	case "aws", "":
		// AWS auth uses IAM credentials automatically
	case "kubernetes", "k8s", "jwt":
		if c.VaultK8sRole == "" {
			return fmt.Errorf("VAULT_K8S_ROLE is required when VAULT_AUTH_METHOD is 'kubernetes', 'k8s', or 'jwt'")
		}
		if c.VaultK8sJWTPath == "" {
			c.VaultK8sJWTPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"
		}
		if err := validation.ValidateK8sJWTPath(c.VaultK8sJWTPath); err != nil {
			return err
		}
	case "approle":
		// AppRole validation would require additional fields not yet implemented
	case "token":
		// Token can come from VAULT_TOKEN env var at runtime
	default:
		return fmt.Errorf("VAULT_AUTH_METHOD must be 'aws', 'kubernetes', 'k8s', 'jwt', 'approle', or 'token', got %q", c.VaultAuthMethod)
	}
	return nil
}

// validateOIDCConfig validates admin OIDC configuration. Auth is off (all
// fields empty) or fully configured (all required fields set); a partial
// configuration is a startup error so a typo doesn't silently disable auth.
// Checks are explicit (not a map + loop) so the reported missing field is
// deterministic when more than one is empty.
func (c *Config) validateOIDCConfig() error {
	if c.OIDCIssuerURL == "" && c.OIDCClientID == "" && c.OIDCClientSecret == "" && c.AdminSessionSecret == "" {
		return nil
	}

	if c.OIDCIssuerURL == "" {
		return fmt.Errorf("RUNS_FLEET_ADMIN_OIDC_ISSUER_URL is required once admin OIDC auth is configured (another OIDC env var is set)")
	}
	if c.OIDCClientID == "" {
		return fmt.Errorf("RUNS_FLEET_ADMIN_OIDC_CLIENT_ID is required once admin OIDC auth is configured (another OIDC env var is set)")
	}
	if c.OIDCClientSecret == "" {
		return fmt.Errorf("RUNS_FLEET_ADMIN_OIDC_CLIENT_SECRET is required once admin OIDC auth is configured (another OIDC env var is set)")
	}
	if c.AdminSessionSecret == "" {
		return fmt.Errorf("RUNS_FLEET_ADMIN_SESSION_SECRET is required once admin OIDC auth is configured (another OIDC env var is set)")
	}

	if err := validateBaseURL(c.OIDCIssuerURL); err != nil {
		return fmt.Errorf("RUNS_FLEET_ADMIN_OIDC_ISSUER_URL: %w", err)
	}
	if c.AdminSessionTTLMinutes <= 0 {
		return fmt.Errorf("RUNS_FLEET_ADMIN_SESSION_TTL_MINUTES must be greater than 0, got %d", c.AdminSessionTTLMinutes)
	}

	return nil
}

// validateMetricsConfig validates metrics-specific configuration.
func (c *Config) validateMetricsConfig() error {
	if c.MetricsDatadogEnabled {
		if c.MetricsDatadogAddr == "" {
			return fmt.Errorf("RUNS_FLEET_METRICS_DATADOG_ADDR is required when Datadog metrics are enabled")
		}
		if err := validateHostPort(c.MetricsDatadogAddr); err != nil {
			return fmt.Errorf("RUNS_FLEET_METRICS_DATADOG_ADDR: %w", err)
		}
	}

	if c.MetricsDatadogSampleRate < 0 {
		slog.Warn("RUNS_FLEET_METRICS_DATADOG_SAMPLE_RATE below 0, clamping to 0.0",
			slog.Float64("value", c.MetricsDatadogSampleRate))
		c.MetricsDatadogSampleRate = 0
	} else if c.MetricsDatadogSampleRate > 1 {
		slog.Warn("RUNS_FLEET_METRICS_DATADOG_SAMPLE_RATE above 1, clamping to 1.0",
			slog.Float64("value", c.MetricsDatadogSampleRate))
		c.MetricsDatadogSampleRate = 1
	}

	if c.MetricsDatadogBufferPoolSize < 0 {
		slog.Warn("RUNS_FLEET_METRICS_DATADOG_BUFFER_POOL_SIZE negative, clamping to 0",
			slog.Int("value", c.MetricsDatadogBufferPoolSize))
		c.MetricsDatadogBufferPoolSize = 0
	}

	return nil
}

// validateBaseURL validates that the orchestrator base URL is an absolute https URL.
func validateBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid URL %q: %w", raw, err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("must be an absolute https:// URL, got scheme %q", u.Scheme)
	}
	if u.Hostname() == "" {
		return fmt.Errorf("must include a host")
	}
	return nil
}

// validateHostPort validates that addr is a valid host:port format (IPv4 or IPv6).
func validateHostPort(addr string) error {
	if addr == "" {
		return fmt.Errorf("address cannot be empty")
	}
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid format %q: %w", addr, err)
	}
	if host == "" {
		return fmt.Errorf("invalid format %q: host cannot be empty", addr)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return fmt.Errorf("invalid port %q: %w", portStr, err)
	}
	if port < 1 || port > 65535 {
		return fmt.Errorf("port %d out of range (1-65535)", port)
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
	slog.Warn("invalid boolean env var, using default",
		slog.String("key", key),
		slog.String("value", value),
		slog.Bool("default", defaultValue))
	return defaultValue
}

func getEnvFloat(key string, defaultValue float64) float64 {
	value := os.Getenv(key)
	if value == "" {
		return defaultValue
	}
	if parsed, err := strconv.ParseFloat(value, 64); err == nil {
		return parsed
	}
	slog.Warn("invalid float env var, using default",
		slog.String("key", key),
		slog.String("value", value),
		slog.Float64("default", defaultValue))
	return defaultValue
}

func getEnvIntDefault(key string, defaultValue int) int {
	result, err := getEnvInt(key, defaultValue)
	if err != nil {
		return defaultValue
	}
	return result
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

// parseTags parses a JSON object of key-value tags.
// Example: {"Environment":"production","Team":"platform","CostCenter":"12345"}
// Validates AWS EC2 tag constraints: max 50 tags, key max 128 chars, value max 256 chars.
// Rejects reserved prefixes: "aws:" and "runs-fleet:".
func parseTags(s string) (map[string]string, error) {
	if s == "" {
		return nil, nil
	}

	var tags map[string]string
	if err := json.Unmarshal([]byte(s), &tags); err != nil {
		return nil, fmt.Errorf("invalid tags JSON: %w", err)
	}

	if err := validateTags(tags); err != nil {
		return nil, err
	}

	return tags, nil
}

func validateTags(tags map[string]string) error {
	const (
		// AWS EC2 allows 50 tags max. System adds these tags:
		// - Name (always)
		// - runs-fleet:run-id (always)
		// - runs-fleet:managed (always)
		// - Application (always; key configurable)
		// - Service (always; key configurable)
		// - runs-fleet:pool (conditional)
		// - runs-fleet:arch (conditional)
		// - Role (conditional)
		// - runs-fleet:runner-image (conditional)
		// - runs-fleet:termination-queue-url (conditional)
		// Reserve 15 for system tags to be safe, allowing 35 custom tags
		systemTagReserve = 15
		maxTags          = 50 - systemTagReserve
		maxKeyLen        = 128
		maxValueLen      = 256
	)

	if len(tags) > maxTags {
		return fmt.Errorf("maximum %d custom tags allowed (AWS limit minus system tags), got %d", maxTags, len(tags))
	}

	for key, value := range tags {
		if key == "" {
			return fmt.Errorf("tag key cannot be empty")
		}
		if len(key) > maxKeyLen {
			return fmt.Errorf("tag key %q exceeds %d characters", key, maxKeyLen)
		}
		if len(value) > maxValueLen {
			return fmt.Errorf("tag value for key %q exceeds %d characters", key, maxValueLen)
		}
		if strings.HasPrefix(strings.ToLower(key), "aws:") {
			return fmt.Errorf("tag key %q uses reserved 'aws:' prefix", key)
		}
		if strings.HasPrefix(strings.ToLower(key), "runs-fleet:") {
			return fmt.Errorf("tag key %q uses reserved 'runs-fleet:' prefix", key)
		}
	}

	return nil
}
