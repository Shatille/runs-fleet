package github

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestValidateSignature(t *testing.T) {
	secret := "test-secret"
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

func TestParseLabels(t *testing.T) {
	tests := []struct {
		name    string
		labels  []string
		want    *JobConfig
		wantErr bool
	}{
		{
			name: "Standard label",
			labels: []string{
				"self-hosted",
				"runs-fleet=12345/runner=2cpu-linux-arm64/pool=default",
			},
			want: &JobConfig{
				RunID:        "12345",
				InstanceType: "t4g.medium",
				Pool:         "default",
				Private:      false,
				Spot:         true,
				RunnerSpec:   "2cpu-linux-arm64",
			},
			wantErr: false,
		},
		{
			name: "Private and On-Demand",
			labels: []string{
				"runs-fleet=67890/runner=4cpu-linux-amd64/private=true/spot=false",
			},
			want: &JobConfig{
				RunID:        "67890",
				InstanceType: "c6i.xlarge",
				Pool:         "",
				Private:      true,
				Spot:         false,
				RunnerSpec:   "4cpu-linux-amd64",
			},
			wantErr: false,
		},
		{
			name: "Large ARM instance",
			labels: []string{
				"runs-fleet=abc/runner=8cpu-linux-arm64",
			},
			want: &JobConfig{
				RunID:        "abc",
				InstanceType: "c7g.2xlarge",
				Spot:         true,
				RunnerSpec:   "8cpu-linux-arm64",
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
				if got.InstanceType != tt.want.InstanceType {
					t.Errorf("ParseLabels() InstanceType = %v, want %v", got.InstanceType, tt.want.InstanceType)
				}
				if got.Pool != tt.want.Pool {
					t.Errorf("ParseLabels() Pool = %v, want %v", got.Pool, tt.want.Pool)
				}
				if got.Private != tt.want.Private {
					t.Errorf("ParseLabels() Private = %v, want %v", got.Private, tt.want.Private)
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
			name: "Simple CPU spec",
			labels: []string{
				"runs-fleet=12345/cpu=4/arch=arm64",
			},
			wantCPUMin:        4,
			wantCPUMax:        0,
			wantArch:          "arm64",
			wantInstanceTypes: 1,
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
			name: "amd64 architecture",
			labels: []string{
				"runs-fleet=12345/cpu=4/arch=amd64",
			},
			wantCPUMin:        4,
			wantArch:          "amd64",
			wantInstanceTypes: 1,
		},
		{
			name: "With other options",
			labels: []string{
				"runs-fleet=12345/cpu=4/arch=arm64/spot=false/private=true/pool=mypool",
			},
			wantCPUMin:        4,
			wantArch:          "arm64",
			wantInstanceTypes: 1,
		},
		{
			name: "Empty arch (defaults to ARM64 families)",
			labels: []string{
				"runs-fleet=12345/cpu=4",
			},
			wantCPUMin:        4,
			wantArch:          "",
			wantInstanceTypes: 1, // Matches ARM64 instances only (legacy template compatibility)
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
			// Verify runner spec is generated
			if got.RunnerSpec == "" {
				t.Error("ParseLabels() RunnerSpec is empty")
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
			labels: []string{"runs-fleet=12345/runner=2cpu-linux-arm64/backend=invalid"},
		},
		{
			name:   "Windows with ARM64 architecture",
			labels: []string{"runs-fleet=12345/runner=2cpu-windows-arm64"},
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
			labels:      []string{"runs-fleet=12345/runner=2cpu-linux-arm64/backend=ec2"},
			wantBackend: "ec2",
		},
		{
			name:        "K8s backend",
			labels:      []string{"runs-fleet=12345/runner=2cpu-linux-arm64/backend=k8s"},
			wantBackend: "k8s",
		},
		{
			name:        "No backend (default)",
			labels:      []string{"runs-fleet=12345/runner=2cpu-linux-arm64"},
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

func TestParseLabels_SpotDiversification(t *testing.T) {
	tests := []struct {
		name              string
		labels            []string
		wantInstanceType  string
		wantInstanceTypes []string
	}{
		{
			name:              "ARM64 2cpu has diversification types",
			labels:            []string{"runs-fleet=12345/runner=2cpu-linux-arm64"},
			wantInstanceType:  "t4g.medium",
			wantInstanceTypes: []string{"t4g.medium", "t4g.large"},
		},
		{
			name:              "amd64 4cpu has diversification types",
			labels:            []string{"runs-fleet=12345/runner=4cpu-linux-amd64"},
			wantInstanceType:  "c6i.xlarge",
			wantInstanceTypes: []string{"c6i.xlarge", "m6i.xlarge", "c7i.xlarge"},
		},
		{
			name:              "amd64 8cpu has diversification types",
			labels:            []string{"runs-fleet=12345/runner=8cpu-linux-amd64"},
			wantInstanceType:  "c6i.2xlarge",
			wantInstanceTypes: []string{"c6i.2xlarge", "m6i.2xlarge", "c7i.2xlarge"},
		},
		{
			name:              "Windows has diversification types",
			labels:            []string{"runs-fleet=12345/runner=4cpu-windows-amd64"},
			wantInstanceType:  "m6i.xlarge",
			wantInstanceTypes: []string{"m6i.xlarge", "m7i.xlarge", "c6i.xlarge"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseLabels(tt.labels)
			if err != nil {
				t.Errorf("ParseLabels() error = %v", err)
				return
			}
			if got.InstanceType != tt.wantInstanceType {
				t.Errorf("ParseLabels() InstanceType = %v, want %v", got.InstanceType, tt.wantInstanceType)
			}
			if len(got.InstanceTypes) != len(tt.wantInstanceTypes) {
				t.Errorf("ParseLabels() InstanceTypes has %d types, want %d", len(got.InstanceTypes), len(tt.wantInstanceTypes))
				return
			}
			for i, wantType := range tt.wantInstanceTypes {
				if got.InstanceTypes[i] != wantType {
					t.Errorf("ParseLabels() InstanceTypes[%d] = %v, want %v", i, got.InstanceTypes[i], wantType)
				}
			}
		})
	}
}

func TestResolveSpotDiversificationTypes(t *testing.T) {
	tests := []struct {
		runnerSpec string
		wantLen    int
		wantFirst  string
	}{
		{"2cpu-linux-arm64", 2, "t4g.medium"},
		{"4cpu-linux-arm64", 3, "c7g.xlarge"},
		{"8cpu-linux-arm64", 3, "c7g.2xlarge"},
		{"2cpu-linux-amd64", 2, "t3.medium"},
		{"4cpu-linux-amd64", 3, "c6i.xlarge"},
		{"8cpu-linux-amd64", 3, "c6i.2xlarge"},
		{"unknown-spec", 1, "t4g.medium"}, // Fallback to single type
	}

	for _, tt := range tests {
		t.Run(tt.runnerSpec, func(t *testing.T) {
			got := resolveSpotDiversificationTypes(tt.runnerSpec)
			if len(got) != tt.wantLen {
				t.Errorf("resolveSpotDiversificationTypes(%q) returned %d types, want %d", tt.runnerSpec, len(got), tt.wantLen)
			}
			if len(got) > 0 && got[0] != tt.wantFirst {
				t.Errorf("resolveSpotDiversificationTypes(%q)[0] = %q, want %q", tt.runnerSpec, got[0], tt.wantFirst)
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
			name:              "Standard runner spec",
			labels:            []string{"runs-fleet=12345/runner=2cpu-linux-arm64"},
			wantOriginalLabel: "runs-fleet=12345/runner=2cpu-linux-arm64",
		},
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
