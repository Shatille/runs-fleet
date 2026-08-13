package db

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func TestDeleteExpiredInstanceClaims(t *testing.T) {
	t.Parallel()

	now := time.Now()
	expired := fmt.Sprintf("%d", now.Add(-time.Hour).Unix())
	live := fmt.Sprintf("%d", now.Add(time.Hour).Unix())

	claim := func(id, expiry string) map[string]types.AttributeValue {
		return map[string]types.AttributeValue{
			"pool_name":    &types.AttributeValueMemberS{Value: instanceClaimPrefix + id},
			"claim_expiry": &types.AttributeValueMemberN{Value: expiry},
		}
	}

	const (
		keyExpired1 = instanceClaimPrefix + "i-expired-1"
		keyExpired2 = instanceClaimPrefix + "i-expired-2"
		keyRenewed  = instanceClaimPrefix + "i-renewed"
		keyLive     = instanceClaimPrefix + "i-live"
		keyPool     = "real-pool"
	)

	var scanCalls int
	var deleted []string
	mock := &MockDynamoDBAPI{
		ScanFunc: func(_ context.Context, params *dynamodb.ScanInput, _ ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
			scanCalls++
			if scanCalls == 1 {
				if params.ExclusiveStartKey != nil {
					t.Error("first page must not set ExclusiveStartKey")
				}
				const wantFilter = "begins_with(pool_name, :p) AND claim_expiry < :cutoff"
				if params.FilterExpression == nil || *params.FilterExpression != wantFilter {
					t.Errorf("Scan FilterExpression = %v, want %q", params.FilterExpression, wantFilter)
				}
				if v, ok := params.ExpressionAttributeValues[":p"].(*types.AttributeValueMemberS); !ok || v.Value != instanceClaimPrefix {
					t.Errorf("Scan :p = %#v, want %q", params.ExpressionAttributeValues[":p"], instanceClaimPrefix)
				}
				if v, ok := params.ExpressionAttributeValues[":cutoff"].(*types.AttributeValueMemberN); !ok || v.Value != fmt.Sprintf("%d", now.Unix()) {
					t.Errorf("Scan :cutoff = %#v, want %d", params.ExpressionAttributeValues[":cutoff"], now.Unix())
				}
				return &dynamodb.ScanOutput{
					Items: []map[string]types.AttributeValue{
						claim("i-expired-1", expired),
						claim("i-expired-2", expired),
						claim("i-live", live),
						{"pool_name": &types.AttributeValueMemberS{Value: keyPool}},
					},
					LastEvaluatedKey: map[string]types.AttributeValue{
						"pool_name": &types.AttributeValueMemberS{Value: keyExpired2},
					},
				}, nil
			}
			if params.ExclusiveStartKey == nil {
				t.Error("second page must carry ExclusiveStartKey")
			}
			return &dynamodb.ScanOutput{
				Items: []map[string]types.AttributeValue{claim("i-renewed", expired)},
			}, nil
		},
		DeleteItemFunc: func(_ context.Context, params *dynamodb.DeleteItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error) {
			keyAttr, ok := params.Key["pool_name"].(*types.AttributeValueMemberS)
			if !ok {
				t.Fatalf("DeleteItem key pool_name not a string: %#v", params.Key["pool_name"])
			}
			key := keyAttr.Value
			const wantCond = "claim_expiry < :cutoff"
			if params.ConditionExpression == nil || *params.ConditionExpression != wantCond {
				t.Errorf("DeleteItem for %q condition = %v, want %q", key, params.ConditionExpression, wantCond)
			}
			// Simulate the server-side guard: the live claim, the pool row (no
			// claim_expiry), and a claim renewed between scan and delete all fail
			// the claim_expiry < :cutoff condition, so they must be skipped, not counted.
			switch key {
			case keyLive, keyPool, keyRenewed:
				return nil, &types.ConditionalCheckFailedException{Message: nil}
			}
			deleted = append(deleted, key)
			return &dynamodb.DeleteItemOutput{}, nil
		},
	}

	client := &Client{dynamoClient: mock, poolsTable: testPoolsTable}

	count, err := client.DeleteExpiredInstanceClaims(context.Background(), now)
	if err != nil {
		t.Fatalf("DeleteExpiredInstanceClaims() error = %v", err)
	}
	if scanCalls != 2 {
		t.Fatalf("expected 2 scan pages, got %d", scanCalls)
	}
	if count != 2 {
		t.Fatalf("deleted count = %d, want 2", count)
	}
	wantDeleted := map[string]bool{keyExpired1: true, keyExpired2: true}
	if len(deleted) != len(wantDeleted) {
		t.Fatalf("deleted keys = %v, want %v", deleted, wantDeleted)
	}
	for _, k := range deleted {
		if !wantDeleted[k] {
			t.Errorf("unexpectedly deleted %q (live claim, pool, and renewed claim must be untouched)", k)
		}
	}
}

func TestDeleteExpiredInstanceClaims_NoTable(t *testing.T) {
	t.Parallel()

	client := &Client{dynamoClient: &MockDynamoDBAPI{}, poolsTable: ""}
	if _, err := client.DeleteExpiredInstanceClaims(context.Background(), time.Now()); err == nil {
		t.Error("DeleteExpiredInstanceClaims() should error when pools table not configured")
	}
}

func TestDeleteExpiredInstanceClaims_ScanError(t *testing.T) {
	t.Parallel()

	mock := &MockDynamoDBAPI{
		ScanFunc: func(_ context.Context, _ *dynamodb.ScanInput, _ ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
			return nil, errors.New("dynamodb scan error")
		},
	}
	client := &Client{dynamoClient: mock, poolsTable: testPoolsTable}
	if _, err := client.DeleteExpiredInstanceClaims(context.Background(), time.Now()); err == nil {
		t.Error("DeleteExpiredInstanceClaims() should propagate scan error")
	}
}

func TestDeleteExpiredInstanceClaims_DeleteError(t *testing.T) {
	t.Parallel()

	now := time.Now()
	mock := &MockDynamoDBAPI{
		ScanFunc: func(_ context.Context, _ *dynamodb.ScanInput, _ ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
			return &dynamodb.ScanOutput{
				Items: []map[string]types.AttributeValue{
					{
						"pool_name":    &types.AttributeValueMemberS{Value: instanceClaimPrefix + "i-1"},
						"claim_expiry": &types.AttributeValueMemberN{Value: fmt.Sprintf("%d", now.Add(-time.Hour).Unix())},
					},
				},
			}, nil
		},
		DeleteItemFunc: func(_ context.Context, _ *dynamodb.DeleteItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error) {
			return nil, errors.New("dynamodb delete error")
		},
	}
	client := &Client{dynamoClient: mock, poolsTable: testPoolsTable}
	if _, err := client.DeleteExpiredInstanceClaims(context.Background(), now); err == nil {
		t.Error("DeleteExpiredInstanceClaims() should propagate non-conditional delete error")
	}
}
