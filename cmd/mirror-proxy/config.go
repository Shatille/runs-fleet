package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// DefaultBuildkitdConfig is where the buildx shim and buildx-setup.service
// look for the BuildKit mirror configuration. The proxy writes it rather than
// the AMI baking it, because the address BuildKit must use is the bridge
// gateway the proxy just bound — a second, independent lookup could resolve a
// different address, and BuildKit answers an address nothing listens on by
// pulling from Docker Hub without saying so.
const DefaultBuildkitdConfig = "/opt/runs-fleet/buildkitd.toml"

// buildkitdConfig renders the mirror configuration for every upstream the
// registry has a pull-through rule for, all pointed at mirrorAddr. Upstreams
// are emitted in sorted order so the file is byte-stable across restarts.
func buildkitdConfig(mirrorAddr string, rules map[string]string) string {
	upstreams := make([]string, 0, len(rules))
	for upstream := range rules {
		upstreams = append(upstreams, upstream)
	}
	sort.Strings(upstreams)

	var b strings.Builder
	for _, upstream := range upstreams {
		fmt.Fprintf(&b, "[registry.%q]\n  mirrors = [%q]\n", upstream, mirrorAddr)
	}
	// The mirror speaks plaintext; without this BuildKit would try HTTPS.
	fmt.Fprintf(&b, "[registry.%q]\n  http = true\n", mirrorAddr)
	return b.String()
}

// writeBuildkitdConfig replaces path atomically, so a reader never observes a
// half-written config.
func writeBuildkitdConfig(path, contents string) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".buildkitd.toml.*")
	if err != nil {
		return fmt.Errorf("create temp config in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}()

	if _, err := tmp.WriteString(contents); err != nil {
		return fmt.Errorf("write %s: %w", tmpName, err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		return fmt.Errorf("chmod %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename %s to %s: %w", tmpName, path, err)
	}
	return nil
}

// removeBuildkitdConfig drops a config the proxy can no longer honour. An
// absent file is success: the buildx shim treats absence as "not opted in" and
// skips, which is the honest outcome when there is no reachable mirror.
func removeBuildkitdConfig(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return nil
}
