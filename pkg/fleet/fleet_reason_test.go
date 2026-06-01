package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/Shavakan/runs-fleet/pkg/logging"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// captureCtxLogs sets slog.Default() to a contextHandler-wrapped JSON handler
// writing into buf, mirroring the production chain so package-level loggers
// resolve to it at log time.
func captureCtxLogs(t *testing.T, buf *bytes.Buffer) {
	t.Helper()
	inner := slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	slog.SetDefault(slog.New(logging.NewContextHandler(inner)))
	t.Cleanup(func() {
		slog.SetDefault(slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	})
}

func findLog(t *testing.T, buf *bytes.Buffer, msg string) map[string]any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(buf.Bytes()))
	for dec.More() {
		var rec map[string]any
		if err := dec.Decode(&rec); err != nil {
			t.Fatalf("decode log record: %v (buf: %s)", err, buf.String())
		}
		if rec["msg"] == msg {
			return rec
		}
	}
	t.Fatalf("no log record with msg %q found in: %s", msg, buf.String())
	return nil
}

// TestCreateOnDemandInstance_LogsReason proves the create log carries the
// caller-supplied reason so operators can tell why a pool instance was launched.
func TestCreateOnDemandInstance_LogsReason(t *testing.T) {
	var buf bytes.Buffer
	captureCtxLogs(t, &buf)

	mock := &mockEC2Client{
		RunInstancesFunc: func(_ context.Context, _ *ec2.RunInstancesInput, _ ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error) {
			return &ec2.RunInstancesOutput{
				Instances: []types.Instance{{InstanceId: aws.String(testInstanceID)}},
			}, nil
		},
	}
	manager := &Manager{ec2Client: mock, config: &config.Config{LaunchTemplateName: "runs-fleet-runner"}}

	spec := &LaunchSpec{
		RunID:        12345,
		InstanceType: "t4g.medium",
		SubnetID:     "subnet-1",
		Pool:         "default",
		Arch:         "arm64",
		Reason:       "ready_deficit",
	}
	if _, err := manager.CreateOnDemandInstance(context.Background(), spec); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	rec := findLog(t, &buf, "on-demand pool instance created")
	if got := rec["reason"]; got != "ready_deficit" {
		t.Errorf("reason = %v, want ready_deficit", got)
	}
}
