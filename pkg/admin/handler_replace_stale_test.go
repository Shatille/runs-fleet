package admin

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
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
			staleInstance("i-0stale-arm", staleARM64AMI, "arm64", "cc"),
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
			staleInstance("i-0busy", staleARM64AMI, "arm64", "cc"),
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
			staleInstance("i-0unknown", staleARM64AMI, "arm64", "cc"),
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
		instances = append(instances, staleInstance(id, staleARM64AMI, "arm64", "cc"))
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
			staleInstance("i-0stale", staleARM64AMI, "arm64", "cc"),
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
