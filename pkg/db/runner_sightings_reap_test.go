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

func TestDeleteStaleRunnerSightings(t *testing.T) {
	t.Parallel()

	now := time.Now()
	stale := fmt.Sprintf("%d", now.Add(-8*24*time.Hour).Unix())
	fresh := fmt.Sprintf("%d", now.Add(-time.Hour).Unix())
	cutoff := now.Add(-runnerSightingTTL).Unix()

	sighting := func(repo string, id int64, seen string) map[string]types.AttributeValue {
		return map[string]types.AttributeValue{
			"pool_name":          &types.AttributeValueMemberS{Value: runnerSightingKey(repo, id)},
			"first_seen_offline": &types.AttributeValueMemberN{Value: seen},
		}
	}

	keyStale1 := runnerSightingKey("devsisters/llm-gateway", 12431)
	keyStale2 := runnerSightingKey("devsisters/cs-ai", 216)
	keyFresh := runnerSightingKey("devsisters/cc-server", 2324)

	var scanCalls int
	var deleted []string
	mock := &MockDynamoDBAPI{
		ScanFunc: func(_ context.Context, params *dynamodb.ScanInput, _ ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
			scanCalls++
			if scanCalls == 1 {
				if params.ExclusiveStartKey != nil {
					t.Error("first page must not set ExclusiveStartKey")
				}
				const wantFilter = "begins_with(pool_name, :p) AND first_seen_offline < :cutoff"
				if params.FilterExpression == nil || *params.FilterExpression != wantFilter {
					t.Errorf("Scan FilterExpression = %v, want %q", params.FilterExpression, wantFilter)
				}
				if v, ok := params.ExpressionAttributeValues[":p"].(*types.AttributeValueMemberS); !ok || v.Value != runnerSightingPrefix {
					t.Errorf("Scan :p = %#v, want %q", params.ExpressionAttributeValues[":p"], runnerSightingPrefix)
				}
				if v, ok := params.ExpressionAttributeValues[":cutoff"].(*types.AttributeValueMemberN); !ok || v.Value != fmt.Sprintf("%d", cutoff) {
					t.Errorf("Scan :cutoff = %#v, want %d", params.ExpressionAttributeValues[":cutoff"], cutoff)
				}
				return &dynamodb.ScanOutput{
					Items: []map[string]types.AttributeValue{
						sighting("devsisters/llm-gateway", 12431, stale),
						sighting("devsisters/cs-ai", 216, stale),
					},
					LastEvaluatedKey: map[string]types.AttributeValue{
						"pool_name": &types.AttributeValueMemberS{Value: keyStale2},
					},
				}, nil
			}
			if params.ExclusiveStartKey == nil {
				t.Error("second page must carry ExclusiveStartKey")
			}
			return &dynamodb.ScanOutput{
				Items: []map[string]types.AttributeValue{
					sighting("devsisters/cc-server", 2324, fresh),
				},
			}, nil
		},
		DeleteItemFunc: func(_ context.Context, params *dynamodb.DeleteItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error) {
			key, ok := params.Key["pool_name"].(*types.AttributeValueMemberS)
			if !ok {
				t.Fatal("DeleteItem key must be a string pool_name")
			}
			const wantCond = "first_seen_offline < :cutoff"
			if params.ConditionExpression == nil || *params.ConditionExpression != wantCond {
				t.Errorf("DeleteItem ConditionExpression = %v, want %q", params.ConditionExpression, wantCond)
			}
			// A runner that came back online between the scan and the delete has
			// had its sighting rewritten; the condition rejects it.
			if key.Value == keyFresh {
				return nil, &types.ConditionalCheckFailedException{}
			}
			deleted = append(deleted, key.Value)
			return &dynamodb.DeleteItemOutput{}, nil
		},
	}

	client := &Client{poolsTable: testPoolsTable, dynamoClient: mock}

	count, err := client.DeleteStaleRunnerSightings(context.Background(), now)
	if err != nil {
		t.Fatalf("DeleteStaleRunnerSightings() error = %v", err)
	}
	if scanCalls != 2 {
		t.Errorf("expected 2 scan pages, got %d", scanCalls)
	}
	if count != 2 {
		t.Errorf("DeleteStaleRunnerSightings() = %d, want 2", count)
	}
	if len(deleted) != 2 || deleted[0] != keyStale1 || deleted[1] != keyStale2 {
		t.Errorf("deleted = %v, want [%s %s]", deleted, keyStale1, keyStale2)
	}
}

func TestDeleteStaleRunnerSightingsNoTable(t *testing.T) {
	t.Parallel()

	client := &Client{dynamoClient: &MockDynamoDBAPI{}}
	if _, err := client.DeleteStaleRunnerSightings(context.Background(), time.Now()); err == nil {
		t.Error("expected error when pools table is not configured")
	}
}

func TestDeleteStaleRunnerSightingsScanError(t *testing.T) {
	t.Parallel()

	client := &Client{
		poolsTable: testPoolsTable,
		dynamoClient: &MockDynamoDBAPI{
			ScanFunc: func(_ context.Context, _ *dynamodb.ScanInput, _ ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
				return nil, errors.New("scan boom")
			},
		},
	}

	if _, err := client.DeleteStaleRunnerSightings(context.Background(), time.Now()); err == nil {
		t.Error("expected scan error to propagate")
	}
}

func TestDeleteStaleRunnerSightingsDeleteError(t *testing.T) {
	t.Parallel()

	now := time.Now()
	client := &Client{
		poolsTable: testPoolsTable,
		dynamoClient: &MockDynamoDBAPI{
			ScanFunc: func(_ context.Context, _ *dynamodb.ScanInput, _ ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
				return &dynamodb.ScanOutput{
					Items: []map[string]types.AttributeValue{
						{"pool_name": &types.AttributeValueMemberS{Value: runnerSightingKey("devsisters/llm-gateway", 1)}},
					},
				}, nil
			},
			DeleteItemFunc: func(_ context.Context, _ *dynamodb.DeleteItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error) {
				return nil, errors.New("delete boom")
			},
		},
	}

	if _, err := client.DeleteStaleRunnerSightings(context.Background(), now); err == nil {
		t.Error("expected delete error to propagate")
	}
}
