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
