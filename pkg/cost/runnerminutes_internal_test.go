package cost

import (
	"strings"
	"testing"
)

func TestRunnerArchVcpuCombos(t *testing.T) {
	t.Parallel()
	combos := runnerArchVcpuCombos()
	if len(combos) == 0 {
		t.Fatal("expected at least one combo from the instance catalog")
	}

	seen := make(map[archVcpu]bool)
	var sawArm64x4 bool
	for _, c := range combos {
		if seen[c] {
			t.Errorf("duplicate combo %+v", c)
		}
		seen[c] = true
		if c.arch != "arm64" && c.arch != "amd64" {
			t.Errorf("unexpected arch %q", c.arch)
		}
		if c.vcpu <= 0 {
			t.Errorf("non-positive vcpu for %+v", c)
		}
		if c.arch == "arm64" && c.vcpu == 4 {
			sawArm64x4 = true
		}
	}
	if !sawArm64x4 {
		t.Error("expected an arm64/4-vCPU combo (c7g.xlarge) in the catalog")
	}
}

func TestRunnerSecondsQueryID(t *testing.T) {
	t.Parallel()
	got := runnerSecondsQueryID(archVcpu{arch: "arm64", vcpu: 4})
	if got != "rxs_arm64_4" {
		t.Errorf("query ID = %q, want rxs_arm64_4", got)
	}
}

func TestRunnerSecondsQueries(t *testing.T) {
	t.Parallel()
	combos := []archVcpu{{arch: "arm64", vcpu: 4}, {arch: "amd64", vcpu: 8}}
	queries := runnerSecondsQueries(combos)
	if len(queries) != len(combos) {
		t.Fatalf("got %d queries, want %d", len(queries), len(combos))
	}
	for i, q := range queries {
		if q.Id == nil || q.Expression == nil {
			t.Fatalf("query %d missing Id/Expression", i)
		}
		expr := *q.Expression
		if !strings.Contains(expr, `MetricName="RunnerExecutionSeconds"`) {
			t.Errorf("query %d expr missing metric name: %s", i, expr)
		}
		if !strings.HasPrefix(expr, "SUM(SEARCH(") {
			t.Errorf("query %d expr should sum a search: %s", i, expr)
		}
	}
}

func TestDefaultRunnerMinuteRates(t *testing.T) {
	t.Parallel()
	rates := DefaultRunnerMinuteRates()
	// arm64 must be cheaper than amd64 per vCPU-minute, matching typical
	// hosted-runner pricing.
	if rates["arm64"] >= rates["amd64"] {
		t.Errorf("arm64 rate %v should be below amd64 rate %v", rates["arm64"], rates["amd64"])
	}
	// The accessor must return a copy: mutating it must not corrupt the canonical map.
	rates["arm64"] = 999
	if defaultRunnerMinuteRates["arm64"] == 999 {
		t.Error("DefaultRunnerMinuteRates() returned an alias to the package global, not a copy")
	}
}
