package db

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// ErrPoolAlreadyExists is returned when trying to create an ephemeral pool that already exists.
var ErrPoolAlreadyExists = errors.New("pool already exists")

// PoolSchedule defines time-based pool sizing for cost optimization.
type PoolSchedule struct {
	Name           string `dynamodbav:"name"`
	StartHour      int    `dynamodbav:"start_hour"`      // 0-23
	EndHour        int    `dynamodbav:"end_hour"`        // 0-23
	DaysOfWeek     []int  `dynamodbav:"days_of_week"`    // 0=Sunday, 1=Monday, etc.
	DesiredRunning int    `dynamodbav:"desired_running"` // Desired running instances during this schedule
	DesiredStopped int    `dynamodbav:"desired_stopped"` // Desired stopped instances during this schedule
}

// PoolConfig represents pool configuration from DynamoDB.
type PoolConfig struct {
	PoolName     string `dynamodbav:"pool_name"`
	InstanceType string `dynamodbav:"instance_type"`
	// DesiredRunning is the number of ready (idle) instances to maintain.
	// Busy instances (running jobs) are not counted toward this target.
	DesiredRunning     int            `dynamodbav:"desired_running"`
	DesiredStopped     int            `dynamodbav:"desired_stopped"`
	CurrentRunning     int            `dynamodbav:"current_running,omitempty"`
	CurrentStopped     int            `dynamodbav:"current_stopped,omitempty"`
	IdleTimeoutMinutes int            `dynamodbav:"idle_timeout_minutes,omitempty"`
	Schedules          []PoolSchedule `dynamodbav:"schedules,omitempty"`
	// Ephemeral pool support
	Ephemeral   bool      `dynamodbav:"ephemeral,omitempty"`
	LastJobTime time.Time `dynamodbav:"last_job_time,omitempty"`

	// Reconciliation observability, written by the reconcile loop via
	// UpdatePoolReconcileResult. Not part of SavePoolConfig's SET list, so pool
	// CRUD never clobbers them.
	LastReconcileAt     time.Time `dynamodbav:"last_reconcile_at,omitempty"`
	LastReconcileResult string    `dynamodbav:"last_reconcile_result,omitempty"`

	// The targets the last reconcile pass actually resolved, written with the
	// current counts by UpdatePoolState. They differ from DesiredRunning/
	// DesiredStopped whenever ephemeral auto-scaling or a hot-pool linger floor
	// overrides the configured seed, so comparing current against the seed is
	// meaningless for those pools. Also absent from SavePoolConfig's SET list.
	// Nil (never reconciled) is distinct from a target that resolved to zero.
	EffectiveDesiredRunning *int `dynamodbav:"effective_desired_running,omitempty"`
	EffectiveDesiredStopped *int `dynamodbav:"effective_desired_stopped,omitempty"`

	// Hot-pool override + auto-tune. The overrides are admin-written and three-state
	// (nil = "use auto", &0 = "force cold", &N = fixed); they ARE part of
	// SavePoolConfig's SET list so a nil pointer clears the override. AutoTune is
	// tuner-written and read-only to admin CRUD; it is deliberately EXCLUDED from
	// SavePoolConfig's SET list (like last_reconcile_*) and only ever set by
	// UpdatePoolAutoTune, so a pool save can never clobber a recommendation.
	OverrideLingerMinutes *int         `dynamodbav:"override_linger_minutes,omitempty"`
	OverrideMaxHot        *int         `dynamodbav:"override_max_hot,omitempty"`
	AutoTune              *AutoTuneRec `dynamodbav:"auto_tune,omitempty"`

	// Flexible instance spec (inherited from first job for ephemeral pools)
	Arch     string   `dynamodbav:"arch,omitempty"`     // arm64, amd64
	CPUMin   int      `dynamodbav:"cpu_min,omitempty"`  // Minimum vCPUs
	CPUMax   int      `dynamodbav:"cpu_max,omitempty"`  // Maximum vCPUs
	RAMMin   float64  `dynamodbav:"ram_min,omitempty"`  // Minimum RAM in GB
	RAMMax   float64  `dynamodbav:"ram_max,omitempty"`  // Maximum RAM in GB
	Families []string `dynamodbav:"families,omitempty"` // Instance families (e.g., c7g, m7g)

	// Multi-spec pool support: allow pool to hold instances with different specs.
	// When enabled, pool creates instances based on recent job demand patterns
	// rather than fixed pool-level spec. Jobs are matched to compatible instances.
	MultiSpec bool `dynamodbav:"multi_spec,omitempty"`
}

// AutoTuneRec is the hot-pool tuner's per-pool recommendation plus the evidence
// that produced it. The tuner writes it hourly via UpdatePoolAutoTune; the
// effective-spec resolver reads RecommendedLingerMinutes/RecommendedMaxHot, and
// the admin UI shows the whole struct so an operator can see why a pool is
// recommended hot (or kept cold). Every field is persisted, including for the
// cold cases (Reason "insufficient-history" / "no-burst-pattern"), so the
// rationale round-trips.
type AutoTuneRec struct {
	RecommendedLingerMinutes int       `dynamodbav:"recommended_linger_minutes" json:"recommended_linger_minutes"`
	RecommendedMaxHot        int       `dynamodbav:"recommended_max_hot" json:"recommended_max_hot"`
	WindowDays               int       `dynamodbav:"window_days" json:"window_days"`
	JobCount                 int       `dynamodbav:"job_count" json:"job_count"`
	BurstCount               int       `dynamodbav:"burst_count" json:"burst_count"`
	P90IntraBurstGapSeconds  int       `dynamodbav:"p90_intra_burst_gap_seconds" json:"p90_intra_burst_gap_seconds"`
	PeakConcurrency          int       `dynamodbav:"peak_concurrency" json:"peak_concurrency"`
	Reason                   string    `dynamodbav:"reason" json:"reason"`
	TunedAt                  time.Time `dynamodbav:"tuned_at" json:"tuned_at"`
}

// IsReservedPoolKey reports whether a pools-table partition key is an internal
// reserved record — a housekeeping task lock, an instance claim, or a runner
// offline sighting — rather than a real pool configuration. These records share
// the pools table but are keyed by a sentinel prefix, so every path that
// enumerates or resolves pools must exclude them; otherwise they get reconciled
// as phantom pools and inflate per-pool CloudWatch metric cardinality (one
// zero-valued series per ephemeral instance ID).
func IsReservedPoolKey(poolName string) bool {
	return strings.HasPrefix(poolName, taskLockPrefix) ||
		strings.HasPrefix(poolName, instanceClaimPrefix) ||
		strings.HasPrefix(poolName, runnerSightingPrefix) ||
		strings.HasPrefix(poolName, fleetDayPrefix)
}

// reapReservedRows deletes every pools-table row whose key carries prefix and
// whose attr is below cutoff, returning how many were removed. Both reserved-row
// reapers (instance claims, runner sightings) are this same sweep over a
// different attribute.
//
// Each row is re-checked with a conditional DeleteItem rather than deleted
// outright: a row rewritten between the scan and the delete is still live, and
// dropping it would undo whatever the rewrite recorded. A failed condition means
// exactly that and is skipped, not counted, not an error.
func (c *Client) reapReservedRows(ctx context.Context, prefix, attr string, cutoff int64, what string) (int, error) {
	if c.poolsTable == "" {
		return 0, fmt.Errorf("pools table not configured")
	}

	cutoffVal := strconv.FormatInt(cutoff, 10)
	cond := attr + " < :cutoff"
	input := &dynamodb.ScanInput{
		TableName:            aws.String(c.poolsTable),
		ProjectionExpression: aws.String("pool_name"),
		FilterExpression:     aws.String("begins_with(pool_name, :p) AND " + cond),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":p":      &types.AttributeValueMemberS{Value: prefix},
			":cutoff": &types.AttributeValueMemberN{Value: cutoffVal},
		},
	}

	var deleted int
	var lastEvaluatedKey map[string]types.AttributeValue
	for {
		input.ExclusiveStartKey = lastEvaluatedKey

		output, err := c.dynamoClient.Scan(ctx, input)
		if err != nil {
			return deleted, fmt.Errorf("failed to scan %s: %w", what, err)
		}

		for _, item := range output.Items {
			key, ok := item["pool_name"]
			if !ok {
				continue
			}

			_, err := c.dynamoClient.DeleteItem(ctx, &dynamodb.DeleteItemInput{
				TableName:           aws.String(c.poolsTable),
				Key:                 map[string]types.AttributeValue{"pool_name": key},
				ConditionExpression: aws.String(cond),
				ExpressionAttributeValues: map[string]types.AttributeValue{
					":cutoff": &types.AttributeValueMemberN{Value: cutoffVal},
				},
			})
			if err != nil {
				var condErr *types.ConditionalCheckFailedException
				if errors.As(err, &condErr) {
					continue
				}
				return deleted, fmt.Errorf("failed to delete %s: %w", what, err)
			}
			deleted++
		}

		lastEvaluatedKey = output.LastEvaluatedKey
		if lastEvaluatedKey == nil {
			return deleted, nil
		}
	}
}

// GetPoolConfig retrieves pool configuration from DynamoDB. Reserved keys (task
// locks, instance claims) are not pools and resolve to (nil, nil).
func (c *Client) GetPoolConfig(ctx context.Context, poolName string) (*PoolConfig, error) {
	if poolName == "" {
		return nil, fmt.Errorf("pool name cannot be empty")
	}
	if IsReservedPoolKey(poolName) {
		return nil, nil
	}

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

// ListPools returns all pool names from DynamoDB.
func (c *Client) ListPools(ctx context.Context) ([]string, error) {
	if c.poolsTable == "" {
		return nil, fmt.Errorf("pools table not configured")
	}

	input := &dynamodb.ScanInput{
		TableName:            aws.String(c.poolsTable),
		ProjectionExpression: aws.String("pool_name"),
	}

	var pools []string
	var lastEvaluatedKey map[string]types.AttributeValue
	for {
		input.ExclusiveStartKey = lastEvaluatedKey

		output, err := c.dynamoClient.Scan(ctx, input)
		if err != nil {
			return nil, fmt.Errorf("failed to scan pools table: %w", err)
		}

		for _, item := range output.Items {
			if name, ok := item["pool_name"]; ok {
				var poolName string
				if err := attributevalue.Unmarshal(name, &poolName); err != nil {
					continue
				}
				if IsReservedPoolKey(poolName) {
					continue
				}
				pools = append(pools, poolName)
			}
		}

		lastEvaluatedKey = output.LastEvaluatedKey
		if lastEvaluatedKey == nil {
			return pools, nil
		}
	}
}

// UpdatePoolState records what the reconciler observed and what it was targeting,
// in one write so the two can never disagree: comparing an observed count against
// a target from a different pass is what makes a converged pool look stale.
// Pool must exist in the table before calling this method.
func (c *Client) UpdatePoolState(ctx context.Context, poolName string, running, stopped, effectiveDesiredRunning, effectiveDesiredStopped int) error {
	if poolName == "" {
		return fmt.Errorf("pool name cannot be empty")
	}
	if running < 0 || stopped < 0 || effectiveDesiredRunning < 0 || effectiveDesiredStopped < 0 {
		return fmt.Errorf("running, stopped, and effective desired counts must be non-negative")
	}

	key, err := attributevalue.MarshalMap(map[string]string{
		"pool_name": poolName,
	})
	if err != nil {
		return fmt.Errorf("failed to marshal key: %w", err)
	}

	update := "SET current_running = :r, current_stopped = :s, " +
		"effective_desired_running = :edr, effective_desired_stopped = :eds"
	exprValues, err := attributevalue.MarshalMap(map[string]int{
		":r":   running,
		":s":   stopped,
		":edr": effectiveDesiredRunning,
		":eds": effectiveDesiredStopped,
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
		var condErr *types.ConditionalCheckFailedException
		if errors.As(err, &condErr) {
			return ErrPoolNotFound
		}
		return fmt.Errorf("failed to update item: %w", err)
	}

	return nil
}

// attrOrNull returns the attribute value from the map, or NULL if not present.
// This prevents nil attribute values when fields have zero values with omitempty.
func attrOrNull(m map[string]types.AttributeValue, key string) types.AttributeValue {
	if v, ok := m[key]; ok {
		return v
	}
	return &types.AttributeValueMemberNULL{Value: true}
}

// SavePoolConfig saves or updates a pool configuration in DynamoDB.
func (c *Client) SavePoolConfig(ctx context.Context, config *PoolConfig) error {
	if config == nil {
		return fmt.Errorf("pool config cannot be nil")
	}
	if config.PoolName == "" {
		return fmt.Errorf("pool name cannot be empty")
	}

	item, err := attributevalue.MarshalMap(config)
	if err != nil {
		return fmt.Errorf("failed to marshal pool config: %w", err)
	}

	_, err = c.dynamoClient.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(c.poolsTable),
		Key: map[string]types.AttributeValue{
			"pool_name": &types.AttributeValueMemberS{Value: config.PoolName},
		},
		// auto_tune is intentionally absent from this SET list: it is tuner-written
		// (UpdatePoolAutoTune) and must never be clobbered by an admin pool save,
		// the same treatment as last_reconcile_*. The override_* fields ARE set here
		// via attrOrNull so a nil pointer clears the override (NULL).
		UpdateExpression: aws.String(
			"SET instance_type = :it, desired_running = :dr, desired_stopped = :ds, " +
				"idle_timeout_minutes = :itm, schedules = :sc, " +
				"ephemeral = :eph, last_job_time = :ljt, arch = :arch, cpu_min = :cpumin, " +
				"cpu_max = :cpumax, ram_min = :rammin, ram_max = :rammax, families = :fam, " +
				"multi_spec = :ms, override_linger_minutes = :olm, override_max_hot = :omh"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":it":     attrOrNull(item, "instance_type"),
			":dr":     attrOrNull(item, "desired_running"),
			":ds":     attrOrNull(item, "desired_stopped"),
			":itm":    attrOrNull(item, "idle_timeout_minutes"),
			":sc":     attrOrNull(item, "schedules"),
			":eph":    attrOrNull(item, "ephemeral"),
			":ljt":    attrOrNull(item, "last_job_time"),
			":arch":   attrOrNull(item, "arch"),
			":cpumin": attrOrNull(item, "cpu_min"),
			":cpumax": attrOrNull(item, "cpu_max"),
			":rammin": attrOrNull(item, "ram_min"),
			":rammax": attrOrNull(item, "ram_max"),
			":fam":    attrOrNull(item, "families"),
			":ms":     attrOrNull(item, "multi_spec"),
			":olm":    attrOrNull(item, "override_linger_minutes"),
			":omh":    attrOrNull(item, "override_max_hot"),
		},
	})
	if err != nil {
		return fmt.Errorf("failed to save pool config: %w", err)
	}

	return nil
}

// CreateEphemeralPool creates a new ephemeral pool only if it doesn't already exist.
// Returns ErrPoolAlreadyExists if the pool is already present.
// This prevents race conditions when multiple concurrent jobs try to create the same pool.
func (c *Client) CreateEphemeralPool(ctx context.Context, config *PoolConfig) error {
	if config == nil {
		return fmt.Errorf("pool config cannot be nil")
	}
	if config.PoolName == "" {
		return fmt.Errorf("pool name cannot be empty")
	}
	if !config.Ephemeral {
		return fmt.Errorf("CreateEphemeralPool can only be used for ephemeral pools")
	}

	item, err := attributevalue.MarshalMap(config)
	if err != nil {
		return fmt.Errorf("failed to marshal pool config: %w", err)
	}

	_, err = c.dynamoClient.PutItem(ctx, &dynamodb.PutItemInput{
		TableName:           aws.String(c.poolsTable),
		Item:                item,
		ConditionExpression: aws.String("attribute_not_exists(pool_name)"),
	})
	if err != nil {
		var condErr *types.ConditionalCheckFailedException
		if errors.As(err, &condErr) {
			return ErrPoolAlreadyExists
		}
		return fmt.Errorf("failed to create ephemeral pool: %w", err)
	}

	return nil
}

// TouchPoolActivity updates the last job time for an ephemeral pool.
// This is called when a new job is assigned to the pool to prevent premature cleanup.
func (c *Client) TouchPoolActivity(ctx context.Context, poolName string) error {
	if poolName == "" {
		return fmt.Errorf("pool name cannot be empty")
	}

	if c.poolsTable == "" {
		return fmt.Errorf("pools table not configured")
	}

	now := time.Now()
	nowStr, err := attributevalue.Marshal(now)
	if err != nil {
		return fmt.Errorf("failed to marshal time: %w", err)
	}

	_, err = c.dynamoClient.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(c.poolsTable),
		Key: map[string]types.AttributeValue{
			"pool_name": &types.AttributeValueMemberS{Value: poolName},
		},
		UpdateExpression:          aws.String("SET last_job_time = :ljt"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":ljt": nowStr},
		ConditionExpression:       aws.String("attribute_exists(pool_name)"),
	})
	if err != nil {
		var condErr *types.ConditionalCheckFailedException
		if errors.As(err, &condErr) {
			return fmt.Errorf("pool %s does not exist", poolName)
		}
		return fmt.Errorf("failed to update pool activity: %w", err)
	}

	return nil
}

// UpdatePoolReconcileResult records the timestamp and outcome of a pool
// reconciliation pass. Targeted UpdateItem (like TouchPoolActivity) so it never
// clobbers concurrent lock state or pool config. Returns ErrPoolNotFound if the
// pool was deleted mid-reconcile, letting the caller ignore that benign race.
func (c *Client) UpdatePoolReconcileResult(ctx context.Context, poolName, result string, at time.Time) error {
	if poolName == "" {
		return fmt.Errorf("pool name cannot be empty")
	}
	if c.poolsTable == "" {
		return fmt.Errorf("pools table not configured")
	}

	atAttr, err := attributevalue.Marshal(at)
	if err != nil {
		return fmt.Errorf("failed to marshal time: %w", err)
	}

	_, err = c.dynamoClient.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(c.poolsTable),
		Key: map[string]types.AttributeValue{
			"pool_name": &types.AttributeValueMemberS{Value: poolName},
		},
		UpdateExpression: aws.String("SET last_reconcile_at = :at, last_reconcile_result = :res"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":at":  atAttr,
			":res": &types.AttributeValueMemberS{Value: result},
		},
		ConditionExpression: aws.String("attribute_exists(pool_name)"),
	})
	if err != nil {
		var condErr *types.ConditionalCheckFailedException
		if errors.As(err, &condErr) {
			return ErrPoolNotFound
		}
		return fmt.Errorf("failed to update pool reconcile result: %w", err)
	}

	return nil
}

// UpdatePoolAutoTune records the hot-pool tuner's recommendation and evidence for
// a pool. Targeted UpdateItem (like UpdatePoolReconcileResult) writing only
// auto_tune, so it never clobbers admin pool config or the override_* fields, and
// SavePoolConfig in turn never clobbers auto_tune. Returns ErrPoolNotFound if the
// pool was deleted between the tuner's list and this write.
func (c *Client) UpdatePoolAutoTune(ctx context.Context, poolName string, rec AutoTuneRec) error {
	if poolName == "" {
		return fmt.Errorf("pool name cannot be empty")
	}
	if c.poolsTable == "" {
		return fmt.Errorf("pools table not configured")
	}

	recAttr, err := attributevalue.MarshalMap(rec)
	if err != nil {
		return fmt.Errorf("failed to marshal auto-tune recommendation: %w", err)
	}

	_, err = c.dynamoClient.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(c.poolsTable),
		Key: map[string]types.AttributeValue{
			"pool_name": &types.AttributeValueMemberS{Value: poolName},
		},
		UpdateExpression: aws.String("SET auto_tune = :m"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":m": &types.AttributeValueMemberM{Value: recAttr},
		},
		ConditionExpression: aws.String("attribute_exists(pool_name)"),
	})
	if err != nil {
		var condErr *types.ConditionalCheckFailedException
		if errors.As(err, &condErr) {
			return ErrPoolNotFound
		}
		return fmt.Errorf("failed to update pool auto-tune: %w", err)
	}

	return nil
}

// DeletePoolConfig removes an ephemeral pool configuration from DynamoDB.
// Only ephemeral pools can be deleted to prevent accidental deletion of persistent pools.
func (c *Client) DeletePoolConfig(ctx context.Context, poolName string) error {
	if poolName == "" {
		return fmt.Errorf("pool name cannot be empty")
	}

	if c.poolsTable == "" {
		return fmt.Errorf("pools table not configured")
	}

	_, err := c.dynamoClient.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(c.poolsTable),
		Key: map[string]types.AttributeValue{
			"pool_name": &types.AttributeValueMemberS{Value: poolName},
		},
		// Safety: only allow deletion of ephemeral pools
		ConditionExpression: aws.String("ephemeral = :true"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":true": &types.AttributeValueMemberBOOL{Value: true},
		},
	})
	if err != nil {
		var condErr *types.ConditionalCheckFailedException
		if errors.As(err, &condErr) {
			return fmt.Errorf("pool %s is not ephemeral or does not exist", poolName)
		}
		return fmt.Errorf("failed to delete pool config: %w", err)
	}

	return nil
}
