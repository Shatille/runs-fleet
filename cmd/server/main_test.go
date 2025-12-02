package main

import (
	"testing"

	"github.com/Shavakan/runs-fleet/pkg/queue"
)

func TestBuildRunnerLabel(t *testing.T) {
	tests := []struct {
		name string
		job  *queue.JobMessage
		want string
	}{
		{
			name: "uses original label when present",
			job: &queue.JobMessage{
				RunID:         "12345",
				RunnerSpec:    "2cpu-linux",
				OriginalLabel: "runs-fleet=12345/cpu=2",
				Spot:          true,
			},
			want: "runs-fleet=12345/cpu=2",
		},
		{
			name: "uses original label with flexible spec",
			job: &queue.JobMessage{
				RunID:         "67890",
				RunnerSpec:    "4cpu-linux-arm64",
				OriginalLabel: "runs-fleet=67890/cpu=4+8/arch=arm64",
				Spot:          true,
			},
			want: "runs-fleet=67890/cpu=4+8/arch=arm64",
		},
		{
			name: "basic label fallback",
			job: &queue.JobMessage{
				RunID:      "12345",
				RunnerSpec: "2cpu-linux-arm64",
				Spot:       true,
			},
			want: "runs-fleet=12345/runner=2cpu-linux-arm64",
		},
		{
			name: "with pool",
			job: &queue.JobMessage{
				RunID:      "12345",
				RunnerSpec: "2cpu-linux-arm64",
				Pool:       "default",
				Spot:       true,
			},
			want: "runs-fleet=12345/runner=2cpu-linux-arm64/pool=default",
		},
		{
			name: "with private",
			job: &queue.JobMessage{
				RunID:      "12345",
				RunnerSpec: "4cpu-linux-amd64",
				Private:    true,
				Spot:       true,
			},
			want: "runs-fleet=12345/runner=4cpu-linux-amd64/private=true",
		},
		{
			name: "with spot=false",
			job: &queue.JobMessage{
				RunID:      "12345",
				RunnerSpec: "2cpu-linux-arm64",
				Spot:       false,
			},
			want: "runs-fleet=12345/runner=2cpu-linux-arm64/spot=false",
		},
		{
			name: "all modifiers",
			job: &queue.JobMessage{
				RunID:      "67890",
				RunnerSpec: "8cpu-linux-arm64",
				Pool:       "mypool",
				Private:    true,
				Spot:       false,
			},
			want: "runs-fleet=67890/runner=8cpu-linux-arm64/pool=mypool/private=true/spot=false",
		},
		{
			name: "pool and private",
			job: &queue.JobMessage{
				RunID:      "11111",
				RunnerSpec: "2cpu-linux",
				Pool:       "default",
				Private:    true,
				Spot:       true,
			},
			want: "runs-fleet=11111/runner=2cpu-linux/pool=default/private=true",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildRunnerLabel(tt.job)
			if got != tt.want {
				t.Errorf("buildRunnerLabel() = %q, want %q", got, tt.want)
			}
		})
	}
}
