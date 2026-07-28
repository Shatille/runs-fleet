package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/events"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"
)

const (
	testJobsTable = "runs-fleet-jobs"
	testPool      = "default"
)

type mockEC2API struct {
	output *ec2.DescribeInstancesOutput
	err    error

	spotRequests    []types.SpotInstanceRequest
	describeSpotErr error
	cancelSpotErr   error
	terminateErr    error

	calls            []string
	terminatedIDs    []string
	cancelledSpotIDs []string
}

func (m *mockEC2API) DescribeInstances(_ context.Context, _ *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	m.calls = append(m.calls, "describe")
	return m.output, m.err
}

func (m *mockEC2API) TerminateInstances(_ context.Context, params *ec2.TerminateInstancesInput, _ ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error) {
	m.calls = append(m.calls, "terminate")
	if m.terminateErr != nil {
		return nil, m.terminateErr
	}
	m.terminatedIDs = append(m.terminatedIDs, params.InstanceIds...)
	return &ec2.TerminateInstancesOutput{}, nil
}

func (m *mockEC2API) DescribeSpotInstanceRequests(_ context.Context, _ *ec2.DescribeSpotInstanceRequestsInput, _ ...func(*ec2.Options)) (*ec2.DescribeSpotInstanceRequestsOutput, error) {
	m.calls = append(m.calls, "describe-spot")
	if m.describeSpotErr != nil {
		return nil, m.describeSpotErr
	}
	return &ec2.DescribeSpotInstanceRequestsOutput{SpotInstanceRequests: m.spotRequests}, nil
}

func (m *mockEC2API) CancelSpotInstanceRequests(_ context.Context, params *ec2.CancelSpotInstanceRequestsInput, _ ...func(*ec2.Options)) (*ec2.CancelSpotInstanceRequestsOutput, error) {
	m.calls = append(m.calls, "cancel-spot")
	if m.cancelSpotErr != nil {
		return nil, m.cancelSpotErr
	}
	m.cancelledSpotIDs = append(m.cancelledSpotIDs, params.SpotInstanceRequestIds...)
	return &ec2.CancelSpotInstanceRequestsOutput{}, nil
}

type mockInstancesDB struct {
	busyIDs map[string][]string

	job       *events.JobInfo
	jobErr    error
	markErr   error
	markCalls []string
}

func (m *mockInstancesDB) GetPoolBusyInstanceIDs(_ context.Context, poolName string) ([]string, error) {
	return m.busyIDs[poolName], nil
}

func (m *mockInstancesDB) GetJobByInstance(_ context.Context, _ string) (*events.JobInfo, error) {
	if m.jobErr != nil {
		return nil, m.jobErr
	}
	return m.job, nil
}

func (m *mockInstancesDB) MarkInstanceTerminating(_ context.Context, instanceID string) error {
	m.markCalls = append(m.markCalls, instanceID)
	return m.markErr
}

func newTestInstancesHandler(ec2Mock *mockEC2API, dbMock *mockInstancesDB) *InstancesHandler {
	return NewInstancesHandler(ec2Mock, dbMock, testJobsTable, nil, &AuthMiddleware{requireAuth: false})
}

func TestInstancesHandler_ListInstances(t *testing.T) {
	t.Parallel()

	launchTime := time.Now().Add(-1 * time.Hour)

	tests := []struct {
		name           string
		ec2Output      *ec2.DescribeInstancesOutput
		busyIDs        map[string][]string
		query          string
		wantCount      int
		wantBusyCount  int
		wantStatusCode int
	}{
		{
			name: "list all instances",
			ec2Output: &ec2.DescribeInstancesOutput{
				Reservations: []types.Reservation{
					{
						Instances: []types.Instance{
							{
								InstanceId:       aws.String("i-abc123"),
								InstanceType:     types.InstanceTypeT4gMedium,
								State:            &types.InstanceState{Name: types.InstanceStateNameRunning},
								LaunchTime:       &launchTime,
								PrivateIpAddress: aws.String("10.0.1.100"),
								Tags: []types.Tag{
									{Key: aws.String("runs-fleet:pool"), Value: aws.String(testPool)},
									{Key: aws.String("runs-fleet:managed"), Value: aws.String("true")},
								},
							},
							{
								InstanceId:   aws.String("i-def456"),
								InstanceType: types.InstanceTypeC7gXlarge,
								State:        &types.InstanceState{Name: types.InstanceStateNameStopped},
								Tags: []types.Tag{
									{Key: aws.String("runs-fleet:pool"), Value: aws.String(testPool)},
									{Key: aws.String("runs-fleet:managed"), Value: aws.String("true")},
								},
							},
						},
					},
				},
			},
			busyIDs:        map[string][]string{testPool: {"i-abc123"}},
			wantCount:      2,
			wantBusyCount:  1,
			wantStatusCode: http.StatusOK,
		},
		{
			name: "filter by pool",
			ec2Output: &ec2.DescribeInstancesOutput{
				Reservations: []types.Reservation{
					{
						Instances: []types.Instance{
							{
								InstanceId:   aws.String("i-pool1"),
								InstanceType: types.InstanceTypeT4gMedium,
								State:        &types.InstanceState{Name: types.InstanceStateNameRunning},
								Tags: []types.Tag{
									{Key: aws.String("runs-fleet:pool"), Value: aws.String("pool1")},
								},
							},
						},
					},
				},
			},
			busyIDs:        map[string][]string{},
			query:          "?pool=pool1",
			wantCount:      1,
			wantStatusCode: http.StatusOK,
		},
		{
			name: "spot instance detection",
			ec2Output: &ec2.DescribeInstancesOutput{
				Reservations: []types.Reservation{
					{
						Instances: []types.Instance{
							{
								InstanceId:        aws.String("i-spot"),
								InstanceType:      types.InstanceTypeC7gXlarge,
								State:             &types.InstanceState{Name: types.InstanceStateNameRunning},
								InstanceLifecycle: types.InstanceLifecycleTypeSpot,
								Tags: []types.Tag{
									{Key: aws.String("runs-fleet:pool"), Value: aws.String(testPool)},
								},
							},
						},
					},
				},
			},
			busyIDs:        map[string][]string{},
			wantCount:      1,
			wantStatusCode: http.StatusOK,
		},
		{
			name: "empty result",
			ec2Output: &ec2.DescribeInstancesOutput{
				Reservations: []types.Reservation{},
			},
			busyIDs:        map[string][]string{},
			wantCount:      0,
			wantStatusCode: http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ec2Mock := &mockEC2API{output: tt.ec2Output}
			dbMock := &mockInstancesDB{busyIDs: tt.busyIDs}
			handler := newTestInstancesHandler(ec2Mock, dbMock)

			mux := http.NewServeMux()
			handler.RegisterRoutes(mux)

			req := httptest.NewRequest("GET", "/api/instances"+tt.query, nil)
			rec := httptest.NewRecorder()

			mux.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatusCode {
				t.Errorf("got status %d, want %d", rec.Code, tt.wantStatusCode)
			}

			if tt.wantStatusCode == http.StatusOK {
				var resp struct {
					Instances []InstanceResponse `json:"instances"`
					Total     int                `json:"total"`
				}
				if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
					t.Fatalf("failed to decode response: %v", err)
				}

				if resp.Total != tt.wantCount {
					t.Errorf("got total %d, want %d", resp.Total, tt.wantCount)
				}

				busyCount := 0
				for _, inst := range resp.Instances {
					if inst.Busy {
						busyCount++
					}
				}
				if busyCount != tt.wantBusyCount {
					t.Errorf("got busy count %d, want %d", busyCount, tt.wantBusyCount)
				}
			}
		})
	}
}

func TestInstancesHandler_InvalidStateFilter(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		state string
		want  int
	}{
		{"valid state running", "running", http.StatusOK},
		{"valid state stopped", "stopped", http.StatusOK},
		{"valid state shutting-down", "shutting-down", http.StatusOK},
		{"invalid state", "bogus", http.StatusBadRequest},
		{"invalid state active", "active", http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ec2Mock := &mockEC2API{output: &ec2.DescribeInstancesOutput{}}
			dbMock := &mockInstancesDB{busyIDs: map[string][]string{}}
			handler := newTestInstancesHandler(ec2Mock, dbMock)

			mux := http.NewServeMux()
			handler.RegisterRoutes(mux)

			req := httptest.NewRequest("GET", "/api/instances?state="+tt.state, nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			if rec.Code != tt.want {
				t.Errorf("state=%q: got status %d, want %d", tt.state, rec.Code, tt.want)
			}
		})
	}
}

func TestInstanceIDPattern(t *testing.T) {
	t.Parallel()

	tests := []struct {
		id    string
		valid bool
	}{
		{"i-0123456789abcdef0", true},   // 17 hex (current form)
		{"i-1234abcd", true},            // 8 hex (legacy form)
		{"i-1234ABCD", true},            // case-insensitive
		{"i-0123456789abcd", false},     // 14 hex: never issued by AWS
		{"i-0123456", false},            // 7 hex: too short
		{"i-0123456789abcdef01", false}, // 18 hex: too long
		{"i-0123456789abcdeg0", false},  // non-hex char
		{"not-an-id", false},
		{"", false},
	}

	for _, tt := range tests {
		if got := instanceIDPattern.MatchString(tt.id); got != tt.valid {
			t.Errorf("instanceIDPattern.MatchString(%q) = %v, want %v", tt.id, got, tt.valid)
		}
	}
}

func TestInstancesHandler_GetInstance(t *testing.T) {
	t.Parallel()

	launchTime := time.Now().Add(-30 * time.Minute)
	found := &ec2.DescribeInstancesOutput{
		Reservations: []types.Reservation{
			{Instances: []types.Instance{
				{
					InstanceId:        aws.String("i-0123456789abcdef0"),
					InstanceType:      types.InstanceTypeC7gXlarge,
					State:             &types.InstanceState{Name: types.InstanceStateNameRunning},
					LaunchTime:        &launchTime,
					PrivateIpAddress:  aws.String("10.0.1.5"),
					ImageId:           aws.String("ami-abc123"),
					SubnetId:          aws.String("subnet-1"),
					Architecture:      types.ArchitectureValuesArm64,
					InstanceLifecycle: types.InstanceLifecycleTypeSpot,
					Placement:         &types.Placement{AvailabilityZone: aws.String("ap-northeast-1a")},
					Tags: []types.Tag{
						{Key: aws.String("runs-fleet:pool"), Value: aws.String(testPool)},
						{Key: aws.String("runs-fleet:managed"), Value: aws.String("true")},
					},
				},
			}},
		},
	}

	tests := []struct {
		name       string
		id         string
		output     *ec2.DescribeInstancesOutput
		ec2Err     error
		busyIDs    map[string][]string
		wantStatus int
		wantBusy   bool
	}{
		{name: "found and busy", id: "i-0123456789abcdef0", output: found, busyIDs: map[string][]string{testPool: {"i-0123456789abcdef0"}}, wantStatus: http.StatusOK, wantBusy: true},
		{name: "found idle", id: "i-0123456789abcdef0", output: found, busyIDs: map[string][]string{}, wantStatus: http.StatusOK, wantBusy: false},
		{name: "unmanaged or absent", id: "i-0123456789abcdef0", output: &ec2.DescribeInstancesOutput{}, wantStatus: http.StatusNotFound},
		{name: "invalid id", id: "not-an-id", output: &ec2.DescribeInstancesOutput{}, wantStatus: http.StatusBadRequest},
		{name: "aws not found error", id: "i-0123456789abcdef0", ec2Err: &smithy.GenericAPIError{Code: "InvalidInstanceID.NotFound"}, wantStatus: http.StatusNotFound},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ec2Mock := &mockEC2API{output: tt.output, err: tt.ec2Err}
			dbMock := &mockInstancesDB{busyIDs: tt.busyIDs}
			handler := newTestInstancesHandler(ec2Mock, dbMock)

			mux := http.NewServeMux()
			handler.RegisterRoutes(mux)

			req := httptest.NewRequest("GET", "/api/instances/"+tt.id, nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("got status %d, want %d", rec.Code, tt.wantStatus)
			}
			if tt.wantStatus != http.StatusOK {
				return
			}

			var resp InstanceDetailResponse
			if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
				t.Fatalf("failed to decode response: %v", err)
			}
			if resp.InstanceID != tt.id {
				t.Errorf("instance_id = %q, want %q", resp.InstanceID, tt.id)
			}
			if resp.AvailabilityZone != "ap-northeast-1a" {
				t.Errorf("availability_zone = %q, want ap-northeast-1a", resp.AvailabilityZone)
			}
			if resp.ImageID != "ami-abc123" {
				t.Errorf("image_id = %q, want ami-abc123", resp.ImageID)
			}
			if !resp.Spot {
				t.Error("spot = false, want true")
			}
			if resp.Busy != tt.wantBusy {
				t.Errorf("busy = %v, want %v", resp.Busy, tt.wantBusy)
			}
			if resp.Tags["runs-fleet:pool"] != testPool {
				t.Errorf("tags[runs-fleet:pool] = %q, want default", resp.Tags["runs-fleet:pool"])
			}
		})
	}
}

func managedInstanceOutput(id, pool string, state types.InstanceStateName) *ec2.DescribeInstancesOutput {
	return &ec2.DescribeInstancesOutput{
		Reservations: []types.Reservation{
			{Instances: []types.Instance{
				{
					InstanceId:        aws.String(id),
					InstanceType:      types.InstanceTypeC7gXlarge,
					State:             &types.InstanceState{Name: state},
					InstanceLifecycle: types.InstanceLifecycleTypeSpot,
					Tags: []types.Tag{
						{Key: aws.String("runs-fleet:pool"), Value: aws.String(pool)},
						{Key: aws.String("runs-fleet:managed"), Value: aws.String("true")},
					},
				},
			}},
		},
	}
}

func TestInstancesHandler_TerminateInstance(t *testing.T) {
	t.Parallel()

	const id = "i-0123456789abcdef0"
	activeJob := &events.JobInfo{JobID: 42, RunID: 99, Repo: "org/repo"}

	tests := []struct {
		name            string
		id              string
		query           string
		jobsTable       string
		output          *ec2.DescribeInstancesOutput
		ec2Err          error
		job             *events.JobInfo
		jobErr          error
		markErr         error
		terminateErr    error
		wantStatus      int
		wantTerminated  bool
		wantMarked      bool
		wantAuditResult string
	}{
		{
			name:            "idle instance terminates",
			id:              id,
			output:          managedInstanceOutput(id, testPool, types.InstanceStateNameRunning),
			wantStatus:      http.StatusOK,
			wantTerminated:  true,
			wantAuditResult: "success",
		},
		{
			name:            "active job refused without force",
			id:              id,
			output:          managedInstanceOutput(id, testPool, types.InstanceStateNameRunning),
			job:             activeJob,
			wantStatus:      http.StatusConflict,
			wantAuditResult: "denied",
		},
		{
			name:            "active job terminates with force",
			id:              id,
			query:           "?force=true",
			output:          managedInstanceOutput(id, testPool, types.InstanceStateNameRunning),
			job:             activeJob,
			wantStatus:      http.StatusOK,
			wantTerminated:  true,
			wantMarked:      true,
			wantAuditResult: "success",
		},
		{
			name:            "force on an idle instance does not mark any job",
			id:              id,
			query:           "?force=true",
			output:          managedInstanceOutput(id, testPool, types.InstanceStateNameRunning),
			wantStatus:      http.StatusOK,
			wantTerminated:  true,
			wantAuditResult: "success",
		},
		{
			name:       "invalid instance id",
			id:         "not-an-id",
			output:     &ec2.DescribeInstancesOutput{},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:            "unmanaged or absent",
			id:              id,
			output:          &ec2.DescribeInstancesOutput{},
			wantStatus:      http.StatusNotFound,
			wantAuditResult: "denied",
		},
		{
			name:            "aws reports instance not found",
			id:              id,
			ec2Err:          &smithy.GenericAPIError{Code: "InvalidInstanceID.NotFound"},
			wantStatus:      http.StatusNotFound,
			wantAuditResult: "denied",
		},
		{
			name:            "describe failure",
			id:              id,
			ec2Err:          errors.New("throttled"),
			wantStatus:      http.StatusInternalServerError,
			wantAuditResult: "error",
		},
		{
			name:            "job lookup failure fails closed",
			id:              id,
			output:          managedInstanceOutput(id, testPool, types.InstanceStateNameRunning),
			jobErr:          errors.New("dynamodb unavailable"),
			wantStatus:      http.StatusInternalServerError,
			wantAuditResult: "error",
		},
		{
			// The instance is gone either way; a stale job record is left for the
			// orphaned-jobs sweep rather than reported as a failed termination.
			name:            "mark terminating failure still reports success",
			id:              id,
			query:           "?force=true",
			output:          managedInstanceOutput(id, testPool, types.InstanceStateNameRunning),
			job:             activeJob,
			markErr:         errors.New("conditional check failed"),
			wantStatus:      http.StatusOK,
			wantTerminated:  true,
			wantMarked:      true,
			wantAuditResult: "success",
		},
		{
			name:            "terminate failure on a busy instance leaves the job unmarked",
			id:              id,
			query:           "?force=true",
			output:          managedInstanceOutput(id, testPool, types.InstanceStateNameRunning),
			job:             activeJob,
			terminateErr:    errors.New("UnauthorizedOperation"),
			wantStatus:      http.StatusInternalServerError,
			wantAuditResult: "error",
		},
		{
			name:            "terminate failure",
			id:              id,
			output:          managedInstanceOutput(id, testPool, types.InstanceStateNameRunning),
			terminateErr:    errors.New("UnauthorizedOperation"),
			wantStatus:      http.StatusInternalServerError,
			wantAuditResult: "error",
		},
		{
			name:       "jobs table unconfigured",
			id:         id,
			jobsTable:  "-",
			output:     managedInstanceOutput(id, testPool, types.InstanceStateNameRunning),
			wantStatus: http.StatusServiceUnavailable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ec2Mock := &mockEC2API{output: tt.output, err: tt.ec2Err, terminateErr: tt.terminateErr}
			dbMock := &mockInstancesDB{job: tt.job, jobErr: tt.jobErr, markErr: tt.markErr}
			jobsTable := testJobsTable
			if tt.jobsTable == "-" {
				jobsTable = ""
			}
			auditDB := &mockAuditDB{}
			handler := NewInstancesHandler(ec2Mock, dbMock, jobsTable, auditDB, &AuthMiddleware{requireAuth: false})

			mux := http.NewServeMux()
			handler.RegisterRoutes(mux)

			req := httptest.NewRequest("DELETE", "/api/instances/"+tt.id+tt.query, nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("got status %d, want %d: %s", rec.Code, tt.wantStatus, rec.Body.String())
			}

			if got := len(ec2Mock.terminatedIDs) > 0; got != tt.wantTerminated {
				t.Errorf("terminated = %v, want %v (ids %v)", got, tt.wantTerminated, ec2Mock.terminatedIDs)
			}
			if got := len(dbMock.markCalls) > 0; got != tt.wantMarked {
				t.Errorf("marked terminating = %v, want %v", got, tt.wantMarked)
			}

			// The 400 and 503 branches deliberately record nothing: no action was
			// attempted against a real target.
			if tt.wantAuditResult == "" {
				if len(auditDB.entries) != 0 {
					t.Errorf("recorded %d audit entries, want 0", len(auditDB.entries))
				}
			} else {
				if len(auditDB.entries) != 1 {
					t.Fatalf("recorded %d audit entries, want 1", len(auditDB.entries))
				}
				if got := auditDB.entries[0].Result; got != tt.wantAuditResult {
					t.Errorf("audit result = %q, want %q", got, tt.wantAuditResult)
				}
			}

			switch tt.wantStatus {
			case http.StatusConflict:
				var resp TerminateInstanceConflict
				if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
					t.Fatalf("decode: %v", err)
				}
				if resp.ActiveJob == nil {
					t.Fatal("active_job = nil, want the blocking job")
				}
				if resp.ActiveJob.JobID != 42 || resp.ActiveJob.RunID != 99 || resp.ActiveJob.Repo != "org/repo" {
					t.Errorf("active_job = %+v, want job 42 run 99 org/repo", *resp.ActiveJob)
				}
				if resp.Error == "" {
					t.Error("error = empty, want a message the UI can show")
				}
			case http.StatusOK:
				var resp TerminateInstanceResponse
				if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
					t.Fatalf("decode: %v", err)
				}
				if resp.InstanceID != tt.id {
					t.Errorf("instance_id = %q, want %q", resp.InstanceID, tt.id)
				}
				if resp.Pool != testPool {
					t.Errorf("pool = %q, want default", resp.Pool)
				}
				if resp.Forced != (tt.query != "") {
					t.Errorf("forced = %v, want %v", resp.Forced, tt.query != "")
				}
				if tt.wantMarked && resp.ActiveJob == nil {
					t.Error("active_job = nil, want the job that was killed")
				}
				if !tt.wantMarked && resp.ActiveJob != nil {
					t.Errorf("active_job = %+v, want nil", *resp.ActiveJob)
				}
			}
		})
	}
}

func TestTerminateInstance_CancelsSpotRequestBeforeTerminating(t *testing.T) {
	t.Parallel()

	const id = "i-0123456789abcdef0"

	tests := []struct {
		name          string
		spotRequests  []types.SpotInstanceRequest
		describeErr   error
		cancelErr     error
		wantCancelled []string
		wantCalls     []string
	}{
		{
			name:          "persistent request cancelled first",
			spotRequests:  []types.SpotInstanceRequest{{SpotInstanceRequestId: aws.String("sir-1")}},
			wantCancelled: []string{"sir-1"},
			wantCalls:     []string{"describe", "describe-spot", "cancel-spot", "terminate"},
		},
		{
			name:      "no request to cancel",
			wantCalls: []string{"describe", "describe-spot", "terminate"},
		},
		{
			name:        "describe failure still terminates",
			describeErr: errors.New("throttled"),
			wantCalls:   []string{"describe", "describe-spot", "terminate"},
		},
		{
			name:         "cancel failure still terminates",
			spotRequests: []types.SpotInstanceRequest{{SpotInstanceRequestId: aws.String("sir-1")}},
			cancelErr:    errors.New("throttled"),
			wantCalls:    []string{"describe", "describe-spot", "cancel-spot", "terminate"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ec2Mock := &mockEC2API{
				output:          managedInstanceOutput(id, testPool, types.InstanceStateNameRunning),
				spotRequests:    tt.spotRequests,
				describeSpotErr: tt.describeErr,
				cancelSpotErr:   tt.cancelErr,
			}
			handler := newTestInstancesHandler(ec2Mock, &mockInstancesDB{})

			mux := http.NewServeMux()
			handler.RegisterRoutes(mux)

			req := httptest.NewRequest("DELETE", "/api/instances/"+id, nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("got status %d, want 200: %s", rec.Code, rec.Body.String())
			}
			if got := len(ec2Mock.terminatedIDs); got != 1 {
				t.Errorf("terminated %d instances, want 1", got)
			}
			if len(ec2Mock.cancelledSpotIDs) != len(tt.wantCancelled) {
				t.Errorf("cancelled spot requests = %v, want %v", ec2Mock.cancelledSpotIDs, tt.wantCancelled)
			}
			if got := ec2Mock.calls; !slices.Equal(got, tt.wantCalls) {
				t.Errorf("call order = %v, want %v", got, tt.wantCalls)
			}
		})
	}
}

func TestTerminateInstance_Audit(t *testing.T) {
	t.Parallel()

	const id = "i-0123456789abcdef0"
	activeJob := &events.JobInfo{JobID: 42, RunID: 99, Repo: "org/repo"}

	tests := []struct {
		name         string
		query        string
		job          *events.JobInfo
		terminateErr error
		wantResult   string
		wantForced   bool
	}{
		{name: "success", wantResult: "success"},
		{name: "denied by active job", job: activeJob, wantResult: "denied"},
		{name: "forced success", query: "?force=true", job: activeJob, wantResult: "success", wantForced: true},
		{name: "terminate error", terminateErr: errors.New("boom"), wantResult: "error"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ec2Mock := &mockEC2API{
				output:       managedInstanceOutput(id, testPool, types.InstanceStateNameRunning),
				terminateErr: tt.terminateErr,
			}
			auditDB := &mockAuditDB{}
			handler := NewInstancesHandler(ec2Mock, &mockInstancesDB{job: tt.job}, testJobsTable, auditDB, &AuthMiddleware{requireAuth: false})

			mux := http.NewServeMux()
			handler.RegisterRoutes(mux)

			req := httptest.NewRequest("DELETE", "/api/instances/"+id+tt.query, nil)
			req = req.WithContext(context.WithValue(req.Context(), UserContextKey, UserInfo{Username: "carol"}))
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			if len(auditDB.entries) != 1 {
				t.Fatalf("recorded %d audit entries, want 1", len(auditDB.entries))
			}
			entry := auditDB.entries[0]
			if entry.User != "carol" {
				t.Errorf("user = %q, want carol", entry.User)
			}
			if entry.Action != "instance.terminate" {
				t.Errorf("action = %q, want instance.terminate", entry.Action)
			}
			if entry.Target != id {
				t.Errorf("target = %q, want %q", entry.Target, id)
			}
			if entry.Result != tt.wantResult {
				t.Errorf("result = %q, want %q", entry.Result, tt.wantResult)
			}
			if entry.Details["forced"] != tt.wantForced {
				t.Errorf("details[forced] = %v, want %v", entry.Details["forced"], tt.wantForced)
			}
			if entry.Details["pool"] != testPool {
				t.Errorf("details[pool] = %v, want default", entry.Details["pool"])
			}
			if tt.job != nil && entry.Details["job_id"] != int64(42) {
				t.Errorf("details[job_id] = %v, want 42", entry.Details["job_id"])
			}
		})
	}
}

func TestTerminateInstance_AuditPersistenceFailureDoesNotFailRequest(t *testing.T) {
	t.Parallel()

	const id = "i-0123456789abcdef0"
	ec2Mock := &mockEC2API{output: managedInstanceOutput(id, testPool, types.InstanceStateNameRunning)}
	auditDB := &mockAuditDB{err: errors.New("dynamodb down")}
	handler := NewInstancesHandler(ec2Mock, &mockInstancesDB{}, testJobsTable, auditDB, &AuthMiddleware{requireAuth: false})

	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest("DELETE", "/api/instances/"+id, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("got status %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if len(ec2Mock.terminatedIDs) != 1 {
		t.Errorf("terminated %d instances, want 1", len(ec2Mock.terminatedIDs))
	}
}

func TestTerminateInstance_SkipsPersistenceWhenTableUnset(t *testing.T) {
	t.Parallel()

	const id = "i-0123456789abcdef0"
	ec2Mock := &mockEC2API{output: managedInstanceOutput(id, testPool, types.InstanceStateNameRunning)}
	auditDB := &mockAuditDB{tableUnset: true}
	handler := NewInstancesHandler(ec2Mock, &mockInstancesDB{}, testJobsTable, auditDB, &AuthMiddleware{requireAuth: false})

	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest("DELETE", "/api/instances/"+id, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("got status %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if len(auditDB.entries) != 0 {
		t.Errorf("recorded %d audit entries, want 0", len(auditDB.entries))
	}
}
