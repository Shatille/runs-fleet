package github

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/google/go-github/v57/github"
)

// ValidateSignature validates the GitHub webhook signature
func ValidateSignature(payload []byte, signatureHeader string, secret string) error {
	if secret == "" {
		return nil // Dev mode or no secret configured
	}

	if !strings.HasPrefix(signatureHeader, "sha256=") {
		return errors.New("invalid signature format")
	}

	signature := strings.TrimPrefix(signatureHeader, "sha256=")
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	expectedMAC := mac.Sum(nil)
	expectedSignature := hex.EncodeToString(expectedMAC)

	if !hmac.Equal([]byte(signature), []byte(expectedSignature)) {
		return errors.New("signature mismatch")
	}

	return nil
}

// ParseWebhook parses the incoming webhook request
func ParseWebhook(r *http.Request, secret string) (interface{}, error) {
	payload, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read request body: %w", err)
	}
	defer r.Body.Close()

	if err := ValidateSignature(payload, r.Header.Get("X-Hub-Signature-256"), secret); err != nil {
		return nil, fmt.Errorf("validation failed: %w", err)
	}

	event := github.WebHookType(r)
	return github.ParseWebHook(event, payload)
}

// JobConfig holds the configuration extracted from labels
type JobConfig struct {
	RunID        string
	InstanceType string
	Pool         string
	Private      bool
	Spot         bool
	RunnerSpec   string
}

// ParseLabels extracts runs-fleet configuration from workflow job labels
func ParseLabels(labels []string) (*JobConfig, error) {
	config := &JobConfig{
		Spot: true, // Default to spot
	}

	var runsFleetLabel string
	for _, label := range labels {
		if strings.HasPrefix(label, "runs-fleet=") {
			runsFleetLabel = label
			break
		}
	}

	if runsFleetLabel == "" {
		return nil, errors.New("no runs-fleet label found")
	}

	// Format: runs-fleet=<run-id>/runner=<spec>/pool=<name>/private=true/spot=false
	parts := strings.Split(strings.TrimPrefix(runsFleetLabel, "runs-fleet="), "/")
	if len(parts) == 0 {
		return nil, errors.New("invalid runs-fleet label format")
	}

	config.RunID = parts[0]

	for _, part := range parts[1:] {
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			continue
		}
		key, value := kv[0], kv[1]

		switch key {
		case "runner":
			config.RunnerSpec = value
			// Map spec to instance type (simple mapping for now)
			switch {
			case strings.HasPrefix(value, "2cpu-linux-arm64"):
				config.InstanceType = "t4g.medium"
			case strings.HasPrefix(value, "4cpu-linux-arm64"):
				config.InstanceType = "c7g.xlarge"
			case strings.HasPrefix(value, "4cpu-linux-x64"):
				config.InstanceType = "c6i.xlarge"
			case strings.HasPrefix(value, "8cpu-linux-arm64"):
				config.InstanceType = "c7g.2xlarge"
			default:
				config.InstanceType = "t4g.medium" // Fallback
			}
		case "pool":
			config.Pool = value
		case "private":
			config.Private = value == "true"
		case "spot":
			config.Spot = value != "false"
		}
	}

	if config.RunID == "" {
		return nil, errors.New("missing run-id in label")
	}

	return config, nil
}
