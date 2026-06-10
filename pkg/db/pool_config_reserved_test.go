package db

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func TestIsReservedPoolKey(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		key  string
		want bool
	}{
		{"real pool", testPoolDefault, false},
		{"real pool with separators", "ci-arm64_pool", false},
		{"task lock", taskLockPrefix + "pool_audit", true},
		{"instance claim", instanceClaimPrefix + "i-0abc123def456", true},
		{"empty", "", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := IsReservedPoolKey(tc.key); got != tc.want {
				t.Errorf("IsReservedPoolKey(%q) = %v, want %v", tc.key, got, tc.want)
			}
		})
	}
}

func TestListPoolsExcludesReservedKeys(t *testing.T) {
	t.Parallel()

	poolItem := func(name string) map[string]types.AttributeValue {
		return map[string]types.AttributeValue{"pool_name": &types.AttributeValueMemberS{Value: name}}
	}
	client := &Client{
		poolsTable: testPoolsTable,
		dynamoClient: &MockDynamoDBAPI{
			ScanFunc: func(_ context.Context, _ *dynamodb.ScanInput, _ ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
				return &dynamodb.ScanOutput{
					Items: []map[string]types.AttributeValue{
						poolItem(testPoolDefault),
						poolItem(taskLockPrefix + "pool_audit"),
						poolItem(instanceClaimPrefix + "i-0abc123"),
						poolItem(instanceClaimPrefix + "i-0def456"),
						poolItem("ci-arm64"),
					},
				}, nil
			},
		},
	}

	pools, err := client.ListPools(context.Background())
	if err != nil {
		t.Fatalf("ListPools() error = %v", err)
	}

	want := []string{testPoolDefault, "ci-arm64"}
	if len(pools) != len(want) {
		t.Fatalf("ListPools() = %v, want %v", pools, want)
	}
	for i, p := range want {
		if pools[i] != p {
			t.Errorf("ListPools()[%d] = %q, want %q", i, pools[i], p)
		}
	}
}

func TestGetPoolConfigReservedKeyShortCircuits(t *testing.T) {
	t.Parallel()

	for _, key := range []string{taskLockPrefix + "pool_audit", instanceClaimPrefix + "i-0abc123"} {
		client := &Client{
			poolsTable: testPoolsTable,
			dynamoClient: &MockDynamoDBAPI{
				GetItemFunc: func(_ context.Context, _ *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
					t.Errorf("GetItem must not be called for reserved key %q", key)
					return &dynamodb.GetItemOutput{}, nil
				},
			},
		}

		cfg, err := client.GetPoolConfig(context.Background(), key)
		if err != nil {
			t.Fatalf("GetPoolConfig(%q) error = %v", key, err)
		}
		if cfg != nil {
			t.Errorf("GetPoolConfig(%q) = %+v, want nil", key, cfg)
		}
	}
}
