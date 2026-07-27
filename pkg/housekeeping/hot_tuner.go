package housekeeping

import (
	"math"
	"sort"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/Shavakan/runs-fleet/pkg/db"
)

// Auto-tune reasons, persisted so the admin UI can explain a recommendation.
const (
	autoTuneReasonTuned        = "tuned"
	autoTuneReasonInsufficient = "insufficient-history"
	autoTuneReasonNoBurst      = "no-burst-pattern"
)

// deriveAutoTune deduces a pool's hot recommendation from its recent job history,
// returning both the recommendation and the evidence behind it. It is pure (no
// I/O, no clock beyond TunedAt) so the derivation is fully table-testable.
//
// Cold-until-proven: a pool with fewer than caps.MinJobsToActivate jobs in the
// window recommends linger 0 ("insufficient-history"); a pool whose jobs never
// cluster (every inter-job gap exceeds caps.BurstGapMinutes) recommends linger 0
// ("no-burst-pattern"). Otherwise the recommended linger tracks the p90
// intra-burst gap — long enough to keep a spare warm between pipeline stages —
// and the recommended maxHot tracks peak concurrency, both clamped to caps.
func deriveAutoTune(entries []db.AdminJobEntry, caps config.HotPoolCaps) db.AutoTuneRec {
	rec := db.AutoTuneRec{
		WindowDays: caps.LookbackDays,
		JobCount:   len(entries),
		TunedAt:    time.Now().UTC(),
	}

	if len(entries) < caps.MinJobsToActivate {
		rec.Reason = autoTuneReasonInsufficient
		return rec
	}

	sorted := make([]db.AdminJobEntry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].CreatedAt.Before(sorted[j].CreatedAt) })

	// Walk consecutive jobs: a gap over BurstGapMinutes starts a new burst; a gap
	// within it is an intra-burst gap that a warm spare would bridge.
	burstGap := time.Duration(caps.BurstGapMinutes) * time.Minute
	burstCount := 1
	var intraGaps []float64
	for i := 1; i < len(sorted); i++ {
		if sorted[i].CreatedAt.IsZero() || sorted[i-1].CreatedAt.IsZero() {
			continue
		}
		gap := sorted[i].CreatedAt.Sub(sorted[i-1].CreatedAt)
		if gap > burstGap {
			burstCount++
			continue
		}
		intraGaps = append(intraGaps, gap.Seconds())
	}
	rec.BurstCount = burstCount

	if len(intraGaps) == 0 {
		rec.Reason = autoTuneReasonNoBurst
		return rec
	}

	p90Gap := p90Float(intraGaps)
	rec.P90IntraBurstGapSeconds = int(math.Round(p90Gap))
	rec.RecommendedLingerMinutes = clampInt(int(math.Ceil(p90Gap/60.0)), 1, caps.MaxLingerMinutes)

	rec.PeakConcurrency = peakConcurrency(sorted)
	rec.RecommendedMaxHot = clampInt(rec.PeakConcurrency, 1, caps.MaxHot)

	rec.Reason = autoTuneReasonTuned
	return rec
}

// p90Float returns the 90th-percentile value of vals using the same index rule as
// GetPoolP90Concurrency (floor(0.9*(N-1)) over the ascending-sorted samples).
func p90Float(vals []float64) float64 {
	s := append([]float64(nil), vals...)
	sort.Float64s(s)
	return s[int(0.9*float64(len(s)-1))]
}

// peakConcurrency returns the maximum number of jobs whose [CreatedAt, CompletedAt]
// intervals overlap, via a +1/-1 event sweep (modeled on GetPoolP90Concurrency).
// Jobs missing either timestamp are skipped. At an equal timestamp a start is
// ordered before an end so a back-to-back job counts as overlapping (peak, not
// undercount).
func peakConcurrency(entries []db.AdminJobEntry) int {
	type event struct {
		t time.Time
		d int
	}
	var events []event
	for _, e := range entries {
		if e.CreatedAt.IsZero() || e.CompletedAt.IsZero() {
			continue
		}
		events = append(events, event{e.CreatedAt, 1}, event{e.CompletedAt, -1})
	}
	if len(events) == 0 {
		return 0
	}
	sort.Slice(events, func(i, j int) bool {
		if events[i].t.Equal(events[j].t) {
			return events[i].d > events[j].d
		}
		return events[i].t.Before(events[j].t)
	})

	current, peak := 0, 0
	for _, e := range events {
		current += e.d
		if current > peak {
			peak = current
		}
	}
	return peak
}

// clampInt clamps v to [lo, hi].
func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
