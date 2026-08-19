package admin

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/Shavakan/runs-fleet/pkg/events"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

func staleInstance(id, ami, arch, pool string) types.Instance {
	inst := instanceOn(id, ami, arch)
	inst.Tags = []types.Tag{
		{Key: aws.String("runs-fleet:managed"), Value: aws.String("true")},
	}
	if pool != "" {
		inst.Tags = append(inst.Tags, types.Tag{Key: aws.String("runs-fleet:pool"), Value: aws.String(pool)})
	}
	return inst
}

func stoppedStaleInstance(id, ami, arch, pool string) types.Instance {
	inst := staleInstance(id, ami, arch, pool)
	inst.State = &types.InstanceState{Name: types.InstanceStateNameStopped}
	return inst
}

func replaceStale(t *testing.T, ec2Mock *mockEC2API, db *mockInstancesDB, query string) (*httptest.ResponseRecorder, ReplaceStaleResponse) {
	t.Helper()

	handler := NewInstancesHandler(ec2Mock, db, testJobsTable, &mockAuditDB{}, &AuthMiddleware{requireAuth: false})
	handler.SetAMISource(healthyLaunchTemplates(), "runs-fleet-runner")

	w := httptest.NewRecorder()
	handler.ReplaceStaleInstances(w, httptest.NewRequest(http.MethodPost, "/api/instances/replace-stale"+query, nil))

	var resp ReplaceStaleResponse
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v (body %s)", err, w.Body.String())
		}
	}
	return w, resp
}

// Only stale pool members are replaced, and the comparison is per architecture:
// an amd64 instance on the current amd64 AMI is not stale just because the arm64
// template moved.
func TestReplaceStale_TargetsOnlyStalePoolMembers(t *testing.T) {
	t.Parallel()

	ec2Mock := &mockEC2API{output: &ec2.DescribeInstancesOutput{
		Reservations: []types.Reservation{{Instances: []types.Instance{
			stoppedStaleInstance("i-0stale-arm", staleARM64AMI, "arm64", "cc"),
			staleInstance("i-0current-arm", currentARM64AMI, "arm64", "cc"),
			staleInstance("i-0current-amd", currentAMD64AMI, "x86_64", "cc"),
			staleInstance("i-0stale-nopool", staleARM64AMI, "arm64", ""),
		}}},
	}}

	w, resp := replaceStale(t, ec2Mock, &mockInstancesDB{}, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}

	if resp.Stale != 1 || len(resp.Terminated) != 1 || resp.Terminated[0] != "i-0stale-arm" {
		t.Errorf("response = %+v, want only i-0stale-arm replaced", resp)
	}
	if len(ec2Mock.terminatedIDs) != 1 || ec2Mock.terminatedIDs[0] != "i-0stale-arm" {
		t.Errorf("terminated %v, want only the stale pool member", ec2Mock.terminatedIDs)
	}
}

// A job running on a stale instance outranks the AMI. It is reported so the
// operator can decide, never force-terminated.
func TestReplaceStale_ReportsBusyRatherThanKilling(t *testing.T) {
	t.Parallel()

	ec2Mock := &mockEC2API{output: &ec2.DescribeInstancesOutput{
		Reservations: []types.Reservation{{Instances: []types.Instance{
			stoppedStaleInstance("i-0busy", staleARM64AMI, "arm64", "cc"),
		}}},
	}}
	db := &mockInstancesDB{job: &events.JobInfo{JobID: 93654578869, RunID: 31450797546, Repo: "devsisters/cc-data"}}

	_, resp := replaceStale(t, ec2Mock, db, "")
	if len(resp.Terminated) != 0 || len(resp.Busy) != 1 || resp.Busy[0] != "i-0busy" {
		t.Errorf("response = %+v, want the busy instance reported and untouched", resp)
	}
	if len(ec2Mock.terminatedIDs) != 0 {
		t.Errorf("terminated %v, want nothing", ec2Mock.terminatedIDs)
	}
}

// An active-job lookup that fails is not evidence the instance is idle.
func TestReplaceStale_JobLookupFailureIsTreatedAsBusy(t *testing.T) {
	t.Parallel()

	ec2Mock := &mockEC2API{output: &ec2.DescribeInstancesOutput{
		Reservations: []types.Reservation{{Instances: []types.Instance{
			stoppedStaleInstance("i-0unknown", staleARM64AMI, "arm64", "cc"),
		}}},
	}}
	db := &mockInstancesDB{jobErr: errors.New("dynamo unavailable")}

	_, resp := replaceStale(t, ec2Mock, db, "")
	if len(resp.Terminated) != 0 || len(resp.Busy) != 1 {
		t.Errorf("response = %+v, want the unverifiable instance left alone", resp)
	}
}

// The cap exists so a pool is never drained; what it leaves is reported rather
// than silently dropped.
func TestReplaceStale_CapIsReported(t *testing.T) {
	t.Parallel()

	var instances []types.Instance
	for _, id := range []string{"i-0a", "i-0b", "i-0c"} {
		instances = append(instances, stoppedStaleInstance(id, staleARM64AMI, "arm64", "cc"))
	}
	ec2Mock := &mockEC2API{output: &ec2.DescribeInstancesOutput{
		Reservations: []types.Reservation{{Instances: instances}},
	}}

	_, resp := replaceStale(t, ec2Mock, &mockInstancesDB{}, "?max=2")
	if len(resp.Terminated) != 2 || len(resp.Skipped) != 1 {
		t.Errorf("response = %+v, want 2 replaced and 1 left for a later call", resp)
	}
	if resp.Stale != 3 {
		t.Errorf("stale = %d, want the full count 3", resp.Stale)
	}
}

func TestReplaceStale_DryRunTouchesNothing(t *testing.T) {
	t.Parallel()

	ec2Mock := &mockEC2API{output: &ec2.DescribeInstancesOutput{
		Reservations: []types.Reservation{{Instances: []types.Instance{
			stoppedStaleInstance("i-0stale", staleARM64AMI, "arm64", "cc"),
		}}},
	}}

	_, resp := replaceStale(t, ec2Mock, &mockInstancesDB{}, "?dry_run=true")
	if !resp.DryRun || len(resp.Terminated) != 1 {
		t.Errorf("response = %+v, want a dry run naming its one target", resp)
	}
	if len(ec2Mock.terminatedIDs) != 0 {
		t.Errorf("terminated %v, want nothing on a dry run", ec2Mock.terminatedIDs)
	}
}

func TestReplaceStale_Unconfigured(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		jobsTable string
		withAMIs  bool
		wantCode  int
	}{
		{name: "no AMI source", jobsTable: testJobsTable, withAMIs: false, wantCode: http.StatusServiceUnavailable},
		{name: "no jobs table", jobsTable: "", withAMIs: true, wantCode: http.StatusServiceUnavailable},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ec2Mock := &mockEC2API{output: &ec2.DescribeInstancesOutput{}}
			handler := NewInstancesHandler(ec2Mock, &mockInstancesDB{}, tt.jobsTable, &mockAuditDB{}, &AuthMiddleware{requireAuth: false})
			if tt.withAMIs {
				handler.SetAMISource(healthyLaunchTemplates(), "runs-fleet-runner")
			}

			w := httptest.NewRecorder()
			handler.ReplaceStaleInstances(w, httptest.NewRequest(http.MethodPost, "/api/instances/replace-stale", nil))
			if w.Code != tt.wantCode {
				t.Errorf("status = %d, want %d", w.Code, tt.wantCode)
			}
			if len(ec2Mock.terminatedIDs) != 0 {
				t.Errorf("terminated %v, want nothing", ec2Mock.terminatedIDs)
			}
		})
	}
}

func TestReplaceStale_RejectsBadMax(t *testing.T) {
	t.Parallel()

	for _, q := range []string{"?max=0", "?max=nope", "?max=999"} {
		w, _ := replaceStale(t, &mockEC2API{output: &ec2.DescribeInstancesOutput{}}, &mockInstancesDB{}, q)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", q, w.Code)
		}
	}
}

// EC2 does not re-image an instance on start, but a running instance holds a
// registered runner GitHub can dispatch to at any moment. It is reported so the
// operator knows why the stale count exceeds what was replaced, never terminated.
func TestReplaceStale_RunningStaleReportedNotTerminated(t *testing.T) {
	t.Parallel()

	ec2Mock := &mockEC2API{output: &ec2.DescribeInstancesOutput{
		Reservations: []types.Reservation{{Instances: []types.Instance{
			staleInstance("i-0running", staleARM64AMI, "arm64", "cc"),
			stoppedStaleInstance("i-0stopped", staleARM64AMI, "arm64", "cc"),
		}}},
	}}

	_, resp := replaceStale(t, ec2Mock, &mockInstancesDB{}, "")
	if len(resp.Terminated) != 1 || resp.Terminated[0] != "i-0stopped" {
		t.Errorf("terminated = %v, want only the stopped instance", resp.Terminated)
	}
	if len(resp.Running) != 1 || resp.Running[0] != "i-0running" {
		t.Errorf("running = %v, want the running instance reported", resp.Running)
	}
	if resp.Stale != 2 {
		t.Errorf("stale = %d, want the full count 2", resp.Stale)
	}
	if len(ec2Mock.terminatedIDs) != 1 || ec2Mock.terminatedIDs[0] != "i-0stopped" {
		t.Errorf("terminated %v, want only the stopped instance", ec2Mock.terminatedIDs)
	}
}

// A pool instance is claimed in DynamoDB before it is started, so it reads
// stopped while a job is already waiting on it. The jobs table cannot see that
// window; the claim is the only signal that does.
func TestReplaceStale_ClaimedStoppedInstanceLeftAlone(t *testing.T) {
	t.Parallel()

	ec2Mock := &mockEC2API{output: &ec2.DescribeInstancesOutput{
		Reservations: []types.Reservation{{Instances: []types.Instance{
			stoppedStaleInstance("i-0claimed", staleARM64AMI, "arm64", "cc"),
		}}},
	}}
	db := &mockInstancesDB{claims: map[string]bool{"i-0claimed": true}}

	_, resp := replaceStale(t, ec2Mock, db, "")
	if len(resp.Terminated) != 0 || len(resp.Busy) != 1 || resp.Busy[0] != "i-0claimed" {
		t.Errorf("response = %+v, want the claimed instance reported and untouched", resp)
	}
	if len(ec2Mock.terminatedIDs) != 0 {
		t.Errorf("terminated %v, want nothing", ec2Mock.terminatedIDs)
	}
}

// A claim lookup that fails is not evidence the instance is unclaimed.
func TestReplaceStale_ClaimCheckFailureIsTreatedAsBusy(t *testing.T) {
	t.Parallel()

	ec2Mock := &mockEC2API{output: &ec2.DescribeInstancesOutput{
		Reservations: []types.Reservation{{Instances: []types.Instance{
			stoppedStaleInstance("i-0unverifiable", staleARM64AMI, "arm64", "cc"),
		}}},
	}}
	db := &mockInstancesDB{claimErr: errors.New("dynamo unavailable")}

	_, resp := replaceStale(t, ec2Mock, db, "")
	if len(resp.Terminated) != 0 || len(resp.Busy) != 1 {
		t.Errorf("response = %+v, want the unverifiable instance left alone", resp)
	}
	if len(ec2Mock.terminatedIDs) != 0 {
		t.Errorf("terminated %v, want nothing", ec2Mock.terminatedIDs)
	}
}

// Running instances are never terminable, so they must not consume the cap that
// exists to avoid draining a pool.
func TestReplaceStale_CapCountsOnlyTerminableCandidates(t *testing.T) {
	t.Parallel()

	instances := []types.Instance{
		staleInstance("i-0run-a", staleARM64AMI, "arm64", "cc"),
		staleInstance("i-0run-b", staleARM64AMI, "arm64", "cc"),
		staleInstance("i-0run-c", staleARM64AMI, "arm64", "cc"),
		stoppedStaleInstance("i-0stop-a", staleARM64AMI, "arm64", "cc"),
		stoppedStaleInstance("i-0stop-b", staleARM64AMI, "arm64", "cc"),
	}
	ec2Mock := &mockEC2API{output: &ec2.DescribeInstancesOutput{
		Reservations: []types.Reservation{{Instances: instances}},
	}}

	_, resp := replaceStale(t, ec2Mock, &mockInstancesDB{}, "?max=2")
	if len(resp.Terminated) != 2 {
		t.Errorf("terminated = %v, want both stopped instances", resp.Terminated)
	}
	if len(resp.Skipped) != 0 {
		t.Errorf("skipped = %v, want nothing: running instances must not consume the cap", resp.Skipped)
	}
	if len(resp.Running) != 3 {
		t.Errorf("running = %v, want all 3 reported", resp.Running)
	}
}

// An instance whose state EC2 did not report must not be assumed stopped.
func TestReplaceStale_UnreadableStateIsNeverTerminated(t *testing.T) {
	t.Parallel()

	unreadable := staleInstance("i-0nostate", staleARM64AMI, "arm64", "cc")
	unreadable.State = nil
	ec2Mock := &mockEC2API{output: &ec2.DescribeInstancesOutput{
		Reservations: []types.Reservation{{Instances: []types.Instance{unreadable}}},
	}}

	_, resp := replaceStale(t, ec2Mock, &mockInstancesDB{}, "")
	if len(resp.Terminated) != 0 {
		t.Errorf("terminated = %v, want nothing: an unreadable state must fail closed", resp.Terminated)
	}
	if len(resp.Running) != 1 || resp.Running[0] != "i-0nostate" {
		t.Errorf("running = %v, want the unreadable instance reported", resp.Running)
	}
	if len(ec2Mock.terminatedIDs) != 0 {
		t.Errorf("terminated %v, want nothing", ec2Mock.terminatedIDs)
	}
}

// A terminate that fails partway through leaves the earlier instances already
// destroyed. Both the audit record and the operator's error must name them, or a
// destructive action reads as having done nothing.
func TestReplaceStale_PartialFailureStillAuditsWhatDied(t *testing.T) {
	t.Parallel()

	ec2Mock := &mockEC2API{
		output: &ec2.DescribeInstancesOutput{
			Reservations: []types.Reservation{{Instances: []types.Instance{
				stoppedStaleInstance("i-0first", staleARM64AMI, "arm64", "cc"),
				stoppedStaleInstance("i-0second", staleARM64AMI, "arm64", "cc"),
			}}},
		},
		terminateErrByID: map[string]error{"i-0second": errors.New("RequestLimitExceeded")},
	}
	audit := &mockAuditDB{}

	handler := NewInstancesHandler(ec2Mock, &mockInstancesDB{}, testJobsTable, audit, &AuthMiddleware{requireAuth: false})
	handler.SetAMISource(healthyLaunchTemplates(), "runs-fleet-runner")

	w := httptest.NewRecorder()
	handler.ReplaceStaleInstances(w, httptest.NewRequest(http.MethodPost, "/api/instances/replace-stale", nil))

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
	if !slices.Equal(ec2Mock.terminatedIDs, []string{"i-0first"}) {
		t.Fatalf("terminated = %v, want only i-0first", ec2Mock.terminatedIDs)
	}
	if !strings.Contains(w.Body.String(), "i-0first") {
		t.Errorf("body = %s, want the already-replaced instance named", w.Body.String())
	}

	if len(audit.entries) != 1 {
		t.Fatalf("audit entries = %d, want 1", len(audit.entries))
	}
	entry := audit.entries[0]
	if entry.Result != auditResultError {
		t.Errorf("audit result = %q, want error", entry.Result)
	}
	if got, ok := entry.Details["terminated"].(string); !ok || got != "i-0first" {
		t.Errorf("audit terminated = %v, want i-0first", entry.Details["terminated"])
	}
}
