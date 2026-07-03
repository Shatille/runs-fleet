package db

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/oklog/ulid/v2"
)

// userTimestampIndexName is the GSI (hash: user, range: timestamp) the audit
// table is provisioned with -- required by schema, not optional like the
// jobs table's GSIs, so there is no setter to configure it.
const userTimestampIndexName = "user-index"

const auditTimestampAttr = "timestamp"

// auditRetention is how long an audit entry survives before DynamoDB TTL
// reaps it. 90 days matches the plan's compliance/ops-review window without
// keeping the table growing unbounded.
const auditRetention = 90 * 24 * time.Hour

// auditRecord represents an audit log entry stored in DynamoDB.
// Primary key is id (ULID, time-sortable); GSI user-index on (user, timestamp).
type auditRecord struct {
	ID        string         `dynamodbav:"id"`
	User      string         `dynamodbav:"user"`
	Action    string         `dynamodbav:"action"`
	Target    string         `dynamodbav:"target,omitempty"`
	Result    string         `dynamodbav:"result"`
	Details   map[string]any `dynamodbav:"details,omitempty"`
	ClientIP  string         `dynamodbav:"client_ip,omitempty"`
	Timestamp string         `dynamodbav:"timestamp"`
	TTL       int64          `dynamodbav:"ttl"`
}

// AuditEntry describes an admin action, either to record (ID/Timestamp
// ignored on write, generated internally) or as returned by ListAuditLogs.
type AuditEntry struct {
	ID        string
	User      string
	Action    string
	Target    string
	Result    string
	Details   map[string]any
	ClientIP  string
	Timestamp time.Time
}

// AuditFilter narrows ListAuditLogs results. Zero-value fields are unfiltered.
type AuditFilter struct {
	User   string
	Action string
	Since  time.Time
	Until  time.Time
	Limit  int
	Offset int
}

// RecordAudit persists an audit log entry. The id and timestamp are
// generated internally so callers never fabricate them.
func (c *Client) RecordAudit(ctx context.Context, entry AuditEntry) error {
	if c.auditTable == "" {
		return fmt.Errorf("audit table not configured")
	}

	user := entry.User
	if user == "" {
		user = "anonymous"
	}

	now := time.Now()
	record := auditRecord{
		ID:        ulid.Make().String(),
		User:      user,
		Action:    entry.Action,
		Target:    entry.Target,
		Result:    entry.Result,
		Details:   entry.Details,
		ClientIP:  entry.ClientIP,
		Timestamp: now.Format(time.RFC3339),
		TTL:       now.Add(auditRetention).Unix(),
	}

	item, err := attributevalue.MarshalMap(record)
	if err != nil {
		return fmt.Errorf("failed to marshal audit record: %w", err)
	}

	_, err = c.dynamoClient.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(c.auditTable),
		Item:      item,
	})
	if err != nil {
		return fmt.Errorf("failed to save audit record: %w", err)
	}

	return nil
}

// ListAuditLogs returns audit entries matching filter, newest first is not
// guaranteed (DynamoDB does not sort Scan results; callers needing strict
// ordering should filter by User to take the sorted GSI path).
func (c *Client) ListAuditLogs(ctx context.Context, filter AuditFilter) ([]AuditEntry, error) {
	if c.auditTable == "" {
		return nil, fmt.Errorf("audit table not configured")
	}

	if filter.User != "" {
		entries, err := c.listAuditLogsViaGSI(ctx, filter)
		if err == nil {
			return entries, nil
		}
		if !isGSIValidationError(err) {
			return nil, err
		}
		dbLog.Warn(ctx, "GSI query failed, falling back to scan",
			"gsi", userTimestampIndexName,
			"error", err,
		)
	}

	return c.listAuditLogsViaScan(ctx, filter)
}

func (c *Client) listAuditLogsViaGSI(ctx context.Context, filter AuditFilter) ([]AuditEntry, error) {
	names := map[string]string{"#user": "user"}
	values := map[string]types.AttributeValue{
		":user": &types.AttributeValueMemberS{Value: filter.User},
	}
	keyCondition := "#user = :user"
	if !filter.Since.IsZero() {
		names["#"+auditTimestampAttr] = auditTimestampAttr
		values[":since"] = &types.AttributeValueMemberS{Value: filter.Since.Format(time.RFC3339)}
		keyCondition += " AND #" + auditTimestampAttr + " >= :since"
	}

	input := &dynamodb.QueryInput{
		TableName:              aws.String(c.auditTable),
		IndexName:              aws.String(userTimestampIndexName),
		KeyConditionExpression: aws.String(keyCondition),
	}
	// action/until aren't part of the key condition (the GSI is keyed on
	// user+timestamp only), so they're applied as a post-query filter.
	filterExpr, filterClauses := auditFilterClauses(filter, false)
	for k, v := range filterClauses.names {
		names[k] = v
	}
	for k, v := range filterClauses.values {
		values[k] = v
	}
	if filterExpr != "" {
		input.FilterExpression = aws.String(filterExpr)
	}
	input.ExpressionAttributeNames = names
	input.ExpressionAttributeValues = values

	output, err := c.dynamoClient.Query(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("failed to query GSI: %w", err)
	}

	return paginateAuditEntries(parseAuditItems(output.Items), filter), nil
}

func (c *Client) listAuditLogsViaScan(ctx context.Context, filter AuditFilter) ([]AuditEntry, error) {
	input := &dynamodb.ScanInput{
		TableName: aws.String(c.auditTable),
	}
	filterExpr, clauses := auditFilterClauses(filter, true)
	if filterExpr != "" {
		input.FilterExpression = aws.String(filterExpr)
		input.ExpressionAttributeNames = clauses.names
		input.ExpressionAttributeValues = clauses.values
	}

	var entries []AuditEntry
	var lastEvaluatedKey map[string]types.AttributeValue
	for {
		input.ExclusiveStartKey = lastEvaluatedKey

		output, err := c.dynamoClient.Scan(ctx, input)
		if err != nil {
			return nil, fmt.Errorf("failed to scan audit logs: %w", err)
		}

		entries = append(entries, parseAuditItems(output.Items)...)

		lastEvaluatedKey = output.LastEvaluatedKey
		if lastEvaluatedKey == nil {
			break
		}
	}

	return paginateAuditEntries(entries, filter), nil
}

type auditExpressionAttrs struct {
	names  map[string]string
	values map[string]types.AttributeValue
}

// auditFilterClauses builds a FilterExpression for action/since/until,
// shared by both the GSI query and the scan fallback. includeUser adds a
// bare user filter clause for the scan path (no GSI available there); the
// GSI path already binds :user in its key condition, so it passes false.
func auditFilterClauses(filter AuditFilter, includeUser bool) (string, auditExpressionAttrs) {
	attrs := auditExpressionAttrs{names: map[string]string{}, values: map[string]types.AttributeValue{}}
	var clauses []string

	if includeUser && filter.User != "" {
		attrs.names["#user"] = "user"
		attrs.values[":user"] = &types.AttributeValueMemberS{Value: filter.User}
		clauses = append(clauses, "#user = :user")
	}
	if filter.Action != "" {
		attrs.names["#action"] = "action"
		attrs.values[":action"] = &types.AttributeValueMemberS{Value: filter.Action}
		clauses = append(clauses, "#action = :action")
	}
	if !filter.Since.IsZero() {
		attrs.names["#"+auditTimestampAttr] = auditTimestampAttr
		attrs.values[":since"] = &types.AttributeValueMemberS{Value: filter.Since.Format(time.RFC3339)}
		clauses = append(clauses, "#"+auditTimestampAttr+" >= :since")
	}
	if !filter.Until.IsZero() {
		attrs.names["#"+auditTimestampAttr] = auditTimestampAttr
		attrs.values[":until"] = &types.AttributeValueMemberS{Value: filter.Until.Format(time.RFC3339)}
		clauses = append(clauses, "#"+auditTimestampAttr+" <= :until")
	}

	if len(clauses) == 0 {
		return "", attrs
	}

	filterExpr := clauses[0]
	for _, clause := range clauses[1:] {
		filterExpr += " AND " + clause
	}
	return filterExpr, attrs
}

// paginateAuditEntries applies filter.Offset/Limit in memory. DynamoDB has
// no native offset; the audit table is expected to stay small enough
// (90-day TTL) that in-memory pagination over a filtered result set is fine.
func paginateAuditEntries(entries []AuditEntry, filter AuditFilter) []AuditEntry {
	if filter.Offset > 0 {
		if filter.Offset >= len(entries) {
			return nil
		}
		entries = entries[filter.Offset:]
	}
	if filter.Limit > 0 && filter.Limit < len(entries) {
		entries = entries[:filter.Limit]
	}
	return entries
}

func parseAuditItems(items []map[string]types.AttributeValue) []AuditEntry {
	var entries []AuditEntry
	for _, item := range items {
		var record auditRecord
		if err := attributevalue.UnmarshalMap(item, &record); err != nil {
			continue
		}
		entry := AuditEntry{
			ID:       record.ID,
			User:     record.User,
			Action:   record.Action,
			Target:   record.Target,
			Result:   record.Result,
			Details:  record.Details,
			ClientIP: record.ClientIP,
		}
		if record.Timestamp != "" {
			if t, err := time.Parse(time.RFC3339, record.Timestamp); err == nil {
				entry.Timestamp = t
			}
		}
		entries = append(entries, entry)
	}
	return entries
}
