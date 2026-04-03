// Package tracing provides OpenTelemetry SDK initialization and configuration.
package tracing

import (
	"log/slog"
	"os"
	"strconv"
)

// Config holds OpenTelemetry tracing configuration.
type Config struct {
	Enabled     bool
	Endpoint    string
	Insecure    bool
	ServiceName string
	Environment string
}

// ParseConfig reads tracing configuration from environment variables.
func ParseConfig() Config {
	return Config{
		Enabled:     envBool("RUNS_FLEET_TRACING_ENABLED", false),
		Endpoint:    envString("RUNS_FLEET_OTEL_ENDPOINT", ""),
		Insecure:    envBool("RUNS_FLEET_OTEL_INSECURE", true),
		ServiceName: envString("RUNS_FLEET_OTEL_SERVICE_NAME", "runs-fleet"),
		Environment: envString("RUNS_FLEET_ENV", ""),
	}
}

func envString(key, defaultValue string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultValue
}

func envBool(key string, defaultValue bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return defaultValue
	}
	if parsed, err := strconv.ParseBool(v); err == nil {
		return parsed
	}
	slog.Warn("invalid boolean env var, using default",
		slog.String("key", key),
		slog.String("value", v),
		slog.Bool("default", defaultValue))
	return defaultValue
}
