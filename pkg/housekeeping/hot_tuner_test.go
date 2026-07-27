package housekeeping

import (
	"testing"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/Shavakan/runs-fleet/pkg/db"
)

var tunerBase = time.Date(2026, 7, 20, 9, 0, 0, 0, time.UTC)

// jobAt builds an AdminJobEntry created at offset from tunerBase running durSec.
func jobAt(offset time.Duration, durSec int) db.AdminJobEntry {
	created := tunerBase.Add(offset)
	return db.AdminJobEntry{CreatedAt: created, CompletedAt: created.Add(time.Duration(durSec) * time.Second)}
}

// burstEntries builds `bursts` bursts of `perBurst` jobs each; jobs within a
// burst are `intraGap` apart with duration durSec; bursts are `interGap` apart.
func burstEntries(bursts, perBurst int, intraGap, interGap time.Duration, durSec int) []db.AdminJobEntry {
	var entries []db.AdminJobEntry
	var cursor time.Duration
	for b := 0; b < bursts; b++ {
		for j := 0; j < perBurst; j++ {
			entries = append(entries, jobAt(cursor, durSec))
			cursor += intraGap
		}
		cursor += interGap
	}
	return entries
}

func TestDeriveAutoTune(t *testing.T) {
	t.Parallel()

	defaults := config.DefaultHotPoolCaps() // MinJobs 20, burstGap 20m, lookback 7, maxLinger 30, maxHot 3

	tests := []struct {
		name    string
		entries []db.AdminJobEntry
		caps    config.HotPoolCaps
		want    db.AutoTuneRec // TunedAt ignored
	}{
		{
			name:    "empty history is insufficient, cold",
			entries: nil,
			caps:    defaults,
			want: db.AutoTuneRec{
				WindowDays: 7, JobCount: 0, Reason: autoTuneReasonInsufficient,
			},
		},
		{
			name:    "below MinJobsToActivate is insufficient, cold",
			entries: burstEntries(1, 5, 5*time.Minute, 0, 60),
			caps:    defaults,
			want: db.AutoTuneRec{
				WindowDays: 7, JobCount: 5, Reason: autoTuneReasonInsufficient,
			},
		},
		{
			name:    "sparse jobs (all gaps exceed burstGap) => no-burst-pattern, cold",
			entries: burstEntries(20, 1, 0, 1*time.Hour, 60),
			caps:    defaults,
			want: db.AutoTuneRec{
				WindowDays: 7, JobCount: 20, BurstCount: 20, Reason: autoTuneReasonNoBurst,
			},
		},
		{
			name:    "burst pattern, non-overlapping => tuned, p90 linger, maxHot 1",
			entries: burstEntries(2, 10, 5*time.Minute, 2*time.Hour, 60),
			caps:    defaults,
			want: db.AutoTuneRec{
				WindowDays: 7, JobCount: 20, BurstCount: 2,
				P90IntraBurstGapSeconds:  300,
				RecommendedLingerMinutes: 5,
				PeakConcurrency:          1,
				RecommendedMaxHot:        1,
				Reason:                   autoTuneReasonTuned,
			},
		},
		{
			name: "overlapping jobs drive peak concurrency (clamped to MaxHot)",
			// 20 jobs, 1 min apart, each 30 min long => deeply overlapping single burst.
			entries: burstEntries(1, 20, 1*time.Minute, 0, 30*60),
			caps:    defaults,
			want: db.AutoTuneRec{
				WindowDays: 7, JobCount: 20, BurstCount: 1,
				P90IntraBurstGapSeconds:  60,
				RecommendedLingerMinutes: 1,
				PeakConcurrency:          20,
				RecommendedMaxHot:        3, // clamped to caps.MaxHot
				Reason:                   autoTuneReasonTuned,
			},
		},
		{
			name:    "caps clamp linger and maxHot",
			entries: burstEntries(1, 20, 5*time.Minute, 0, 30*60),
			caps:    config.HotPoolCaps{MaxLingerMinutes: 3, MaxHot: 1, MinJobsToActivate: 20, LookbackDays: 3, BurstGapMinutes: 20},
			want: db.AutoTuneRec{
				WindowDays: 3, JobCount: 20, BurstCount: 1,
				P90IntraBurstGapSeconds:  300,
				RecommendedLingerMinutes: 3, // ceil(300/60)=5 clamped to 3
				PeakConcurrency:          7, // 30m jobs, 5m apart => ~7 overlap
				RecommendedMaxHot:        1, // clamped to 1
				Reason:                   autoTuneReasonTuned,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := deriveAutoTune(tt.entries, tt.caps)
			if got.TunedAt.IsZero() {
				t.Error("TunedAt not set")
			}
			got.TunedAt = time.Time{} // normalize for comparison
			if got != tt.want {
				t.Errorf("deriveAutoTune()\n got  = %+v\n want = %+v", got, tt.want)
			}
		})
	}
}

func TestPeakConcurrency(t *testing.T) {
	t.Parallel()

	// Two jobs overlapping, one disjoint => peak 2.
	entries := []db.AdminJobEntry{
		jobAt(0, 600),              // [0, 10m]
		jobAt(5*time.Minute, 600),  // [5m, 15m] overlaps first
		jobAt(30*time.Minute, 600), // [30m, 40m] disjoint
	}
	if got := peakConcurrency(entries); got != 2 {
		t.Errorf("peakConcurrency() = %d, want 2", got)
	}

	// Missing CompletedAt is skipped.
	if got := peakConcurrency([]db.AdminJobEntry{{CreatedAt: tunerBase}}); got != 0 {
		t.Errorf("peakConcurrency(no completed) = %d, want 0", got)
	}
}

func TestP90Float(t *testing.T) {
	t.Parallel()

	vals := make([]float64, 10)
	for i := range vals {
		vals[i] = float64(i + 1) // 1..10
	}
	// floor(0.9*(10-1)) = floor(8.1) = 8 => 9th value (0-indexed) = 9.
	if got := p90Float(vals); got != 9 {
		t.Errorf("p90Float(1..10) = %v, want 9", got)
	}
}
