// Package db provides DynamoDB client operations for pool configuration and state management.
package db

import (
	"context"

	"github.com/Shavakan/runs-fleet/pkg/logging"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
)

var dbLog = logging.WithComponent(logging.LogTypeDB, "dynamo")

// DynamoDBAPI defines DynamoDB operations for pool configuration storage.
//
//nolint:dupl // Mock struct in test file mirrors this interface - intentional pattern
type DynamoDBAPI interface {
	GetItem(ctx context.Context, params *dynamodb.GetItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
	UpdateItem(ctx context.Context, params *dynamodb.UpdateItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error)
	Scan(ctx context.Context, params *dynamodb.ScanInput, optFns ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error)
	Query(ctx context.Context, params *dynamodb.QueryInput, optFns ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error)
	PutItem(ctx context.Context, params *dynamodb.PutItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error)
	DeleteItem(ctx context.Context, params *dynamodb.DeleteItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error)
}

// Client provides DynamoDB operations for pool configuration and state.
type Client struct {
	dynamoClient DynamoDBAPI
	poolsTable   string
	jobsTable    string
}

// NewClient creates DynamoDB client for specified pools and jobs tables.
func NewClient(cfg aws.Config, poolsTable, jobsTable string) *Client {
	return &Client{
		dynamoClient: dynamodb.NewFromConfig(cfg),
		poolsTable:   poolsTable,
		jobsTable:    jobsTable,
	}
}

// HasJobsTable returns true if the jobs table is configured.
// Use this to check before calling SaveJob or other job-related methods.
func (c *Client) HasJobsTable() bool {
	return c.jobsTable != ""
}
