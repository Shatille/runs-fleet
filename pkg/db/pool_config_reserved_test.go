package db

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// TestIsReservedPoolKeyCoversEverySentinelPrefix fails when a sentinel prefix is
// declared without being taught to IsReservedPoolKey. Prefixes are declared in
// per-feature files while the predicate lives here, and twice now a new record
// kind has leaked into ListPools and rendered as a phantom pool.
func TestIsReservedPoolKeyCoversEverySentinelPrefix(t *testing.T) {
	t.Parallel()

	sources, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob package sources: %v", err)
	}

	fset := token.NewFileSet()
	found := false
	for _, path := range sources {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			spec, ok := n.(*ast.ValueSpec)
			if !ok {
				return true
			}
			for _, v := range spec.Values {
				lit, ok := v.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				prefix, err := strconv.Unquote(lit.Value)
				if err != nil || !strings.HasPrefix(prefix, "__") {
					continue
				}
				found = true
				if !IsReservedPoolKey(prefix + "probe") {
					t.Errorf("sentinel prefix %q (%s) is not filtered by IsReservedPoolKey; "+
						"rows keyed with it will surface as phantom pools", prefix, path)
				}
			}
			return true
		})
	}

	if !found {
		t.Fatal("no sentinel prefixes discovered; the scan is no longer finding declarations")
	}
}

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
		{"runner sighting", runnerSightingKey("devsisters/llm-gateway", 12431), true},
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
						poolItem(runnerSightingKey("devsisters/llm-gateway", 12431)),
						poolItem(runnerSightingKey("devsisters/cs-ai", 216)),
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

func TestListPoolsPaginates(t *testing.T) {
	t.Parallel()

	poolItem := func(name string) map[string]types.AttributeValue {
		return map[string]types.AttributeValue{"pool_name": &types.AttributeValueMemberS{Value: name}}
	}

	var calls int
	client := &Client{
		poolsTable: testPoolsTable,
		dynamoClient: &MockDynamoDBAPI{
			ScanFunc: func(_ context.Context, params *dynamodb.ScanInput, _ ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
				calls++
				if calls == 1 {
					if params.ExclusiveStartKey != nil {
						t.Error("first page must not set ExclusiveStartKey")
					}
					return &dynamodb.ScanOutput{
						Items: []map[string]types.AttributeValue{
							poolItem(testPoolDefault),
							poolItem(instanceClaimPrefix + "i-0abc123"),
						},
						LastEvaluatedKey: poolItem(testPoolDefault),
					}, nil
				}
				if params.ExclusiveStartKey == nil {
					t.Error("second page must carry ExclusiveStartKey")
				}
				return &dynamodb.ScanOutput{
					Items: []map[string]types.AttributeValue{poolItem("ci-arm64")},
				}, nil
			},
		},
	}

	pools, err := client.ListPools(context.Background())
	if err != nil {
		t.Fatalf("ListPools() error = %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected 2 scan pages, got %d", calls)
	}

	want := []string{testPoolDefault, "ci-arm64"}
	if len(pools) != len(want) {
		t.Fatalf("ListPools() = %v, want %v (both pages, reserved key dropped)", pools, want)
	}
	for i, p := range want {
		if pools[i] != p {
			t.Errorf("ListPools()[%d] = %q, want %q", i, pools[i], p)
		}
	}
}

func TestGetPoolConfigReservedKeyShortCircuits(t *testing.T) {
	t.Parallel()

	for _, key := range []string{
		taskLockPrefix + "pool_audit",
		instanceClaimPrefix + "i-0abc123",
		runnerSightingKey("devsisters/llm-gateway", 12431),
	} {
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
