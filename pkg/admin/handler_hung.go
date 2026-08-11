package admin

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/db"
	"github.com/Shavakan/runs-fleet/pkg/logging"
)

// GitHubJobStatus is GitHub's view of a workflow job: the authority on whether
// work is actually in progress. Our own record cannot answer that — a runner we
// minted and confirmed may never have been handed the job it was minted for.
type GitHubJobStatus struct {
	Status     string // "queued", "in_progress", "completed"
	Conclusion string // "success", "failure", "cancelled", ...
	RunnerName string // the runner GitHub gave the job to, if any
}

// GitHubJobStatusChecker queries GitHub for one workflow job.
type GitHubJobStatusChecker interface {
	GetWorkflowJobStatus(ctx context.Context, repo string, jobID int64) (*GitHubJobStatus, error)
}

// Classifications of an old, still-open job record.
const (
	// ClassificationHung means GitHub still has the job queued: nothing is
	// running it, so the runner we provisioned never took it.
	ClassificationHung = "hung"
	// ClassificationRunning means the job really is executing and is simply long.
	ClassificationRunning = "running"
	// ClassificationCompletedUpstream means the job finished at GitHub and only
	// our bookkeeping is behind.
	ClassificationCompletedUpstream = "completed_upstream"
	// ClassificationUnknown means we could not ask. It is never a guess.
	ClassificationUnknown = "unknown"
)

const (
	defaultHungLimit = 25
	maxHungLimit     = 50
	// hungCheckConcurrency bounds in-flight GitHub calls. The scheduled
	// stale-jobs task budgets 30 calls per 5-minute cycle against a 5000/hour
	// limit; an operator-triggered page is far rarer than that, and a handful
	// at a time keeps the page responsive without a burst.
	hungCheckConcurrency = 5
)

// HungHandler answers "which jobs are actually stuck?" — the question the
// console could not previously ask.
//
// Age alone cannot answer it: an old open record may be a healthy long build.
// So age selects candidates and GitHub returns the verdict.
type HungHandler struct {
	db     JobsDB
	github GitHubJobStatusChecker
	auth   *AuthMiddleware
	log    *logging.Logger
}

// NewHungHandler creates the hung-jobs handler. github is optional: without it
// the endpoint still reports age-based candidates, classified unknown, rather
// than failing outright.
func NewHungHandler(jobsDB JobsDB, github GitHubJobStatusChecker, auth *AuthMiddleware) *HungHandler {
	return &HungHandler{
		db:     jobsDB,
		github: github,
		auth:   auth,
		log:    logging.WithComponent(logging.LogTypeAdmin, "hung"),
	}
}

// RegisterRoutes registers the hung-jobs routes.
func (h *HungHandler) RegisterRoutes(mux *http.ServeMux) {
	mux.Handle("GET /api/jobs/hung", h.auth.WrapFunc(h.ListHungJobs))
	mux.Handle("GET /api/jobs/{id}/github", h.auth.WrapFunc(h.GetJobGitHubStatus))
}

// HungJob is one candidate with GitHub's verdict attached.
type HungJob struct {
	JobResponse
	GitHubStatus     string `json:"github_status,omitempty"`
	GitHubConclusion string `json:"github_conclusion,omitempty"`
	RunnerName       string `json:"runner_name,omitempty"`
	Classification   string `json:"classification"`
	// Detail explains an unknown classification. Empty otherwise.
	Detail string `json:"detail,omitempty"`
}

// HungJobsResponse reports one hung-jobs sweep.
type HungJobsResponse struct {
	Jobs []HungJob `json:"jobs"`
	// Candidates is every old open record found, before the check cap.
	Candidates int `json:"candidates"`
	// Checked is how many of them were verified against GitHub.
	Checked         int  `json:"checked"`
	Truncated       bool `json:"truncated"`
	GitHubAvailable bool `json:"github_available"`
	StaleMinutes    int  `json:"stale_minutes"`
}

// GitHubJobStatusResponse is GitHub's view of a single job.
type GitHubJobStatusResponse struct {
	JobID      int64  `json:"job_id"`
	Repo       string `json:"repo"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion,omitempty"`
	RunnerName string `json:"runner_name,omitempty"`
}

// ListHungJobs handles GET /api/jobs/hung.
//
// Query params:
//   - stale_minutes: how old an open record must be to be a candidate (default 15)
//   - limit: how many candidates to verify against GitHub (default 25, max 50)
func (h *HungHandler) ListHungJobs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()

	staleAfter, err := parseStaleAfter(q.Get("stale_minutes"))
	if err != nil {
		h.writeError(w, http.StatusBadRequest, "Invalid stale_minutes", err.Error())
		return
	}

	limit := defaultHungLimit
	if raw := q.Get("limit"); raw != "" {
		n, convErr := strconv.Atoi(raw)
		if convErr != nil || n <= 0 || n > maxHungLimit {
			h.writeError(w, http.StatusBadRequest, "Invalid limit",
				"must be a whole number between 1 and "+strconv.Itoa(maxHungLimit))
			return
		}
		limit = n
	}

	now := time.Now()
	// No DB-side limit: ListJobsForAdmin paginates newest-first, and the rows
	// worth an operator's attention are the oldest. Cap after sorting instead.
	entries, _, err := h.db.ListJobsForAdmin(ctx, db.AdminJobFilter{StaleBefore: now.Add(-staleAfter)})
	if err != nil {
		h.log.Error(ctx, "failed to list stale jobs", slog.String(logging.KeyError, err.Error()))
		h.writeError(w, http.StatusInternalServerError, "Failed to list stale jobs", err.Error())
		return
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].CreatedAt.Before(entries[j].CreatedAt)
	})

	resp := HungJobsResponse{
		Candidates:      len(entries),
		GitHubAvailable: h.github != nil,
		StaleMinutes:    int(staleAfter.Minutes()),
		Jobs:            []HungJob{},
	}
	if len(entries) > limit {
		entries = entries[:limit]
		resp.Truncated = true
	}
	resp.Checked = len(entries)

	jobs := make([]HungJob, len(entries))
	var wg sync.WaitGroup
	sem := make(chan struct{}, hungCheckConcurrency)
	for i, e := range entries {
		jobs[i] = HungJob{JobResponse: jobEntryToResponse(e, now, staleAfter)}

		if h.github == nil {
			jobs[i].Classification = ClassificationUnknown
			jobs[i].Detail = "no GitHub client configured; classified by age only"
			continue
		}
		if e.Repo == "" {
			jobs[i].Classification = ClassificationUnknown
			jobs[i].Detail = "record has no repo, so GitHub cannot be asked"
			continue
		}

		wg.Add(1)
		go func(i int, e db.AdminJobEntry) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			status, err := h.github.GetWorkflowJobStatus(ctx, e.Repo, e.JobID)
			if err != nil {
				jobs[i].Classification = ClassificationUnknown
				jobs[i].Detail = err.Error()
				return
			}
			jobs[i].GitHubStatus = status.Status
			jobs[i].GitHubConclusion = status.Conclusion
			jobs[i].RunnerName = status.RunnerName
			jobs[i].Classification = classify(status.Status)
			if jobs[i].Classification == ClassificationUnknown {
				jobs[i].Detail = "unrecognized GitHub status " + strconv.Quote(status.Status)
			}
		}(i, e)
	}
	wg.Wait()

	resp.Jobs = jobs
	h.writeJSON(w, http.StatusOK, resp)
}

// classify turns GitHub's job status into a verdict on our own record.
func classify(githubStatus string) string {
	switch githubStatus {
	case "queued", "waiting", "pending":
		return ClassificationHung
	case "in_progress":
		return ClassificationRunning
	case "completed":
		return ClassificationCompletedUpstream
	default:
		return ClassificationUnknown
	}
}

// GetJobGitHubStatus handles GET /api/jobs/{id}/github.
func (h *HungHandler) GetJobGitHubStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	jobID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		h.writeError(w, http.StatusBadRequest, "Invalid job ID", err.Error())
		return
	}

	if h.github == nil {
		h.writeError(w, http.StatusServiceUnavailable, "GitHub not configured",
			"no GitHub App credentials are wired into the orchestrator")
		return
	}

	entry, err := h.db.GetJobForAdmin(ctx, jobID)
	if err != nil {
		h.log.Error(ctx, "failed to get job",
			slog.Int64(logging.KeyJobID, jobID),
			slog.String(logging.KeyError, err.Error()))
		h.writeError(w, http.StatusInternalServerError, "Failed to get job", err.Error())
		return
	}
	if entry == nil {
		h.writeError(w, http.StatusNotFound, "Job not found", "")
		return
	}
	if entry.Repo == "" {
		h.writeError(w, http.StatusNotFound, "Job has no repo", "GitHub cannot be asked about a record with no repo")
		return
	}

	status, err := h.github.GetWorkflowJobStatus(ctx, entry.Repo, jobID)
	if err != nil {
		h.writeError(w, http.StatusBadGateway, "GitHub lookup failed", err.Error())
		return
	}

	h.writeJSON(w, http.StatusOK, GitHubJobStatusResponse{
		JobID:      jobID,
		Repo:       entry.Repo,
		Status:     status.Status,
		Conclusion: status.Conclusion,
		RunnerName: status.RunnerName,
	})
}

func (h *HungHandler) writeJSON(w http.ResponseWriter, status int, data interface{}) {
	// Response-writer helper with no request/context in scope.
	ctx := context.Background()
	buf, err := json.Marshal(data)
	if err != nil {
		h.log.Error(ctx, "json encode failed", slog.String(logging.KeyError, err.Error()))
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	buf = append(buf, '\n')
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := w.Write(buf); err != nil {
		h.log.Error(ctx, "response write failed", slog.String(logging.KeyError, err.Error()))
	}
}

func (h *HungHandler) writeError(w http.ResponseWriter, status int, message, details string) {
	resp := ErrorResponse{Error: message}
	if details != "" {
		resp.Details = details
	}
	h.writeJSON(w, status, resp)
}
