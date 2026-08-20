package buildxshim

import (
	"net"
	"regexp"
	"strings"
)

// mirrorAddrPattern captures the address in the first `mirrors = ["host:port"]`
// entry of a buildkitd config.
var mirrorAddrPattern = regexp.MustCompile(`mirrors\s*=\s*\[\s*"([^"]+)"`)

// hostPortPattern matches a quoted host:port anywhere in a buildkitd config —
// both the `mirrors = ["..."]` entries and the `[registry."..."]` table headers
// that carry the matching `http = true`.
var hostPortPattern = regexp.MustCompile(`"([^"\s]+):(\d+)"`)

// MirrorAddrFromConfig returns the mirror address our own baked config points
// at, or "" if it names none. This is the address the proxy actually bound, so
// it is the only correct target to redirect a user config onto.
func MirrorAddrFromConfig(config string) string {
	m := mirrorAddrPattern.FindStringSubmatch(config)
	if m == nil {
		return ""
	}
	return m[1]
}

// RewriteMirrorHosts redirects host-local mirror addresses in a user-supplied
// buildkitd config onto mirrorAddr, returning the result and how many it
// changed.
//
// A workflow that brings its own config is skipped by DecideCreate, so without
// this it keeps whatever address it guessed. devsisters/docker-setup-buildx-action
// guesses the host's default-route address, which is right on a Kubernetes
// runner (the mirror is in the pod) and wrong here: buildx's docker-container
// driver runs buildkitd in a bridge container that can only reach the bridge
// gateway. BuildKit answers a refused mirror by pulling from Docker Hub without
// saying so, so the config silently buys nothing until Hub throttles.
//
// Only addresses on the mirror's own port whose host is local (any address of
// this machine, or loopback) are redirected — a registry that genuinely lives
// elsewhere on that port belongs to somebody else.
func RewriteMirrorHosts(config, mirrorAddr string, localHosts map[string]bool) (string, int) {
	if mirrorAddr == "" {
		return config, 0
	}
	mirrorHost, mirrorPort, err := net.SplitHostPort(mirrorAddr)
	if err != nil {
		return config, 0
	}

	changed := 0
	rewritten := hostPortPattern.ReplaceAllStringFunc(config, func(match string) string {
		m := hostPortPattern.FindStringSubmatch(match)
		host, port := m[1], m[2]
		if port != mirrorPort || host == mirrorHost || !isLocalHost(host, localHosts) {
			return match
		}
		changed++
		return `"` + mirrorAddr + `"`
	})
	return rewritten, changed
}

func isLocalHost(host string, localHosts map[string]bool) bool {
	if localHosts[host] {
		return true
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// UserConfigFlag returns the buildkitd-config flag a `buildx create` argv
// carries, its value, and whether that value is inline TOML rather than a path.
func UserConfigFlag(args []string) (flag, value string, inline, ok bool) {
	for i := 0; i < len(args); i++ {
		name, inlineValue, hasInlineValue := cutFlag(args[i])
		if !createConfigFlags[name] {
			continue
		}
		isInlineFlag := strings.HasSuffix(name, "-inline")
		if hasInlineValue {
			return name, inlineValue, isInlineFlag, true
		}
		if i+1 < len(args) {
			return name, args[i+1], isInlineFlag, true
		}
	}
	return "", "", false, false
}

// ReplaceFlagValue returns a copy of argv with flag's value replaced, handling
// both `--flag value` and `--flag=value`.
func ReplaceFlagValue(argv []string, flag, value string) []string {
	out := append([]string{}, argv...)
	for i := 0; i < len(out); i++ {
		name, _, hasInlineValue := cutFlag(out[i])
		if name != flag {
			continue
		}
		if hasInlineValue {
			out[i] = flag + "=" + value
			return out
		}
		if i+1 < len(out) {
			out[i+1] = value
			return out
		}
	}
	return out
}

// OutcomeSkippedUserConfig is the outcome for a `buildx create` that brings
// its own buildkitd config the shim left untouched.
const OutcomeSkippedUserConfig = outcomeSkipped + ":user-config"

// OutcomeEngagedUserConfig is the outcome for a user-supplied config whose
// mirror address the shim redirected onto the bound mirror.
const OutcomeEngagedUserConfig = outcomeEngaged + ":user-config-redirect"
