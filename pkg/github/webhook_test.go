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
				"runs-fleet=67890/runner=4cpu-linux-x64/private=true/spot=false",
			},
			want: &JobConfig{
				RunID:        "67890",
				InstanceType: "c6i.xlarge",
				Pool:         "",
				Private:      true,
				Spot:         false,
				RunnerSpec:   "4cpu-linux-x64",
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
			name: "x64 architecture",
			labels: []string{
				"runs-fleet=12345/cpu=4/arch=x64",
			},
			wantCPUMin:        4,
			wantArch:          "x64",
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

func TestParseRange(t *testing.T) {
	tests := []struct {
		input   string
		wantMin int
		wantMax int
	}{
		{"4", 4, 0},
		{"4+16", 4, 16},
		{"2+8", 2, 8},
		{"0", 0, 0},
		{"", 0, 0},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			min, max := parseRange(tt.input)
			if min != tt.wantMin {
				t.Errorf("parseRange(%q) min = %d, want %d", tt.input, min, tt.wantMin)
			}
			if max != tt.wantMax {
				t.Errorf("parseRange(%q) max = %d, want %d", tt.input, max, tt.wantMax)
			}
		})
	}
}

func TestParseRangeFloat(t *testing.T) {
	tests := []struct {
		input   string
		wantMin float64
		wantMax float64
	}{
		{"8", 8, 0},
		{"8+32", 8, 32},
		{"16.5+64", 16.5, 64},
		{"0", 0, 0},
		{"", 0, 0},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			min, max := parseRangeFloat(tt.input)
			if min != tt.wantMin {
				t.Errorf("parseRangeFloat(%q) min = %f, want %f", tt.input, min, tt.wantMin)
			}
			if max != tt.wantMax {
				t.Errorf("parseRangeFloat(%q) max = %f, want %f", tt.input, max, tt.wantMax)
			}
		})
	}
}
