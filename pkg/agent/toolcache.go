package agent

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// DefaultToolCacheDir is the GitHub Actions tool cache location on runners; the
// runs-fleet-agent unit points AGENT_TOOLSDIRECTORY here.
const DefaultToolCacheDir = "/opt/hostedtoolcache"

// maxToolCacheMisses caps how many misses a single job reports. Real jobs install a
// handful of versions; the cap bounds the SQS payload and the metric cardinality
// against a job that writes many bogus .complete markers into the shared cache dir.
const maxToolCacheMisses = 50

// SnapshotToolCache returns the set of completed tool-cache entries under dir, keyed
// as "<Tool>/<version>/<platform>" (derived from the `<platform>.complete` markers the
// Actions tool cache writes when an install finishes). Using the marker — not the
// version directory — counts only completed installs, so a partial/aborted download is
// ignored.
//
// It returns whatever it collected plus any walk error (e.g. the tool cache dir is
// missing or unreadable — WalkDir returns this, it does not panic). Callers treat the
// result as best-effort telemetry: use the (possibly partial) set and log the error,
// never fail the job on it.
func SnapshotToolCache(dir string) (map[string]struct{}, error) {
	entries := make(map[string]struct{})
	// Surface a root-level problem (dir missing/unreadable) as an error the caller can
	// log; per-entry errors below are skipped so a single bad entry doesn't abort the walk.
	if _, err := os.Stat(dir); err != nil {
		return entries, err
	}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // best-effort: skip unreadable entries, never abort the walk
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".complete") {
			return nil
		}
		rel, relErr := filepath.Rel(dir, path)
		if relErr != nil {
			return nil
		}
		// rel is "<Tool>/<version>/<platform>.complete"; key is the same minus ".complete".
		key := strings.TrimSuffix(rel, ".complete")
		if strings.Count(key, string(filepath.Separator)) != 2 {
			return nil // not the expected Tool/version/platform shape; ignore
		}
		entries[filepath.ToSlash(key)] = struct{}{}
		return nil
	})
	return entries, err
}

// DiffToolCache returns the keys present in after but not before, sorted. These are the
// tool-cache entries installed on-demand between the two snapshots (the misses).
func DiffToolCache(before, after map[string]struct{}) []string {
	var misses []string
	for k := range after {
		if _, ok := before[k]; !ok {
			misses = append(misses, k)
		}
	}
	sort.Strings(misses)
	if len(misses) > maxToolCacheMisses {
		misses = misses[:maxToolCacheMisses]
	}
	return misses
}
