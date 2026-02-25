package worker

import (
	"testing"

	"github.com/Shavakan/runs-fleet/pkg/queue"
)

func TestBuildRunnerConditions(t *testing.T) {
	tests := []struct {
		name string
		job  *queue.JobMessage
		want string
	}{
		{
			name: "arch only",
			job:  &queue.JobMessage{Arch: "arm64"},
			want: "arm64",
		},
		{
			name: "arch and cpu",
			job:  &queue.JobMessage{Arch: "arm64", CPUMin: 4},
			want: "arm64-cpu4",
		},
		{
			name: "all resource labels",
			job: &queue.JobMessage{
				Arch:       "amd64",
				CPUMin:     8,
				RAMMin:     16,
				StorageGiB: 200,
				Families:   []string{"c7g", "m7g"},
				Gen:        8,
			},
			want: "amd64-cpu8-ram16-disk200-c7g-m7g-gen8",
		},
		{
			name: "cpu and ram without arch",
			job:  &queue.JobMessage{CPUMin: 4, RAMMin: 8},
			want: "cpu4-ram8",
		},
		{
			name: "empty job",
			job:  &queue.JobMessage{},
			want: "",
		},
		{
			name: "families only",
			job:  &queue.JobMessage{Families: []string{"c7g"}},
			want: "c7g",
		},
		{
			name: "fractional ram rounds down",
			job:  &queue.JobMessage{RAMMin: 15.5},
			want: "ram15",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BuildRunnerConditions(tt.job)
			if got != tt.want {
				t.Errorf("BuildRunnerConditions() = %q, want %q", got, tt.want)
			}
		})
	}
}
