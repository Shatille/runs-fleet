package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/Shavakan/runs-fleet/pkg/queue"
)

func labelWarnRecords(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	dec := json.NewDecoder(bytes.NewReader(buf.Bytes()))
	for dec.More() {
		var rec map[string]any
		if err := dec.Decode(&rec); err != nil {
			t.Fatalf("decode log record: %v (buf: %s)", err, buf.String())
		}
		if rec["level"] == "WARN" && rec["retry_count"] != nil {
			out = append(out, rec)
		}
	}
	return out
}

// A re-dispatch that reaches the synthesized fallback has lost the label its
// first dispatch carried, so it will register a runner the starving job cannot
// match. That is the failure this guard exists to make visible; a first dispatch
// legitimately has no label yet and must stay quiet.
func TestBuildRunnerLabel_WarnsOnlyWhenRedispatchLostItsLabel(t *testing.T) {
	tests := []struct {
		name     string
		job      *queue.JobMessage
		wantWarn bool
	}{
		{
			name:     "re-dispatch without a label warns",
			job:      &queue.JobMessage{JobID: 94023466800, RunID: 31566820776, RetryCount: 1},
			wantWarn: true,
		},
		{
			name:     "first dispatch without a label is quiet",
			job:      &queue.JobMessage{JobID: 94023466800, RunID: 31566820776},
			wantWarn: false,
		},
		{
			name: "re-dispatch that kept its label is quiet",
			job: &queue.JobMessage{
				JobID:         94023466800,
				RunID:         31566820776,
				RetryCount:    1,
				OriginalLabel: "runs-fleet/cpu=2/pool=lingua-franca",
			},
			wantWarn: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			buf := &bytes.Buffer{}
			captureCtxLogs(t, buf)

			BuildRunnerLabel(context.Background(), tt.job)

			got := labelWarnRecords(t, buf)
			if tt.wantWarn && len(got) == 0 {
				t.Fatalf("expected a warn carrying retry_count, got none (buf: %s)", buf.String())
			}
			if !tt.wantWarn && len(got) > 0 {
				t.Fatalf("expected no warn, got %v", got)
			}
			if tt.wantWarn {
				if got[0]["retry_count"] != float64(tt.job.RetryCount) {
					t.Errorf("retry_count = %v, want %d", got[0]["retry_count"], tt.job.RetryCount)
				}
				if got[0]["job_id"] != float64(tt.job.JobID) {
					t.Errorf("job_id = %v, want %d", got[0]["job_id"], tt.job.JobID)
				}
			}
		})
	}
}

// The label a re-dispatch carries must be the one handed to GitHub verbatim:
// dispatch is exact label-set membership, so any rewriting strands the job.
func TestBuildRunnerLabel_PreservesRequestedLabelOnRedispatch(t *testing.T) {
	const want = "runs-fleet/cpu=2/pool=lingua-franca"
	got := BuildRunnerLabel(context.Background(), &queue.JobMessage{
		JobID:         94023466800,
		RunID:         31566820776,
		RetryCount:    2,
		Pool:          "lingua-franca",
		OriginalLabel: want,
	})
	if got != want {
		t.Errorf("BuildRunnerLabel() = %q, want %q", got, want)
	}
}
