package cost

import (
	"context"
	"fmt"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/fleet"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
)

// BlacksmithRatePerVcpuMinute is Blacksmith's published "GitHub Actions Minutes"
// rate per vCPU-minute, by architecture. Blacksmith bills linearly in vCPU
// (arm64 runners ~37% cheaper than amd64), so a workload's counterfactual cost is
// just billable vCPU-minutes × the matching rate.
//
// These are list prices used only to produce a "what this would have cost on a
// per-minute competitor" comparison line — not a runs-fleet billing figure.
var BlacksmithRatePerVcpuMinute = map[string]float64{
	"amd64": 0.002,
	"arm64": 0.00125,
}

// archVcpu identifies a distinct (architecture, vCPU count) runner shape.
type archVcpu struct {
	arch string
	vcpu int
}

// runnerArchVcpuCombos returns the distinct (arch, vCPU) shapes the fleet can
// launch, derived from the instance catalog. This bounds the counterfactual
// query to the shapes that can actually appear on the RunnerExecutionSeconds
// metric (one metric-math query per shape).
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

// blacksmithQueryID builds a GetMetricData query ID for an (arch, vCPU) shape.
// IDs must start with a lowercase letter and contain only [a-zA-Z0-9_]; arch
// values ("arm64"/"amd64") already satisfy this.
func blacksmithQueryID(c archVcpu) string {
	return fmt.Sprintf("rxs_%s_%d", c.arch, c.vcpu)
}

// blacksmithQueries builds one metric-math query per runner shape that sums
// RunnerExecutionSeconds across the Spot and Result dimensions (Blacksmith bills
// the same regardless), leaving a single per-shape total-seconds series.
func blacksmithQueries(combos []archVcpu) []cwtypes.MetricDataQuery {
	queries := make([]cwtypes.MetricDataQuery, 0, len(combos))
	for _, c := range combos {
		expr := fmt.Sprintf(
			`SUM(SEARCH('{RunsFleet,Arch,Result,Spot,Vcpu} MetricName="RunnerExecutionSeconds" Arch="%s" Vcpu="%d"', 'Sum', 3600))`,
			c.arch, c.vcpu)
		queries = append(queries, cwtypes.MetricDataQuery{
			Id:         aws.String(blacksmithQueryID(c)),
			Expression: aws.String(expr),
			ReturnData: aws.Bool(true),
		})
	}
	return queries
}

// blacksmithCounterfactual queries the RunnerExecutionSeconds metric and returns
// the billable vCPU-minutes and the cost the same usage would have incurred on
// Blacksmith's per-minute pricing. Returns zeros (no error) when the metric has
// no data yet.
func (r *Reporter) blacksmithCounterfactual(ctx context.Context, startTime, endTime time.Time) (vcpuMinutes, cost float64, err error) {
	combos := runnerArchVcpuCombos()
	output, err := r.cwClient.GetMetricData(ctx, &cloudwatch.GetMetricDataInput{
		StartTime:         aws.Time(startTime),
		EndTime:           aws.Time(endTime),
		MetricDataQueries: blacksmithQueries(combos),
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
		rate, priceable := BlacksmithRatePerVcpuMinute[c.arch]
		seconds := secondsByID[blacksmithQueryID(c)]
		if seconds <= 0 || !priceable {
			continue
		}
		shapeVcpuMinutes := (seconds / 60) * float64(c.vcpu)
		vcpuMinutes += shapeVcpuMinutes
		cost += shapeVcpuMinutes * rate
	}
	return vcpuMinutes, cost, nil
}
