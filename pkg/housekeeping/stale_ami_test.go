package housekeeping

import (
	"context"
	"errors"
	"testing"

	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/aws/aws-sdk-go-v2/aws"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

const (
	amiCurrentARM64 = "ami-02078444e5f1bbf2b"
	amiCurrentAMD64 = "ami-0bc3c3b60cc3576fb"
	amiStaleARM64   = "ami-01d3832c683462b85"
	stateFilterName = "instance-state-name"
)

type stubAMIReference struct {
	ids map[string]string
	err error
}

func (s *stubAMIReference) CurrentImageIDs(context.Context) (map[string]string, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.ids, nil
}

func currentAMIs() *stubAMIReference {
	return &stubAMIReference{ids: map[string]string{"arm64": amiCurrentARM64, "x86_64": amiCurrentAMD64}}
}

// poolInstance builds a managed pool member in the given state on the given AMI.
func poolInstance(id, pool, ami, arch string, state ec2types.InstanceStateName) ec2types.Instance {
	return ec2types.Instance{
		InstanceId:   aws.String(id),
		ImageId:      aws.String(ami),
		Architecture: ec2types.ArchitectureValues(arch),
		State:        &ec2types.InstanceState{Name: state},
		Tags: []ec2types.Tag{
			{Key: aws.String("runs-fleet:managed"), Value: aws.String("true")},
			{Key: aws.String("runs-fleet:pool"), Value: aws.String(pool)},
		},
	}
}

func staleAMITasks(ec2Client *mockEC2API, ref AMIReference) *Tasks {
	tasks := &Tasks{
		ec2Client:    ec2Client,
		dynamoClient: &mockTaskDynamoDBAPI{},
		config:       &config.Config{JobsTableName: "jobs-table"},
	}
	if ref != nil {
		tasks.SetAMIReference(ref)
	}
	return tasks
}

// Only stopped members are retired. A stopped instance holds zero work and will
// never re-image on its own; a running one either cycles after its next job or
// is hung, which is a different problem with a different fix.
func TestExecuteStaleAMIInstances_OnlyStoppedPoolMembers(t *testing.T) {
	ec2Client := &mockEC2API{instances: []ec2types.Reservation{{Instances: []ec2types.Instance{
		poolInstance("i-0stopped-stale", "cc", amiStaleARM64, "arm64", ec2types.InstanceStateNameStopped),
		poolInstance("i-0running-stale", "cc", amiStaleARM64, "arm64", ec2types.InstanceStateNameRunning),
		poolInstance("i-0stopped-current", "cc", amiCurrentARM64, "arm64", ec2types.InstanceStateNameStopped),
	}}}}

	if err := staleAMITasks(ec2Client, currentAMIs()).ExecuteStaleAMIInstances(context.Background()); err != nil {
		t.Fatalf("ExecuteStaleAMIInstances(): %v", err)
	}

	if len(ec2Client.terminatedIDs) != 1 || ec2Client.terminatedIDs[0] != "i-0stopped-stale" {
		t.Errorf("terminated %v, want only the stopped stale member", ec2Client.terminatedIDs)
	}
}

// One per pool per cycle. A pool of ten stale spares converges over ten cycles
// and never dips more than one below target.
func TestExecuteStaleAMIInstances_OnePerPoolPerCycle(t *testing.T) {
	ec2Client := &mockEC2API{instances: []ec2types.Reservation{{Instances: []ec2types.Instance{
		poolInstance("i-0cc1", "cc", amiStaleARM64, "arm64", ec2types.InstanceStateNameStopped),
		poolInstance("i-0cc2", "cc", amiStaleARM64, "arm64", ec2types.InstanceStateNameStopped),
		poolInstance("i-0cc3", "cc", amiStaleARM64, "arm64", ec2types.InstanceStateNameStopped),
		poolInstance("i-0other1", "portal-api", amiStaleARM64, "arm64", ec2types.InstanceStateNameStopped),
		poolInstance("i-0other2", "portal-api", amiStaleARM64, "arm64", ec2types.InstanceStateNameStopped),
	}}}}

	if err := staleAMITasks(ec2Client, currentAMIs()).ExecuteStaleAMIInstances(context.Background()); err != nil {
		t.Fatalf("ExecuteStaleAMIInstances(): %v", err)
	}

	if len(ec2Client.terminatedIDs) != 2 {
		t.Fatalf("terminated %v, want exactly one per pool", ec2Client.terminatedIDs)
	}
	pools := map[string]bool{}
	for _, id := range ec2Client.terminatedIDs {
		switch id {
		case "i-0cc1", "i-0cc2", "i-0cc3":
			pools["cc"] = true
		case "i-0other1", "i-0other2":
			pools["portal-api"] = true
		default:
			t.Errorf("unexpected termination: %s", id)
		}
	}
	if len(pools) != 2 {
		t.Errorf("terminated %v, want one from each of the two pools", ec2Client.terminatedIDs)
	}
}

// An instance with no pool is a cold-start instance: ephemeral, terminates
// itself after its job, and replacing it buys nothing.
func TestExecuteStaleAMIInstances_LeavesPoollessInstances(t *testing.T) {
	stray := poolInstance("i-0coldstart", "", amiStaleARM64, "arm64", ec2types.InstanceStateNameStopped)
	stray.Tags = stray.Tags[:1]
	ec2Client := &mockEC2API{instances: []ec2types.Reservation{{Instances: []ec2types.Instance{stray}}}}

	if err := staleAMITasks(ec2Client, currentAMIs()).ExecuteStaleAMIInstances(context.Background()); err != nil {
		t.Fatalf("ExecuteStaleAMIInstances(): %v", err)
	}
	if len(ec2Client.terminatedIDs) != 0 {
		t.Errorf("terminated %v, want nothing without a pool to replenish it", ec2Client.terminatedIDs)
	}
}

// Without a known reference AMI the sweep must terminate nothing. Guessing here
// would roll the fleet on a transient EC2 error.
func TestExecuteStaleAMIInstances_NoReferenceTerminatesNothing(t *testing.T) {
	tests := []struct {
		name string
		ref  AMIReference
	}{
		{name: "no reference wired", ref: nil},
		{name: "reference lookup fails", ref: &stubAMIReference{err: errors.New("AccessDenied")}},
		{name: "reference is empty", ref: &stubAMIReference{ids: map[string]string{}}},
		{name: "this architecture is unknown", ref: &stubAMIReference{ids: map[string]string{"x86_64": amiCurrentAMD64}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ec2Client := &mockEC2API{instances: []ec2types.Reservation{{Instances: []ec2types.Instance{
				poolInstance("i-0stopped-stale", "cc", amiStaleARM64, "arm64", ec2types.InstanceStateNameStopped),
			}}}}

			// A failure to resolve is not a task failure; there is simply nothing to do.
			if err := staleAMITasks(ec2Client, tt.ref).ExecuteStaleAMIInstances(context.Background()); err != nil {
				t.Fatalf("ExecuteStaleAMIInstances(): %v", err)
			}
			if len(ec2Client.terminatedIDs) != 0 {
				t.Errorf("terminated %v with no reference AMI", ec2Client.terminatedIDs)
			}
		})
	}
}

// The list can be seconds old. An instance that started in the meantime is
// serving a job, so the state is re-read against EC2 before the terminate.
func TestExecuteStaleAMIInstances_RereadsStateBeforeTerminating(t *testing.T) {
	ec2Client := &mockEC2API{
		instances: []ec2types.Reservation{{Instances: []ec2types.Instance{
			poolInstance("i-0started-since", "cc", amiStaleARM64, "arm64", ec2types.InstanceStateNameStopped),
		}}},
		// The confirmation read sees it running.
		stateByID: map[string]ec2types.InstanceStateName{
			"i-0started-since": ec2types.InstanceStateNameRunning,
		},
	}

	if err := staleAMITasks(ec2Client, currentAMIs()).ExecuteStaleAMIInstances(context.Background()); err != nil {
		t.Fatalf("ExecuteStaleAMIInstances(): %v", err)
	}
	if len(ec2Client.terminatedIDs) != 0 {
		t.Errorf("terminated %v, want nothing — it started since the scan", ec2Client.terminatedIDs)
	}
}

// A describe failure must not be read as "nothing is stale" nor as licence to
// terminate; it surfaces as an error so the schedule reports it.
func TestExecuteStaleAMIInstances_DescribeFailureSurfaces(t *testing.T) {
	ec2Client := &mockEC2API{describeErr: errors.New("throttled")}

	err := staleAMITasks(ec2Client, currentAMIs()).ExecuteStaleAMIInstances(context.Background())
	if err == nil {
		t.Fatal("expected an error when instances cannot be listed")
	}
	if len(ec2Client.terminatedIDs) != 0 {
		t.Errorf("terminated %v after a describe failure", ec2Client.terminatedIDs)
	}
}

func TestExecuteStaleAMIInstances_ScopesToManagedStoppedInstances(t *testing.T) {
	ec2Client := &mockEC2API{instances: []ec2types.Reservation{}}
	if err := staleAMITasks(ec2Client, currentAMIs()).ExecuteStaleAMIInstances(context.Background()); err != nil {
		t.Fatalf("ExecuteStaleAMIInstances(): %v", err)
	}

	if ec2Client.describeInput == nil {
		t.Fatal("no DescribeInstances call recorded")
	}
	var managed, stopped bool
	for _, f := range ec2Client.describeInput.Filters {
		switch aws.ToString(f.Name) {
		case "tag:runs-fleet:managed":
			managed = true
		case stateFilterName:
			stopped = len(f.Values) == 1 && f.Values[0] == "stopped"
		}
	}
	if !managed || !stopped {
		t.Errorf("filters = %+v, want managed=true and state=stopped only", ec2Client.describeInput.Filters)
	}
}

// A state re-read that errors is not permission to proceed: the instance may
// have started, and the next step cannot be undone.
func TestExecuteStaleAMIInstances_RereadErrorSkips(t *testing.T) {
	ec2Client := &mockEC2API{
		instances: []ec2types.Reservation{{Instances: []ec2types.Instance{
			poolInstance("i-0unverifiable", "cc", amiStaleARM64, "arm64", ec2types.InstanceStateNameStopped),
		}}},
		// The initial scan succeeds; the confirmation read fails.
		describeErrOnCall: 2,
	}

	err := staleAMITasks(ec2Client, currentAMIs()).ExecuteStaleAMIInstances(context.Background())
	if err != nil {
		t.Fatalf("a single unverifiable candidate is not a task failure: %v", err)
	}
	if len(ec2Client.terminatedIDs) != 0 {
		t.Errorf("terminated %v after a failed state re-read", ec2Client.terminatedIDs)
	}
}

// One pool's terminate failing must not abandon the others, and the failure
// must surface rather than being swallowed as a clean cycle.
func TestExecuteStaleAMIInstances_TerminateFailureIsReported(t *testing.T) {
	ec2Client := &mockEC2API{
		instances: []ec2types.Reservation{{Instances: []ec2types.Instance{
			poolInstance("i-0cc", "cc", amiStaleARM64, "arm64", ec2types.InstanceStateNameStopped),
		}}},
		terminateErr: errors.New("RequestLimitExceeded"),
	}

	if err := staleAMITasks(ec2Client, currentAMIs()).ExecuteStaleAMIInstances(context.Background()); err == nil {
		t.Fatal("expected the terminate failure to surface")
	}
}
