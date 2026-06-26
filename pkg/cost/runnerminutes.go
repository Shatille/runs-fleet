package cost

import (
	"context"
	"fmt"
	"maps"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/fleet"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
)

// defaultRunnerMinuteRates is the per-vCPU-minute rate by architecture, used to
// express runner usage in the standard hosted-runner cost unit (vCPU-minutes).
// Managed runners typically bill linearly in vCPU with arm64 cheaper than
// amd64; these are representative list rates. The product is a relative
// comparison figure, not a runs-fleet billing figure.
//
// Kept unexported so the canonical map can't be mutated by callers; use
// DefaultRunnerMinuteRates for a safe copy.
var defaultRunnerMinuteRates = map[string]float64{
	"amd64": 0.002,
	"arm64": 0.00125,
}

// DefaultRunnerMinuteRates returns a fresh copy of the default per-vCPU-minute
// rates by architecture, safe for the caller to retain or mutate.
func DefaultRunnerMinuteRates() map[string]float64 {
	return maps.Clone(defaultRunnerMinuteRates)
}

// archVcpu identifies a distinct (architecture, vCPU count) runner shape.
type archVcpu struct {
	arch string
	vcpu int
}

// runnerArchVcpuCombos returns the distinct (arch, vCPU) shapes the fleet can
// launch, derived from the instance catalog. This bounds the query to the
// shapes that can actually appear on the RunnerExecutionSeconds metric (one
// metric-math query per shape).
func runnerArchVcpuCombos() []archVcpu {
	seen := make(map[archVcpu]bool)
	combos := make([]archVcpu, 0)
	for _, spec := range fleet.InstanceCatalog {
		k := archVcpu{arch: spec.Arch, vcpu: spec.CPU}
		if seen[k] {
			continue
		}
		seen[k] = true
		combos = append(combos, k)
	}
	return combos
}

// runnerSecondsQueryID builds a GetMetricData query ID for an (arch, vCPU)
// shape. IDs must start with a lowercase letter and contain only [a-zA-Z0-9_];
// arch values ("arm64"/"amd64") already satisfy this.
func runnerSecondsQueryID(c archVcpu) string {
	return fmt.Sprintf("rxs_%s_%d", c.arch, c.vcpu)
}

// runnerSecondsQueries builds one metric-math query per runner shape that sums
// RunnerExecutionSeconds across the Spot and Result dimensions (the cost unit
// is the same regardless), leaving a single per-shape total-seconds series.
func runnerSecondsQueries(combos []archVcpu) []cwtypes.MetricDataQuery {
	queries := make([]cwtypes.MetricDataQuery, 0, len(combos))
	for _, c := range combos {
		expr := fmt.Sprintf(
			`SUM(SEARCH('{RunsFleet,Arch,Result,Spot,Vcpu} MetricName="RunnerExecutionSeconds" Arch="%s" Vcpu="%d"', 'Sum', 3600))`,
			c.arch, c.vcpu)
		queries = append(queries, cwtypes.MetricDataQuery{
			Id:         aws.String(runnerSecondsQueryID(c)),
			Expression: aws.String(expr),
			ReturnData: aws.Bool(true),
		})
	}
	return queries
}

// runnerMinuteCost queries the RunnerExecutionSeconds metric and returns the
// billable vCPU-minutes and the standardized runner-minute cost for the window
// (vCPU-minutes × per-vCPU-minute rate). Returns zeros (no error) when the
// metric has no data yet.
func (r *Reporter) runnerMinuteCost(ctx context.Context, startTime, endTime time.Time) (vcpuMinutes, cost float64, err error) {
	rates := defaultRunnerMinuteRates
	combos := runnerArchVcpuCombos()
	output, err := r.cwClient.GetMetricData(ctx, &cloudwatch.GetMetricDataInput{
		StartTime:         aws.Time(startTime),
		EndTime:           aws.Time(endTime),
		MetricDataQueries: runnerSecondsQueries(combos),
	})
	if err != nil {
		return 0, 0, fmt.Errorf("failed to get runner execution metric data: %w", err)
	}

	secondsByID := make(map[string]float64, len(output.MetricDataResults))
	for _, result := range output.MetricDataResults {
		if result.Id == nil {
			continue
		}
		sum := 0.0
		for _, v := range result.Values {
			sum += v
		}
		secondsByID[*result.Id] += sum
	}

	for _, c := range combos {
		rate, priceable := rates[c.arch]
		seconds := secondsByID[runnerSecondsQueryID(c)]
		if seconds <= 0 || !priceable {
			continue
		}
		shapeVcpuMinutes := (seconds / 60) * float64(c.vcpu)
		vcpuMinutes += shapeVcpuMinutes
		cost += shapeVcpuMinutes * rate
	}
	return vcpuMinutes, cost, nil
}
