package config

import (
	"fmt"
	"os"
	"time"
)

// defaultReportTimezone is the zone cost days and months are bucketed in.
//
// Not UTC: the fleet is operated from Korea, and a UTC boundary splits a
// working day at 09:00 local, so a "cost per day" chart would cut every day in
// the middle of the morning and a month-to-date total would roll over
// mid-morning on the 1st.
const defaultReportTimezone = "Asia/Seoul"

// LoadReportLocation resolves the reporting timezone from the environment.
//
// An unparseable zone is an error rather than a silent fallback: bucketing cost
// into the wrong days is invisible once it starts, and the deployment would
// carry the mistake indefinitely.
func LoadReportLocation() (*time.Location, error) {
	name := os.Getenv("RUNS_FLEET_REPORT_TIMEZONE")
	if name == "" {
		name = defaultReportTimezone
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("invalid RUNS_FLEET_REPORT_TIMEZONE=%q: %w", name, err)
	}
	return loc, nil
}

// SetReportLocationForTest overrides the reporting zone. Tests that assert
// day bucketing need a fixed zone; production always goes through Load.
func (c *Config) SetReportLocationForTest(loc *time.Location) {
	c.reportLocation = loc
}

// ReportLocation returns the zone cost reporting buckets days and months in.
//
// Falls back to UTC when unset so a zero Config (tests, partially built
// fixtures) cannot panic a caller that formats a timestamp.
func (c *Config) ReportLocation() *time.Location {
	if c == nil || c.reportLocation == nil {
		return time.UTC
	}
	return c.reportLocation
}
