package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// HotPoolSpec is the per-pool hot-linger policy: after job activity, keep MaxHot
// running spares for LingerMinutes past the last job so pipeline stages skip the
// stopped-instance boot. Opt-in per pool via RUNS_FLEET_HOT_POOLS; a pool absent
// from the map is never lingered (identical to today's behavior).
type HotPoolSpec struct {
	// LingerMinutes is how long after the last job the pool stays hot. Bounded
	// 1-120 so linger stays well inside the 4h ephemeral-cleanup window.
	LingerMinutes int `json:"lingerMinutes"`
	// MaxHot is how many running spares to keep during the linger window.
	// Bounded 1-5; defaults to 1 when omitted or zero (cost-first).
	MaxHot int `json:"maxHot"`
}

const (
	hotPoolLingerMin = 1
	hotPoolLingerMax = 120
	hotPoolMaxHotMin = 1
	hotPoolMaxHotMax = 5
)

// hotPoolNameRe mirrors validPoolName (cmd/server/main.go) so a hot-pool key can
// only name a pool the webhook would also accept.
var hotPoolNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`)

// ParseHotPools builds the per-pool hot-linger allowlist from a JSON object
// keyed by pool name. Blank input yields a nil map (feature off). It fails on
// malformed JSON, an unknown field, an out-of-range bound, or an invalid pool
// name, so misconfiguration surfaces at startup rather than at reconcile time.
// MaxHot defaults to 1 when omitted or zero.
func ParseHotPools(jsonStr string) (map[string]HotPoolSpec, error) {
	trimmed := strings.TrimSpace(jsonStr)
	if trimmed == "" {
		return nil, nil
	}

	dec := json.NewDecoder(bytes.NewReader([]byte(trimmed)))
	dec.DisallowUnknownFields()
	var raw map[string]HotPoolSpec
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("invalid hot pools JSON: %w", err)
	}

	pools := make(map[string]HotPoolSpec, len(raw))
	for name, spec := range raw {
		if !isValidHotPoolName(name) {
			return nil, fmt.Errorf("invalid pool name %q: must be 1-63 chars matching %s", name, hotPoolNameRe.String())
		}
		if spec.LingerMinutes < hotPoolLingerMin || spec.LingerMinutes > hotPoolLingerMax {
			return nil, fmt.Errorf("pool %q: lingerMinutes must be %d-%d, got %d", name, hotPoolLingerMin, hotPoolLingerMax, spec.LingerMinutes)
		}
		if spec.MaxHot == 0 {
			spec.MaxHot = hotPoolMaxHotMin
		}
		if spec.MaxHot < hotPoolMaxHotMin || spec.MaxHot > hotPoolMaxHotMax {
			return nil, fmt.Errorf("pool %q: maxHot must be %d-%d, got %d", name, hotPoolMaxHotMin, hotPoolMaxHotMax, spec.MaxHot)
		}
		pools[name] = spec
	}

	return pools, nil
}

// isValidHotPoolName reports whether s is usable as a warm-pool name.
func isValidHotPoolName(s string) bool {
	return len(s) <= 63 && hotPoolNameRe.MatchString(s)
}
