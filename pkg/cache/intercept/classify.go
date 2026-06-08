// Package intercept holds the request-routing logic for the on-host cache
// interceptor. The interceptor terminates TLS for the GitHub Actions results
// host and must steer cache RPCs to the local S3-backed service while passing
// everything else — crucially the artifacts service, which shares the same
// host — straight through to GitHub unchanged.
package intercept

import "strings"

// Target identifies which backend a results-host request belongs to.
type Target int

const (
	// TargetOther is anything not handled locally (artifacts service is the
	// notable case, plus OIDC and any future result service): pass through.
	TargetOther Target = iota
	// TargetCacheService is the v2 cache Twirp service: serve locally from S3.
	TargetCacheService
)

const (
	cacheServicePrefix    = "/twirp/github.actions.results.api.v1.CacheService/"
	artifactServicePrefix = "/twirp/github.actions.results.api.v1.ArtifactService/"
)

// Classify returns the routing target for a results-host request path. Only the
// cache service is claimed locally; the artifacts service and everything else
// pass through, so transparent caching never disturbs upload/download-artifact.
func Classify(path string) Target {
	if strings.HasPrefix(path, cacheServicePrefix) {
		return TargetCacheService
	}
	return TargetOther
}

// IsArtifactService reports whether path targets the artifacts service. It is
// kept distinct from Classify so the interceptor can log/asserts that artifact
// traffic is being passed through rather than silently lumped into "other".
func IsArtifactService(path string) bool {
	return strings.HasPrefix(path, artifactServicePrefix)
}

func (t Target) String() string {
	switch t {
	case TargetCacheService:
		return "cache-service"
	default:
		return "other"
	}
}
