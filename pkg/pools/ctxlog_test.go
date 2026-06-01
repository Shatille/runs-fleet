package pools

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/Shavakan/runs-fleet/pkg/db"
	"github.com/Shavakan/runs-fleet/pkg/fleet"
	"github.com/Shavakan/runs-fleet/pkg/logging"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// captureCtxLogs sets slog.Default() to a contextHandler-wrapped JSON handler
// writing into buf, mirroring the production chain so attrs stashed via
// logging.ContextWith are emitted on *Context log calls.
func captureCtxLogs(t *testing.T, buf *bytes.Buffer) {
	t.Helper()
	inner := slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	slog.SetDefault(slog.New(logging.NewContextHandler(inner)))
	t.Cleanup(func() {
		slog.SetDefault(slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	})
}

// decodeLogRecords decodes newline-delimited JSON log records from buf.
func decodeLogRecords(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var records []map[string]any
	dec := json.NewDecoder(bytes.NewReader(buf.Bytes()))
	for dec.More() {
		var rec map[string]any
		if err := dec.Decode(&rec); err != nil {
			t.Fatalf("decode log record: %v (buf: %s)", err, buf.String())
		}
		records = append(records, rec)
	}
	return records
}

// findLog returns the first decoded log record whose msg matches.
func findLog(t *testing.T, buf *bytes.Buffer, msg string) map[string]any {
	t.Helper()
	for _, rec := range decodeLogRecords(t, buf) {
		if rec["msg"] == msg {
			return rec
		}
	}
	t.Fatalf("no log record with msg %q found in: %s", msg, buf.String())
	return nil
}

// TestReconcilePoolLogCarriesPoolName proves that a log emitted deep in pool
// reconciliation inherits the pool_name stashed on the context at the
// per-pool entry point, without the log site adding it manually.
func TestReconcilePoolLogCarriesPoolName(t *testing.T) {
	var buf bytes.Buffer
	captureCtxLogs(t, &buf)

	mockDB := &MockDBClient{
		ListPoolsFunc: func(_ context.Context) ([]string, error) {
			return []string{"ctxlog-pool"}, nil
		},
		GetPoolConfigFunc: func(_ context.Context, _ string) (*db.PoolConfig, error) {
			return &db.PoolConfig{DesiredRunning: 2, DesiredStopped: 0, InstanceType: "t3.medium"}, nil
		},
		UpdatePoolStateFunc: func(_ context.Context, _ string, _, _ int) error { return nil },
	}

	mockFleet := &MockFleetAPI{
		CreateOnDemandInstanceFunc: func(_ context.Context, _ *fleet.LaunchSpec) (string, error) {
			return testInstanceNewID, nil
		},
	}

	mockEC2 := &MockEC2API{
		DescribeInstancesFunc: func(_ context.Context, _ *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
			return &ec2.DescribeInstancesOutput{
				Reservations: []ec2types.Reservation{
					{Instances: []ec2types.Instance{{
						InstanceId:   aws.String("i-running1"),
						InstanceType: ec2types.InstanceTypeT3Medium,
						State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning},
					}}},
				},
			}, nil
		},
	}

	manager := NewManager(mockDB, mockFleet, &config.Config{SubnetIDs: []string{"subnet-1"}})
	manager.SetEC2Client(mockEC2)

	manager.reconcile(context.Background())

	rec := findLog(t, &buf, "pool reconciled")
	if got := rec[logging.KeyPoolName]; got != "ctxlog-pool" {
		t.Errorf("%s = %v, want ctxlog-pool (log must inherit ctx-stashed pool_name)", logging.KeyPoolName, got)
	}
}
