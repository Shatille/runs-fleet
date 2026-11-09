package worker

import (
	"testing"

	gh "github.com/Shavakan/runs-fleet/pkg/github"
	"github.com/Shavakan/runs-fleet/pkg/queue"
)

func testAliasResolver(t *testing.T) *gh.AliasResolver {
	t.Helper()
	aliases, err := gh.ParseAliasRules(`[{"match":"custom-arm-runners","spec":"cpu=8/arch=arm64/disk=256"}]`)
	if err != nil {
		t.Fatal(err)
	}
	return aliases
}

func TestRecoverJobPlacement(t *testing.T) {
	tests := []struct {
		name     string
		label    string
		instance string
		cpuMin   int
		cpuMax   int
		arch     string
		aliased  bool
		wantErr  bool
	}{
		{name: "native label overrides previously incompatible assignment", label: "runs-fleet/cpu=8/arch=arm64", instance: "m6i.large", cpuMin: 8, cpuMax: 16, arch: "arm64"},
		{name: "legacy marker keeps explicit range", label: "runs-fleet=99/cpu=8+8/arch=arm64", cpuMin: 8, cpuMax: 8, arch: "arm64"},
		{name: "alias with only selected type is rejected", label: "custom-arm-runners", instance: "m7g.2xlarge", wantErr: true},
		{name: "configured alias restores its resolved placement", label: "custom-arm-runners", instance: "m6i.large", cpuMin: 8, cpuMax: 16, arch: "arm64", aliased: true},
		{name: "unconfigured alias is rejected even with rules loaded", label: "other-runners", instance: "m7g.2xlarge", aliased: true, wantErr: true},
		{name: "unlabeled legacy request retains its existing behavior", instance: "m7i.large"},
		{name: "invalid native constraint is rejected", label: "runs-fleet/cpu=invalid", instance: "m7i.large", wantErr: true},
		{name: "unknown alias without recoverable hardware is rejected", label: "custom-arm-runners", instance: "unknown", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := &queue.JobMessage{OriginalLabel: tt.label, InstanceType: tt.instance, RetryCount: 1, ForceOnDemand: true}
			var aliases *gh.AliasResolver
			if tt.aliased {
				aliases = testAliasResolver(t)
			}
			err := recoverJobPlacement(job, aliases)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error=%v wantErr=%v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if job.CPUMin != tt.cpuMin || job.CPUMax != tt.cpuMax || job.Arch != tt.arch {
				t.Fatalf("incorrect placement: %+v", job)
			}
			if job.Spot || !job.ForceOnDemand || job.RetryCount != 1 || job.OriginalLabel != tt.label {
				t.Fatalf("recovery policy or identity changed: %+v", job)
			}
		})
	}
}

func TestRecoverJobPlacementRetainsAdditionalConstraints(t *testing.T) {
	job := &queue.JobMessage{RetryCount: 1, OriginalLabel: "runs-fleet/cpu=8+16/ram=16+64/arch=arm64/family=c7g+m7g/gen=7/disk=256"}
	if err := recoverJobPlacement(job, nil); err != nil {
		t.Fatal(err)
	}
	if job.RAMMin != 16 || job.RAMMax != 64 || job.Gen != 7 || job.StorageGiB != 256 || len(job.Families) != 2 || job.Families[0] != "c7g" || job.Families[1] != "m7g" {
		t.Fatalf("lost placement constraints: %+v", job)
	}
}

func TestRecoverJobPlacementPreservesResolvedAliasConstraints(t *testing.T) {
	job := &queue.JobMessage{RetryCount: 1, OriginalLabel: "custom-arm-runners", CPUMin: 8, CPUMax: 16, Arch: "arm64", StorageGiB: 256}
	if err := recoverJobPlacement(job, nil); err != nil {
		t.Fatal(err)
	}
	if job.CPUMin != 8 || job.CPUMax != 16 || job.Arch != "arm64" || job.StorageGiB != 256 {
		t.Fatalf("changed resolved placement: %+v", job)
	}
}

func TestRecoverNativePlacementRepairsPartiallyRetainedConstraints(t *testing.T) {
	job := &queue.JobMessage{RetryCount: 1, OriginalLabel: "runs-fleet/cpu=8/arch=arm64", CPUMin: 2, CPUMax: 4, Arch: "amd64"}
	if err := recoverJobPlacement(job, nil); err != nil {
		t.Fatal(err)
	}
	if job.CPUMin != 8 || job.CPUMax != 16 || job.Arch != "arm64" {
		t.Fatalf("native request weakened by stale recovery fields: %+v", job)
	}
}

func TestRecoverAliasPlacementRestoresDiskAndKeepsPool(t *testing.T) {
	job := &queue.JobMessage{RetryCount: 1, OriginalLabel: "custom-arm-runners", Pool: "requeued-pool", InstanceType: "m6i.large"}
	if err := recoverJobPlacement(job, testAliasResolver(t)); err != nil {
		t.Fatal(err)
	}
	if job.StorageGiB != 256 || job.Pool != "requeued-pool" || job.InstanceType == "m6i.large" {
		t.Fatalf("alias recovery placement: %+v", job)
	}
}
