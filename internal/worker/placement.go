package worker

import (
	"fmt"
	"strings"

	gh "github.com/Shavakan/runs-fleet/pkg/github"
	"github.com/Shavakan/runs-fleet/pkg/queue"
)

func recoverJobPlacement(job *queue.JobMessage, aliases *gh.AliasResolver) error {
	if job.RetryCount == 0 {
		return nil
	}

	// Recovery records retain the registration label but not the parsed placement fields.
	label := job.OriginalLabel
	_, aliased := aliases.Resolve(label)
	if aliased || label == "runs-fleet" || strings.HasPrefix(label, "runs-fleet/") || strings.HasPrefix(label, "runs-fleet=") {
		cfg, err := gh.ParseLabelsWithAliases([]string{label}, aliases)
		if err != nil {
			return fmt.Errorf("recover requested runner placement: %w", err)
		}
		job.CPUMin, job.CPUMax = cfg.CPUMin, cfg.CPUMax
		job.RAMMin, job.RAMMax = cfg.RAMMin, cfg.RAMMax
		job.Arch, job.Families, job.Gen = cfg.Arch, cfg.Families, cfg.Gen
		job.InstanceType, job.InstanceTypes = cfg.InstanceType, cfg.InstanceTypes
		job.StorageGiB = cfg.StorageGiB
		return nil
	}

	if label == "" || (job.CPUMin > 0 && job.CPUMax > 0) {
		return nil
	}

	// Instance type alone cannot recover alias options such as disk size.
	return fmt.Errorf("cannot recover placement for runner label %q without resolved constraints", label)
}
