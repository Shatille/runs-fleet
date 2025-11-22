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
