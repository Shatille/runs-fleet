package db

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// OriginalLabel must survive a SaveJob -> read round-trip. GitHub dispatches by
// exact label-set membership, so a requeue that rebuilds its launch message from
// this record and cannot read the label registers the runner under the
// synthesized legacy form, which the job's runs-on can never match.
func TestSaveJob_OriginalLabelRoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		label string
	}{
		{name: "requested label persists", label: "runs-fleet/cpu=2/pool=lingua-franca"},
		{name: "absent label omits attribute (inert)", label: ""},
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
				JobID:         12345,
				RunID:         67890,
				InstanceID:    "i-label",
				InstanceType:  "c7g.xlarge",
				Pool:          "lingua-franca",
				OriginalLabel: tt.label,
			})
			if err != nil {
				t.Fatalf("SaveJob() error = %v", err)
			}

			// omitempty: an empty label must not be written at all, so records
			// written before this field existed read back identically.
			_, present := savedItem["original_label"]
			if present != (tt.label != "") {
				t.Errorf("original_label attribute present = %v, want %v", present, tt.label != "")
			}

			info, err := client.GetJobByInstance(context.Background(), "i-label")
			if err != nil {
				t.Fatalf("GetJobByInstance() error = %v", err)
			}
			if info == nil {
				t.Fatal("GetJobByInstance() returned nil job")
			}
			if info.OriginalLabel != tt.label {
				t.Errorf("round-tripped OriginalLabel = %q, want %q", info.OriginalLabel, tt.label)
			}
		})
	}
}
