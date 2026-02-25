package worker

import (
	"fmt"
	"strings"

	"github.com/Shavakan/runs-fleet/pkg/queue"
)

// BuildRunnerConditions builds a conditions string from job resource labels.
// Used in runner names and EC2 Name tags to identify the instance spec.
// Format: arch-cpuN-ramN-diskN-families-genN (only specified labels included)
func BuildRunnerConditions(job *queue.JobMessage) string {
	var parts []string

	if job.Arch != "" {
		parts = append(parts, job.Arch)
	}
	if job.CPUMin > 0 {
		parts = append(parts, fmt.Sprintf("cpu%d", job.CPUMin))
	}
	if job.RAMMin > 0 {
		parts = append(parts, fmt.Sprintf("ram%d", int(job.RAMMin)))
	}
	if job.StorageGiB > 0 {
		parts = append(parts, fmt.Sprintf("disk%d", job.StorageGiB))
	}
	if len(job.Families) > 0 {
		parts = append(parts, strings.Join(job.Families, "-"))
	}
	if job.Gen > 0 {
		parts = append(parts, fmt.Sprintf("gen%d", job.Gen))
	}

	return strings.Join(parts, "-")
}
