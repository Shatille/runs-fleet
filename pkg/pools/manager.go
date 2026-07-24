// Package pools manages warm pool operations for maintaining pre-provisioned EC2 instances.
package pools

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/Shavakan/runs-fleet/pkg/db"
	"github.com/Shavakan/runs-fleet/pkg/fleet"
	"github.com/Shavakan/runs-fleet/pkg/github"
	"github.com/Shavakan/runs-fleet/pkg/logging"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"
	"github.com/google/uuid"
)

var poolLog = logging.WithComponent(logging.LogTypePool, "ec2-manager")

// Instance state constants
const (
	stateRunning = "running"
	stateStopped = "stopped"
)

// defaultBootstrapGracePeriod is how long after launch a warm-pool spare is left
// alone before it may be stopped. Stopping a spare mid-boot begins an OS shutdown
// that collides with agent-bootstrap's `systemctl start` (systemd "destructive
// transaction"); the boot shim mistakes that for a bootstrap failure and
// self-terminates the spare, churning the pool. This exceeds a baked-AMI boot
// (~60-120s) so the spare finishes bootstrapping first. Held per-Manager (settable
// in tests) rather than as a mutable global.
const defaultBootstrapGracePeriod = 3 * time.Minute

// reconcileInterval is how often ReconcileLoop reconciles every pool.
const reconcileInterval = 60 * time.Second

// reconcileLockTTL bounds a per-pool reconcile lock. It exceeds reconcileInterval
// so a lock left behind by a crashed owner is reclaimable within one interval
// rather than deadlocking the pool; the buffer also absorbs clock skew and a
// reconcile that slightly overruns its interval. Kept derived from
// reconcileInterval so the two can't drift out of that ordering.
const reconcileLockTTL = reconcileInterval + 5*time.Second

// defaultReadyDwellPeriod is how long a running instance must be observed
// continuously not-busy before reconciliation may stop it. It exceeds one
// reconcileInterval so a single missed "busy" observation — a brief pool-status
// GSI consistency lag, or the claiming window before SaveJob writes launched —
// cannot get a live instance stopped mid-job: the instance would have to read as
// not-busy across two consecutive reconciles. This guards the case the busy set
// alone cannot (an instance whose job status is momentarily unqueryable). Held
// per-Manager (settable in tests) rather than as a mutable global.
const defaultReadyDwellPeriod = 90 * time.Second

// maxReconcileResultLen caps the persisted last_reconcile_result string so a
// pathologically long wrapped error can't bloat the pool item.
const maxReconcileResultLen = 300

// DBClient defines DynamoDB operations for pool configuration.
//
//nolint:dupl // Mock struct in test file mirrors this interface - intentional pattern
type DBClient interface {
	GetPoolConfig(ctx context.Context, poolName string) (*db.PoolConfig, error)
	UpdatePoolState(ctx context.Context, poolName string, running, stopped int) error
	ListPools(ctx context.Context) ([]string, error)
	GetPoolP90Concurrency(ctx context.Context, poolName string, windowHours int) (int, error)
	GetPoolBusyInstanceIDs(ctx context.Context, poolName string) ([]string, error)
	AcquirePoolReconcileLock(ctx context.Context, poolName, owner string, ttl time.Duration) error
	ReleasePoolReconcileLock(ctx context.Context, poolName, owner string) error
	UpdatePoolReconcileResult(ctx context.Context, poolName, result string, at time.Time) error
	ClaimInstanceForJob(ctx context.Context, instanceID string, jobID int64, ttl time.Duration) error
	ReleaseInstanceClaim(ctx context.Context, instanceID string, jobID int64) error
}

// FleetAPI defines EC2 fleet operations for instance provisioning.
type FleetAPI interface {
	CreateFleet(ctx context.Context, spec *fleet.LaunchSpec) ([]string, error)
	CreateOnDemandInstance(ctx context.Context, spec *fleet.LaunchSpec) (string, error)
	RankInstanceTypesByPrice(ctx context.Context, instanceTypes []string) []string
}

// MetricsAPI publishes pool scaling metrics.
type MetricsAPI interface {
	PublishPoolAction(ctx context.Context, pool, action, reason string) error
	PublishPoolDesired(ctx context.Context, pool, kind string, n int) error
	PublishPoolInstances(ctx context.Context, pool, state string, n int) error
	PublishPoolReconcileSeconds(ctx context.Context, seconds float64) error
	PublishLockWaitSeconds(ctx context.Context, lock string, seconds float64) error
	PublishInstances(ctx context.Context, state, capacity, pool string, n int) error
}

// Lock labels for PublishLockWaitSeconds.
const (
	lockPoolReconcile = "pool_reconcile"
	lockClaim         = "claim"
)

// Pool instances are on-demand only (warm pools favor stop/start reliability
// over spot savings), so the global instances gauge is dimensioned accordingly.
const capacityOnDemand = "on_demand"

// Pool action labels for PublishPoolAction.
const (
	poolActionCreate    = "create"
	poolActionStop      = "stop"
	poolActionTerminate = "terminate"
	poolActionStart     = "start"
)

// poolActionReasonLinger attributes a scale-up to the hot-pool linger floor
// (rather than an ordinary ready deficit) on the pool_actions metric.
const poolActionReasonLinger = "linger"

// EC2API defines EC2 operations for instance management.
//
//nolint:dupl // Mock struct in test file mirrors this interface - intentional pattern
type EC2API interface {
	DescribeInstances(ctx context.Context, params *ec2.DescribeInstancesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
	StartInstances(ctx context.Context, params *ec2.StartInstancesInput, optFns ...func(*ec2.Options)) (*ec2.StartInstancesOutput, error)
	StopInstances(ctx context.Context, params *ec2.StopInstancesInput, optFns ...func(*ec2.Options)) (*ec2.StopInstancesOutput, error)
	TerminateInstances(ctx context.Context, params *ec2.TerminateInstancesInput, optFns ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error)
	CreateTags(ctx context.Context, params *ec2.CreateTagsInput, optFns ...func(*ec2.Options)) (*ec2.CreateTagsOutput, error)
}

// PoolInstance represents an EC2 instance in a pool.
type PoolInstance struct {
	InstanceID   string
	State        string
	LaunchTime   time.Time
	InstanceType string
	Spot         bool      // one-time spot instance (cold-start overflow); cannot be stopped
	IdleSince    time.Time // When the instance became idle (no job assigned)

	// Spec from InstanceCatalog lookup (populated for multi-spec pool matching)
	CPU    int
	RAM    float64
	Arch   string
	Family string
	Gen    int
}

// matchesFlexibleSpec checks if this instance matches the given flexible spec requirements.
// Returns true if spec is nil (legacy behavior: any instance matches).
// Returns false if instance type is not in InstanceCatalog (CPU=0) when spec is provided.
func (p PoolInstance) matchesFlexibleSpec(spec *fleet.FlexibleSpec) bool {
	if spec == nil {
		return true
	}

	// Instance type must be in catalog for spec matching
	// (instances not in catalog have CPU=0 and would fail any CPUMin check)
	if p.CPU == 0 {
		return false
	}

	instSpec := fleet.InstanceSpec{
		Type:   p.InstanceType,
		CPU:    p.CPU,
		RAM:    p.RAM,
		Arch:   p.Arch,
		Family: p.Family,
		Gen:    p.Gen,
	}
	return instSpec.MatchesFlexibleSpec(*spec)
}

// Manager orchestrates warm pool operations and instance assignment.
//
// Locking model (see pkg/pools for the contention rationale):
//   - idleMu guards only the instanceIdle map. Its critical sections are tiny
//     and never span an AWS or DynamoDB call.
//   - poolLocks holds one *poolLock per pool, serializing local candidate
//     selection for ClaimAndStartPoolInstance so two goroutines in this process
//     never pick the same stopped instance. Different pools use different locks
//     and therefore never block one another.
//   - A poolLock also tracks instances this process has selected but not yet
//     resolved against DynamoDB (inFlight), so concurrent local claims skip a
//     candidate another goroutine is already attempting.
//
// The authoritative cross-process guard against double-claims is the DynamoDB
// conditional write in ClaimInstanceForJob; the in-memory locks only prevent
// redundant work and lost updates within a single process and are never held
// across network calls.
type Manager struct {
	dbClient     DBClient
	fleetManager FleetAPI
	ec2Client    EC2API
	config       *config.Config
	metrics      MetricsAPI
	instanceID   string // Unique identifier for this instance (for distributed locking)

	// bootstrapGracePeriod gates how long a warm-pool spare is left running before
	// it may be stopped (see defaultBootstrapGracePeriod). A field, not a global, so
	// tests set it per-Manager without racing.
	bootstrapGracePeriod time.Duration

	// readyDwellPeriod gates how long a running instance must be seen continuously
	// not-busy before it may be stopped (see defaultReadyDwellPeriod). 0 disables
	// the dwell so the instance is stoppable as soon as it is otherwise eligible.
	readyDwellPeriod time.Duration

	idleMu       sync.Mutex           // Guards instanceIdle and readySince
	instanceIdle map[string]time.Time // Tracks when instances became idle
	// readySince tracks, per pool and running instance, when the instance's current
	// continuous not-busy streak began; cleared when the instance reads busy or is
	// forgotten (stopped, terminated, claimed). Keyed by pool because reconcile walks
	// every pool in a single pass — a global map would let one pool's prune wipe
	// another pool's streaks, so the dwell could never be satisfied. Drives the
	// readyDwellPeriod guard.
	readySince map[string]map[string]time.Time

	poolLocks sync.Map // poolName -> *poolLock

	subnetIndex uint64
	randIntn    func(int) int

	demandCh chan string // Pool names requesting immediate reconciliation
}

// poolLock serializes per-pool candidate selection and records which stopped
// instances a local goroutine is currently attempting to claim. It is never
// held across an AWS or DynamoDB call.
type poolLock struct {
	mu       sync.Mutex
	inFlight map[string]struct{} // instance IDs being claimed by a local goroutine
}

// poolLockFor returns the per-pool lock, creating it on first use.
func (m *Manager) poolLockFor(poolName string) *poolLock {
	if v, ok := m.poolLocks.Load(poolName); ok {
		return v.(*poolLock)
	}
	pl := &poolLock{inFlight: make(map[string]struct{})}
	actual, _ := m.poolLocks.LoadOrStore(poolName, pl)
	return actual.(*poolLock)
}

// NewManager creates pool manager with DB and fleet clients.
// Generates a unique instance ID for distributed locking.
func NewManager(dbClient DBClient, fleetManager FleetAPI, cfg *config.Config) *Manager {
	return &Manager{
		dbClient:             dbClient,
		fleetManager:         fleetManager,
		config:               cfg,
		instanceID:           uuid.New().String(),
		instanceIdle:         make(map[string]time.Time),
		readySince:           make(map[string]map[string]time.Time),
		randIntn:             rand.IntN,
		demandCh:             make(chan string, 64),
		bootstrapGracePeriod: defaultBootstrapGracePeriod,
		readyDwellPeriod:     defaultReadyDwellPeriod,
	}
}

// NotifyPoolDemand signals the reconciliation loop to reconcile the given pool
// immediately rather than waiting for the next ticker interval. Non-blocking:
// if the channel is full, the notification is dropped (the ticker will catch up).
func (m *Manager) NotifyPoolDemand(poolName string) {
	select {
	case m.demandCh <- poolName:
	default:
	}
}

// SetEC2Client sets the EC2 client for instance management.
func (m *Manager) SetEC2Client(ec2Client EC2API) {
	m.ec2Client = ec2Client
}

// SetMetrics sets the metrics publisher for the manager.
func (m *Manager) SetMetrics(metrics MetricsAPI) {
	m.metrics = metrics
}

// selectSubnet returns the next private subnet ID in round-robin fashion.
func (m *Manager) selectSubnet() string {
	if len(m.config.SubnetIDs) == 0 {
		return ""
	}
	idx := atomic.AddUint64(&m.subnetIndex, 1) - 1
	return m.config.SubnetIDs[idx%uint64(len(m.config.SubnetIDs))]
}

// ReconcileLoop runs periodically to maintain pool size.
// In addition to the 60-second ticker, it accepts demand notifications via
// NotifyPoolDemand to reconcile specific pools immediately when webhooks arrive.
func (m *Manager) ReconcileLoop(ctx context.Context) {
	ticker := time.NewTicker(reconcileInterval)
	defer ticker.Stop()

	// Initial reconciliation
	m.reconcile(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.reconcile(ctx)
		case poolName := <-m.demandCh:
			m.reconcileDemand(ctx, poolName)
		}
	}
}

// reconcileDemand handles a demand-driven reconciliation for a specific pool.
// It drains the demand channel to deduplicate concurrent notifications and
// reconciles each unique pool exactly once.
func (m *Manager) reconcileDemand(ctx context.Context, first string) {
	pools := map[string]struct{}{first: {}}
	for {
		select {
		case name := <-m.demandCh:
			pools[name] = struct{}{}
		default:
			goto reconcile
		}
	}
reconcile:
	if m.ec2Client == nil {
		return
	}

	for poolName := range pools {
		if ctx.Err() != nil {
			break
		}
		pctx := logging.ContextWith(ctx, slog.String(logging.KeyPoolName, poolName))
		if err := m.reconcilePool(pctx, poolName); err != nil {
			if isShutdownErr(err) {
				poolLog.Debug(pctx, "reconciliation aborted: shutting down",
					slog.String("error", err.Error()))
				break
			}
			poolLog.Error(pctx, "demand-driven reconciliation failed",
				slog.String("error", err.Error()))
		}
	}
}

// isShutdownErr reports whether err stems from context cancellation or deadline
// expiry, which during graceful shutdown is expected rather than a fault.
func isShutdownErr(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func (m *Manager) reconcile(ctx context.Context) {
	if m.ec2Client == nil {
		return
	}

	pools, err := m.dbClient.ListPools(ctx)
	if err != nil {
		poolLog.Error(ctx, "list pools failed", slog.String("error", err.Error()))
		return
	}

	for _, poolName := range pools {
		if ctx.Err() != nil {
			break
		}
		pctx := logging.ContextWith(ctx, slog.String(logging.KeyPoolName, poolName))
		if err := m.reconcilePool(pctx, poolName); err != nil {
			if isShutdownErr(err) {
				poolLog.Debug(pctx, "reconciliation aborted: shutting down",
					slog.String("error", err.Error()))
				break
			}
			poolLog.Error(pctx, "pool reconciliation failed",
				slog.String("error", err.Error()))
		}
	}
}

//nolint:gocyclo // Core reconciliation logic with multiple scale up/down paths
func (m *Manager) reconcilePool(ctx context.Context, poolName string) (err error) {
	// Time the whole reconcile pass (lock wait + AWS work) so slow passes are
	// observable. The pass only runs when this instance wins the lock; contended
	// passes that skip are captured by the lock-wait histogram below.
	reconcileStart := time.Now()

	// Acquire per-pool lock (TTL > reconcile interval). Time the acquire wait —
	// this is the contention signal that would have surfaced #298.
	lockStart := time.Now()
	// Assign to the named return (not a shadow) so the outcome recorder below sees
	// the real error; the recorder is only registered after this block, so these
	// early returns are never recorded (another instance owns the lock, or the
	// pool is gone).
	if err = m.dbClient.AcquirePoolReconcileLock(ctx, poolName, m.instanceID, reconcileLockTTL); err != nil {
		if m.metrics != nil {
			_ = m.metrics.PublishLockWaitSeconds(ctx, lockPoolReconcile, time.Since(lockStart).Seconds())
		}
		// Lock held by another instance or pool deleted - skip silently
		if errors.Is(err, db.ErrPoolReconcileLockHeld) || errors.Is(err, db.ErrPoolNotFound) {
			return nil
		}
		return fmt.Errorf("failed to acquire pool lock: %w", err)
	}
	if m.metrics != nil {
		_ = m.metrics.PublishLockWaitSeconds(ctx, lockPoolReconcile, time.Since(lockStart).Seconds())
	}
	defer func() {
		if m.metrics != nil {
			_ = m.metrics.PublishPoolReconcileSeconds(ctx, time.Since(reconcileStart).Seconds())
		}
	}()
	defer func() {
		if relErr := m.dbClient.ReleasePoolReconcileLock(ctx, poolName, m.instanceID); relErr != nil {
			if isShutdownErr(relErr) {
				poolLog.Debug(ctx, "pool lock release aborted: shutting down",
					slog.String("error", relErr.Error()))
				return
			}
			poolLog.Error(ctx, "pool lock release failed",
				slog.String("error", relErr.Error()))
		}
	}()
	// Record the reconcile outcome. Registered after (so it runs before, LIFO) the
	// lock release, i.e. while this instance still owns the pool. A failed record
	// is logged and ignored -- a missing reconcile timestamp must never fail the
	// reconcile, and ErrPoolNotFound just means the pool was deleted mid-pass.
	defer func() {
		result := "success"
		if err != nil {
			result = "failed: " + err.Error()
			if r := []rune(result); len(r) > maxReconcileResultLen {
				result = string(r[:maxReconcileResultLen])
			}
		}
		if recErr := m.dbClient.UpdatePoolReconcileResult(ctx, poolName, result, time.Now()); recErr != nil {
			if errors.Is(recErr, db.ErrPoolNotFound) || isShutdownErr(recErr) {
				return
			}
			poolLog.Warn(ctx, "pool reconcile result update failed",
				slog.String("error", recErr.Error()))
		}
	}()

	poolConfig, err := m.dbClient.GetPoolConfig(ctx, poolName)
	if err != nil {
		return fmt.Errorf("failed to get pool config: %w", err)
	}
	if poolConfig == nil {
		return fmt.Errorf("pool config not found")
	}

	// Hoisted so the linger floor and the not-busy-streak refresh below share one
	// timestamp for the whole pass.
	now := time.Now()

	desiredRunning, desiredStopped := m.getScheduledDesiredCounts(poolConfig)

	// For ephemeral pools, override with auto-scaled values based on peak concurrency
	// Ephemeral pools scale stopped instances (not running) for on-demand starts
	if ephRunning, ephStopped, ok := m.getEphemeralAutoScaledCount(ctx, poolName, poolConfig); ok {
		desiredRunning = ephRunning
		desiredStopped = ephStopped
	}

	// Hot-pool linger floor: for an allowlisted pool with recent job activity,
	// keep MaxHot running spares until the linger window elapses, then decay. This
	// composes with schedules/ephemeral via max() — it only ever raises the running
	// target, never lowers it. Zero (and thus a no-op) unless RUNS_FLEET_HOT_POOLS
	// names this pool, so the gate-off path is unchanged.
	lingerHot := m.lingerDesiredRunning(poolConfig, now)
	lingerActive := lingerHot > desiredRunning
	if lingerActive {
		desiredRunning = lingerHot
	}

	instances, err := m.getPoolInstances(ctx, poolName)
	if err != nil {
		return fmt.Errorf("failed to get pool instances: %w", err)
	}

	busyIDs, err := m.dbClient.GetPoolBusyInstanceIDs(ctx, poolName)
	if err != nil {
		return fmt.Errorf("failed to get busy instance IDs: %w", err)
	}

	running, stopped, _, _ := countInstanceStates(instances, busyIDs)

	// Busy count = running instances that have jobs assigned (instance-job intersection).
	// Only count actual running instances with matching job records - orphaned job records
	// (from terminated instances) must NOT inflate busy count or they block scale-down.
	runningInstances := m.filterByState(instances, stateRunning)
	busy := len(filterMatchingInstances(runningInstances, busyIDs))
	ready := running - busy

	// Refresh not-busy streaks every pass (both scale directions) so the dwell guard
	// has continuous history when a later pass considers scaling down.
	m.updateReadySince(poolName, runningInstances, busyIDs, now)

	// Track changes for logging
	var started, stoppedCount, created, terminated int

	// Ready spares still within the bootstrap grace: not stopped yet, but pending
	// stopped-spares. Credited against the replenish deficit so the grace doesn't
	// trigger creation of duplicate spares while these finish booting.
	var withinGraceSpares int

	// Scale based on ready count, not total running
	// desiredRunning represents desired READY instances (idle, available for jobs)
	if ready < desiredRunning {
		deficit := desiredRunning - ready

		// Attribute this scale-up to the linger floor when the running target
		// exists only because of it, so pool_actions distinguishes hot-pool warmth
		// from ordinary ready-deficit provisioning. Attribution is best-effort at
		// the margin: if a scheduled/ephemeral target happens to equal MaxHot,
		// lingerActive is false and a genuinely linger-window start is labeled
		// "ready_deficit". This mislabels the metric, never the behavior.
		reason := "ready_deficit"
		if lingerActive {
			reason = poolActionReasonLinger
		}

		stoppedInstances := m.filterByState(instances, stateStopped)
		toStart := min(deficit, len(stoppedInstances))
		if toStart > 0 {
			instanceIDs := make([]string, toStart)
			for i := 0; i < toStart; i++ {
				instanceIDs[i] = stoppedInstances[i].InstanceID
			}
			if err := m.startInstances(ctx, poolName, instanceIDs, reason); err != nil {
				poolLog.Error(ctx, "instances start failed", slog.String("error", err.Error()))
			} else {
				started += toStart
				deficit -= toStart
			}
		}

		if deficit > 0 {
			created += m.createPoolFleetInstances(ctx, poolName, reason, deficit, poolConfig)
		}
	} else if ready > desiredRunning {
		// Too many ready (idle) instances - stop or terminate excess
		// Only scale down idle instances, never busy ones
		excess := ready - desiredRunning
		runningInstances := m.filterByState(instances, stateRunning)

		// For warm pools (desiredRunning=0), stop instances immediately
		// For hot pools, only stop instances that exceeded idle timeout
		var candidateInstances []PoolInstance
		if desiredRunning == 0 && desiredStopped > 0 {
			// Warm pool: ready instances are candidates for stopping, but skip ones
			// still within the bootstrap grace window — stopping mid-boot churns the
			// pool (see defaultBootstrapGracePeriod). They become candidates on a later pass.
			// filterNotInFlight also excludes a running instance a concurrent claim has
			// selected as a hot spare (the assignment path reserves it in-flight before
			// it flips the DB record to busy), so linger-decay can't race a claim and
			// stop a spare that's being assigned a job. A no-op when nothing is in-flight,
			// which is always the case with hot pools off.
			readyInstances := m.poolLockFor(poolName).filterNotInFlight(m.filterReadyInstances(runningInstances, busyIDs))
			candidateInstances = make([]PoolInstance, 0, len(readyInstances))
			var graceDeferred, dwellDeferred int
			for _, inst := range readyInstances {
				// Treat an unknown launch time (zero value) as still-booting: acting on
				// it could stop a spare mid-boot, the exact churn this guards against.
				if inst.LaunchTime.IsZero() || now.Sub(inst.LaunchTime) < m.bootstrapGracePeriod {
					graceDeferred++
					// Only on-demand spares become stopped spares, so only they may be
					// credited against the replenish deficit below. A within-grace spot
					// instance is cold-start overflow that gets terminated once it ages
					// out — crediting it would wrongly suppress real stopped-spare creation.
					if !inst.Spot {
						withinGraceSpares++
					}
					continue
				}
				// Defer instances that have not been continuously not-busy for the dwell
				// window: the busy set can momentarily miss a live instance (GSI lag, the
				// claiming window), and stopping on a single such observation is what
				// killed running jobs mid-flight. Credited like within-grace spares since
				// a genuinely-idle one is a pending stopped-spare.
				if !m.readyLongEnough(poolName, inst.InstanceID, now) {
					dwellDeferred++
					if !inst.Spot {
						withinGraceSpares++
					}
					continue
				}
				candidateInstances = append(candidateInstances, inst)
			}
			if graceDeferred > 0 {
				poolLog.Info(ctx, "deferring stop of still-bootstrapping spares",
					slog.String("pool_name", poolName),
					slog.Int("within_grace", graceDeferred))
			}
			if dwellDeferred > 0 {
				poolLog.Info(ctx, "deferring stop of recently-active spares",
					slog.String("pool_name", poolName),
					slog.Int("within_dwell", dwellDeferred))
			}
		} else {
			// Hot pool: only idle instances that exceeded timeout
			candidateInstances = m.filterIdleInstances(runningInstances, poolConfig.IdleTimeoutMinutes)
		}

		// One-time spot instances (cold-start overflow that got pool-tagged) cannot
		// be stopped — StopInstances rejects them — so only on-demand spares are
		// stoppable. Spot excess falls through to the terminate path below.
		var stoppable, spotExcess []PoolInstance
		for _, ci := range candidateInstances {
			if ci.Spot {
				spotExcess = append(spotExcess, ci)
			} else {
				stoppable = append(stoppable, ci)
			}
		}

		stoppedNow := 0
		toStop := min(excess, len(stoppable))
		if toStop > 0 && stopped < desiredStopped {
			canStop := min(toStop, desiredStopped-stopped)
			instanceIDs := make([]string, canStop)
			for i := 0; i < canStop; i++ {
				instanceIDs[i] = stoppable[i].InstanceID
			}
			if err := m.stopInstances(ctx, poolName, instanceIDs, "excess_ready"); err != nil {
				if isIncorrectInstanceStateError(err) {
					// An instance can leave the running state between the DescribeInstances
					// snapshot and this call — it got claimed for a job and is pending/
					// stopping. EC2 rejects the batch with IncorrectInstanceState; that is a
					// benign race, not an operator-actionable failure.
					poolLog.Info(ctx, "skipped stopping instances not in a stoppable state",
						slog.String("pool_name", poolName),
						slog.Any("instance_ids", instanceIDs))
				} else {
					poolLog.Error(ctx, "instances stop failed", slog.String("error", err.Error()))
				}
				// A failed stop (benign race or a transient/retriable error) must not
				// cascade into terminating these instances: they may be transitioning
				// (claimed for a job) or the error may clear next pass. Mark them handled
				// so the terminate path skips them, but do not credit them as banked stops.
				stoppedNow = canStop
			} else {
				stoppedCount += canStop
				stoppedNow = canStop
			}
			excess -= canStop
		}

		if excess > 0 {
			// Terminate the remaining excess: spot candidates (can't be stopped
			// spares) plus any on-demand not already stopped this pass.
			terminable := append(spotExcess, stoppable[stoppedNow:]...)
			toTerminate := min(excess, len(terminable))
			if toTerminate > 0 {
				instanceIDs := make([]string, toTerminate)
				for i := 0; i < toTerminate; i++ {
					instanceIDs[i] = terminable[i].InstanceID
				}
				if err := m.terminateInstances(ctx, poolName, instanceIDs, "excess_idle"); err != nil {
					poolLog.Error(ctx, "instances terminate failed", slog.String("error", err.Error()))
				} else {
					terminated += toTerminate
				}
			}
		}
	}

	if stopped > desiredStopped {
		excess := stopped - desiredStopped
		// Exclude instances a local claim is mid-flight on: the claim has already
		// issued (or is about to issue) StartInstances, but EC2 may still report
		// the instance as stopped, and terminating it here would kill a job's runner.
		stoppedInstances := m.poolLockFor(poolName).filterNotInFlight(m.filterByState(instances, stateStopped))
		toTerminate := min(excess, len(stoppedInstances))
		if toTerminate > 0 {
			instanceIDs := make([]string, toTerminate)
			for i := 0; i < toTerminate; i++ {
				instanceIDs[i] = stoppedInstances[i].InstanceID
			}
			if err := m.terminateInstances(ctx, poolName, instanceIDs, "over_desired_stopped"); err != nil {
				poolLog.Error(ctx, "stopped instances terminate failed", slog.String("error", err.Error()))
			} else {
				terminated += toTerminate
			}
		}
	} else if stopped < desiredStopped && desiredRunning == 0 {
		// Need more stopped instances for warm/ephemeral pools (desiredRunning=0, desiredStopped>0).
		// Credit instances stopped into the reserve during this same pass: stoppedCount
		// instances are joining the stopped reserve but EC2 still reports them as running
		// in the snapshot, so creating against the stale stopped count over-provisions.
		// Also credit ready spares still within the bootstrap grace — they are pending
		// stopped-spares, so counting them prevents duplicate creation while they boot.
		deficit := desiredStopped - stopped - stoppedCount - withinGraceSpares
		if deficit > 0 {
			created += m.createPoolFleetInstances(ctx, poolName, "stopped_replenish", deficit, poolConfig)
		}
	}

	// Log reconciliation only when changes occurred
	if started+stoppedCount+created+terminated > 0 {
		poolLog.Info(ctx, "pool reconciled",
			slog.Int("desired_running", desiredRunning),
			slog.Int("desired_stopped", desiredStopped),
			slog.Int("running", running),
			slog.Int("stopped", stopped),
			slog.Int("ready", ready),
			slog.Int("busy", busy),
			slog.Int("started", started),
			slog.Int("stopped_count", stoppedCount),
			slog.Int("created", created),
			slog.Int("terminated", terminated))
	}

	// Publish the post-reconcile pool state gauges every cycle (utilization is
	// derivable from these states).
	if m.metrics != nil {
		_ = m.metrics.PublishPoolInstances(ctx, poolName, "running", running)
		_ = m.metrics.PublishPoolInstances(ctx, poolName, "stopped", stopped)
		_ = m.metrics.PublishPoolInstances(ctx, poolName, "ready", ready)
		_ = m.metrics.PublishPoolInstances(ctx, poolName, "busy", busy)
		_ = m.metrics.PublishPoolDesired(ctx, poolName, "running", desiredRunning)
		_ = m.metrics.PublishPoolDesired(ctx, poolName, "stopped", desiredStopped)
		// Feed the global instances gauge from the authoritative per-pool counts
		// computed above (real running/stopped totals, not per-event 0/1 deltas
		// that drift and lose state on restart). Pool instances are on-demand.
		_ = m.metrics.PublishInstances(ctx, stateRunning, capacityOnDemand, poolName, running)
		_ = m.metrics.PublishInstances(ctx, stateStopped, capacityOnDemand, poolName, stopped)
	}

	if err := m.dbClient.UpdatePoolState(ctx, poolName, running, stopped); err != nil {
		poolLog.Error(ctx, "pool state update failed", slog.String("error", err.Error()))
	}

	return nil
}

// ephemeralScaleDownWindow is the time window for keeping at least one instance
// after the last job. This prevents premature scale-to-zero when jobs are infrequent
// but the pool is still actively used.
const ephemeralScaleDownWindow = 4 * time.Hour

// getEphemeralAutoScaledCount returns the auto-scaled desired counts for ephemeral pools.
// Ephemeral pools scale stopped instances (not running) based on peak concurrency,
// since jobs claim and start stopped instances on-demand via ClaimAndStartPoolInstance.
// Returns (desiredRunning, desiredStopped, true) if scaling was applied, or (0, 0, false) for non-ephemeral pools.
func (m *Manager) getEphemeralAutoScaledCount(ctx context.Context, poolName string, poolConfig *db.PoolConfig) (int, int, bool) {
	if !poolConfig.Ephemeral {
		return 0, 0, false
	}

	// Check recent activity first (fast path, no query needed)
	// This also ensures minimum capacity is maintained if peak query fails
	var recentlyActive bool
	var sinceLastJob time.Duration
	if !poolConfig.LastJobTime.IsZero() {
		sinceLastJob = time.Since(poolConfig.LastJobTime)
		recentlyActive = sinceLastJob < ephemeralScaleDownWindow
	}

	p90, err := m.dbClient.GetPoolP90Concurrency(ctx, poolName, 1) // 1 hour window
	if err != nil {
		poolLog.Error(ctx, "p90 concurrency query failed",
			slog.String("error", err.Error()))
		// If recently active, keep minimum capacity despite query failure
		if recentlyActive {
			poolLog.Debug(ctx, "ephemeral pool keeping minimum capacity (query failed)",
				slog.Time("last_job_time", poolConfig.LastJobTime),
				slog.Duration("since_last_job", sinceLastJob))
			return 0, 1, true
		}
		return 0, poolConfig.DesiredStopped, true // Ephemeral pools always have desiredRunning=0
	}

	if p90 > 0 {
		desired := max(1, p90)
		poolLog.Info(ctx, "ephemeral pool auto-scaling",
			slog.Int("p90_concurrency", p90),
			slog.Int("desired_stopped", desired))
		return 0, desired, true
	}

	// P90 is 0, but check if pool was recently active
	if recentlyActive {
		poolLog.Debug(ctx, "ephemeral pool keeping minimum capacity",
			slog.Time("last_job_time", poolConfig.LastJobTime),
			slog.Duration("since_last_job", sinceLastJob))
		return 0, 1, true
	}

	return 0, poolConfig.DesiredStopped, true // Ephemeral pools always have desiredRunning=0
}

// lingerDesiredRunning returns the hot-pool running floor for this pool: MaxHot
// while the pool has had job activity within its linger window, else 0. It is a
// floor, not an override — reconcilePool applies it via max() so it only raises
// the running target during a burst tail and never lowers a scheduled/admin
// target.
//
// Returns 0 unless the pool is named in the RUNS_FLEET_HOT_POOLS allowlist, so
// with the feature off (Config.HotPools nil) this reads nothing new and changes
// no decision — the sole cost gate is the env allowlist. It relies on
// LastJobTime, which only ephemeral pools stamp; persistent pools never activate
// linger (they use desired_running/schedules instead), which is intended: hot
// pools target the label-created ephemeral pools that pay the stopped-boot today.
func (m *Manager) lingerDesiredRunning(poolConfig *db.PoolConfig, now time.Time) int {
	if m.config == nil || len(m.config.HotPools) == 0 {
		return 0
	}
	spec, ok := m.config.HotPools[poolConfig.PoolName]
	if !ok {
		return 0
	}
	if poolConfig.LastJobTime.IsZero() {
		return 0
	}
	if now.Sub(poolConfig.LastJobTime) >= time.Duration(spec.LingerMinutes)*time.Minute {
		return 0
	}
	return spec.MaxHot
}

// getScheduledDesiredCounts returns the desired running and stopped counts based on schedule.
func (m *Manager) getScheduledDesiredCounts(poolConfig *db.PoolConfig) (running, stopped int) {
	if len(poolConfig.Schedules) == 0 {
		return poolConfig.DesiredRunning, poolConfig.DesiredStopped
	}

	now := time.Now()
	currentHour := now.Hour()
	currentDay := now.Weekday()

	// Find matching schedule
	for _, schedule := range poolConfig.Schedules {
		if m.scheduleMatches(schedule, currentHour, currentDay) {
			return schedule.DesiredRunning, schedule.DesiredStopped
		}
	}

	// Fall back to default
	return poolConfig.DesiredRunning, poolConfig.DesiredStopped
}

// scheduleMatches checks if a schedule applies to the current time.
func (m *Manager) scheduleMatches(schedule db.PoolSchedule, hour int, day time.Weekday) bool {
	if len(schedule.DaysOfWeek) > 0 {
		dayMatch := false
		for _, d := range schedule.DaysOfWeek {
			if d == int(day) {
				dayMatch = true
				break
			}
		}
		if !dayMatch {
			return false
		}
	}

	if schedule.StartHour <= schedule.EndHour {
		// Normal range (e.g., 9-17)
		return hour >= schedule.StartHour && hour < schedule.EndHour
	}
	// Overnight range (e.g., 22-6)
	return hour >= schedule.StartHour || hour < schedule.EndHour
}

func (m *Manager) getPoolInstances(ctx context.Context, poolName string) ([]PoolInstance, error) {
	input := &ec2.DescribeInstancesInput{
		Filters: []types.Filter{
			{
				Name:   aws.String("tag:runs-fleet:pool"),
				Values: []string{poolName},
			},
			{
				Name:   aws.String("tag:runs-fleet:managed"),
				Values: []string{"true"},
			},
			{
				Name:   aws.String("instance-state-name"),
				Values: []string{"pending", "running", "stopping", "stopped"},
			},
		},
	}

	output, err := m.ec2Client.DescribeInstances(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("failed to describe instances: %w", err)
	}

	var instances []PoolInstance
	for _, reservation := range output.Reservations {
		for _, inst := range reservation.Instances {
			instanceType := string(inst.InstanceType)
			instance := PoolInstance{
				InstanceID:   aws.ToString(inst.InstanceId),
				State:        string(inst.State.Name),
				InstanceType: instanceType,
				Spot:         inst.InstanceLifecycle == types.InstanceLifecycleTypeSpot,
			}
			if inst.LaunchTime != nil {
				instance.LaunchTime = *inst.LaunchTime
			}

			// Populate spec fields from InstanceCatalog for multi-spec pool matching
			if spec, ok := fleet.GetInstanceSpec(instanceType); ok {
				instance.CPU = spec.CPU
				instance.RAM = spec.RAM
				instance.Arch = spec.Arch
				instance.Family = spec.Family
				instance.Gen = spec.Gen
			} else {
				poolLog.Warn(ctx, "instance type not in catalog, spec matching may fail",
					slog.String(logging.KeyInstanceID, instance.InstanceID),
					slog.String("instance_type", instanceType))
			}

			// Check idle tracking
			m.idleMu.Lock()
			if idleSince, ok := m.instanceIdle[instance.InstanceID]; ok {
				instance.IdleSince = idleSince
			} else if instance.State == stateRunning {
				// Initialize idle tracking for new running instances
				now := time.Now()
				m.instanceIdle[instance.InstanceID] = now
				instance.IdleSince = now
			}
			m.idleMu.Unlock()

			instances = append(instances, instance)
		}
	}

	return instances, nil
}

func (m *Manager) filterByState(instances []PoolInstance, state string) []PoolInstance {
	var filtered []PoolInstance
	for _, inst := range instances {
		if inst.State == state {
			filtered = append(filtered, inst)
		}
	}
	return filtered
}

func (m *Manager) filterIdleInstances(instances []PoolInstance, idleTimeoutMinutes int) []PoolInstance {
	if idleTimeoutMinutes <= 0 {
		idleTimeoutMinutes = 10 // Default 10 minutes
	}

	threshold := time.Now().Add(-time.Duration(idleTimeoutMinutes) * time.Minute)
	var idle []PoolInstance
	for _, inst := range instances {
		if !inst.IdleSince.IsZero() && inst.IdleSince.Before(threshold) {
			idle = append(idle, inst)
		}
	}
	return idle
}

// filterReadyInstances returns running instances that are not busy (no running job).
// Used for warm pools where we want to stop instances immediately without idle timeout.
func (m *Manager) filterReadyInstances(instances []PoolInstance, busyInstanceIDs []string) []PoolInstance {
	busySet := make(map[string]struct{}, len(busyInstanceIDs))
	for _, id := range busyInstanceIDs {
		busySet[id] = struct{}{}
	}

	var ready []PoolInstance
	for _, inst := range instances {
		if inst.State != stateRunning {
			continue
		}
		if _, isBusy := busySet[inst.InstanceID]; !isBusy {
			ready = append(ready, inst)
		}
	}
	return ready
}

// filterMatchingInstances returns instances whose IDs are in the given list.
func filterMatchingInstances(instances []PoolInstance, instanceIDs []string) []PoolInstance {
	idSet := make(map[string]struct{}, len(instanceIDs))
	for _, id := range instanceIDs {
		idSet[id] = struct{}{}
	}

	var matched []PoolInstance
	for _, inst := range instances {
		if _, ok := idSet[inst.InstanceID]; ok {
			matched = append(matched, inst)
		}
	}
	return matched
}

func (m *Manager) startInstances(ctx context.Context, poolName string, instanceIDs []string, reason string) error {
	if len(instanceIDs) == 0 {
		return nil
	}

	_, err := m.ec2Client.StartInstances(ctx, &ec2.StartInstancesInput{
		InstanceIds: instanceIDs,
	})
	if err != nil {
		return fmt.Errorf("failed to start instances: %w", err)
	}

	m.publishPoolAction(ctx, poolName, poolActionStart, reason, len(instanceIDs))

	poolLog.Info(ctx, "instances started",
		slog.Int(logging.KeyCount, len(instanceIDs)),
		slog.Any("instance_ids", instanceIDs))
	return nil
}

// publishPoolAction emits one pool action metric per affected instance so the
// pool_actions counter reflects instance-level scaling volume.
func (m *Manager) publishPoolAction(ctx context.Context, poolName, action, reason string, count int) {
	if m.metrics == nil {
		return
	}
	for i := 0; i < count; i++ {
		_ = m.metrics.PublishPoolAction(ctx, poolName, action, reason)
	}
}

// isIncorrectInstanceStateError reports whether err is EC2's IncorrectInstanceState,
// returned when an instance is no longer in a state from which it can be stopped
// (already stopping/stopped, or still pending). Modeled on fleet.isCapacityError:
// prefer the typed smithy code, fall back to a substring match.
func isIncorrectInstanceStateError(err error) bool {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		return apiErr.ErrorCode() == "IncorrectInstanceState"
	}
	return strings.Contains(err.Error(), "IncorrectInstanceState")
}

func (m *Manager) stopInstances(ctx context.Context, poolName string, instanceIDs []string, reason string) error {
	if len(instanceIDs) == 0 {
		return nil
	}

	_, err := m.ec2Client.StopInstances(ctx, &ec2.StopInstancesInput{
		InstanceIds: instanceIDs,
	})
	if err != nil {
		return fmt.Errorf("failed to stop instances: %w", err)
	}

	// Remove from idle tracking
	m.forgetInstances(instanceIDs...)

	m.publishPoolAction(ctx, poolName, poolActionStop, reason, len(instanceIDs))

	poolLog.Info(ctx, "instances stopped",
		slog.Int(logging.KeyCount, len(instanceIDs)),
		slog.String("reason", reason),
		slog.Any("instance_ids", instanceIDs))
	return nil
}

func (m *Manager) terminateInstances(ctx context.Context, poolName string, instanceIDs []string, reason string) error {
	if len(instanceIDs) == 0 {
		return nil
	}

	_, err := m.ec2Client.TerminateInstances(ctx, &ec2.TerminateInstancesInput{
		InstanceIds: instanceIDs,
	})
	if err != nil {
		return fmt.Errorf("failed to terminate instances: %w", err)
	}

	// Remove from idle tracking
	m.forgetInstances(instanceIDs...)

	m.publishPoolAction(ctx, poolName, poolActionTerminate, reason, len(instanceIDs))

	poolLog.Info(ctx, "instances terminated",
		slog.Int(logging.KeyCount, len(instanceIDs)),
		slog.String("reason", reason),
		slog.Any("instance_ids", instanceIDs))
	return nil
}

// forgetInstances drops instances from idle tracking under idleMu. It is the one
// place idle entries are deleted outside MarkInstanceIdle, so the stop/terminate/
// claim paths share the same lock discipline instead of hand-rolling it.
func (m *Manager) forgetInstances(instanceIDs ...string) {
	m.idleMu.Lock()
	defer m.idleMu.Unlock()
	for _, id := range instanceIDs {
		delete(m.instanceIdle, id)
		// An instance belongs to a single pool, so clearing it from every pool's
		// streak map is correct and cheap (it exists in at most one).
		for _, streaks := range m.readySince {
			delete(streaks, id)
		}
	}
}

// updateReadySince refreshes a pool's per-instance not-busy streaks: a busy
// instance has its streak cleared; a not-busy instance starts a streak when it has
// none (an existing streak is preserved so the dwell keeps accruing across passes).
// Streaks for instances no longer running in this pool are pruned so the map stays
// bounded. Scoped to poolName so reconciling one pool never disturbs another's
// streaks in the same pass.
func (m *Manager) updateReadySince(poolName string, running []PoolInstance, busyIDs []string, now time.Time) {
	busySet := make(map[string]struct{}, len(busyIDs))
	for _, id := range busyIDs {
		busySet[id] = struct{}{}
	}

	m.idleMu.Lock()
	defer m.idleMu.Unlock()
	streaks := m.readySince[poolName]
	if streaks == nil {
		streaks = make(map[string]time.Time)
		m.readySince[poolName] = streaks
	}
	live := make(map[string]struct{}, len(running))
	for _, inst := range running {
		live[inst.InstanceID] = struct{}{}
		if _, isBusy := busySet[inst.InstanceID]; isBusy {
			delete(streaks, inst.InstanceID)
			continue
		}
		if _, ok := streaks[inst.InstanceID]; !ok {
			streaks[inst.InstanceID] = now
		}
	}
	for id := range streaks {
		if _, ok := live[id]; !ok {
			delete(streaks, id)
		}
	}
}

// readyLongEnough reports whether an instance in the pool has been continuously
// not-busy for at least readyDwellPeriod. A disabled dwell (<=0) always passes; a
// missing streak (never observed not-busy, or just cleared) never passes.
func (m *Manager) readyLongEnough(poolName, instanceID string, now time.Time) bool {
	if m.readyDwellPeriod <= 0 {
		return true
	}
	m.idleMu.Lock()
	defer m.idleMu.Unlock()
	since, ok := m.readySince[poolName][instanceID]
	if !ok {
		return false
	}
	return now.Sub(since) >= m.readyDwellPeriod
}

// MarkInstanceBusy marks an instance as busy (has an assigned job).
func (m *Manager) MarkInstanceBusy(instanceID string) {
	m.forgetInstances(instanceID)
}

// MarkInstanceIdle marks an instance as idle (no assigned job).
func (m *Manager) MarkInstanceIdle(instanceID string) {
	m.idleMu.Lock()
	defer m.idleMu.Unlock()
	m.instanceIdle[instanceID] = time.Now()
}

// ErrNoAvailableInstance is returned when no instance is available in the pool.
var ErrNoAvailableInstance = errors.New("no available instance in pool")

// ErrClaimContextDone is returned when a claim is attempted with an already
// expired or cancelled context. Callers should treat this as retryable (e.g.
// leave the SQS message for redelivery) rather than proceeding to make AWS
// calls that would immediately fail and trigger a DOA cascade.
var ErrClaimContextDone = errors.New("claim aborted: context already done")

// AvailableInstance represents an instance that can be assigned to a job.
type AvailableInstance struct {
	InstanceID   string
	InstanceType string
	State        string // "stopped" or "running"
}

// GetAvailableInstance finds a stopped instance in the pool that can be started for a job.
// Returns ErrNoAvailableInstance if no stopped instances are available.
// Running idle instances are not returned - they require agent changes for reuse.
// Note: This method only finds an instance. Use ClaimAndStartPoolInstance for atomic claim+start.
func (m *Manager) GetAvailableInstance(ctx context.Context, poolName string) (*AvailableInstance, error) {
	if m.ec2Client == nil {
		return nil, fmt.Errorf("EC2 client not configured")
	}

	ctx = logging.ContextWith(ctx, slog.String(logging.KeyPoolName, poolName))

	instances, err := m.getPoolInstances(ctx, poolName)
	if err != nil {
		return nil, fmt.Errorf("failed to get pool instances: %w", err)
	}

	// Find stopped instances (can be started with new config)
	for _, inst := range instances {
		if inst.State == stateStopped {
			return &AvailableInstance{
				InstanceID:   inst.InstanceID,
				InstanceType: inst.InstanceType,
				State:        inst.State,
			}, nil
		}
	}

	return nil, ErrNoAvailableInstance
}

// StartInstanceForJob starts a stopped pool instance for a job.
// The caller must have already updated the instance's runner config in SSM.
// The repo parameter is used to tag the instance with the Role tag for cost allocation.
// Returns the instance ID on success.
// Note: Prefer ClaimAndStartPoolInstance for atomic claim+start to avoid race conditions.
func (m *Manager) StartInstanceForJob(ctx context.Context, instanceID, repo string) error {
	if m.ec2Client == nil {
		return fmt.Errorf("EC2 client not configured")
	}

	ctx = logging.ContextWith(ctx, slog.String(logging.KeyInstanceID, instanceID))

	_, err := m.ec2Client.StartInstances(ctx, &ec2.StartInstancesInput{
		InstanceIds: []string{instanceID},
	})
	if err != nil {
		return fmt.Errorf("failed to start instance %s: %w", instanceID, err)
	}

	// Tag the instance with Role for cost allocation
	// Pool instances are created without this tag since the repo is unknown at creation time
	if repo != "" {
		_, err := m.ec2Client.CreateTags(ctx, &ec2.CreateTagsInput{
			Resources: []string{instanceID},
			Tags: []types.Tag{
				{
					Key:   aws.String("Role"),
					Value: aws.String(repo),
				},
			},
		})
		if err != nil {
			// Log but don't fail - tagging is for cost allocation, not critical path
			poolLog.Warn(ctx, "failed to tag instance with Role",
				slog.String("error", err.Error()))
		}
	}

	// Mark as busy immediately to prevent reconciler from touching it
	m.forgetInstances(instanceID)

	poolLog.Info(ctx, "pool instance started for job")
	return nil
}

// instanceClaimTTL is the TTL for instance claims in DynamoDB.
// Set to 5 minutes to allow for EC2 start + SSM config + agent boot.
const instanceClaimTTL = 5 * time.Minute

// ClaimAndStartPoolInstance atomically finds, claims, and starts a stopped pool instance.
// Returns the claimed instance on success, ErrNoAvailableInstance if no instance available,
// or ErrClaimContextDone (retryable) if the caller's budget is already spent on entry.
//
// The jobID parameter is used for distributed locking via DynamoDB conditional writes.
// This prevents race conditions in multi-orchestrator deployments where multiple
// orchestrators might try to claim the same stopped instance for different jobs.
//
// The repo parameter is used to tag the instance with the Role tag for cost allocation.
// Pool instances are created without knowing which repo will use them, so the tag is
// applied when the instance is assigned to a job.
//
// The spec parameter is used for multi-spec pool matching. If non-nil, only instances
// that satisfy the spec requirements are considered. Matching instances are sorted
// by CPU ascending (best-fit: smallest instance that meets requirements).
// If nil, any stopped instance is eligible (legacy behavior).
//
// Concurrency: this method never holds an in-memory lock across an AWS or DynamoDB
// call. Candidate selection takes a short per-pool lock to mark the chosen instance
// in-flight (so two local goroutines never attempt the same one), then releases it
// before claiming in DynamoDB and starting the instance. The DynamoDB conditional
// write in ClaimInstanceForJob remains the authoritative cross-process guard against
// double-claims. Different pools use independent locks and do not serialize.
//
// The claim flow is:
//  1. Reject immediately if the context is already done (retryable; avoids DOA AWS calls)
//  2. Find all stopped instances in the pool (DescribeInstances, unlocked)
//  3. Filter by spec requirements (if spec is provided) and sort by CPU ascending
//  4. For each candidate: reserve it locally under a short per-pool lock, then
//     atomically claim it in DynamoDB (unlocked); on conflict, release the local
//     reservation and try the next candidate
//  5. Start the EC2 instance and tag it with the repo (unlocked)
//  6. If start fails, release the DynamoDB claim and return error
//
// The caller is responsible for releasing the claim after job completion or failure
// by calling the DB's ReleaseInstanceClaim method.
func (m *Manager) ClaimAndStartPoolInstance(ctx context.Context, poolName string, jobID int64, repo string, spec *fleet.FlexibleSpec) (*AvailableInstance, error) {
	if m.dbClient == nil {
		return nil, fmt.Errorf("DB client not configured for distributed locking")
	}

	// Dead-context guard: if the budget is already spent, fail fast and retryable
	// instead of issuing AWS calls that would error at 0ms and cascade.
	if err := ctx.Err(); err != nil {
		poolLog.Warn(ctx, "claim aborted: context already done on entry",
			slog.String(logging.KeyPoolName, poolName),
			slog.String("error", err.Error()))
		return nil, fmt.Errorf("%w: %w", ErrClaimContextDone, err)
	}

	ctx = logging.ContextWith(ctx, slog.String(logging.KeyPoolName, poolName))

	// Get all stopped instances in the pool (no lock held across this AWS call).
	instances, err := m.getPoolInstances(ctx, poolName)
	if err != nil {
		return nil, fmt.Errorf("failed to get pool instances: %w", err)
	}

	// Filter stopped instances matching the spec.
	var candidates []PoolInstance
	for _, inst := range instances {
		if inst.State != stateStopped {
			continue
		}
		if !inst.matchesFlexibleSpec(spec) {
			continue
		}
		candidates = append(candidates, inst)
	}

	// Sort by CPU ascending (best-fit: smallest instance that meets requirements).
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].CPU != candidates[j].CPU {
			return candidates[i].CPU < candidates[j].CPU
		}
		return candidates[i].RAM < candidates[j].RAM
	})

	pl := m.poolLockFor(poolName)

	// Time the DynamoDB claim acquire across all candidates: from the first
	// attempt to either a successful claim or exhaustion. This is the cross-
	// instance contention signal (the conditional write is the authoritative
	// guard); emitted once per claim attempt regardless of outcome.
	claimStart := time.Now()
	claimEmitted := false
	emitClaimWait := func() {
		if !claimEmitted && m.metrics != nil {
			_ = m.metrics.PublishLockWaitSeconds(ctx, lockClaim, time.Since(claimStart).Seconds())
		}
		claimEmitted = true
	}
	defer emitClaimWait()

	// Try to claim and start each candidate instance.
	for _, inst := range candidates {
		// Reserve the candidate locally under a short per-pool lock so two
		// goroutines in this process never attempt the same instance. The lock
		// is released before any AWS/DynamoDB call.
		if !pl.reserve(inst.InstanceID) {
			// Another local goroutine is already attempting this instance.
			continue
		}

		instance := &AvailableInstance{
			InstanceID:   inst.InstanceID,
			InstanceType: inst.InstanceType,
			State:        inst.State,
		}

		// Atomically claim this instance in DynamoDB (authoritative guard).
		if err := m.dbClient.ClaimInstanceForJob(ctx, instance.InstanceID, jobID, instanceClaimTTL); err != nil {
			pl.release(instance.InstanceID)
			if errors.Is(err, db.ErrInstanceAlreadyClaimed) {
				continue
			}
			return nil, fmt.Errorf("failed to claim instance in DynamoDB: %w", err)
		}
		// Claim acquired: record the wait now so the subsequent StartInstances
		// call is not counted as lock-wait time.
		emitClaimWait()

		// Start the instance. The DynamoDB claim holds the cross-process lock;
		// the local reservation can be dropped now that the claim is authoritative.
		if err := m.StartInstanceForJob(ctx, instance.InstanceID, repo); err != nil {
			// Release the DynamoDB claim since we failed to start.
			if releaseErr := m.dbClient.ReleaseInstanceClaim(ctx, instance.InstanceID, jobID); releaseErr != nil {
				poolLog.Error(ctx, "instance claim release failed after start failure",
					slog.String(logging.KeyInstanceID, instance.InstanceID),
					slog.String("error", releaseErr.Error()))
			}
			pl.release(instance.InstanceID)
			return nil, fmt.Errorf("failed to start claimed instance: %w", err)
		}

		pl.release(instance.InstanceID)
		return instance, nil
	}

	return nil, ErrNoAvailableInstance
}

// reserve marks an instance as being claimed by a local goroutine. It returns
// false if another local goroutine has already reserved it. The critical section
// is in-memory only and never spans an AWS or DynamoDB call.
func (pl *poolLock) reserve(instanceID string) bool {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	if _, ok := pl.inFlight[instanceID]; ok {
		return false
	}
	pl.inFlight[instanceID] = struct{}{}
	return true
}

// release clears a local reservation for an instance.
func (pl *poolLock) release(instanceID string) {
	pl.mu.Lock()
	delete(pl.inFlight, instanceID)
	pl.mu.Unlock()
}

// filterNotInFlight returns the subset of instances that are not currently
// reserved by a local claim. Used by reconciliation to avoid terminating an
// instance a concurrent claim has selected. The critical section is in-memory
// only.
func (pl *poolLock) filterNotInFlight(instances []PoolInstance) []PoolInstance {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	if len(pl.inFlight) == 0 {
		return instances
	}
	filtered := make([]PoolInstance, 0, len(instances))
	for _, inst := range instances {
		if _, ok := pl.inFlight[inst.InstanceID]; ok {
			continue
		}
		filtered = append(filtered, inst)
	}
	return filtered
}

// StopPoolInstance stops a pool instance (e.g., after failed SSM config).
func (m *Manager) StopPoolInstance(ctx context.Context, instanceID string) error {
	if m.ec2Client == nil {
		return fmt.Errorf("EC2 client not configured")
	}

	ctx = logging.ContextWith(ctx, slog.String(logging.KeyInstanceID, instanceID))

	_, err := m.ec2Client.StopInstances(ctx, &ec2.StopInstancesInput{
		InstanceIds: []string{instanceID},
	})
	if err != nil {
		return fmt.Errorf("failed to stop instance %s: %w", instanceID, err)
	}

	// Remove from busy tracking
	m.forgetInstances(instanceID)

	poolLog.Info(ctx, "pool instance stopped")
	return nil
}

// countInstanceStates counts running, stopped, busy, and ready instances.
// Busy count is intersected with actual pool instances to prevent stale job records
// from inflating the count and triggering runaway scaling.
func countInstanceStates(instances []PoolInstance, busyIDs []string) (running, stopped, busy, ready int) {
	for _, inst := range instances {
		switch inst.State {
		case stateRunning:
			running++
		case stateStopped:
			stopped++
		}
	}

	busySet := make(map[string]struct{}, len(busyIDs))
	for _, id := range busyIDs {
		busySet[id] = struct{}{}
	}
	for _, inst := range instances {
		if inst.State != stateRunning {
			continue
		}
		if _, ok := busySet[inst.InstanceID]; ok {
			busy++
		}
	}
	ready = running - busy
	return
}

// createPoolFleetInstances creates new on-demand instances for a pool.
// Returns the number of instances successfully created.
// Uses on-demand instances for warm pools because stop/start reliability
// matters more than spot savings on short-lived job execution.
func (m *Manager) createPoolFleetInstances(ctx context.Context, poolName, reason string, count int, poolConfig *db.PoolConfig) int {
	instanceTypes, arch := resolvePoolInstanceTypes(ctx, poolConfig)
	if len(instanceTypes) == 0 {
		poolLog.Warn(ctx, "no instance types resolved")
		return 0
	}

	ranked := m.fleetManager.RankInstanceTypesByPrice(ctx, instanceTypes)

	var created int
	for i := 0; i < count; i++ {
		subnetID := m.selectSubnet()
		if subnetID == "" {
			poolLog.Error(ctx, "no subnets configured for pool")
			break
		}
		spec := &fleet.LaunchSpec{
			RunID:        time.Now().UnixNano(),
			InstanceType: ranked[m.randIntn(len(ranked))],
			SubnetID:     subnetID,
			SubnetIDs:    m.config.SubnetIDs,
			Pool:         poolName,
			Arch:         arch,
			Reason:       reason,
		}
		if _, err := m.fleetManager.CreateOnDemandInstance(ctx, spec); err != nil {
			poolLog.Error(ctx, "on-demand instance creation failed",
				slog.String("error", err.Error()))
			continue
		}
		created++
		m.publishPoolAction(ctx, poolName, poolActionCreate, reason, 1)
	}
	return created
}

// resolvePoolInstanceTypes resolves instance types for a pool configuration.
// For ephemeral pools with flexible specs, uses GitHub's resolution logic.
// For legacy pools with a single InstanceType, returns that type.
func resolvePoolInstanceTypes(ctx context.Context, poolConfig *db.PoolConfig) ([]string, string) {
	// If pool has flexible specs, resolve them
	if poolConfig.CPUMin > 0 || poolConfig.CPUMax > 0 {
		jobConfig := &github.JobConfig{
			Arch:     poolConfig.Arch,
			CPUMin:   poolConfig.CPUMin,
			CPUMax:   poolConfig.CPUMax,
			RAMMin:   poolConfig.RAMMin,
			RAMMax:   poolConfig.RAMMax,
			Families: poolConfig.Families,
		}
		if err := github.ResolveFlexibleSpec(jobConfig); err != nil {
			poolLog.Error(ctx, "flexible spec resolution failed", slog.String("error", err.Error()))
			// Fall back to a pinned InstanceType if one exists. Ephemeral pools no
			// longer pin one (they carry only the flexible spec), so this only helps
			// legacy admin-created pools; for ephemeral pools an unsatisfiable spec
			// yields no launch this cycle (resolution is deterministic over a static
			// catalog, so this effectively never triggers).
			if poolConfig.InstanceType != "" {
				return []string{poolConfig.InstanceType}, poolConfig.Arch
			}
			return nil, ""
		}
		return jobConfig.InstanceTypes, jobConfig.Arch
	}

	// Legacy pool with single instance type
	if poolConfig.InstanceType != "" {
		return []string{poolConfig.InstanceType}, poolConfig.Arch
	}

	return nil, ""
}
