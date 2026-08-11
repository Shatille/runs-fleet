package fleet

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// The real AMIs and template versions in the fleet as of 2026-08-11.
const (
	currentARM64AMI = "ami-02078444e5f1bbf2b"
	currentAMD64AMI = "ami-0bc3c3b60cc3576fb"
	arm64Template   = "runs-fleet-runner-arm64"
	amd64Template   = "runs-fleet-runner-amd64"
)

type mockLaunchTemplateAPI struct {
	byName map[string]*types.LaunchTemplateVersion
	err    error

	mu    sync.Mutex
	calls int
}

func (m *mockLaunchTemplateAPI) DescribeLaunchTemplateVersions(_ context.Context, in *ec2.DescribeLaunchTemplateVersionsInput, _ ...func(*ec2.Options)) (*ec2.DescribeLaunchTemplateVersionsOutput, error) {
	m.mu.Lock()
	m.calls++
	m.mu.Unlock()

	if m.err != nil {
		return nil, m.err
	}
	v, ok := m.byName[aws.ToString(in.LaunchTemplateName)]
	if !ok {
		return nil, errors.New("launch template not found: " + aws.ToString(in.LaunchTemplateName))
	}
	return &ec2.DescribeLaunchTemplateVersionsOutput{LaunchTemplateVersions: []types.LaunchTemplateVersion{*v}}, nil
}

func (m *mockLaunchTemplateAPI) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

func templateVersion(name, ami string, version int64) *types.LaunchTemplateVersion {
	return &types.LaunchTemplateVersion{
		LaunchTemplateName: aws.String(name),
		VersionNumber:      aws.Int64(version),
		CreateTime:         aws.Time(time.Date(2026, 8, 11, 10, 52, 0, 0, time.UTC)),
		LaunchTemplateData: &types.ResponseLaunchTemplateData{ImageId: aws.String(ami)},
	}
}

func healthyLaunchTemplates() *mockLaunchTemplateAPI {
	return &mockLaunchTemplateAPI{byName: map[string]*types.LaunchTemplateVersion{
		arm64Template: templateVersion(arm64Template, currentARM64AMI, 69),
		amd64Template: templateVersion(amd64Template, currentAMD64AMI, 65),
	}}
}

// $Latest is what every launch path pins, so it is what a new instance actually
// boots — comparing against $Default would call the fleet stale a version early.
func TestAMIResolver_ResolvesLatestPerArch(t *testing.T) {
	t.Parallel()

	lt := healthyLaunchTemplates()
	got, err := NewAMIResolver(lt, "runs-fleet-runner").Current(context.Background())
	if err != nil {
		t.Fatalf("Current(): %v", err)
	}
	if got["arm64"].ImageID != currentARM64AMI || got["arm64"].Version != 69 {
		t.Errorf("arm64 = %+v, want %s v69", got["arm64"], currentARM64AMI)
	}
	if got["x86_64"].ImageID != currentAMD64AMI || got["x86_64"].LaunchTemplate != amd64Template {
		t.Errorf("x86_64 = %+v, want %s from the amd64 template", got["x86_64"], currentAMD64AMI)
	}
}

func TestAMIResolver_CurrentImageIDs(t *testing.T) {
	t.Parallel()

	got, err := NewAMIResolver(healthyLaunchTemplates(), "runs-fleet-runner").CurrentImageIDs(context.Background())
	if err != nil {
		t.Fatalf("CurrentImageIDs(): %v", err)
	}
	if got["arm64"] != currentARM64AMI || got["x86_64"] != currentAMD64AMI {
		t.Errorf("got %v, want the two current AMIs keyed by EC2 architecture", got)
	}
}

// A launch template changes a few times a month while the page auto-refreshes,
// so re-resolving per request would spend an API call to learn nothing.
func TestAMIResolver_CachesWithinTTL(t *testing.T) {
	t.Parallel()

	lt := healthyLaunchTemplates()
	r := NewAMIResolver(lt, "runs-fleet-runner")

	for range 3 {
		if _, err := r.Current(context.Background()); err != nil {
			t.Fatalf("current(): %v", err)
		}
	}
	if lt.callCount() != 2 {
		t.Errorf("template calls = %d, want 2 (one per architecture, then cached)", lt.callCount())
	}

	r.expireForTest()
	if _, err := r.Current(context.Background()); err != nil {
		t.Fatalf("current(): %v", err)
	}
	if lt.callCount() != 4 {
		t.Errorf("template calls = %d, want 4 after the cache expired", lt.callCount())
	}
}

// A failed resolve must not be cached as "no AMIs" for the whole TTL — that
// would keep the console blind long after the outage cleared.
func TestAMIResolver_DoesNotCacheFailures(t *testing.T) {
	t.Parallel()

	lt := &mockLaunchTemplateAPI{err: errors.New("throttled")}
	r := NewAMIResolver(lt, "runs-fleet-runner")

	for range 2 {
		if _, err := r.Current(context.Background()); err == nil {
			t.Fatal("expected an error while the API is failing")
		}
	}
	if lt.callCount() < 4 {
		t.Errorf("template calls = %d, want a retry on every request while failing", lt.callCount())
	}
}

// A flaky call on one architecture must not discard what the last refresh
// learned about it. Dropping it would take every instance of that architecture
// back to "unknown" for a whole TTL for no reason.
func TestAMIResolver_RefreshFailureKeepsTheLastKnownGood(t *testing.T) {
	t.Parallel()

	lt := healthyLaunchTemplates()
	r := NewAMIResolver(lt, "runs-fleet-runner")

	first, err := r.Current(context.Background())
	if err != nil {
		t.Fatalf("current(): %v", err)
	}
	if len(first) != 2 {
		t.Fatalf("first resolve returned %d architectures, want 2", len(first))
	}

	// amd64 goes away; arm64 keeps answering.
	delete(lt.byName, amd64Template)
	r.expireForTest()

	second, err := r.Current(context.Background())
	if err != nil {
		t.Fatalf("current() after partial failure: %v", err)
	}
	if got, ok := second["x86_64"]; !ok || got.ImageID != currentAMD64AMI {
		t.Errorf("x86_64 = %+v, want the previously resolved %s carried forward", got, currentAMD64AMI)
	}
	if got := second["arm64"]; got.ImageID != currentARM64AMI {
		t.Errorf("arm64 = %+v, want %s", got, currentARM64AMI)
	}
	if len(r.UnresolvedArchs()) != 0 {
		t.Errorf("unresolved = %v, want none — the value is known, just not freshly", r.UnresolvedArchs())
	}
}

// A refresh that learns nothing must not reset the clock, or a blip would hold
// the console on stale data for a full TTL.
func TestAMIResolver_TotalRefreshFailureServesCacheAndStaysDue(t *testing.T) {
	t.Parallel()

	lt := healthyLaunchTemplates()
	r := NewAMIResolver(lt, "runs-fleet-runner")
	if _, err := r.Current(context.Background()); err != nil {
		t.Fatalf("current(): %v", err)
	}

	lt.err = errors.New("throttled")
	r.expireForTest()

	before := lt.callCount()
	got, err := r.Current(context.Background())
	if err != nil {
		t.Fatalf("current() must serve the last known good, not fail: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("got %d architectures, want the 2 cached ones", len(got))
	}

	// Still due, so the very next call retries rather than waiting out the TTL.
	if _, err := r.Current(context.Background()); err != nil {
		t.Fatalf("current(): %v", err)
	}
	if lt.callCount() <= before+2 {
		t.Errorf("calls went %d -> %d, want a retry on the following request too", before, lt.callCount())
	}
}

// The resolver is shared across concurrent requests; a cache miss must not let
// them stampede the EC2 API.
func TestAMIResolver_ConcurrentMissResolvesOnce(t *testing.T) {
	t.Parallel()

	lt := healthyLaunchTemplates()
	r := NewAMIResolver(lt, "runs-fleet-runner")

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := r.Current(context.Background()); err != nil {
				t.Errorf("current(): %v", err)
			}
		}()
	}
	wg.Wait()

	if lt.callCount() != 2 {
		t.Errorf("template calls = %d, want 2 — concurrent misses must single-flight", lt.callCount())
	}
}

// The display path and the destroy path have different tolerances. A template
// that keeps failing leaves its last known value on the card, but must not go
// on condemning instances: the reference could be several versions behind by
// then, and every current instance of that architecture would look stale.
func TestAMIResolver_StaleReferenceDropsOutOfTheTerminateOracle(t *testing.T) {
	t.Parallel()

	lt := healthyLaunchTemplates()
	r := NewAMIResolver(lt, "runs-fleet-runner")
	if _, err := r.Current(context.Background()); err != nil {
		t.Fatalf("Current(): %v", err)
	}

	// amd64's template starts failing, and keeps failing past the freshness bound.
	delete(lt.byName, amd64Template)
	r.expireForTest()
	if _, err := r.Current(context.Background()); err != nil {
		t.Fatalf("Current(): %v", err)
	}
	r.ageResolvedForTest("x86_64", amiTerminateFreshness+time.Minute)

	current, err := r.Current(context.Background())
	if err != nil {
		t.Fatalf("Current(): %v", err)
	}
	if _, ok := current["x86_64"]; !ok {
		t.Error("Current dropped x86_64; the console should still show the last known value")
	}

	ids, err := r.CurrentImageIDs(context.Background())
	if err != nil {
		t.Fatalf("CurrentImageIDs(): %v", err)
	}
	if _, ok := ids["x86_64"]; ok {
		t.Error("CurrentImageIDs still offers a reference nothing has confirmed for longer than the freshness bound")
	}
	if ids["arm64"] != currentARM64AMI {
		t.Errorf("arm64 = %q, want the healthy architecture unaffected", ids["arm64"])
	}
	if got := r.UnresolvedArchs(); len(got) != 1 || got[0] != "x86_64" {
		t.Errorf("UnresolvedArchs() = %v, want [x86_64]", got)
	}
}
