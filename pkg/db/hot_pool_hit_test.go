package db

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// HotPoolHit must survive a SaveJob -> read round-trip: the flag is written to
// the jobs table and reconstructed on GetJobByInstance so the termination
// handler can attribute the provision-latency metric to source=hot_pool.
func TestSaveJob_HotPoolHitRoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		hot  bool
	}{
		{name: "hot pool hit persists", hot: true},
		{name: "non-hot omits attribute (inert)", hot: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var savedItem map[string]types.AttributeValue
			mockDB := &MockDynamoDBAPI{
				PutItemFunc: func(_ context.Context, params *dynamodb.PutItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
					savedItem = params.Item
					return &dynamodb.PutItemOutput{}, nil
				},
				ScanFunc: func(_ context.Context, _ *dynamodb.ScanInput, _ ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
					return &dynamodb.ScanOutput{Items: []map[string]types.AttributeValue{savedItem}}, nil
				},
			}
			client := &Client{dynamoClient: mockDB, jobsTable: "jobs-table"}

			err := client.SaveJob(context.Background(), &JobRecord{
				JobID:        12345,
				RunID:        67890,
				InstanceID:   "i-hot",
				InstanceType: "c7g.xlarge",
				Pool:         "hot",
				WarmPoolHit:  true,
				HotPoolHit:   tt.hot,
			})
			if err != nil {
				t.Fatalf("SaveJob() error = %v", err)
			}

			// omitempty: a false HotPoolHit must not be written at all, so an old
			// reader is unaffected and the attribute is inert when the feature is off.
			_, present := savedItem["hot_pool_hit"]
			if present != tt.hot {
				t.Errorf("hot_pool_hit attribute present = %v, want %v", present, tt.hot)
			}

			info, err := client.GetJobByInstance(context.Background(), "i-hot")
			if err != nil {
				t.Fatalf("GetJobByInstance() error = %v", err)
			}
			if info == nil {
				t.Fatal("GetJobByInstance() returned nil job")
			}
			if info.HotPoolHit != tt.hot {
				t.Errorf("round-tripped HotPoolHit = %v, want %v", info.HotPoolHit, tt.hot)
			}
		})
	}
}
