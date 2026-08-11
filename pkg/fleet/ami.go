package fleet

import (
	"context"
	"fmt"
	"maps"
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

// defaultLaunchTemplateBase is the launch-template name prefix used when the
// config leaves it unset.
const defaultLaunchTemplateBase = "runs-fleet-runner"

// amiCacheTTL bounds how stale the reference AMI may be. A template changes a
// few times a month and callers poll, so resolving per call would spend API
// requests to learn the same answer.
const amiCacheTTL = 5 * time.Minute

// amiTerminateFreshness bounds how old a reference may be and still be used to
// decide that an instance should be destroyed. Current tolerates a merged-in
// value of any age so the console keeps rendering; CurrentImageIDs does not,
// because a persistently failing template would otherwise let a value drift
// arbitrarily far behind and mark a current instance stale.
const amiTerminateFreshness = 2 * amiCacheTTL

// amiResolveTimeout bounds one refresh. The resolver's lock is held for its
// duration, so an unbounded call would queue every caller behind a hung EC2 API.
const amiResolveTimeout = 5 * time.Second

// TemplateArchSuffix maps an EC2 instance architecture to the launch template
// that would launch it. EC2 reports x86_64; the template is named amd64.
var TemplateArchSuffix = map[string]string{
	"arm64":  "arm64",
	"x86_64": "amd64",
}

// CurrentAMI is what a new instance of one architecture would boot today.
type CurrentAMI struct {
	Architecture   string
	ImageID        string
	LaunchTemplate string
	Version        int64
	VersionCreated time.Time
}

// AMIResolver caches the per-architecture reference AMI.
//
// Every launch path pins $Latest, so $Latest — not $Default — is what a new
// instance actually gets, and therefore what "stale" means.
type AMIResolver struct {
	api          LaunchTemplateAPI
	templateBase string
	mu           sync.Mutex
	cached       map[string]CurrentAMI
	cachedAt     time.Time
	// resolvedAt records when each architecture was last confirmed against its
	// template, which is not the same as when the cache was last written: a
	// merge carries an older value forward.
	resolvedAt map[string]time.Time
}

// NewAMIResolver builds a resolver over the launch templates derived from
// templateBase, matching the names the fleet manager launches from.
func NewAMIResolver(api LaunchTemplateAPI, templateBase string) *AMIResolver {
	if templateBase == "" {
		templateBase = defaultLaunchTemplateBase
	}
	return &AMIResolver{api: api, templateBase: templateBase}
}

// Current returns the reference AMI per EC2 architecture. It errors only when
// nothing at all has ever been read: one architecture's outage must not blind
// the other, and a refresh that fails must not discard what the last one learned.
func (r *AMIResolver) Current(ctx context.Context) (map[string]CurrentAMI, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.cached != nil && time.Since(r.cachedAt) < amiCacheTTL {
		return r.cached, nil
	}

	// Bound the refresh: the lock is held across these calls, so an EC2 API that
	// hangs would otherwise stall every caller behind it.
	fetchCtx, cancel := context.WithTimeout(ctx, amiResolveTimeout)
	defer cancel()

	resolved := make(map[string]CurrentAMI, len(TemplateArchSuffix))
	for arch, suffix := range TemplateArchSuffix {
		if ami, ok := r.resolveArch(fetchCtx, arch, suffix); ok {
			resolved[arch] = ami
		}
	}

	// Merge over the last known good rather than replacing it. A single flaky
	// call would otherwise take a previously-resolved architecture back to
	// "unknown" for a whole TTL for no reason.
	merged := make(map[string]CurrentAMI, len(TemplateArchSuffix))
	maps.Copy(merged, r.cached)
	maps.Copy(merged, resolved)
	if len(merged) == 0 {
		// Nothing cached and nothing readable. Not stored, so the next call
		// retries instead of staying blind for the whole TTL.
		return nil, fmt.Errorf("no launch template AMI could be resolved")
	}

	now := time.Now()
	if r.resolvedAt == nil {
		r.resolvedAt = make(map[string]time.Time, len(TemplateArchSuffix))
	}
	for arch := range resolved {
		r.resolvedAt[arch] = now
	}

	r.cached = merged
	if len(resolved) > 0 {
		// Only a refresh that learned something resets the clock; one that
		// learned nothing leaves the entry due so the next call retries.
		r.cachedAt = time.Now()
	}
	return merged, nil
}

// CurrentImageIDs is Current reduced to architecture -> image id, restricted to
// architectures confirmed recently enough to act on.
//
// This is the oracle for destroying instances, so it is deliberately stricter
// than Current: an architecture whose template has been failing keeps its last
// known value for display, but drops out here rather than letting a drifting
// reference condemn an instance that is in fact current.
func (r *AMIResolver) CurrentImageIDs(ctx context.Context) (map[string]string, error) {
	current, err := r.Current(ctx)
	if err != nil {
		return nil, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	ids := make(map[string]string, len(current))
	for arch, ami := range current {
		if time.Since(r.resolvedAt[arch]) > amiTerminateFreshness {
			continue
		}
		ids[arch] = ami.ImageID
	}
	return ids, nil
}

// UnresolvedArchs reports the architectures with no reference confirmed recently
// enough to trust. Call after Current.
func (r *AMIResolver) UnresolvedArchs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	var unresolved []string
	for arch := range TemplateArchSuffix {
		if _, ok := r.cached[arch]; !ok {
			unresolved = append(unresolved, arch)
			continue
		}
		if time.Since(r.resolvedAt[arch]) > amiTerminateFreshness {
			unresolved = append(unresolved, arch)
		}
	}
	slices.Sort(unresolved)
	return unresolved
}

// resolveArch reads one architecture's reference AMI. A miss of any kind —
// error, empty result, template with no image — reports not-ok rather than a
// zero value, so the caller can tell "unknown" from "resolved to nothing".
func (r *AMIResolver) resolveArch(ctx context.Context, arch, suffix string) (CurrentAMI, bool) {
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
		ami.VersionCreated = v.CreateTime.UTC()
	}
	return ami, true
}

// ageResolvedForTest backdates one architecture's confirmation time.
func (r *AMIResolver) ageResolvedForTest(arch string, by time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.resolvedAt[arch] = r.resolvedAt[arch].Add(-by)
}

func (r *AMIResolver) expireForTest() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cachedAt = time.Time{}
}
