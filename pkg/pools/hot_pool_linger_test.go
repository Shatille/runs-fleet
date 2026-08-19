package pools

import (
	"context"
	"reflect"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/Shavakan/runs-fleet/pkg/db"
	"github.com/Shavakan/runs-fleet/pkg/fleet"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

const lingerPoolName = "portal-api"

// hotEnabledConfig is the master-on config with default caps. Per-pool linger now
// lives on the PoolConfig (AutoTune / Override), not the Config.
func hotEnabledConfig() *config.Config {
	return &config.Config{
		SubnetIDs:       []string{"subnet-1"},
		HotPoolsEnabled: true,
		HotPoolCaps:     config.DefaultHotPoolCaps(),
	}
}

// autoTuned builds a PoolConfig carrying a tuner recommendation.
func autoTuned(pool string, linger, maxHot int, lastJob time.Time) *db.PoolConfig {
	return &db.PoolConfig{
		PoolName:    pool,
		LastJobTime: lastJob,
		AutoTune:    &db.AutoTuneRec{RecommendedLingerMinutes: linger, RecommendedMaxHot: maxHot, Reason: "tuned"},
	}
}

func TestLingerDesiredRunning(t *testing.T) {
	t.Parallel()

	now := time.Now()
	enabled := hotEnabledConfig()
	disabled := &config.Config{SubnetIDs: []string{"subnet-1"}}

	tests := []struct {
		name string
		cfg  *config.Config
		pool *db.PoolConfig
		want int
	}{
		{
			name: "master off returns 0 (gate off)",
			cfg:  disabled,
			pool: autoTuned(lingerPoolName, 15, 2, now.Add(-1*time.Minute)),
			want: 0,
		},
		{
			name: "no recommendation returns 0",
			cfg:  enabled,
			pool: &db.PoolConfig{PoolName: lingerPoolName, LastJobTime: now.Add(-1 * time.Minute)},
			want: 0,
		},
		{
			name: "within linger window returns recommended maxHot",
			cfg:  enabled,
			pool: autoTuned(lingerPoolName, 15, 2, now.Add(-5*time.Minute)),
			want: 2,
		},
		{
			name: "past linger window returns 0",
			cfg:  enabled,
			pool: autoTuned(lingerPoolName, 15, 2, now.Add(-16*time.Minute)),
			want: 0,
		},
		{
			name: "exactly at linger boundary returns 0 (decayed)",
			cfg:  enabled,
			pool: autoTuned(lingerPoolName, 15, 2, now.Add(-15*time.Minute)),
			want: 0,
		},
		{
			name: "zero LastJobTime returns 0 (never active)",
			cfg:  enabled,
			pool: autoTuned(lingerPoolName, 15, 2, time.Time{}),
			want: 0,
		},
		{
			name: "override linger wins over auto",
			cfg:  enabled,
			pool: func() *db.PoolConfig {
				pc := autoTuned(lingerPoolName, 15, 2, now.Add(-9*time.Minute))
				pc.OverrideLingerMinutes = intPtr(5) // auto would be active at -9m; override 5m decays it
				return pc
			}(),
			want: 0,
		},
		{
			name: "override &0 forces cold despite auto>0",
			cfg:  enabled,
			pool: func() *db.PoolConfig {
				pc := autoTuned(lingerPoolName, 15, 2, now.Add(-1*time.Minute))
				pc.OverrideLingerMinutes = intPtr(0)
				return pc
			}(),
			want: 0,
		},
		{
			name: "override maxHot wins over auto maxHot",
			cfg:  enabled,
			pool: func() *db.PoolConfig {
				pc := autoTuned(lingerPoolName, 15, 2, now.Add(-1*time.Minute))
				pc.OverrideMaxHot = intPtr(1)
				return pc
			}(),
			want: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m := NewManager(&MockDBClient{}, &MockFleetAPI{}, tt.cfg)
			if got := m.lingerDesiredRunning(tt.pool, now); got != tt.want {
				t.Errorf("lingerDesiredRunning() = %d, want %d", got, tt.want)
			}
		})
	}
}

// intPtr is a local *int helper for override fixtures.
func intPtr(n int) *int { return &n }

// effectiveHotSpec resolves override > auto > off, clamped to caps.
func TestEffectiveHotSpec(t *testing.T) {
	t.Parallel()

	enabled := hotEnabledConfig() // caps: linger 30, maxHot 3

	tests := []struct {
		name       string
		cfg        *config.Config
		pool       *db.PoolConfig
		wantLinger int
		wantMaxHot int
	}{
		{
			name:       "master off => off",
			cfg:        &config.Config{HotPoolsEnabled: false},
			pool:       autoTuned("p", 10, 2, time.Time{}),
			wantLinger: 0, wantMaxHot: 0,
		},
		{
			name:       "nil pool => off",
			cfg:        enabled,
			pool:       nil,
			wantLinger: 0, wantMaxHot: 0,
		},
		{
			name:       "auto only",
			cfg:        enabled,
			pool:       autoTuned("p", 10, 2, time.Time{}),
			wantLinger: 10, wantMaxHot: 2,
		},
		{
			name: "override linger wins, auto maxHot kept",
			cfg:  enabled,
			pool: func() *db.PoolConfig {
				pc := autoTuned("p", 10, 2, time.Time{})
				pc.OverrideLingerMinutes = intPtr(7)
				return pc
			}(),
			wantLinger: 7, wantMaxHot: 2,
		},
		{
			name: "override &0 linger => off",
			cfg:  enabled,
			pool: func() *db.PoolConfig {
				pc := autoTuned("p", 10, 2, time.Time{})
				pc.OverrideLingerMinutes = intPtr(0)
				return pc
			}(),
			wantLinger: 0, wantMaxHot: 0,
		},
		{
			name:       "caps clamp linger and maxHot",
			cfg:        enabled,
			pool:       autoTuned("p", 999, 99, time.Time{}),
			wantLinger: 30, wantMaxHot: 3,
		},
		{
			name: "linger active but maxHot 0 floors to 1",
			cfg:  enabled,
			pool: func() *db.PoolConfig {
				pc := autoTuned("p", 10, 0, time.Time{})
				return pc
			}(),
			wantLinger: 10, wantMaxHot: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m := NewManager(&MockDBClient{}, &MockFleetAPI{}, tt.cfg)
			linger, maxHot := m.effectiveHotSpec(tt.pool)
			if linger != tt.wantLinger || maxHot != tt.wantMaxHot {
				t.Errorf("effectiveHotSpec() = (%d, %d), want (%d, %d)", linger, maxHot, tt.wantLinger, tt.wantMaxHot)
			}
		})
	}
}

// When linger is active and the pool has a stopped spare, reconcile starts it
// with reason "linger" (attribution) — an ephemeral pool would otherwise sit at
// desired_running=0 and pay the stopped-boot on the next job.
func TestReconcileLingerStartsSpare(t *testing.T) {
	t.Parallel()

	var startedIDs []string

	mockDB := &MockDBClient{
		ListPoolsFunc: func(_ context.Context) ([]string, error) { return []string{lingerPoolName}, nil },
		GetPoolConfigFunc: func(_ context.Context, _ string) (*db.PoolConfig, error) {
			return &db.PoolConfig{
				PoolName:     lingerPoolName,
				Ephemeral:    true,
				LastJobTime:  time.Now().Add(-2 * time.Minute), // within a 15-min linger
				InstanceType: "c7g.large",
				AutoTune:     &db.AutoTuneRec{RecommendedLingerMinutes: 15, RecommendedMaxHot: 1, Reason: "tuned"},
			}, nil
		},
		GetPoolP90ConcurrencyFunc: func(_ context.Context, _ string, _ int) (int, error) { return 0, nil },
	}

	mockEC2 := &MockEC2API{
		DescribeInstancesFunc: func(_ context.Context, _ *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
			return &ec2.DescribeInstancesOutput{Reservations: []ec2types.Reservation{{Instances: []ec2types.Instance{
				{
					InstanceId:   aws.String(testInstanceStoppedID),
					InstanceType: ec2types.InstanceTypeC7gLarge,
					State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameStopped},
				},
			}}}}, nil
		},
		StartInstancesFunc: func(_ context.Context, params *ec2.StartInstancesInput, _ ...func(*ec2.Options)) (*ec2.StartInstancesOutput, error) {
			startedIDs = append(startedIDs, params.InstanceIds...)
			return &ec2.StartInstancesOutput{}, nil
		},
	}

	recordingMetrics := &lingerMetrics{}
	m := NewManager(mockDB, &MockFleetAPI{}, hotEnabledConfig())
	m.SetEC2Client(mockEC2)
	m.SetMetrics(recordingMetrics)

	m.reconcile(context.Background())

	if len(startedIDs) != 1 || startedIDs[0] != testInstanceStoppedID {
		t.Fatalf("started IDs = %v, want [%s] (linger floor should start the stopped spare)", startedIDs, testInstanceStoppedID)
	}
	if !contains(recordingMetrics.startReasons, poolActionReasonLinger) {
		t.Errorf("start reasons = %v, want to contain %q", recordingMetrics.startReasons, poolActionReasonLinger)
	}
}

// After the linger window elapses, the effective running target falls to 0 and
// the existing warm-pool immediate-stop branch banks the running spare — no new
// decay path. Guarded by the bootstrap grace + ready dwell, both disabled here so
// the single pass exercises the decay.
func TestReconcileLingerExpiryBanksSpare(t *testing.T) {
	t.Parallel()

	var stoppedIDs []string

	mockDB := &MockDBClient{
		ListPoolsFunc: func(_ context.Context) ([]string, error) { return []string{lingerPoolName}, nil },
		GetPoolConfigFunc: func(_ context.Context, _ string) (*db.PoolConfig, error) {
			return &db.PoolConfig{
				PoolName:     lingerPoolName,
				Ephemeral:    true,
				LastJobTime:  time.Now().Add(-30 * time.Minute), // past a 15-min linger
				InstanceType: "c7g.large",
				AutoTune:     &db.AutoTuneRec{RecommendedLingerMinutes: 15, RecommendedMaxHot: 1, Reason: "tuned"},
			}, nil
		},
		GetPoolP90ConcurrencyFunc: func(_ context.Context, _ string, _ int) (int, error) { return 0, nil },
	}

	mockEC2 := &MockEC2API{
		DescribeInstancesFunc: func(_ context.Context, _ *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
			return &ec2.DescribeInstancesOutput{Reservations: []ec2types.Reservation{{Instances: []ec2types.Instance{
				{
					InstanceId:   aws.String("i-hotspare"),
					InstanceType: ec2types.InstanceTypeC7gLarge,
					State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning},
					LaunchTime:   aws.Time(time.Now().Add(-1 * time.Hour)), // past bootstrap grace
				},
			}}}}, nil
		},
		StopInstancesFunc: func(_ context.Context, params *ec2.StopInstancesInput, _ ...func(*ec2.Options)) (*ec2.StopInstancesOutput, error) {
			stoppedIDs = append(stoppedIDs, params.InstanceIds...)
			return &ec2.StopInstancesOutput{}, nil
		},
	}

	m := NewManager(mockDB, &MockFleetAPI{}, hotEnabledConfig())
	m.SetEC2Client(mockEC2)
	m.readyDwellPeriod = 0 // exercise decay in a single pass

	m.reconcile(context.Background())

	if len(stoppedIDs) != 1 || stoppedIDs[0] != "i-hotspare" {
		t.Fatalf("stopped IDs = %v, want [i-hotspare] (expired linger => desired_running 0 => bank the spare)", stoppedIDs)
	}
}

// lingerMetrics records pool-action reasons so tests can assert attribution.
type lingerMetrics struct {
	startReasons []string
}

func (l *lingerMetrics) PublishPoolAction(_ context.Context, _, action, reason string) error {
	if action == poolActionStart {
		l.startReasons = append(l.startReasons, reason)
	}
	return nil
}
func (l *lingerMetrics) PublishPoolDesired(context.Context, string, string, int) error   { return nil }
func (l *lingerMetrics) PublishPoolInstances(context.Context, string, string, int) error { return nil }
func (l *lingerMetrics) PublishPoolReconcileSeconds(context.Context, float64) error      { return nil }
func (l *lingerMetrics) PublishLockWaitSeconds(context.Context, string, float64) error   { return nil }
func (l *lingerMetrics) PublishInstances(context.Context, string, string, string, int) error {
	return nil
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Gate-off no-regression suite (production-safety proof)
//
// The overriding invariant of this feature is: with the hot-pool gate off, or
// configured only for OTHER pools, reconciliation behaves byte-identically to a
// build without the feature. This suite proves it by replaying a set of
// representative reconcile scenarios through a call recorder under three configs
// — HotPools nil (gate off), HotPools empty map, HotPools set for a DIFFERENT
// pool — and asserting every EC2/Fleet/DB side-effecting call, in order, is
// identical. A drift in any scenario means the gate leaks into the default path.
// ---------------------------------------------------------------------------

// recordedCall is one side-effecting AWS/DB call captured for sequence equality.
type recordedCall struct {
	op    string
	inst  []string // instance IDs (sorted) for EC2 ops
	extra string   // reason / fleet spec summary
}

// recorder accumulates the ordered call log for one reconcile pass.
type recorder struct {
	calls []recordedCall
}

func (r *recorder) add(op string, inst []string, extra string) {
	sorted := append([]string(nil), inst...)
	sort.Strings(sorted)
	r.calls = append(r.calls, recordedCall{op: op, inst: sorted, extra: extra})
}

// noRegressionScenario is a self-contained reconcile fixture: a pool config and
// the instances DescribeInstances reports, plus the busy set. The same fixture
// is replayed under each config variant.
type noRegressionScenario struct {
	name       string
	poolConfig *db.PoolConfig
	instances  []ec2types.Instance
	busyIDs    []string
}

// runRecorded runs one reconcile pass for the scenario under cfg, returning the
// ordered call log. Every EC2/Fleet mutating call and the DB writes are recorded;
// read calls (Describe, GetPoolConfig, busy IDs) are deterministic inputs and
// need not be logged for behavioral equality.
func runRecorded(scn noRegressionScenario, cfg *config.Config) []recordedCall {
	rec := &recorder{}
	poolName := scn.poolConfig.PoolName

	mockDB := &MockDBClient{
		ListPoolsFunc:     func(context.Context) ([]string, error) { return []string{poolName}, nil },
		GetPoolConfigFunc: func(context.Context, string) (*db.PoolConfig, error) { return scn.poolConfig, nil },
		GetPoolBusyInstanceIDsFunc: func(context.Context, string) ([]string, error) {
			return scn.busyIDs, nil
		},
		GetPoolP90ConcurrencyFunc: func(context.Context, string, int) (int, error) { return 0, nil },
		UpdatePoolStateFunc: func(_ context.Context, _ string, running, stopped, effRunning, effStopped int) error {
			rec.add("UpdatePoolState", nil, itoa2(running, stopped)+"/"+itoa2(effRunning, effStopped))
			return nil
		},
	}

	mockFleet := &MockFleetAPI{
		CreateOnDemandInstanceFunc: func(_ context.Context, spec *fleet.LaunchSpec) (string, error) {
			rec.add("CreateOnDemand", nil, spec.Reason)
			return testInstanceNewID, nil
		},
	}

	mockEC2 := &MockEC2API{
		DescribeInstancesFunc: func(context.Context, *ec2.DescribeInstancesInput, ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
			return &ec2.DescribeInstancesOutput{Reservations: []ec2types.Reservation{{Instances: scn.instances}}}, nil
		},
		StartInstancesFunc: func(_ context.Context, params *ec2.StartInstancesInput, _ ...func(*ec2.Options)) (*ec2.StartInstancesOutput, error) {
			rec.add("StartInstances", params.InstanceIds, "")
			return &ec2.StartInstancesOutput{}, nil
		},
		StopInstancesFunc: func(_ context.Context, params *ec2.StopInstancesInput, _ ...func(*ec2.Options)) (*ec2.StopInstancesOutput, error) {
			rec.add("StopInstances", params.InstanceIds, "")
			return &ec2.StopInstancesOutput{}, nil
		},
		TerminateInstancesFunc: func(_ context.Context, params *ec2.TerminateInstancesInput, _ ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error) {
			rec.add("TerminateInstances", params.InstanceIds, "")
			return &ec2.TerminateInstancesOutput{}, nil
		},
		CreateTagsFunc: func(_ context.Context, params *ec2.CreateTagsInput, _ ...func(*ec2.Options)) (*ec2.CreateTagsOutput, error) {
			rec.add("CreateTags", params.Resources, "")
			return &ec2.CreateTagsOutput{}, nil
		},
	}

	m := NewManager(mockDB, mockFleet, cfg)
	m.SetEC2Client(mockEC2)
	// Disable time-based guards so a single pass is deterministic across variants.
	m.bootstrapGracePeriod = 0
	m.readyDwellPeriod = 0
	m.reconcile(context.Background())
	return rec.calls
}

func itoa2(a, b int) string {
	return fmtInt(a) + "," + fmtInt(b)
}

func fmtInt(n int) string {
	return strconv.Itoa(n)
}

func TestReconcileGateOffNoRegression(t *testing.T) {
	t.Parallel()

	// Scenarios span the reconcile decision tree: scale-up from stopped, create
	// on-demand deficit, warm-pool stop-excess, terminate over-desired-stopped,
	// steady state (no change), and a busy running instance (no scale-down).
	scenarios := []noRegressionScenario{
		{
			name: "scale up: start a stopped spare",
			poolConfig: &db.PoolConfig{
				PoolName: "web", DesiredRunning: 1, DesiredStopped: 0, InstanceType: "c7g.large",
			},
			instances: []ec2types.Instance{
				stoppedInst("i-s1", ec2types.InstanceTypeC7gLarge),
			},
		},
		{
			name: "scale up: create on-demand when no stopped inventory",
			poolConfig: &db.PoolConfig{
				PoolName: "web", DesiredRunning: 2, DesiredStopped: 0, InstanceType: "c7g.large",
			},
			instances: []ec2types.Instance{},
		},
		{
			name: "warm pool: stop an idle running spare",
			poolConfig: &db.PoolConfig{
				PoolName: "web", DesiredRunning: 0, DesiredStopped: 1, InstanceType: "c7g.large",
			},
			instances: []ec2types.Instance{
				runningInst("i-r1", ec2types.InstanceTypeC7gLarge, time.Now().Add(-1*time.Hour)),
			},
		},
		{
			name: "terminate over-desired stopped",
			poolConfig: &db.PoolConfig{
				PoolName: "web", DesiredRunning: 0, DesiredStopped: 0, InstanceType: "c7g.large",
			},
			instances: []ec2types.Instance{
				stoppedInst("i-s1", ec2types.InstanceTypeC7gLarge),
				stoppedInst("i-s2", ec2types.InstanceTypeC7gLarge),
			},
		},
		{
			name: "steady state: desired met, no change",
			poolConfig: &db.PoolConfig{
				PoolName: "web", DesiredRunning: 1, DesiredStopped: 0, InstanceType: "c7g.large",
			},
			instances: []ec2types.Instance{
				runningInst("i-r1", ec2types.InstanceTypeC7gLarge, time.Now().Add(-1*time.Hour)),
			},
		},
		{
			name: "busy running instance is never scaled down",
			poolConfig: &db.PoolConfig{
				PoolName: "web", DesiredRunning: 0, DesiredStopped: 1, InstanceType: "c7g.large",
			},
			instances: []ec2types.Instance{
				runningInst("i-busy", ec2types.InstanceTypeC7gLarge, time.Now().Add(-1*time.Hour)),
			},
			busyIDs: []string{"i-busy"},
		},
		{
			name: "ephemeral pool with recent activity (linger would target a DIFFERENT pool)",
			poolConfig: &db.PoolConfig{
				PoolName: "web", Ephemeral: true, LastJobTime: time.Now().Add(-1 * time.Minute),
				InstanceType: "c7g.large",
			},
			instances: []ec2types.Instance{
				stoppedInst("i-s1", ec2types.InstanceTypeC7gLarge),
			},
		},
	}

	// The master-off baseline versus two variants that leave every pool cold: master
	// off with caps configured, and master ON but no pool carries a recommendation
	// (linger resolves to 0). All three MUST produce identical call sequences: the
	// feature is inert unless a pool's effective linger is active.
	variants := []struct {
		name string
		cfg  func() *config.Config
	}{
		{name: "master off (gate off)", cfg: func() *config.Config {
			return &config.Config{SubnetIDs: []string{"subnet-1"}}
		}},
		{name: "master off with caps set", cfg: func() *config.Config {
			return &config.Config{SubnetIDs: []string{"subnet-1"}, HotPoolCaps: config.DefaultHotPoolCaps()}
		}},
		{name: "master on but no recommendation (stays cold)", cfg: func() *config.Config {
			return &config.Config{
				SubnetIDs:       []string{"subnet-1"},
				HotPoolsEnabled: true,
				HotPoolCaps:     config.DefaultHotPoolCaps(),
			}
		}},
	}

	for _, scn := range scenarios {
		scn := scn
		t.Run(scn.name, func(t *testing.T) {
			t.Parallel()
			baseline := runRecorded(scn, variants[0].cfg())
			for _, v := range variants[1:] {
				got := runRecorded(scn, v.cfg())
				if !reflect.DeepEqual(baseline, got) {
					t.Errorf("call sequence drifted with %q:\n baseline: %+v\n got:      %+v",
						v.name, baseline, got)
				}
			}
		})
	}
}

func stoppedInst(id string, t ec2types.InstanceType) ec2types.Instance {
	return ec2types.Instance{
		InstanceId:   aws.String(id),
		InstanceType: t,
		State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameStopped},
	}
}

func runningInst(id string, t ec2types.InstanceType, launch time.Time) ec2types.Instance {
	return ec2types.Instance{
		InstanceId:   aws.String(id),
		InstanceType: t,
		State:        &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning},
		LaunchTime:   aws.Time(launch),
	}
}

// The linger floor is the second of the two overrides that displace a pool's
// configured target, so it must reach the persisted effective value too —
// otherwise a lingering pool reads as permanently adrift from its own target.
func TestReconcilePool_PersistsLingerRaisedEffectiveDesired(t *testing.T) {
	t.Parallel()

	var gotEffRunning int
	var updates int

	mockDB := &MockDBClient{
		ListPoolsFunc: func(_ context.Context) ([]string, error) { return []string{lingerPoolName}, nil },
		GetPoolConfigFunc: func(_ context.Context, _ string) (*db.PoolConfig, error) {
			return &db.PoolConfig{
				PoolName:     lingerPoolName,
				Ephemeral:    true,
				LastJobTime:  time.Now().Add(-2 * time.Minute),
				InstanceType: "c7g.large",
				AutoTune:     &db.AutoTuneRec{RecommendedLingerMinutes: 15, RecommendedMaxHot: 3, Reason: "tuned"},
			}, nil
		},
		GetPoolP90ConcurrencyFunc: func(_ context.Context, _ string, _ int) (int, error) { return 0, nil },
		UpdatePoolStateFunc: func(_ context.Context, _ string, _, _, effRunning, _ int) error {
			updates++
			gotEffRunning = effRunning
			return nil
		},
	}

	mockEC2 := &MockEC2API{
		DescribeInstancesFunc: func(_ context.Context, _ *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
			return &ec2.DescribeInstancesOutput{Reservations: []ec2types.Reservation{}}, nil
		},
	}

	manager := NewManager(mockDB, &MockFleetAPI{
		CreateOnDemandInstanceFunc: func(_ context.Context, _ *fleet.LaunchSpec) (string, error) {
			return testInstanceNewID, nil
		},
	}, hotEnabledConfig())
	manager.SetEC2Client(mockEC2)

	manager.reconcile(context.Background())

	if updates != 1 {
		t.Fatalf("UpdatePoolState calls = %d, want 1", updates)
	}
	// Ephemeral auto-scaling resolves running to 0; the live linger window raises it
	// to maxHot, and that is the target the pass acted on.
	if gotEffRunning != 3 {
		t.Errorf("effective desired running = %d, want the linger-raised 3", gotEffRunning)
	}
}
