package config

import (
	"os"
	"testing"
	"time"
)

// Cost days and months are bucketed in this zone. UTC would split a Korean
// working day at 09:00 local, so a "daily" cost chart would cut each day in
// the middle of the morning.
func TestReportLocationDefaultsToSeoul(t *testing.T) {
	t.Setenv("RUNS_FLEET_REPORT_TIMEZONE", "")

	cfg, err := LoadReportLocation()
	if err != nil {
		t.Fatalf("LoadReportLocation() error = %v", err)
	}
	if cfg.String() != defaultReportTimezone {
		t.Errorf("location = %q, want %q", cfg.String(), defaultReportTimezone)
	}
}

func TestReportLocationHonoursTheConfiguredZone(t *testing.T) {
	t.Setenv("RUNS_FLEET_REPORT_TIMEZONE", "America/New_York")

	loc, err := LoadReportLocation()
	if err != nil {
		t.Fatalf("LoadReportLocation() error = %v", err)
	}
	if loc.String() != "America/New_York" {
		t.Errorf("location = %q, want America/New_York", loc.String())
	}
}

func TestReportLocationAcceptsUTC(t *testing.T) {
	t.Setenv("RUNS_FLEET_REPORT_TIMEZONE", "UTC")

	loc, err := LoadReportLocation()
	if err != nil {
		t.Fatalf("LoadReportLocation() error = %v", err)
	}
	if loc.String() != "UTC" {
		t.Errorf("location = %q, want UTC", loc.String())
	}
}

// A typo must fail loudly at startup rather than silently bucketing cost into
// the wrong days for the life of the deployment.
func TestReportLocationRejectsAnUnknownZone(t *testing.T) {
	t.Setenv("RUNS_FLEET_REPORT_TIMEZONE", "Mars/Olympus_Mons")

	if _, err := LoadReportLocation(); err == nil {
		t.Fatal("LoadReportLocation() error = nil, want an error for an unknown zone")
	}
}

// The zone must reach Config so every cost surface buckets identically.
func TestConfigCarriesTheReportLocation(t *testing.T) {
	originalEnv := os.Environ()
	t.Cleanup(func() {
		os.Clearenv()
		for _, e := range originalEnv {
			pair := splitEnv(e)
			_ = os.Setenv(pair[0], pair[1])
		}
	})

	cfg := &Config{}
	if cfg.ReportLocation() == nil {
		t.Fatal("ReportLocation() = nil, want a usable zone even on a zero Config")
	}
	// A zero Config must not panic downstream; UTC is the safe stand-in.
	if cfg.ReportLocation().String() != "UTC" {
		t.Errorf("zero-config location = %q, want UTC as the safe fallback",
			cfg.ReportLocation().String())
	}

	loaded := &Config{reportLocation: time.FixedZone("KST", 9*3600)}
	if loaded.ReportLocation().String() != "KST" {
		t.Errorf("location = %q, want the configured zone", loaded.ReportLocation().String())
	}
}
