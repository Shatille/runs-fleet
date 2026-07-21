// Package buildxshim holds the pure decision logic for the transparent Docker
// layer-cache shim. The shim is installed in place of the docker-buildx CLI
// plugin on runs-fleet runners; it ALWAYS execs the real plugin and only ever
// optionally appends cache flags to argv first. Decide is the sole gate for
// that injection and is deliberately I/O-free so the whole passthrough matrix
// is unit-testable.
package buildxshim

import (
	"runtime"
	"strings"
)

// Environment variable names the agent injects into a job when the feature is
// enabled, plus the workflow-level opt-out.
const (
	envCacheBucket = "RUNS_FLEET_BUILDKIT_CACHE_BUCKET"
	envCacheRegion = "RUNS_FLEET_BUILDKIT_CACHE_REGION"
	envCachePrefix = "RUNS_FLEET_BUILDKIT_CACHE_PREFIX"
	envOptOut      = "RUNS_FLEET_BUILD_CACHE"
)

// Outcome strings recorded to the outcome file and mirrored into the
// BuildCacheInterception telemetry field.
const (
	outcomeEngaged  = "engaged"
	outcomeSkipped  = "skipped"
	outcomeFailed   = "failed"
	outcomeDisabled = "disabled"
)

// OutcomeNeedsCreds is the exact outcome Decide returns when an invocation is
// injection-eligible in every respect except that no credentials were supplied.
// The shim uses it as the single signal to attempt an IMDS fetch, keeping the
// "fetch creds only when it can matter" decision in one place.
const OutcomeNeedsCreds = outcomeFailed + ":no-creds"

// Credentials carries the instance-profile session credentials the shim embeds
// inline in the S3 cache attribute string so the buildkit container never needs
// its own IMDS access. A zero value (any of the three empty) means the IMDS
// fetch failed and injection MUST be suppressed.
type Credentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
}

func (c Credentials) complete() bool {
	return c.AccessKeyID != "" && c.SecretAccessKey != "" && c.SessionToken != ""
}

// Decide inspects a buildx CLI-plugin invocation and returns the extra argv to
// append (nil for pure passthrough) plus an outcome string for telemetry. It is
// inert by default and fails safe: any uncertainty yields no injection.
//
// Injection happens only when ALL hold:
//  1. argv is a buildx `build` invocation
//  2. envCacheBucket + envCacheRegion + envCachePrefix are all present
//  3. no --cache-from/--cache-to already in argv (explicit user config wins)
//  4. opt-out (RUNS_FLEET_BUILD_CACHE=off) not set
//  5. a non-default builder is selected (--builder <name!=default> OR
//     BUILDX_BUILDER non-empty and != "default") — the docker driver does not
//     support --cache-to, so injecting there would fail the build
//  6. complete instance-profile session creds are available
func Decide(argv []string, env map[string]string, creds Credentials) (extraArgs []string, outcome string) {
	if !isBuild(argv) {
		return nil, outcomeSkipped + ":not-build"
	}

	bucket := env[envCacheBucket]
	region := env[envCacheRegion]
	prefix := env[envCachePrefix]
	if bucket == "" || region == "" || prefix == "" {
		return nil, outcomeDisabled
	}

	if strings.EqualFold(env[envOptOut], "off") {
		return nil, outcomeSkipped + ":opt-out"
	}

	if hasCacheFlag(argv) {
		return nil, outcomeSkipped + ":user-cache-flags"
	}

	if !nonDefaultBuilderSelected(argv, env) {
		return nil, outcomeSkipped + ":default-builder"
	}

	if !creds.complete() {
		return nil, OutcomeNeedsCreds
	}

	name := cacheName(prefix, platformSlug(argv))
	base := []string{
		"type=s3",
		"region=" + region,
		"bucket=" + bucket,
		"prefix=" + prefix,
		"name=" + name,
		"access_key_id=" + creds.AccessKeyID,
		"secret_access_key=" + creds.SecretAccessKey,
		"session_token=" + creds.SessionToken,
	}
	cacheFrom := strings.Join(base, ",")
	cacheTo := strings.Join(append(append([]string{}, base...), "mode=max"), ",")

	return []string{
		"--cache-from", cacheFrom,
		"--cache-to", cacheTo,
	}, outcomeEngaged
}

// isBuild reports whether argv is a buildx build invocation. docker invokes the
// plugin with the subcommand as argv[0] when the plugin name is stripped
// ("build ...") and as argv[1] when the plugin name leads ("buildx build ...").
func isBuild(argv []string) bool {
	if len(argv) == 0 {
		return false
	}
	if argv[0] == "docker-cli-plugin-metadata" {
		return false
	}
	sub := argv[0]
	if sub == "buildx" && len(argv) > 1 {
		sub = argv[1]
	}
	return sub == "build"
}

// hasCacheFlag reports whether the user already specified cache flags, in either
// the separate-arg (`--cache-from type=...`) or equals (`--cache-from=type=...`)
// form. Explicit user config always wins.
func hasCacheFlag(argv []string) bool {
	for _, a := range argv {
		if a == "--cache-from" || a == "--cache-to" ||
			strings.HasPrefix(a, "--cache-from=") || strings.HasPrefix(a, "--cache-to=") {
			return true
		}
	}
	return false
}

// nonDefaultBuilderSelected reports whether the invocation targets a builder
// that supports cache export. --builder in argv wins over BUILDX_BUILDER env.
// Absent/empty/"default" → the docker driver → NO injection.
func nonDefaultBuilderSelected(argv []string, env map[string]string) bool {
	if b, ok := builderFromArgv(argv); ok {
		return b != "" && b != "default"
	}
	b := env["BUILDX_BUILDER"]
	return b != "" && b != "default"
}

func builderFromArgv(argv []string) (string, bool) {
	for i, a := range argv {
		if a == "--builder" && i+1 < len(argv) {
			return argv[i+1], true
		}
		if v, ok := strings.CutPrefix(a, "--builder="); ok {
			return v, true
		}
	}
	return "", false
}

// platformSlug derives the cache-name platform component from --platform argv
// (first platform, slashes→dashes) or falls back to the shim's runtime arch.
func platformSlug(argv []string) string {
	if p, ok := platformFromArgv(argv); ok && p != "" {
		first := p
		if idx := strings.IndexByte(first, ','); idx >= 0 {
			first = first[:idx]
		}
		return strings.ReplaceAll(first, "/", "-")
	}
	return runtimePlatformSlug()
}

func platformFromArgv(argv []string) (string, bool) {
	for i, a := range argv {
		if a == "--platform" && i+1 < len(argv) {
			return argv[i+1], true
		}
		if v, ok := strings.CutPrefix(a, "--platform="); ok {
			return v, true
		}
	}
	return "", false
}

// runtimePlatformSlug is the shim's own os/arch as a cache-name slug, used when
// the invocation does not pin a platform.
func runtimePlatformSlug() string {
	return runtime.GOOS + "-" + runtime.GOARCH
}

// cacheName composes the per-repo, per-platform cache manifest name. The repo
// segment is derived from the injected prefix (buildkit/<org>/<repo>/) so
// parallel amd64/arm64 jobs of one repo do not thrash each other's manifests
// (blobs are content-addressed and shared regardless).
func cacheName(prefix, platform string) string {
	repo := repoFromPrefix(prefix)
	if repo == "" {
		repo = "repo"
	}
	return repo + "-" + platform
}

// repoFromPrefix extracts and sanitizes the repo segment of a
// buildkit/<org>/<repo>/ prefix. Returns "" if the prefix has no repo segment.
func repoFromPrefix(prefix string) string {
	parts := strings.Split(strings.Trim(prefix, "/"), "/")
	// Expect ["buildkit", "<org>", "<repo>"]; the repo is the last segment.
	if len(parts) < 3 {
		return ""
	}
	return sanitizeSlug(parts[len(parts)-1])
}

// sanitizeSlug lowercases and reduces a name to alnum-and-dash, collapsing runs
// of non-alnum characters into single dashes and trimming leading/trailing ones.
func sanitizeSlug(s string) string {
	var b strings.Builder
	lastDash := true // suppress a leading dash
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}
