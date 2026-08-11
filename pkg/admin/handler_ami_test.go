package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/fleet"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// The real AMIs and template versions in the fleet as of 2026-08-11.
const (
	currentARM64AMI = "ami-02078444e5f1bbf2b"
	currentAMD64AMI = "ami-0bc3c3b60cc3576fb"
	staleARM64AMI   = "ami-01d3832c683462b85"
	staleAMD64AMI   = "ami-0cc387931f6788ccb"
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

func templateVersion(name, ami string, version int64) *types.LaunchTemplateVersion {
	created := time.Date(2026, 8, 11, 10, 52, 0, 0, time.UTC)
	return &types.LaunchTemplateVersion{
		LaunchTemplateName: aws.String(name),
		VersionNumber:      aws.Int64(version),
		CreateTime:         aws.Time(created),
		LaunchTemplateData: &types.ResponseLaunchTemplateData{ImageId: aws.String(ami)},
	}
}

func healthyLaunchTemplates() *mockLaunchTemplateAPI {
	return &mockLaunchTemplateAPI{byName: map[string]*types.LaunchTemplateVersion{
		"runs-fleet-runner-arm64": templateVersion("runs-fleet-runner-arm64", currentARM64AMI, 69),
		"runs-fleet-runner-amd64": templateVersion("runs-fleet-runner-amd64", currentAMD64AMI, 65),
	}}
}

func instanceOn(id, ami, arch string) types.Instance {
	return types.Instance{
		InstanceId:   aws.String(id),
		InstanceType: types.InstanceTypeM7gXlarge,
		ImageId:      aws.String(ami),
		Architecture: types.ArchitectureValues(arch),
		State:        &types.InstanceState{Name: types.InstanceStateNameRunning},
		Tags: []types.Tag{
			{Key: aws.String("runs-fleet:managed"), Value: aws.String("true")},
			{Key: aws.String("runs-fleet:pool"), Value: aws.String("cc")},
		},
	}
}

func listWith(t *testing.T, instances []types.Instance, lt fleet.LaunchTemplateAPI) map[string]any {
	t.Helper()

	ec2Mock := &mockEC2API{output: &ec2.DescribeInstancesOutput{
		Reservations: []types.Reservation{{Instances: instances}},
	}}
	handler := newTestInstancesHandler(ec2Mock, &mockInstancesDB{})
	if lt != nil {
		handler.SetAMISource(lt, "runs-fleet-runner")
	}

	w := httptest.NewRecorder()
	handler.ListInstances(w, httptest.NewRequest(http.MethodGet, "/api/instances", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}

	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return body
}

func staleFlags(t *testing.T, body map[string]any) map[string]bool {
	t.Helper()

	raw, ok := body["instances"].([]any)
	if !ok {
		t.Fatalf("no instances in body: %v", body)
	}
	got := map[string]bool{}
	for _, item := range raw {
		inst := item.(map[string]any)
		stale, _ := inst["ami_stale"].(bool)
		got[inst["instance_id"].(string)] = stale
	}
	return got
}

// The two architectures run different AMIs by design, so a single comparison
// value would mark every amd64 instance stale the moment the arm64 template
// moved. Staleness is per architecture or it is noise.
func TestListInstances_AMIStalenessIsPerArchitecture(t *testing.T) {
	t.Parallel()

	body := listWith(t, []types.Instance{
		instanceOn("i-0arm64current", currentARM64AMI, "arm64"),
		instanceOn("i-0amd64current", currentAMD64AMI, "x86_64"),
		instanceOn("i-0arm64stale", staleARM64AMI, "arm64"),
		instanceOn("i-0amd64stale", staleAMD64AMI, "x86_64"),
		// An arm64 instance running the current *amd64* AMI is stale: it is not
		// what its own template would launch.
		instanceOn("i-0crossarch", currentAMD64AMI, "arm64"),
	}, healthyLaunchTemplates())

	want := map[string]bool{
		"i-0arm64current": false,
		"i-0amd64current": false,
		"i-0arm64stale":   true,
		"i-0amd64stale":   true,
		"i-0crossarch":    true,
	}
	got := staleFlags(t, body)
	for id, wantStale := range want {
		if got[id] != wantStale {
			t.Errorf("%s ami_stale = %v, want %v", id, got[id], wantStale)
		}
	}

	if unknown, _ := body["ami_current_unknown"].(bool); unknown {
		t.Error("ami_current_unknown = true with healthy templates")
	}
}

// The list carries the image id so the propagation question is answerable
// without opening every row, which is the entire complaint.
func TestListInstances_CarriesImageAndArchitecture(t *testing.T) {
	t.Parallel()

	body := listWith(t, []types.Instance{instanceOn("i-0abc", currentARM64AMI, "arm64")}, healthyLaunchTemplates())
	inst := body["instances"].([]any)[0].(map[string]any)

	if inst["image_id"] != currentARM64AMI {
		t.Errorf("image_id = %v, want %s", inst["image_id"], currentARM64AMI)
	}
	if inst["architecture"] != "arm64" {
		t.Errorf("architecture = %v, want arm64", inst["architecture"])
	}
}

// Failing closed here would paint the whole fleet stale on a transient API
// error and invite an operator to replace all of it. Unknown means unknown.
func TestListInstances_AMILookupFailureMarksNothingStale(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		lt   fleet.LaunchTemplateAPI
	}{
		{name: "lookup fails", lt: &mockLaunchTemplateAPI{err: errors.New("AccessDenied: ec2:DescribeLaunchTemplateVersions")}},
		{name: "no AMI source configured", lt: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			body := listWith(t, []types.Instance{
				instanceOn("i-0arm64stale", staleARM64AMI, "arm64"),
				instanceOn("i-0amd64stale", staleAMD64AMI, "x86_64"),
			}, tt.lt)

			for id, stale := range staleFlags(t, body) {
				if stale {
					t.Errorf("%s marked stale without a known current AMI", id)
				}
			}
			if unknown, _ := body["ami_current_unknown"].(bool); !unknown {
				t.Error("ami_current_unknown = false, want true so the UI can say it does not know")
			}
		})
	}
}

// One template's failure must not silence the other: an amd64 outage should
// still leave arm64 staleness answerable.
func TestListInstances_PartialTemplateFailureKeepsTheOtherArch(t *testing.T) {
	t.Parallel()

	lt := &mockLaunchTemplateAPI{byName: map[string]*types.LaunchTemplateVersion{
		"runs-fleet-runner-arm64": templateVersion("runs-fleet-runner-arm64", currentARM64AMI, 69),
	}}

	body := listWith(t, []types.Instance{
		instanceOn("i-0arm64stale", staleARM64AMI, "arm64"),
		instanceOn("i-0amd64stale", staleAMD64AMI, "x86_64"),
	}, lt)

	got := staleFlags(t, body)
	if !got["i-0arm64stale"] {
		t.Error("arm64 staleness must survive an amd64 template failure")
	}
	if got["i-0amd64stale"] {
		t.Error("amd64 marked stale with no amd64 template to compare against")
	}
	if unknown, _ := body["ami_current_unknown"].(bool); !unknown {
		t.Error("ami_current_unknown = false, want true when one arch could not be resolved")
	}
}

func TestAMIsEndpoint(t *testing.T) {
	t.Parallel()

	handler := newTestInstancesHandler(&mockEC2API{output: &ec2.DescribeInstancesOutput{}}, &mockInstancesDB{})
	handler.SetAMISource(healthyLaunchTemplates(), "runs-fleet-runner")

	w := httptest.NewRecorder()
	handler.CurrentAMIs(w, httptest.NewRequest(http.MethodGet, "/api/instances/amis", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}

	var resp CurrentAMIsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.AMIs) != 2 {
		t.Fatalf("got %d architectures, want 2: %+v", len(resp.AMIs), resp.AMIs)
	}

	byArch := map[string]CurrentAMI{}
	for _, a := range resp.AMIs {
		byArch[a.Architecture] = a
	}
	if got := byArch["arm64"]; got.ImageID != currentARM64AMI || got.Version != 69 {
		t.Errorf("arm64 = %+v, want %s v69", got, currentARM64AMI)
	}
	if got := byArch["x86_64"]; got.ImageID != currentAMD64AMI || got.Version != 65 {
		t.Errorf("x86_64 = %+v, want %s v65", got, currentAMD64AMI)
	}
	if byArch["arm64"].LaunchTemplate != "runs-fleet-runner-arm64" {
		t.Errorf("launch template = %q, want runs-fleet-runner-arm64", byArch["arm64"].LaunchTemplate)
	}
}

func TestAMIsEndpoint_Unconfigured(t *testing.T) {
	t.Parallel()

	handler := newTestInstancesHandler(&mockEC2API{}, &mockInstancesDB{})
	w := httptest.NewRecorder()
	handler.CurrentAMIs(w, httptest.NewRequest(http.MethodGet, "/api/instances/amis", nil))

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 when no AMI source is wired", w.Code)
	}
}
