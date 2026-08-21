package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/cost"
	"github.com/Shavakan/runs-fleet/pkg/db"
	"github.com/Shavakan/runs-fleet/pkg/fleet"
	"github.com/Shavakan/runs-fleet/pkg/logging"
)

// costBucketUnknown labels the catch-all bucket for jobs whose record is
// missing the dimension being grouped on, so their cost is still reported rather
// than silently dropped from a breakdown.
const costBucketUnknown = "unknown"

// CostDB defines the database operations required by the cost handler.
type CostDB interface {
	ListJobsForAdmin(ctx context.Context, filter db.AdminJobFilter) ([]db.AdminJobEntry, int, error)
}

// onDemandPricer supplies live on-demand hourly prices by instance type
// (satisfied by *cost.PriceFetcher). Nil-safe: the handler falls back to the
// hard-coded table when absent.
type onDemandPricer interface {
	GetPrice(ctx context.Context, instanceType string) (float64, error)
}

// spotPricer supplies the current market spot hourly price by instance type
// (satisfied by *fleet.Manager). The bool is false when no price is available,
// so the handler falls back to the fixed spot-discount estimate.
type spotPricer interface {
	SpotPrice(ctx context.Context, instanceType string) (float64, bool)
}

// CostSummaryResponse represents the cost summary API response.
type CostSummaryResponse struct {
	PeriodStart     string                 `json:"period_start"`
	PeriodEnd       string                 `json:"period_end"`
	TotalCost       float64                `json:"total_cost"`
	SpotCost        float64                `json:"spot_cost"`
	OnDemandCost    float64                `json:"on_demand_cost"`
	SpotSavings     float64                `json:"spot_savings"`
	AvgCostPerJob   float64                `json:"avg_cost_per_job"`
	TotalMinutes    float64                `json:"total_minutes"`
	CostPerMinute   float64                `json:"cost_per_minute"`
	JobCount        int                    `json:"job_count"`
	SpotJobCount    int                    `json:"spot_job_count"`
	OnDemandCount   int                    `json:"on_demand_count"`
	FamilyBreakdown []FamilyBreakdownEntry `json:"family_breakdown"`

	// Fleet is the sampled cost of every managed instance, whether or not it
	// ever ran a job. Nil — and omitted — when nothing has been sampled, so the
	// page degrades to its job-only figures rather than showing a false zero.
	// TotalCost above is unchanged and stays the job-attributed headline.
	Fleet *FleetCostBlock `json:"fleet,omitempty"`

	// RunnerMinuteBreakdown carries the per-runner-shape unit price: the cost
	// actually incurred for each (arch, vCPU) shape and its per-minute rate,
	// alongside the hosted-runner baseline for the same usage.
	// RunnerMinuteCost is that baseline's total (Σ vCPU-minutes ×
	// per-vCPU-minute rate), not runs-fleet spend.
	RunnerMinuteCost      float64             `json:"runner_minute_cost"`
	RunnerMinuteRates     map[string]float64  `json:"runner_minute_rates"`
	RunnerMinuteBreakdown []RunnerMinuteEntry `json:"runner_minute_breakdown"`
}

// RunnerMinuteEntry is one (arch, vCPU) runner shape's unit cost. Cost and
// CostPerMinute are what runs-fleet actually incurred; the Baseline fields
// price the identical minutes at the standard per-vCPU-minute rate so the two
// can be compared cell by cell.
type RunnerMinuteEntry struct {
	Arch                  string  `json:"arch"`
	Vcpu                  int     `json:"vcpu"`
	RunnerMinutes         float64 `json:"runner_minutes"`
	VcpuMinutes           float64 `json:"vcpu_minutes"`
	Cost                  float64 `json:"cost"`
	CostPerMinute         float64 `json:"cost_per_minute"`
	BaselineCost          float64 `json:"baseline_cost"`
	BaselineCostPerMinute float64 `json:"baseline_cost_per_minute"`
}

// FamilyBreakdownEntry represents cost breakdown for one instance family.
type FamilyBreakdownEntry struct {
	Family        string  `json:"family"`
	JobCount      int     `json:"job_count"`
	TotalHours    float64 `json:"total_hours"`
	TotalCost     float64 `json:"total_cost"`
	CostPerMinute float64 `json:"cost_per_minute"`
	SpotPercent   float64 `json:"spot_percent"`
}

// CostDailyResponse is the per-day cost time series for the current month.
type CostDailyResponse struct {
	PeriodStart string         `json:"period_start"`
	PeriodEnd   string         `json:"period_end"`
	Days        []CostDayEntry `json:"days"`
}

// CostDayEntry is one calendar day's cost (zero-filled for days with no jobs).
type CostDayEntry struct {
	Date          string  `json:"date"` // YYYY-MM-DD (UTC)
	TotalCost     float64 `json:"total_cost"`
	SpotCost      float64 `json:"spot_cost"`
	OnDemandCost  float64 `json:"on_demand_cost"`
	TotalMinutes  float64 `json:"total_minutes"`
	CostPerMinute float64 `json:"cost_per_minute"`
	JobCount      int     `json:"job_count"`
}

// CostByPoolResponse is month-to-date cost grouped by warm pool.
type CostByPoolResponse struct {
	PeriodStart string          `json:"period_start"`
	PeriodEnd   string          `json:"period_end"`
	Pools       []CostPoolEntry `json:"pools"`
}

// CostPoolEntry is one pool's month-to-date cost. Cold-start (poolless) jobs are
// grouped under the "cold-start" pseudo-pool.
type CostPoolEntry struct {
	Pool          string  `json:"pool"`
	JobCount      int     `json:"job_count"`
	TotalCost     float64 `json:"total_cost"`
	SpotCost      float64 `json:"spot_cost"`
	OnDemandCost  float64 `json:"on_demand_cost"`
	TotalMinutes  float64 `json:"total_minutes"`
	CostPerMinute float64 `json:"cost_per_minute"`
	SpotPercent   float64 `json:"spot_percent"`
}

// CostByRepositoryResponse is month-to-date cost grouped by source repository,
// sorted by cost descending. Every repository is returned rather than a top-N
// slice: the UI filters and pages client-side, and truncating here would leave
// the visible rows failing to sum to the summary's TotalCost.
type CostByRepositoryResponse struct {
	PeriodStart  string                `json:"period_start"`
	PeriodEnd    string                `json:"period_end"`
	Repositories []CostRepositoryEntry `json:"repositories"`
}

// CostRepositoryEntry is one repository's month-to-date cost. Jobs whose record
// carries no repo are grouped under the "unknown" pseudo-repository.
type CostRepositoryEntry struct {
	Repository    string  `json:"repository"`
	JobCount      int     `json:"job_count"`
	TotalCost     float64 `json:"total_cost"`
	SpotCost      float64 `json:"spot_cost"`
	OnDemandCost  float64 `json:"on_demand_cost"`
	AvgCostPerJob float64 `json:"avg_cost_per_job"`
	TotalMinutes  float64 `json:"total_minutes"`
	CostPerMinute float64 `json:"cost_per_minute"`
	SpotPercent   float64 `json:"spot_percent"`
}

// FleetCostBlock is the sampled cost of the whole managed fleet, and how much
// of it the job-based attribution accounts for.
//
// It answers what TotalCost cannot: TotalCost prices job execution time from
// job records, so it cannot see boot and teardown, idle pool capacity, stopped
// instances still paying for storage, or any instance that never ran a job.
type FleetCostBlock struct {
	TotalCost   float64 `json:"total_cost"`
	ComputeCost float64 `json:"compute_cost"`
	EBSCost     float64 `json:"ebs_cost"`

	// AttributedCost is the share incurred while an instance was running a job;
	// UnattributedCost is the rest. AttributedPercent comes from the sampler's
	// own busy-versus-total instance-seconds, not from dividing the job-priced
	// total by this one — job records are deleted after 7 days, so that ratio
	// would decay across a month for calendar reasons rather than real ones.
	AttributedCost    float64 `json:"attributed_cost"`
	UnattributedCost  float64 `json:"unattributed_cost"`
	AttributedPercent float64 `json:"attributed_percent"`

	DaysCovered  int `json:"days_covered"`
	DaysInPeriod int `json:"days_in_period"`

	// Partial marks a total known to understate; Warning says why in words the
	// UI can show directly.
	Partial bool   `json:"partial"`
	Warning string `json:"warning,omitempty"`

	// EBSEstimated is always true: storage is priced from an assumed volume
	// size, because DescribeInstances reports volume IDs but not their sizes.
	EBSEstimated bool `json:"ebs_estimated"`
}

// CostHandler provides HTTP endpoints for cost reporting.
type CostHandler struct {
	db             CostDB
	auth           *AuthMiddleware
	onDemand       onDemandPricer
	spot           spotPricer
	fleetCost      cost.FleetCostStore
	reportLocation *time.Location
	rates          map[string]float64
	log            *logging.Logger
}

// SetReportLocation sets the zone cost days and months are bucketed in. It must
// match the zone the fleet sampler writes its day keys in, or the two disagree
// about which day a cost belongs to. Defaults to UTC when unset.
func (h *CostHandler) SetReportLocation(loc *time.Location) {
	h.reportLocation = loc
}

// location returns the configured reporting zone, or UTC.
func (h *CostHandler) location() *time.Location {
	if h.reportLocation == nil {
		return time.UTC
	}
	return h.reportLocation
}

// SetFleetCostStore wires the sampled fleet-cost reader.
//
// Optional by design: with no store the cost responses omit their fleet fields
// entirely and the page renders exactly as it did before. Omission is the
// honest signal — a zero would sit beside a non-zero attributed cost and read
// as "the fleet has no overhead".
func (h *CostHandler) SetFleetCostStore(s cost.FleetCostStore) {
	h.fleetCost = s
}

// NewCostHandler creates a new cost handler. onDemand and spot supply live
// AWS prices; both may be nil, in which case pricing falls back to the
// hard-coded on-demand table and fixed spot discount.
func NewCostHandler(db CostDB, auth *AuthMiddleware, onDemand onDemandPricer, spot spotPricer) *CostHandler {
	return &CostHandler{
		db:       db,
		auth:     auth,
		onDemand: onDemand,
		spot:     spot,
		// A fresh copy — h.rates is exposed in the JSON response and must never
		// become a handle to the package default map.
		rates: cost.DefaultRunnerMinuteRates(),
		log:   logging.WithComponent(logging.LogTypeAdmin, "cost"),
	}
}

// RegisterRoutes registers cost API routes on the given mux.
func (h *CostHandler) RegisterRoutes(mux *http.ServeMux) {
	mux.Handle("GET /api/cost/summary", h.auth.WrapFunc(h.GetCostSummary))
	mux.Handle("GET /api/cost/daily", h.auth.WrapFunc(h.GetCostDaily))
	mux.Handle("GET /api/cost/by-pool", h.auth.WrapFunc(h.GetCostByPool))
	mux.Handle("GET /api/cost/by-repository", h.auth.WrapFunc(h.GetCostByRepository))
}

// GetCostSummary handles GET /api/cost/summary.
func (h *CostHandler) GetCostSummary(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	jobs, periodStart, periodEnd, err := h.monthToDateJobs(ctx)
	if err != nil {
		h.writeError(w, http.StatusInternalServerError, "Failed to fetch jobs", err.Error())
		return
	}

	summary := h.computeCostSummary(ctx, jobs, periodStart, periodEnd)
	summary.Fleet = h.fleetBlock(ctx, periodStart, periodEnd)
	h.writeJSON(w, http.StatusOK, summary)
}

// fleetBlock reads the sampled fleet cost for the period, or nil when there is
// none to report.
//
// A read failure degrades to nil and a log line rather than an error response:
// the fleet figure is secondary, and losing it must never take down the
// job-based numbers that already work.
func (h *CostHandler) fleetBlock(ctx context.Context, start, end time.Time) *FleetCostBlock {
	if h.fleetCost == nil {
		return nil
	}
	mtd, err := cost.ComputeFleetMTDIn(ctx, h.fleetCost, start, end, h.location())
	if err != nil {
		h.log.Warn(ctx, "fleet cost unavailable",
			slog.String(logging.KeyError, err.Error()))
		return nil
	}
	if mtd == nil {
		return nil
	}
	return &FleetCostBlock{
		TotalCost:         mtd.TotalCost,
		ComputeCost:       mtd.ComputeCost,
		EBSCost:           mtd.EBSCost,
		AttributedCost:    mtd.AttributedCost,
		UnattributedCost:  mtd.UnattributedCost,
		AttributedPercent: mtd.AttributedPercent,
		DaysCovered:       mtd.DaysCovered,
		DaysInPeriod:      mtd.DaysInPeriod,
		Partial:           mtd.Partial,
		Warning:           fleetWarning(mtd.Partial, mtd.DaysCovered, mtd.DaysInPeriod),
		EBSEstimated:      true,
	}
}

// fleetWarning explains, in words the UI can show as-is, why a fleet total
// understates. Empty when the period is fully sampled.
func fleetWarning(partial bool, daysCovered, daysInPeriod int) string {
	if !partial {
		return ""
	}
	return fmt.Sprintf(
		"Sampled %d of %d days in this period, so fleet cost understates actual spend.",
		daysCovered, daysInPeriod)
}

// monthToDateJobs fetches every finished job since the start of the current UTC
// month -- the shared query behind every cost endpoint. "Finished" is keyed on
// completed_at rather than a status value: the stored status is GitHub's raw
// conclusion (success/failure/interrupted/...), all of which burned billable EC2
// time, while unfinished rows have no duration and would be priced with the
// pricer's 0.5h fallback. The query is unlimited on purpose -- aggregation must
// see every row in the month, and Since already bounds the window.
func (h *CostHandler) monthToDateJobs(ctx context.Context) ([]db.AdminJobEntry, time.Time, time.Time, error) {
	// "This month" is the operator's month, not UTC's: a UTC boundary rolls the
	// total over mid-morning on the 1st and buckets the daily chart against a
	// day that starts at 09:00 local.
	loc := h.location()
	now := time.Now().In(loc)
	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, loc)
	jobs, _, err := h.db.ListJobsForAdmin(ctx, db.AdminJobFilter{
		CompletedOnly: true,
		Since:         start,
	})
	return jobs, start, now, err
}

// CostMTD returns the month-to-date total EC2 cost across completed jobs. Shared
// with the metrics summary so both report the same figure.
func (h *CostHandler) CostMTD(ctx context.Context) (float64, error) {
	jobs, _, _, err := h.monthToDateJobs(ctx)
	if err != nil {
		return 0, err
	}
	pricer := cost.NewJobPricer(h.onDemand, h.spot)
	var total float64
	for _, job := range jobs {
		total += pricer.Price(ctx, job).Total
	}
	return total, nil
}

// archVcpuKey keys the runner-minute matrix by (architecture, vCPU count).
type archVcpuKey struct {
	arch string
	vcpu int
}

// costPerMinute turns an aggregate into the unit price that compares directly
// against a hosted runner's per-minute rate. The denominator is billable
// minutes -- the same minutes the numerator was priced from -- so cost always
// equals minutes × the returned rate. Zero when nothing was billed, so an empty
// bucket reads as "no rate" rather than +Inf.
func costPerMinute(total, minutes float64) float64 {
	if minutes <= 0 {
		return 0
	}
	return total / minutes
}

func (h *CostHandler) computeCostSummary(ctx context.Context, jobs []db.AdminJobEntry, start, end time.Time) *CostSummaryResponse {
	type familyAccum struct {
		jobCount  int
		hours     float64
		cost      float64
		spotCount int
	}

	type shapeAccum struct {
		arch         string
		vcpu         int
		runnerMins   float64
		vcpuMinutes  float64
		cost         float64
		baselineCost float64
	}

	families := make(map[string]*familyAccum)
	shapes := make(map[archVcpuKey]*shapeAccum)
	var totalCost, spotCost, onDemandCost, spotSavings, totalMinutes, baselineCost float64
	var spotJobCount, onDemandCount int

	// Per-request pricer so each distinct instance type is priced once, even
	// though the underlying fetchers also cache across requests.
	pricer := cost.NewJobPricer(h.onDemand, h.spot)

	for _, job := range jobs {
		instanceType := job.InstanceType
		if instanceType == "" {
			instanceType = "t4g.medium"
		}

		p := pricer.Price(ctx, job)
		totalCost += p.Total
		spotCost += p.Spot
		onDemandCost += p.OnDemand
		spotSavings += p.Savings
		totalMinutes += p.Hours * 60
		if job.Spot {
			spotJobCount++
		} else {
			onDemandCount++
		}

		family := extractFamily(instanceType)
		acc, ok := families[family]
		if !ok {
			acc = &familyAccum{}
			families[family] = acc
		}
		acc.jobCount++
		acc.hours += p.Hours
		acc.cost += p.Total
		if job.Spot {
			acc.spotCount++
		}

		// Per-shape unit cost, keyed by (arch, vCPU). Uses the actual reported
		// duration (not the EC2-cost 0.5h fallback) so a per-minute rate divides
		// real cost by real minutes. Skips zero-duration jobs (no minutes to
		// price) and instance types not in the catalog (arch/vCPU unknown); the
		// hosted baseline is added only for architectures with a configured rate,
		// while the incurred cost is recorded either way.
		if job.DurationSeconds <= 0 {
			continue
		}
		spec, found := fleet.GetInstanceSpec(instanceType)
		if !found {
			continue
		}
		key := archVcpuKey{arch: spec.Arch, vcpu: spec.CPU}
		shape, ok := shapes[key]
		if !ok {
			shape = &shapeAccum{arch: spec.Arch, vcpu: spec.CPU}
			shapes[key] = shape
		}
		mins := p.Hours * 60
		vcpuMins := mins * float64(spec.CPU)
		shape.runnerMins += mins
		shape.vcpuMinutes += vcpuMins
		shape.cost += p.Total
		if rate, priced := h.rates[spec.Arch]; priced {
			shape.baselineCost += vcpuMins * rate
			baselineCost += vcpuMins * rate
		}
	}

	runnerBreakdown := make([]RunnerMinuteEntry, 0, len(shapes))
	for _, s := range shapes {
		runnerBreakdown = append(runnerBreakdown, RunnerMinuteEntry{
			Arch:                  s.arch,
			Vcpu:                  s.vcpu,
			RunnerMinutes:         s.runnerMins,
			VcpuMinutes:           s.vcpuMinutes,
			Cost:                  s.cost,
			CostPerMinute:         costPerMinute(s.cost, s.runnerMins),
			BaselineCost:          s.baselineCost,
			BaselineCostPerMinute: costPerMinute(s.baselineCost, s.runnerMins),
		})
	}
	sort.Slice(runnerBreakdown, func(i, j int) bool {
		if runnerBreakdown[i].Arch != runnerBreakdown[j].Arch {
			return runnerBreakdown[i].Arch < runnerBreakdown[j].Arch
		}
		return runnerBreakdown[i].Vcpu < runnerBreakdown[j].Vcpu
	})

	breakdown := make([]FamilyBreakdownEntry, 0, len(families))
	for fam, acc := range families {
		spotPct := 0.0
		if acc.jobCount > 0 {
			spotPct = float64(acc.spotCount) / float64(acc.jobCount) * 100
		}
		breakdown = append(breakdown, FamilyBreakdownEntry{
			Family:        fam,
			JobCount:      acc.jobCount,
			TotalHours:    acc.hours,
			TotalCost:     acc.cost,
			CostPerMinute: costPerMinute(acc.cost, acc.hours*60),
			SpotPercent:   spotPct,
		})
	}

	avgCost := 0.0
	if len(jobs) > 0 {
		avgCost = totalCost / float64(len(jobs))
	}

	return &CostSummaryResponse{
		PeriodStart:           start.Format(time.RFC3339),
		PeriodEnd:             end.Format(time.RFC3339),
		TotalCost:             totalCost,
		SpotCost:              spotCost,
		OnDemandCost:          onDemandCost,
		SpotSavings:           spotSavings,
		AvgCostPerJob:         avgCost,
		TotalMinutes:          totalMinutes,
		CostPerMinute:         costPerMinute(totalCost, totalMinutes),
		JobCount:              len(jobs),
		SpotJobCount:          spotJobCount,
		OnDemandCount:         onDemandCount,
		FamilyBreakdown:       breakdown,
		RunnerMinuteCost:      baselineCost,
		RunnerMinuteRates:     h.rates,
		RunnerMinuteBreakdown: runnerBreakdown,
	}
}

// GetCostDaily handles GET /api/cost/daily.
func (h *CostHandler) GetCostDaily(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	jobs, start, end, err := h.monthToDateJobs(ctx)
	if err != nil {
		h.writeError(w, http.StatusInternalServerError, "Failed to fetch jobs", err.Error())
		return
	}
	h.writeJSON(w, http.StatusOK, h.computeDaily(ctx, jobs, start, end))
}

// GetCostByPool handles GET /api/cost/by-pool.
func (h *CostHandler) GetCostByPool(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	jobs, start, end, err := h.monthToDateJobs(ctx)
	if err != nil {
		h.writeError(w, http.StatusInternalServerError, "Failed to fetch jobs", err.Error())
		return
	}
	h.writeJSON(w, http.StatusOK, h.computeByPool(ctx, jobs, start, end))
}

// GetCostByRepository handles GET /api/cost/by-repository.
func (h *CostHandler) GetCostByRepository(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	jobs, start, end, err := h.monthToDateJobs(ctx)
	if err != nil {
		h.writeError(w, http.StatusInternalServerError, "Failed to fetch jobs", err.Error())
		return
	}
	h.writeJSON(w, http.StatusOK, h.computeByRepository(ctx, jobs, start, end))
}

// computeDaily buckets each job's whole cost into the day of its CreatedAt.
// This is intentional: jobs are short-lived CI runs (GitHub caps a job at 6h),
// their cost is a single DurationSeconds-derived lump with no sub-day slices to
// prorate, and CreatedAt is the same dimension monthToDateJobs filters on
// (created_at >= start) -- so every fetched job lands in exactly one zero-filled
// bucket and the daily totals sum to the summary's TotalCost.
func (h *CostHandler) computeDaily(ctx context.Context, jobs []db.AdminJobEntry, start, end time.Time) *CostDailyResponse {
	if start.After(end) {
		return &CostDailyResponse{
			PeriodStart: start.Format(time.RFC3339),
			PeriodEnd:   end.Format(time.RFC3339),
			Days:        []CostDayEntry{},
		}
	}

	type dayAccum struct {
		total, spot, onDemand, minutes float64
		count                          int
	}
	days := make(map[string]*dayAccum)

	pricer := cost.NewJobPricer(h.onDemand, h.spot)
	for _, job := range jobs {
		// Same zone as the zero-filled buckets below, which are generated from
		// start: bucketing in UTC while filling in local would file a job under
		// a key no bucket has.
		key := job.CreatedAt.In(h.location()).Format("2006-01-02")
		p := pricer.Price(ctx, job)
		acc, ok := days[key]
		if !ok {
			acc = &dayAccum{}
			days[key] = acc
		}
		acc.total += p.Total
		acc.spot += p.Spot
		acc.onDemand += p.OnDemand
		acc.minutes += p.Hours * 60
		acc.count++
	}

	// Zero-fill every day from month start through today so the UI can chart a
	// continuous series.
	entries := make([]CostDayEntry, 0)
	for day := start; !day.After(end); day = day.AddDate(0, 0, 1) {
		key := day.Format("2006-01-02")
		entry := CostDayEntry{Date: key}
		if acc, ok := days[key]; ok {
			entry.TotalCost = acc.total
			entry.SpotCost = acc.spot
			entry.OnDemandCost = acc.onDemand
			entry.TotalMinutes = acc.minutes
			entry.CostPerMinute = costPerMinute(acc.total, acc.minutes)
			entry.JobCount = acc.count
		}
		entries = append(entries, entry)
	}

	return &CostDailyResponse{
		PeriodStart: start.Format(time.RFC3339),
		PeriodEnd:   end.Format(time.RFC3339),
		Days:        entries,
	}
}

func (h *CostHandler) computeByPool(ctx context.Context, jobs []db.AdminJobEntry, start, end time.Time) *CostByPoolResponse {
	type poolAccum struct {
		total, spot, onDemand, minutes float64
		count, spotCount               int
	}
	pools := make(map[string]*poolAccum)

	pricer := cost.NewJobPricer(h.onDemand, h.spot)
	for _, job := range jobs {
		pool := job.Pool
		if pool == "" {
			pool = "cold-start"
		}
		p := pricer.Price(ctx, job)
		acc, ok := pools[pool]
		if !ok {
			acc = &poolAccum{}
			pools[pool] = acc
		}
		acc.total += p.Total
		acc.spot += p.Spot
		acc.onDemand += p.OnDemand
		acc.minutes += p.Hours * 60
		acc.count++
		if job.Spot {
			acc.spotCount++
		}
	}

	entries := make([]CostPoolEntry, 0, len(pools))
	for name, acc := range pools {
		spotPct := 0.0
		if acc.count > 0 {
			spotPct = float64(acc.spotCount) / float64(acc.count) * 100
		}
		entries = append(entries, CostPoolEntry{
			Pool:          name,
			JobCount:      acc.count,
			TotalCost:     acc.total,
			SpotCost:      acc.spot,
			OnDemandCost:  acc.onDemand,
			TotalMinutes:  acc.minutes,
			CostPerMinute: costPerMinute(acc.total, acc.minutes),
			SpotPercent:   spotPct,
		})
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].TotalCost > entries[j].TotalCost
	})

	return &CostByPoolResponse{
		PeriodStart: start.Format(time.RFC3339),
		PeriodEnd:   end.Format(time.RFC3339),
		Pools:       entries,
	}
}

func (h *CostHandler) computeByRepository(ctx context.Context, jobs []db.AdminJobEntry, start, end time.Time) *CostByRepositoryResponse {
	type repoAccum struct {
		total, spot, onDemand, minutes float64
		count, spotCount               int
	}
	repos := make(map[string]*repoAccum)

	pricer := cost.NewJobPricer(h.onDemand, h.spot)
	for _, job := range jobs {
		repo := job.Repo
		if repo == "" {
			repo = costBucketUnknown
		}
		p := pricer.Price(ctx, job)
		acc, ok := repos[repo]
		if !ok {
			acc = &repoAccum{}
			repos[repo] = acc
		}
		acc.total += p.Total
		acc.spot += p.Spot
		acc.onDemand += p.OnDemand
		acc.minutes += p.Hours * 60
		acc.count++
		if job.Spot {
			acc.spotCount++
		}
	}

	entries := make([]CostRepositoryEntry, 0, len(repos))
	for name, acc := range repos {
		spotPct, avg := 0.0, 0.0
		if acc.count > 0 {
			spotPct = float64(acc.spotCount) / float64(acc.count) * 100
			avg = acc.total / float64(acc.count)
		}
		entries = append(entries, CostRepositoryEntry{
			Repository:    name,
			JobCount:      acc.count,
			TotalCost:     acc.total,
			SpotCost:      acc.spot,
			OnDemandCost:  acc.onDemand,
			AvgCostPerJob: avg,
			TotalMinutes:  acc.minutes,
			CostPerMinute: costPerMinute(acc.total, acc.minutes),
			SpotPercent:   spotPct,
		})
	}
	// Cost desc, then name for a stable order across equal-cost repositories.
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].TotalCost != entries[j].TotalCost {
			return entries[i].TotalCost > entries[j].TotalCost
		}
		return entries[i].Repository < entries[j].Repository
	})

	return &CostByRepositoryResponse{
		PeriodStart:  start.Format(time.RFC3339),
		PeriodEnd:    end.Format(time.RFC3339),
		Repositories: entries,
	}
}

func (h *CostHandler) writeJSON(w http.ResponseWriter, status int, data interface{}) {
	// Response-writer helper with no request/context in scope.
	ctx := context.Background()
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(data); err != nil {
		h.log.Error(ctx, "json encode failed", slog.String(logging.KeyError, err.Error()))
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := buf.WriteTo(w); err != nil {
		h.log.Error(ctx, "write response failed", slog.String(logging.KeyError, err.Error()))
	}
}

func (h *CostHandler) writeError(w http.ResponseWriter, status int, message, details string) {
	resp := ErrorResponse{Error: message}
	if details != "" {
		resp.Details = details
	}
	h.writeJSON(w, status, resp)
}

func extractFamily(instanceType string) string {
	if instanceType == "" {
		return costBucketUnknown
	}
	parts := strings.SplitN(instanceType, ".", 2)
	if len(parts) >= 1 && parts[0] != "" {
		return parts[0]
	}
	return costBucketUnknown
}
