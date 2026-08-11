package admin

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
)

// LaunchTemplateAPI reads the launch templates the fleet launches from.
type LaunchTemplateAPI interface {
	DescribeLaunchTemplateVersions(ctx context.Context, params *ec2.DescribeLaunchTemplateVersionsInput, optFns ...func(*ec2.Options)) (*ec2.DescribeLaunchTemplateVersionsOutput, error)
}

// amiCacheTTL bounds how stale the reference AMI may be. A template changes a
// few times a month and the instances page auto-refreshes, so resolving per
// request would spend API calls to learn the same answer.
const amiCacheTTL = 5 * time.Minute

// amiResolveTimeout bounds one refresh. The resolver's lock is held for its
// duration, so an unbounded call would queue every instances-page request
// behind a hung EC2 API.
const amiResolveTimeout = 5 * time.Second

// templateArchSuffix maps an EC2 instance architecture to the launch template
// that would launch it. EC2 reports x86_64; the template is named amd64
// (pkg/fleet.getLaunchTemplateForArch).
var templateArchSuffix = map[string]string{
	"arm64":  "arm64",
	"x86_64": "amd64",
}

// CurrentAMI is what a new instance of one architecture would boot today.
type CurrentAMI struct {
	Architecture   string `json:"architecture"`
	ImageID        string `json:"image_id"`
	LaunchTemplate string `json:"launch_template"`
	Version        int64  `json:"version"`
	VersionCreated string `json:"version_created,omitempty"`
}

// CurrentAMIsResponse reports the reference AMI per architecture.
type CurrentAMIsResponse struct {
	AMIs []CurrentAMI `json:"amis"`
	// Unresolved names architectures whose template could not be read. Their
	// instances are reported with no staleness rather than a guessed one.
	Unresolved []string `json:"unresolved,omitempty"`
}

// amiResolver caches the per-architecture reference AMI.
//
// Every launch path pins $Latest (pkg/fleet), so $Latest — not $Default — is
// what a new instance actually gets, and therefore what staleness means.
type amiResolver struct {
	api            LaunchTemplateAPI
	templateBase   string
	mu             sync.Mutex
	cached         map[string]CurrentAMI
	cachedAt       time.Time
	cachedUnresolv []string
}

func newAMIResolver(api LaunchTemplateAPI, templateBase string) *amiResolver {
	if templateBase == "" {
		templateBase = "runs-fleet-runner"
	}
	return &amiResolver{api: api, templateBase: templateBase}
}

// current returns the reference AMI per EC2 architecture, plus the
// architectures that could not be resolved. It errors only when nothing at all
// has ever been read: one architecture's outage must not blind the other, and a
// refresh that fails must not discard what the last one learned.
func (r *amiResolver) current(ctx context.Context) (map[string]CurrentAMI, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.cached != nil && time.Since(r.cachedAt) < amiCacheTTL {
		return r.cached, nil
	}

	// Bound the refresh: the lock is held across these calls, so an EC2 API that
	// hangs would otherwise stall every instances-page request behind it.
	fetchCtx, cancel := context.WithTimeout(ctx, amiResolveTimeout)
	defer cancel()

	resolved := make(map[string]CurrentAMI, len(templateArchSuffix))
	for arch, suffix := range templateArchSuffix {
		if ami, ok := r.resolveArch(fetchCtx, arch, suffix); ok {
			resolved[arch] = ami
		}
	}

	// Merge over the last known good rather than replacing it. A single flaky
	// call would otherwise take a previously-resolved architecture back to
	// "unknown" for a whole TTL for no reason.
	merged := make(map[string]CurrentAMI, len(templateArchSuffix))
	for arch, ami := range r.cached {
		merged[arch] = ami
	}
	for arch, ami := range resolved {
		merged[arch] = ami
	}
	if len(merged) == 0 {
		// Nothing cached and nothing readable. Not stored, so the next request
		// retries instead of staying blind for the whole TTL.
		return nil, fmt.Errorf("no launch template AMI could be resolved")
	}

	var unresolved []string
	for arch := range templateArchSuffix {
		if _, ok := merged[arch]; !ok {
			unresolved = append(unresolved, arch)
		}
	}
	slices.Sort(unresolved)

	r.cached, r.cachedUnresolv = merged, unresolved
	if len(resolved) > 0 {
		// Only a refresh that learned something resets the clock; one that
		// learned nothing leaves the entry due so the next request retries.
		r.cachedAt = time.Now()
	}
	return merged, nil
}

// resolveArch reads one architecture's reference AMI. A miss of any kind —
// error, empty result, template with no image — reports not-ok rather than a
// zero value, so the caller can tell "unknown" from "resolved to nothing".
func (r *amiResolver) resolveArch(ctx context.Context, arch, suffix string) (CurrentAMI, bool) {
	name := r.templateBase + "-" + suffix
	out, err := r.api.DescribeLaunchTemplateVersions(ctx, &ec2.DescribeLaunchTemplateVersionsInput{
		LaunchTemplateName: aws.String(name),
		Versions:           []string{"$Latest"},
	})
	if err != nil || len(out.LaunchTemplateVersions) == 0 {
		return CurrentAMI{}, false
	}

	v := out.LaunchTemplateVersions[0]
	if v.LaunchTemplateData == nil || aws.ToString(v.LaunchTemplateData.ImageId) == "" {
		return CurrentAMI{}, false
	}
	ami := CurrentAMI{
		Architecture:   arch,
		ImageID:        aws.ToString(v.LaunchTemplateData.ImageId),
		LaunchTemplate: name,
		Version:        aws.ToInt64(v.VersionNumber),
	}
	if v.CreateTime != nil {
		ami.VersionCreated = v.CreateTime.UTC().Format(time.RFC3339)
	}
	return ami, true
}

// unresolvedArchs reports the architectures missing from the last successful
// resolve. Call after current.
func (r *amiResolver) unresolvedArchs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cachedUnresolv
}

func (r *amiResolver) expireForTest() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cachedAt = time.Time{}
}

// SetAMISource wires the launch-template reader that makes AMI staleness
// answerable. Without it the instances list still renders, reporting that it
// does not know which AMI is current rather than guessing.
func (h *InstancesHandler) SetAMISource(api LaunchTemplateAPI, launchTemplateName string) {
	h.amis = newAMIResolver(api, launchTemplateName)
}

// CurrentAMIs handles GET /api/instances/amis.
func (h *InstancesHandler) CurrentAMIs(w http.ResponseWriter, r *http.Request) {
	if h.amis == nil {
		h.writeError(w, http.StatusServiceUnavailable, "AMI source not configured",
			"no launch-template reader is wired into the orchestrator")
		return
	}

	current, err := h.amis.current(r.Context())
	if err != nil {
		h.writeError(w, http.StatusBadGateway, "Failed to read launch templates", err.Error())
		return
	}

	resp := CurrentAMIsResponse{Unresolved: h.amis.unresolvedArchs()}
	for _, arch := range sortedArchs(current) {
		resp.AMIs = append(resp.AMIs, current[arch])
	}
	h.writeJSON(w, http.StatusOK, resp)
}

// sortedArchs keeps the response order stable across requests; map iteration
// would otherwise reshuffle the card on every refresh.
func sortedArchs(m map[string]CurrentAMI) []string {
	archs := make([]string, 0, len(m))
	for a := range m {
		archs = append(archs, a)
	}
	slices.Sort(archs)
	return archs
}
