package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// HotPoolCaps are the fleet-wide safety ceilings for the hot-pool auto-tuner and
// the admin overrides. They are the only Helm-configurable knob for hot pools
// besides the master toggle: per-pool linger/maxHot is auto-deduced (or
// operator-overridden in the admin UI) and can never exceed these ceilings, so a
// runaway recommendation cannot burn unbounded EC2 spend. The tuner and the admin
// override validation read the same HotPoolCaps, so there is one source of truth.
type HotPoolCaps struct {
	// MaxLingerMinutes caps the linger window the tuner may recommend or an
	// operator may override. Also the ceiling reconcile clamps to.
	MaxLingerMinutes int `json:"maxLingerMinutes"`
	// MaxHot caps the number of running spares kept during a linger window.
	MaxHot int `json:"maxHot"`
	// MinJobsToActivate is the minimum job count in the lookback window before a
	// pool is eligible for a hot recommendation (cold-until-proven).
	MinJobsToActivate int `json:"minJobsToActivate"`
	// LookbackDays is the history window the tuner samples per tick.
	LookbackDays int `json:"lookbackDays"`
	// BurstGapMinutes is the inter-job gap above which two jobs are treated as
	// belonging to separate bursts (used to detect the burst pattern that hot
	// pools optimize for).
	BurstGapMinutes int `json:"burstGapMinutes"`
}

// Hot-pool cap defaults, applied when a field is omitted or zero.
const (
	defaultMaxLingerMinutes  = 30
	defaultMaxHot            = 3
	defaultMinJobsToActivate = 20
	defaultLookbackDays      = 7
	defaultBurstGapMinutes   = 20
)

// Hot-pool cap ceilings, so a misconfigured chart cannot request an absurd hot
// footprint. Fail-fast at startup rather than at reconcile time.
const (
	maxCapLingerMinutes = 120
	maxCapMaxHot        = 10
	maxCapLookbackDays  = 90
	maxCapBurstGap      = 120
)

// DefaultHotPoolCaps returns the caps with every field at its default. Used when
// the feature is on but no caps JSON is provided.
func DefaultHotPoolCaps() HotPoolCaps {
	return HotPoolCaps{
		MaxLingerMinutes:  defaultMaxLingerMinutes,
		MaxHot:            defaultMaxHot,
		MinJobsToActivate: defaultMinJobsToActivate,
		LookbackDays:      defaultLookbackDays,
		BurstGapMinutes:   defaultBurstGapMinutes,
	}
}

// WithDefaults returns a copy of the caps with every zero-valued field replaced
// by its default, independently per field. Idempotent: caps already fully
// populated (e.g. by ParseHotPoolCaps) are returned unchanged. Callers hold this
// as the single fill-in rule so no consumer has to guess which fields are
// optional or use one field as a canary for the rest.
func (c HotPoolCaps) WithDefaults() HotPoolCaps {
	d := DefaultHotPoolCaps()
	if c.MaxLingerMinutes == 0 {
		c.MaxLingerMinutes = d.MaxLingerMinutes
	}
	if c.MaxHot == 0 {
		c.MaxHot = d.MaxHot
	}
	if c.MinJobsToActivate == 0 {
		c.MinJobsToActivate = d.MinJobsToActivate
	}
	if c.LookbackDays == 0 {
		c.LookbackDays = d.LookbackDays
	}
	if c.BurstGapMinutes == 0 {
		c.BurstGapMinutes = d.BurstGapMinutes
	}
	return c
}

// ParseHotPoolCaps parses the RUNS_FLEET_HOT_POOL_CAPS JSON object into
// HotPoolCaps. Blank input yields all defaults. Omitted or zero fields take their
// default; a negative value or an out-of-range ceiling is a startup error so a
// misconfiguration surfaces immediately rather than at reconcile time. Unknown
// fields are rejected.
func ParseHotPoolCaps(jsonStr string) (HotPoolCaps, error) {
	caps := HotPoolCaps{}
	trimmed := strings.TrimSpace(jsonStr)
	if trimmed != "" {
		dec := json.NewDecoder(bytes.NewReader([]byte(trimmed)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&caps); err != nil {
			return HotPoolCaps{}, fmt.Errorf("invalid hot pool caps JSON: %w", err)
		}
	}

	if caps.MaxLingerMinutes < 0 || caps.MaxHot < 0 || caps.MinJobsToActivate < 0 ||
		caps.LookbackDays < 0 || caps.BurstGapMinutes < 0 {
		return HotPoolCaps{}, fmt.Errorf("hot pool caps must be non-negative, got %+v", caps)
	}

	caps = caps.WithDefaults()

	if caps.MaxLingerMinutes > maxCapLingerMinutes {
		return HotPoolCaps{}, fmt.Errorf("maxLingerMinutes must not exceed %d, got %d", maxCapLingerMinutes, caps.MaxLingerMinutes)
	}
	if caps.MaxHot > maxCapMaxHot {
		return HotPoolCaps{}, fmt.Errorf("maxHot must not exceed %d, got %d", maxCapMaxHot, caps.MaxHot)
	}
	if caps.LookbackDays > maxCapLookbackDays {
		return HotPoolCaps{}, fmt.Errorf("lookbackDays must not exceed %d, got %d", maxCapLookbackDays, caps.LookbackDays)
	}
	if caps.BurstGapMinutes > maxCapBurstGap {
		return HotPoolCaps{}, fmt.Errorf("burstGapMinutes must not exceed %d, got %d", maxCapBurstGap, caps.BurstGapMinutes)
	}

	return caps, nil
}
