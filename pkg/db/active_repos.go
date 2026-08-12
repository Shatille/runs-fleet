package db

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// activeRepoWindow bounds how far back ListActiveRepos looks. A DynamoDB filter
// runs after the read, so this does not reduce the Scan's own cost; what it cuts
// is the repo set, and with it the per-repo GitHub API calls the sweep makes for
// repos that have dispatched nothing recently.
const activeRepoWindow = 7 * 24 * time.Hour

// ListActiveRepos returns the distinct repos that have dispatched a job within
// activeRepoWindow.
//
// The runners API is per-repo, so a sweep needs the set this fleet actually
// serves. Job records are the only record of that, and a repo that stops using
// runs-fleet drops off on its own once its recent records age out.
func (c *Client) ListActiveRepos(ctx context.Context) ([]string, error) {
	if c.jobsTable == "" {
		return nil, nil
	}

	cutoff := time.Now().Add(-activeRepoWindow).Format(time.RFC3339)
	seen := make(map[string]struct{})
	var lastKey map[string]types.AttributeValue
	for {
		out, err := c.dynamoClient.Scan(ctx, &dynamodb.ScanInput{
			TableName:            aws.String(c.jobsTable),
			ProjectionExpression: aws.String("repo"),
			FilterExpression:     aws.String("created_at > :cutoff"),
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":cutoff": &types.AttributeValueMemberS{Value: cutoff},
			},
			ExclusiveStartKey: lastKey,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to scan jobs for repos: %w", err)
		}
		for _, item := range out.Items {
			v, ok := item["repo"].(*types.AttributeValueMemberS)
			if !ok || v.Value == "" {
				continue
			}
			seen[v.Value] = struct{}{}
		}
		lastKey = out.LastEvaluatedKey
		if lastKey == nil {
			break
		}
	}

	repos := make([]string, 0, len(seen))
	for r := range seen {
		repos = append(repos, r)
	}
	return repos, nil
}
