package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"
)

type mockEC2API struct {
	output *ec2.DescribeInstancesOutput
	err    error
}

func (m *mockEC2API) DescribeInstances(_ context.Context, _ *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	return m.output, m.err
}

type mockInstancesDB struct {
	busyIDs map[string][]string
}

func (m *mockInstancesDB) GetPoolBusyInstanceIDs(_ context.Context, poolName string) ([]string, error) {
	return m.busyIDs[poolName], nil
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
									{Key: aws.String("runs-fleet:pool"), Value: aws.String("default")},
									{Key: aws.String("runs-fleet:managed"), Value: aws.String("true")},
								},
							},
							{
								InstanceId:   aws.String("i-def456"),
								InstanceType: types.InstanceTypeC7gXlarge,
								State:        &types.InstanceState{Name: types.InstanceStateNameStopped},
								Tags: []types.Tag{
									{Key: aws.String("runs-fleet:pool"), Value: aws.String("default")},
									{Key: aws.String("runs-fleet:managed"), Value: aws.String("true")},
								},
							},
						},
					},
				},
			},
			busyIDs:        map[string][]string{"default": {"i-abc123"}},
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
									{Key: aws.String("runs-fleet:pool"), Value: aws.String("default")},
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
			auth := &AuthMiddleware{requireAuth: false}
			handler := NewInstancesHandler(ec2Mock, dbMock, auth)

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
			auth := &AuthMiddleware{requireAuth: false}
			handler := NewInstancesHandler(ec2Mock, dbMock, auth)

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
						{Key: aws.String("runs-fleet:pool"), Value: aws.String("default")},
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
		{name: "found and busy", id: "i-0123456789abcdef0", output: found, busyIDs: map[string][]string{"default": {"i-0123456789abcdef0"}}, wantStatus: http.StatusOK, wantBusy: true},
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
			handler := NewInstancesHandler(ec2Mock, dbMock, &AuthMiddleware{requireAuth: false})

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
			if resp.Tags["runs-fleet:pool"] != "default" {
				t.Errorf("tags[runs-fleet:pool] = %q, want default", resp.Tags["runs-fleet:pool"])
			}
		})
	}
}
