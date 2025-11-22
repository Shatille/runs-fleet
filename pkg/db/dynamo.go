// Package db provides DynamoDB client operations for pool configuration and state management.
package db

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
)

type DynamoDBAPI interface {
	GetItem(ctx context.Context, params *dynamodb.GetItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
	UpdateItem(ctx context.Context, params *dynamodb.UpdateItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error)
}

type Client struct {
	dynamoClient DynamoDBAPI
	poolsTable   string
}

func NewClient(cfg aws.Config, poolsTable string) *Client {
	return &Client{
		dynamoClient: dynamodb.NewFromConfig(cfg),
		poolsTable:   poolsTable,
	}
}

type PoolConfig struct {
	PoolName       string `dynamodbav:"pool_name"`
	InstanceType   string `dynamodbav:"instance_type"`
	DesiredRunning int    `dynamodbav:"desired_running"`
	DesiredStopped int    `dynamodbav:"desired_stopped"`
}

func (c *Client) GetPoolConfig(ctx context.Context, poolName string) (*PoolConfig, error) {
	key, err := attributevalue.MarshalMap(map[string]string{
		"pool_name": poolName,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal key: %w", err)
	}

	output, err := c.dynamoClient.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(c.poolsTable),
		Key:       key,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get item: %w", err)
	}

	if output.Item == nil {
		return nil, nil // Not found
	}

	var config PoolConfig
	if err := attributevalue.UnmarshalMap(output.Item, &config); err != nil {
		return nil, fmt.Errorf("failed to unmarshal item: %w", err)
	}

	return &config, nil
}

// UpdatePoolState updates the current state of the pool (e.g., running/stopped counts)
// Note: In a real implementation, this might be more complex or use a separate table/field
func (c *Client) UpdatePoolState(ctx context.Context, poolName string, running, stopped int) error {
	// For MVP, we might just log this or update a status field
	// Here we assume there are fields 'current_running' and 'current_stopped' in the same table
	key, err := attributevalue.MarshalMap(map[string]string{
		"pool_name": poolName,
	})
	if err != nil {
		return fmt.Errorf("failed to marshal key: %w", err)
	}

	update := "SET current_running = :r, current_stopped = :s"
	exprValues, err := attributevalue.MarshalMap(map[string]int{
		":r": running,
		":s": stopped,
	})
	if err != nil {
		return fmt.Errorf("failed to marshal values: %w", err)
	}

	_, err = c.dynamoClient.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:                 aws.String(c.poolsTable),
		Key:                       key,
		UpdateExpression:          aws.String(update),
		ExpressionAttributeValues: exprValues,
		ConditionExpression:       aws.String("attribute_exists(pool_name)"),
	})
	if err != nil {
		return fmt.Errorf("failed to update item: %w", err)
	}

	return nil
}
