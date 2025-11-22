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

func ValidateSignature(payload []byte, signatureHeader string, secret string) error {
	if secret == "" {
		return errors.New("webhook secret not configured")
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

func ParseWebhook(r *http.Request, secret string) (interface{}, error) {
	event := r.Header.Get("X-GitHub-Event")
	if event == "" {
		return nil, errors.New("missing X-GitHub-Event header")
	}

	signatureHeader := r.Header.Get("X-Hub-Signature-256")
	if !strings.HasPrefix(signatureHeader, "sha256=") {
		return nil, errors.New("invalid or missing signature header")
	}

	defer func() { _ = r.Body.Close() }()

	limitedReader := io.LimitReader(r.Body, 1<<20)
	payload, err := io.ReadAll(limitedReader)
	if err != nil {
		return nil, fmt.Errorf("failed to read request body: %w", err)
	}

	if int64(len(payload)) == 1<<20 {
		if _, err := r.Body.Read(make([]byte, 1)); err != io.EOF {
			return nil, errors.New("request body exceeds 1MB limit")
		}
	}

	if err := ValidateSignature(payload, signatureHeader, secret); err != nil {
		return nil, fmt.Errorf("validation failed: %w", err)
	}

	return github.ParseWebHook(event, payload)
}

type JobConfig struct {
	RunID        string
	InstanceType string
	Pool         string
	Private      bool
	Spot         bool
	RunnerSpec   string
}

func ParseLabels(labels []string) (*JobConfig, error) {
	config := &JobConfig{
		Spot: true,
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

	parts := strings.Split(strings.TrimPrefix(runsFleetLabel, "runs-fleet="), "/")
	if len(parts) == 0 {
		return nil, errors.New("invalid runs-fleet label format")
	}

	config.RunID = parts[0]
	if config.RunID == "" {
		return nil, errors.New("empty run-id in label")
	}

	for _, part := range parts[1:] {
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			continue
		}
		key, value := kv[0], kv[1]

		switch key {
		case "runner":
			config.RunnerSpec = value
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
				config.InstanceType = "t4g.medium"
			}
		case "pool":
			config.Pool = value
		case "private":
			config.Private = value == "true"
		case "spot":
			config.Spot = value != "false"
		}
	}

	return config, nil
}
