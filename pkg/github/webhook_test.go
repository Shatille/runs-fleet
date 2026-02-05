package github

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testWebhookSecret = "test-secret"

func TestValidateSignature(t *testing.T) {
	secret := testWebhookSecret
	payload := []byte("test-payload")

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	validSignature := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	tests := []struct {
		name      string
		payload   []byte
		signature string
		secret    string
		wantErr   bool
	}{
		{
			name:      "Valid signature",
			payload:   payload,
			signature: validSignature,
			secret:    secret,
			wantErr:   false,
		},
		{
			name:      "Invalid signature",
			payload:   payload,
			signature: "sha256=invalid",
			secret:    secret,
			wantErr:   true,
		},
		{
			name:      "Wrong secret",
			payload:   payload,
			signature: validSignature,
			secret:    "wrong-secret",
			wantErr:   true,
		},
		{
			name:      "No secret configured",
			payload:   payload,
			signature: "sha256=anything",
			secret:    "",
			wantErr:   true,
		},
		{
			name:      "Malformed signature header",
			payload:   payload,
			signature: "invalid-format",
			secret:    secret,
			wantErr:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := ValidateSignature(tt.payload, tt.signature, tt.secret); (err != nil) != tt.wantErr {
				t.Errorf("ValidateSignature() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestParseWebhook(t *testing.T) {
	secret := testWebhookSecret

	// Create a valid signature for a payload
	signPayload := func(payload []byte) string {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(payload)
		return "sha256=" + hex.EncodeToString(mac.Sum(nil))
	}

	// Valid workflow_job event payload (minimal)
	validPayload := []byte(`{"action":"queued","workflow_job":{"id":123,"run_id":456,"labels":["self-hosted"]}}`)
	validSignature := signPayload(validPayload)

	tests := []struct {
		name        string
		event       string
		signature   string
		body        []byte
		wantErr     bool
		errContains string
	}{
		{
			name:      "valid workflow_job event",
			event:     "workflow_job",
			signature: validSignature,
			body:      validPayload,
			wantErr:   false,
		},
		{
			name:        "missing event header",
			event:       "",
			signature:   validSignature,
			body:        validPayload,
			wantErr:     true,
			errContains: "missing X-GitHub-Event",
		},
		{
			name:        "missing signature header",
			event:       "workflow_job",
			signature:   "",
			body:        validPayload,
			wantErr:     true,
			errContains: "invalid or missing signature",
		},
		{
			name:        "invalid signature prefix",
			event:       "workflow_job",
			signature:   "sha1=invalid",
			body:        validPayload,
			wantErr:     true,
			errContains: "invalid or missing signature",
		},
		{
			name:        "signature mismatch",
			event:       "workflow_job",
			signature:   "sha256=0000000000000000000000000000000000000000000000000000000000000000",
			body:        validPayload,
			wantErr:     true,
			errContains: "validation failed",
		},
		{
			name:        "invalid JSON payload",
			event:       "workflow_job",
			signature:   signPayload([]byte("not-json")),
			body:        []byte("not-json"),
			wantErr:     true,
			errContains: "invalid character",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(tt.body))
			if tt.event != "" {
				req.Header.Set("X-GitHub-Event", tt.event)
			}
			if tt.signature != "" {
				req.Header.Set("X-Hub-Signature-256", tt.signature)
			}

			result, err := ParseWebhook(req, secret)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseWebhook() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr && tt.errContains != "" && !strings.Contains(err.Error(), tt.errContains) {
				t.Errorf("ParseWebhook() error = %v, want error containing %q", err, tt.errContains)
			}
			if !tt.wantErr && result == nil {
				t.Error("ParseWebhook() returned nil for valid webhook")
			}
		})
	}
}

func TestParseWebhook_BodySizeLimit(t *testing.T) {
	secret := testWebhookSecret

	// Create a payload that exceeds the size limit (1MB + 1 byte)
	largePayload := bytes.Repeat([]byte("a"), 1024*1024+1)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(largePayload)
	signature := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(largePayload))
	req.Header.Set("X-GitHub-Event", "workflow_job")
	req.Header.Set("X-Hub-Signature-256", signature)

	_, err := ParseWebhook(req, secret)
	if err == nil {
		t.Error("ParseWebhook() should reject oversized body")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("ParseWebhook() error should mention size limit, got: %v", err)
	}
}

func TestParseWebhook_ReadError(t *testing.T) {
	secret := testWebhookSecret

	// Create a request with an erroring body
	req := httptest.NewRequest(http.MethodPost, "/webhook", &errorReader{})
	req.Header.Set("X-GitHub-Event", "workflow_job")
	req.Header.Set("X-Hub-Signature-256", "sha256=test")

	_, err := ParseWebhook(req, secret)
	if err == nil {
		t.Error("ParseWebhook() should return error on read failure")
	}
	if !strings.Contains(err.Error(), "failed to read") {
		t.Errorf("ParseWebhook() error should mention read failure, got: %v", err)
	}
}

// errorReader is an io.Reader that always returns an error.
type errorReader struct{}

func (e *errorReader) Read(_ []byte) (int, error) {
	return 0, io.ErrUnexpectedEOF
}

func TestParseLabels(t *testing.T) {
	tests := []struct {
		name    string
		labels  []string
		want    *JobConfig
		wantErr bool
	}{
		{
			name: "Standard label with CPU and arch",
			labels: []string{
				"self-hosted",
				"runs-fleet=12345/cpu=2/arch=arm64/pool=default",
			},
			want: &JobConfig{
				RunID: "12345",
				Pool:  "default",
				Spot:  true,
			},
			wantErr: false,
		},
		{
			name: "On-Demand instance",
			labels: []string{
				"runs-fleet=67890/cpu=4/arch=amd64/spot=false",
			},
			want: &JobConfig{
				RunID: "67890",
				Pool:  "",
				Spot:  false,
			},
			wantErr: false,
		},
		{
			name: "Large ARM instance",
			labels: []string{
				"runs-fleet=abc/cpu=8/arch=arm64",
			},
			want: &JobConfig{
				RunID: "abc",
				Spot:  true,
			},
			wantErr: false,
		},
		{
			name:    "Missing runs-fleet label",
			labels:  []string{"self-hosted", "linux"},
			want:    nil,
			wantErr: true,
		},
		{
			name:    "Invalid format",
			labels:  []string{"runs-fleet="},
			want:    nil,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseLabels(tt.labels)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseLabels() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr {
				if got.RunID != tt.want.RunID {
					t.Errorf("ParseLabels() RunID = %v, want %v", got.RunID, tt.want.RunID)
				}
				if got.Pool != tt.want.Pool {
					t.Errorf("ParseLabels() Pool = %v, want %v", got.Pool, tt.want.Pool)
				}
				if got.Spot != tt.want.Spot {
					t.Errorf("ParseLabels() Spot = %v, want %v", got.Spot, tt.want.Spot)
				}
			}
		})
	}
}

func TestParseLabels_FlexibleSpecs(t *testing.T) {
	tests := []struct {
		name              string
		labels            []string
		wantCPUMin        int
		wantCPUMax        int
		wantRAMMin        float64
		wantRAMMax        float64
		wantArch          string
		wantFamilies      []string
		wantInstanceTypes int // minimum number of instance types expected
		wantErr           bool
	}{
		{
			name: "Simple CPU spec (defaults to 2x range)",
			labels: []string{
				"runs-fleet=12345/cpu=4/arch=arm64",
			},
			wantCPUMin:        4,
			wantCPUMax:        8, // Defaults to 2x CPUMin for bounded diversification
			wantArch:          "arm64",
			wantInstanceTypes: 2, // Matches 4 and 8 vCPU instances
		},
		{
			name: "CPU range",
			labels: []string{
				"runs-fleet=12345/cpu=4+16/arch=arm64",
			},
			wantCPUMin:        4,
			wantCPUMax:        16,
			wantArch:          "arm64",
			wantInstanceTypes: 3, // Should match multiple instance sizes
		},
		{
			name: "CPU and RAM range",
			labels: []string{
				"runs-fleet=12345/cpu=4+8/ram=8+32/arch=arm64",
			},
			wantCPUMin:        4,
			wantCPUMax:        8,
			wantRAMMin:        8,
			wantRAMMax:        32,
			wantArch:          "arm64",
			wantInstanceTypes: 2,
		},
		{
			name: "With family filter",
			labels: []string{
				"runs-fleet=12345/cpu=4+8/family=c7g/arch=arm64",
			},
			wantCPUMin:        4,
			wantCPUMax:        8,
			wantArch:          "arm64",
			wantFamilies:      []string{"c7g"},
			wantInstanceTypes: 2,
		},
		{
			name: "Multiple families",
			labels: []string{
				"runs-fleet=12345/cpu=4+8/family=c7g+m7g/arch=arm64",
			},
			wantCPUMin:        4,
			wantCPUMax:        8,
			wantArch:          "arm64",
			wantFamilies:      []string{"c7g", "m7g"},
			wantInstanceTypes: 4, // 2 sizes x 2 families
		},
		{
			name: "amd64 architecture (defaults to 2x range)",
			labels: []string{
				"runs-fleet=12345/cpu=4/arch=amd64",
			},
			wantCPUMin:        4,
			wantCPUMax:        8, // Defaults to 2x CPUMin
			wantArch:          "amd64",
			wantInstanceTypes: 2, // Matches 4 and 8 vCPU instances
		},
		{
			name: "With other options (defaults to 2x range)",
			labels: []string{
				"runs-fleet=12345/cpu=4/arch=arm64/spot=false/private=true/pool=mypool",
			},
			wantCPUMin:        4,
			wantCPUMax:        8, // Defaults to 2x CPUMin
			wantArch:          "arm64",
			wantInstanceTypes: 2, // Matches 4 and 8 vCPU instances
		},
		{
			name: "Empty arch (defaults to 2x range)",
			labels: []string{
				"runs-fleet=12345/cpu=4",
			},
			wantCPUMin:        4,
			wantCPUMax:        8, // Defaults to 2x CPUMin
			wantArch:          "",
			wantInstanceTypes: 2, // Matches 4 and 8 vCPU instances across architectures
		},
		{
			name: "No matching instances",
			labels: []string{
				"runs-fleet=12345/cpu=1000/arch=arm64",
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseLabels(tt.labels)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseLabels() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr {
				return
			}

			if got.CPUMin != tt.wantCPUMin {
				t.Errorf("ParseLabels() CPUMin = %d, want %d", got.CPUMin, tt.wantCPUMin)
			}
			if got.CPUMax != tt.wantCPUMax {
				t.Errorf("ParseLabels() CPUMax = %d, want %d", got.CPUMax, tt.wantCPUMax)
			}
			if got.RAMMin != tt.wantRAMMin {
				t.Errorf("ParseLabels() RAMMin = %f, want %f", got.RAMMin, tt.wantRAMMin)
			}
			if got.RAMMax != tt.wantRAMMax {
				t.Errorf("ParseLabels() RAMMax = %f, want %f", got.RAMMax, tt.wantRAMMax)
			}
			if got.Arch != tt.wantArch {
				t.Errorf("ParseLabels() Arch = %s, want %s", got.Arch, tt.wantArch)
			}
			if tt.wantFamilies != nil {
				if len(got.Families) != len(tt.wantFamilies) {
					t.Errorf("ParseLabels() Families = %v, want %v", got.Families, tt.wantFamilies)
				}
			}
			if tt.wantInstanceTypes > 0 && len(got.InstanceTypes) < tt.wantInstanceTypes {
				t.Errorf("ParseLabels() InstanceTypes has %d types, want at least %d", len(got.InstanceTypes), tt.wantInstanceTypes)
			}
			// Verify primary instance type is set
			if got.InstanceType == "" {
				t.Error("ParseLabels() InstanceType is empty")
			}
		})
	}
}

//nolint:dupl // Similar structure to TestParseRangeFloat but tests different types
func TestParseRange(t *testing.T) {
	tests := []struct {
		input   string
		wantMin int
		wantMax int
		wantErr bool
	}{
		{"4", 4, 0, false},
		{"4+16", 4, 16, false},
		{"2+8", 2, 8, false},
		{"0", 0, 0, false},
		{"", 0, 0, false},
		{"abc", 0, 0, true},
		{"4+abc", 0, 0, true},
		{"abc+16", 0, 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			gotMin, gotMax, err := parseRange(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseRange(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
				return
			}
			if !tt.wantErr {
				if gotMin != tt.wantMin {
					t.Errorf("parseRange(%q) min = %d, want %d", tt.input, gotMin, tt.wantMin)
				}
				if gotMax != tt.wantMax {
					t.Errorf("parseRange(%q) max = %d, want %d", tt.input, gotMax, tt.wantMax)
				}
			}
		})
	}
}

//nolint:dupl // Similar structure to TestParseRange but tests different types
func TestParseRangeFloat(t *testing.T) {
	tests := []struct {
		input   string
		wantMin float64
		wantMax float64
		wantErr bool
	}{
		{"8", 8, 0, false},
		{"8+32", 8, 32, false},
		{"16.5+64", 16.5, 64, false},
		{"0", 0, 0, false},
		{"", 0, 0, false},
		{"abc", 0, 0, true},
		{"8+abc", 0, 0, true},
		{"abc+32", 0, 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			gotMin, gotMax, err := parseRangeFloat(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseRangeFloat(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
				return
			}
			if !tt.wantErr {
				if gotMin != tt.wantMin {
					t.Errorf("parseRangeFloat(%q) min = %f, want %f", tt.input, gotMin, tt.wantMin)
				}
				if gotMax != tt.wantMax {
					t.Errorf("parseRangeFloat(%q) max = %f, want %f", tt.input, gotMax, tt.wantMax)
				}
			}
		})
	}
}

func TestParseLabels_InvalidValues(t *testing.T) {
	tests := []struct {
		name   string
		labels []string
	}{
		{
			name:   "Invalid CPU value",
			labels: []string{"runs-fleet=12345/cpu=abc/arch=arm64"},
		},
		{
			name:   "Invalid CPU max value",
			labels: []string{"runs-fleet=12345/cpu=4+abc/arch=arm64"},
		},
		{
			name:   "Invalid RAM value",
			labels: []string{"runs-fleet=12345/ram=abc/arch=arm64"},
		},
		{
			name:   "Invalid RAM max value",
			labels: []string{"runs-fleet=12345/ram=8+abc/arch=arm64"},
		},
		{
			name:   "Invalid backend value",
			labels: []string{"runs-fleet=12345/cpu=2/arch=arm64/backend=invalid"},
		},
		{
			name:   "Invalid disk value (non-numeric)",
			labels: []string{"runs-fleet=12345/cpu=2/arch=arm64/disk=abc"},
		},
		{
			name:   "Invalid disk value (zero)",
			labels: []string{"runs-fleet=12345/cpu=2/arch=arm64/disk=0"},
		},
		{
			name:   "Invalid disk value (too large)",
			labels: []string{"runs-fleet=12345/cpu=2/arch=arm64/disk=65000"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseLabels(tt.labels)
			if err == nil {
				t.Errorf("ParseLabels() expected error for invalid input, got nil")
			}
		})
	}
}

func TestParseLabels_Backend(t *testing.T) {
	tests := []struct {
		name        string
		labels      []string
		wantBackend string
		wantErr     bool
	}{
		{
			name:        "EC2 backend",
			labels:      []string{"runs-fleet=12345/cpu=2/arch=arm64/backend=ec2"},
			wantBackend: "ec2",
		},
		{
			name:        "K8s backend",
			labels:      []string{"runs-fleet=12345/cpu=2/arch=arm64/backend=k8s"},
			wantBackend: "k8s",
		},
		{
			name:        "No backend (default)",
			labels:      []string{"runs-fleet=12345/cpu=2/arch=arm64"},
			wantBackend: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseLabels(tt.labels)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseLabels() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && got.Backend != tt.wantBackend {
				t.Errorf("ParseLabels() Backend = %v, want %v", got.Backend, tt.wantBackend)
			}
		})
	}
}

func TestParseLabels_Storage(t *testing.T) {
	tests := []struct {
		name           string
		labels         []string
		wantStorageGiB int
		wantErr        bool
	}{
		{
			name:           "Custom disk size",
			labels:         []string{"runs-fleet=12345/cpu=2/arch=arm64/disk=100"},
			wantStorageGiB: 100,
		},
		{
			name:           "Large disk size",
			labels:         []string{"runs-fleet=12345/cpu=4/arch=arm64/disk=500"},
			wantStorageGiB: 500,
		},
		{
			name:           "Maximum valid size (16 TiB gp3 limit)",
			labels:         []string{"runs-fleet=12345/cpu=2/arch=arm64/disk=16384"},
			wantStorageGiB: 16384,
		},
		{
			name:           "Minimum valid size",
			labels:         []string{"runs-fleet=12345/cpu=2/arch=arm64/disk=1"},
			wantStorageGiB: 1,
		},
		{
			name:           "No disk specified (default)",
			labels:         []string{"runs-fleet=12345/cpu=2/arch=arm64"},
			wantStorageGiB: 0,
		},
		{
			name:           "Disk with other options",
			labels:         []string{"runs-fleet=12345/cpu=4/arch=amd64/disk=200/spot=false/pool=ci"},
			wantStorageGiB: 200,
		},
		{
			name:           "Flexible spec with disk",
			labels:         []string{"runs-fleet=12345/cpu=4/arch=arm64/disk=150"},
			wantStorageGiB: 150,
		},
		{
			name:           "Flexible spec CPU range with disk",
			labels:         []string{"runs-fleet=12345/cpu=4+8/ram=8+32/arch=arm64/disk=300"},
			wantStorageGiB: 300,
		},
		{
			name:           "Flexible spec no arch with disk",
			labels:         []string{"runs-fleet=12345/cpu=4/disk=250"},
			wantStorageGiB: 250,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseLabels(tt.labels)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseLabels() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && got.StorageGiB != tt.wantStorageGiB {
				t.Errorf("ParseLabels() StorageGiB = %v, want %v", got.StorageGiB, tt.wantStorageGiB)
			}
		})
	}
}

func TestParseLabels_PublicIP(t *testing.T) {
	tests := []struct {
		name         string
		labels       []string
		wantPublicIP bool
		wantErr      bool
	}{
		{
			name:         "public=true requests public subnet",
			labels:       []string{"runs-fleet=12345/cpu=2/arch=arm64/public=true"},
			wantPublicIP: true,
		},
		{
			name:         "public=false uses private subnet",
			labels:       []string{"runs-fleet=12345/cpu=2/arch=arm64/public=false"},
			wantPublicIP: false,
		},
		{
			name:         "no public label defaults to false",
			labels:       []string{"runs-fleet=12345/cpu=2/arch=arm64"},
			wantPublicIP: false,
		},
		{
			name:         "public with other options",
			labels:       []string{"runs-fleet=12345/cpu=4/arch=amd64/public=true/spot=false/pool=ci"},
			wantPublicIP: true,
		},
		{
			name:         "public=TRUE case insensitive",
			labels:       []string{"runs-fleet=12345/cpu=2/arch=arm64/public=TRUE"},
			wantPublicIP: true,
		},
		{
			name:         "public=1 numeric true",
			labels:       []string{"runs-fleet=12345/cpu=2/arch=arm64/public=1"},
			wantPublicIP: true,
		},
		{
			name:         "public=0 numeric false",
			labels:       []string{"runs-fleet=12345/cpu=2/arch=arm64/public=0"},
			wantPublicIP: false,
		},
		{
			name:    "public=invalid returns error",
			labels:  []string{"runs-fleet=12345/cpu=2/arch=arm64/public=yes"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseLabels(tt.labels)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseLabels() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && got.PublicIP != tt.wantPublicIP {
				t.Errorf("ParseLabels() PublicIP = %v, want %v", got.PublicIP, tt.wantPublicIP)
			}
		})
	}
}

func TestParseLabels_Generation(t *testing.T) {
	tests := []struct {
		name              string
		labels            []string
		wantGen           int
		wantInstanceTypes []string
		wantErr           bool
	}{
		{
			name:              "Gen 8 ARM64",
			labels:            []string{"runs-fleet=12345/cpu=4/arch=arm64/gen=8"},
			wantGen:           8,
			wantInstanceTypes: []string{"c8g.xlarge", "m8g.xlarge"},
		},
		{
			name:              "Gen 7 ARM64",
			labels:            []string{"runs-fleet=12345/cpu=4/arch=arm64/gen=7"},
			wantGen:           7,
			wantInstanceTypes: []string{"c7g.xlarge", "m7g.xlarge"},
		},
		{
			name:              "Gen 7 amd64",
			labels:            []string{"runs-fleet=12345/cpu=4/arch=amd64/gen=7"},
			wantGen:           7,
			wantInstanceTypes: []string{"c7i.xlarge", "m7i.xlarge"},
		},
		{
			name:    "No gen (defaults to 0, any generation)",
			labels:  []string{"runs-fleet=12345/cpu=4/arch=arm64"},
			wantGen: 0,
		},
		{
			name:    "Invalid gen value (non-numeric)",
			labels:  []string{"runs-fleet=12345/cpu=4/arch=arm64/gen=abc"},
			wantErr: true,
		},
		{
			name:    "Invalid gen value (zero)",
			labels:  []string{"runs-fleet=12345/cpu=4/arch=arm64/gen=0"},
			wantErr: true,
		},
		{
			name:    "Invalid gen value (negative)",
			labels:  []string{"runs-fleet=12345/cpu=4/arch=arm64/gen=-1"},
			wantErr: true,
		},
		{
			name:    "Invalid gen value (too large)",
			labels:  []string{"runs-fleet=12345/cpu=4/arch=arm64/gen=11"},
			wantErr: true,
		},
		{
			name:              "Gen with family filter",
			labels:            []string{"runs-fleet=12345/cpu=4/arch=arm64/gen=8/family=c8g"},
			wantGen:           8,
			wantInstanceTypes: []string{"c8g.xlarge"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseLabels(tt.labels)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseLabels() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr {
				return
			}

			if got.Gen != tt.wantGen {
				t.Errorf("ParseLabels() Gen = %d, want %d", got.Gen, tt.wantGen)
			}

			for _, wantType := range tt.wantInstanceTypes {
				found := false
				for _, gotType := range got.InstanceTypes {
					if gotType == wantType {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("ParseLabels() InstanceTypes missing %q, got %v", wantType, got.InstanceTypes)
				}
			}
		})
	}
}

func TestParseLabels_OriginalLabel(t *testing.T) {
	tests := []struct {
		name              string
		labels            []string
		wantOriginalLabel string
	}{
		{
			name:              "Flexible CPU spec",
			labels:            []string{"runs-fleet=67890/cpu=2"},
			wantOriginalLabel: "runs-fleet=67890/cpu=2",
		},
		{
			name:              "Flexible CPU with arch",
			labels:            []string{"runs-fleet=11111/cpu=4+8/arch=arm64"},
			wantOriginalLabel: "runs-fleet=11111/cpu=4+8/arch=arm64",
		},
		{
			name:              "With multiple labels",
			labels:            []string{"self-hosted", "linux", "runs-fleet=22222/cpu=2/pool=default"},
			wantOriginalLabel: "runs-fleet=22222/cpu=2/pool=default",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseLabels(tt.labels)
			if err != nil {
				t.Errorf("ParseLabels() error = %v", err)
				return
			}
			if got.OriginalLabel != tt.wantOriginalLabel {
				t.Errorf("ParseLabels() OriginalLabel = %q, want %q", got.OriginalLabel, tt.wantOriginalLabel)
			}
		})
	}
}
