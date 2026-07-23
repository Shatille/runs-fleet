package buildxshim

import (
	"os"
	"strings"
)

// DefaultRealPathFile is where provision records the discovered real-plugin path.
const DefaultRealPathFile = "/opt/runs-fleet/buildx-real-path"

// DefaultPluginSearch is the compiled-in fallback search list for the real
// docker-buildx plugin, in precedence order. The shim shadows the packaged
// plugin at /usr/local/lib/docker/cli-plugins, so those locations are excluded
// here — only the packaged locations remain.
var DefaultPluginSearch = []string{
	"/usr/libexec/docker/cli-plugins/docker-buildx",
	"/usr/lib/docker/cli-plugins/docker-buildx",
	"/usr/local/libexec/docker/cli-plugins/docker-buildx",
	"/root/.docker/cli-plugins/docker-buildx",
}

// DiscoverRealPlugin resolves the path to the real docker-buildx plugin the
// shim must exec. It prefers the path recorded at provision time; if that file
// is missing or names a nonexistent binary, it falls back to the first existing
// entry in the search list. Returns "" only when nothing is found — in which
// case the caller must still attempt exec of a sensible default so the failure
// text is docker's own, never shim-invented.
func DiscoverRealPlugin(recordFile string, searchList []string) string {
	if recorded := readRecordedPath(recordFile); recorded != "" && isExecutableFile(recorded) {
		return recorded
	}
	for _, p := range searchList {
		if isExecutableFile(p) {
			return p
		}
	}
	return ""
}

func readRecordedPath(recordFile string) string {
	b, err := os.ReadFile(recordFile)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func isExecutableFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0
}
