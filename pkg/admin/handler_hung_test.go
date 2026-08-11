package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/db"
)

type mockJobChecker struct {
	byJob map[int64]*GitHubJobStatus
	err   map[int64]error

	mu    sync.Mutex
	calls int
}

func (m *mockJobChecker) GetWorkflowJobStatus(_ context.Context, _ string, jobID int64) (*GitHubJobStatus, error) {
	m.mu.Lock()
	m.calls++
	m.mu.Unlock()

	if err, ok := m.err[jobID]; ok {
		return nil, err
	}
	if s, ok := m.byJob[jobID]; ok {
		return s, nil
	}
	return nil, fmt.Errorf("job %d not found", jobID)
}

func openJob(id int64, age time.Duration) db.AdminJobEntry {
	now := time.Now()
	return db.AdminJobEntry{
		JobID:      id,
		Repo:       "devsisters/cc-data",
		InstanceID: fmt.Sprintf("i-%012d", id),
		Status:     string(db.JobStatusRunning),
		CreatedAt:  now.Add(-age),
		StartedAt:  now.Add(-age),
	}
}

func decodeHung(t *testing.T, w *httptest.ResponseRecorder) HungJobsResponse {
	t.Helper()
	var resp HungJobsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v (body %s)", err, w.Body.String())
	}
	return resp
}

func TestHungHandler_ClassifiesAgainstGitHub(t *testing.T) {
	t.Parallel()

	jobs := &mockJobsDB{jobs: []db.AdminJobEntry{
		openJob(1, 3*time.Hour),
		openJob(2, 2*time.Hour),
		openJob(3, time.Hour),
	}}
	checker := &mockJobChecker{byJob: map[int64]*GitHubJobStatus{
		// The production hang: our record says running, GitHub never started it.
		1: {Status: "queued"},
		// A genuinely long build looks identical in our table.
		2: {Status: "in_progress", RunnerName: "runs-fleet-runner-cc-000002"},
		3: {Status: "completed", Conclusion: "success"},
	}}

	handler := NewHungHandler(jobs, checker, NewAuthMiddleware(""))
	w := httptest.NewRecorder()
	handler.ListHungJobs(w, httptest.NewRequest(http.MethodGet, "/api/jobs/hung", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	resp := decodeHung(t, w)

	if !resp.GitHubAvailable {
		t.Error("github_available = false, want true")
	}
	if resp.Candidates != 3 || resp.Checked != 3 || resp.Truncated {
		t.Errorf("candidates=%d checked=%d truncated=%v, want 3/3/false", resp.Candidates, resp.Checked, resp.Truncated)
	}

	want := map[int64]string{1: ClassificationHung, 2: ClassificationRunning, 3: ClassificationCompletedUpstream}
	for _, j := range resp.Jobs {
		if got := j.Classification; got != want[j.JobID] {
			t.Errorf("job %d classification = %q, want %q", j.JobID, got, want[j.JobID])
		}
	}
	if len(resp.Jobs) != 3 {
		t.Fatalf("got %d jobs, want 3", len(resp.Jobs))
	}
	if resp.Jobs[0].JobID != 1 {
		t.Errorf("first job = %d, want the oldest (1) at the top", resp.Jobs[0].JobID)
	}
	for _, j := range resp.Jobs {
		if j.JobID == 2 && j.RunnerName != "runs-fleet-runner-cc-000002" {
			t.Errorf("runner name = %q, want it carried through", j.RunnerName)
		}
	}
	if !resp.Jobs[0].Stalled || resp.Jobs[0].ElapsedSeconds < 3600 {
		t.Errorf("job 1 stalled=%v elapsed=%d, want the age carried through", resp.Jobs[0].Stalled, resp.Jobs[0].ElapsedSeconds)
	}
}

func TestHungHandler_StaleFilterIsApplied(t *testing.T) {
	t.Parallel()

	jobs := &mockJobsDB{}
	handler := NewHungHandler(jobs, &mockJobChecker{}, NewAuthMiddleware(""))

	w := httptest.NewRecorder()
	handler.ListHungJobs(w, httptest.NewRequest(http.MethodGet, "/api/jobs/hung?stale_minutes=45", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	if jobs.gotFilter.StaleBefore.IsZero() {
		t.Fatal("StaleBefore was not applied; the scan would return every job in the table")
	}
	if got := time.Since(jobs.gotFilter.StaleBefore); got < 44*time.Minute || got > 46*time.Minute {
		t.Errorf("StaleBefore is %v old, want ~45m", got)
	}
	if jobs.gotFilter.Limit != 0 {
		t.Errorf("Limit = %d, want 0 — a DB-side limit would take the newest rows, not the oldest", jobs.gotFilter.Limit)
	}
}

func TestHungHandler_RejectsBadStaleMinutes(t *testing.T) {
	t.Parallel()

	handler := NewHungHandler(&mockJobsDB{}, &mockJobChecker{}, NewAuthMiddleware(""))
	w := httptest.NewRecorder()
	handler.ListHungJobs(w, httptest.NewRequest(http.MethodGet, "/api/jobs/hung?stale_minutes=nope", nil))

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestHungHandler_TruncationIsReportedAndTakesTheOldest(t *testing.T) {
	t.Parallel()

	jobs := &mockJobsDB{jobs: []db.AdminJobEntry{
		openJob(1, time.Hour),
		openJob(2, 5*time.Hour),
		openJob(3, 3*time.Hour),
	}}
	checker := &mockJobChecker{byJob: map[int64]*GitHubJobStatus{
		1: {Status: "queued"}, 2: {Status: "queued"}, 3: {Status: "queued"},
	}}

	handler := NewHungHandler(jobs, checker, NewAuthMiddleware(""))
	w := httptest.NewRecorder()
	handler.ListHungJobs(w, httptest.NewRequest(http.MethodGet, "/api/jobs/hung?limit=2", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	resp := decodeHung(t, w)

	if resp.Candidates != 3 {
		t.Errorf("candidates = %d, want the full count 3 — a silent top-N reads as 'that is all of them'", resp.Candidates)
	}
	if resp.Checked != 2 || !resp.Truncated {
		t.Errorf("checked=%d truncated=%v, want 2/true", resp.Checked, resp.Truncated)
	}
	if checker.calls != 2 {
		t.Errorf("GitHub calls = %d, want 2 — the cap must bound the API calls, not just the output", checker.calls)
	}
	if len(resp.Jobs) != 2 || resp.Jobs[0].JobID != 2 || resp.Jobs[1].JobID != 3 {
		t.Errorf("jobs = %+v, want the two oldest (2 then 3)", resp.Jobs)
	}
}

func TestHungHandler_WithoutGitHubDegradesToAgeOnly(t *testing.T) {
	t.Parallel()

	jobs := &mockJobsDB{jobs: []db.AdminJobEntry{openJob(1, 3*time.Hour)}}
	handler := NewHungHandler(jobs, nil, NewAuthMiddleware(""))

	w := httptest.NewRecorder()
	handler.ListHungJobs(w, httptest.NewRequest(http.MethodGet, "/api/jobs/hung", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — the age-based list is useful without a verdict", w.Code)
	}
	resp := decodeHung(t, w)
	if resp.GitHubAvailable {
		t.Error("github_available = true, want false")
	}
	if len(resp.Jobs) != 1 || resp.Jobs[0].Classification != ClassificationUnknown {
		t.Fatalf("jobs = %+v, want one unknown", resp.Jobs)
	}
	if resp.Jobs[0].Detail == "" {
		t.Error("unknown classification carries no reason")
	}
}

func TestHungHandler_CheckerErrorIsPerRow(t *testing.T) {
	t.Parallel()

	jobs := &mockJobsDB{jobs: []db.AdminJobEntry{openJob(1, 3*time.Hour), openJob(2, 2*time.Hour)}}
	checker := &mockJobChecker{
		byJob: map[int64]*GitHubJobStatus{2: {Status: "queued"}},
		err:   map[int64]error{1: errors.New("api rate limit exceeded")},
	}

	handler := NewHungHandler(jobs, checker, NewAuthMiddleware(""))
	w := httptest.NewRecorder()
	handler.ListHungJobs(w, httptest.NewRequest(http.MethodGet, "/api/jobs/hung", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	resp := decodeHung(t, w)
	if len(resp.Jobs) != 2 {
		t.Fatalf("got %d jobs, want 2 — one row's failure must not drop the others", len(resp.Jobs))
	}
	if resp.Jobs[0].Classification != ClassificationUnknown {
		t.Errorf("job 1 classification = %q, want unknown", resp.Jobs[0].Classification)
	}
	if resp.Jobs[1].Classification != ClassificationHung {
		t.Errorf("job 2 classification = %q, want hung", resp.Jobs[1].Classification)
	}
}

func TestHungHandler_JobsWithNoRepoAreUnknown(t *testing.T) {
	t.Parallel()

	entry := openJob(1, 3*time.Hour)
	entry.Repo = ""
	jobs := &mockJobsDB{jobs: []db.AdminJobEntry{entry}}
	checker := &mockJobChecker{}

	handler := NewHungHandler(jobs, checker, NewAuthMiddleware(""))
	w := httptest.NewRecorder()
	handler.ListHungJobs(w, httptest.NewRequest(http.MethodGet, "/api/jobs/hung", nil))

	resp := decodeHung(t, w)
	if len(resp.Jobs) != 1 || resp.Jobs[0].Classification != ClassificationUnknown {
		t.Fatalf("jobs = %+v, want one unknown", resp.Jobs)
	}
	if checker.calls != 0 {
		t.Errorf("GitHub calls = %d, want 0 — there is no repo to ask about", checker.calls)
	}
}

func TestHungHandler_ListFailure(t *testing.T) {
	t.Parallel()

	handler := NewHungHandler(&mockJobsDB{err: errors.New("dynamo down")}, &mockJobChecker{}, NewAuthMiddleware(""))
	w := httptest.NewRecorder()
	handler.ListHungJobs(w, httptest.NewRequest(http.MethodGet, "/api/jobs/hung", nil))

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

// The hung routes sit under /api/jobs/ alongside the jobs handler's own
// wildcard patterns. ServeMux panics on a genuine conflict, which would take
// the orchestrator down at boot, so registration is asserted rather than
// assumed.
func TestHungHandler_RoutesCoexistWithJobsHandler(t *testing.T) {
	t.Parallel()

	jobs := &mockJobsDB{jobs: []db.AdminJobEntry{openJob(42, time.Hour)}}
	auth := NewAuthMiddleware("")

	mux := http.NewServeMux()
	NewJobsHandler(jobs, auth, "").RegisterRoutes(mux)
	NewHungHandler(jobs, &mockJobChecker{byJob: map[int64]*GitHubJobStatus{42: {Status: "queued"}}}, auth).RegisterRoutes(mux)

	tests := []struct {
		path      string
		wantRoute string
	}{
		{path: "/api/jobs/hung", wantRoute: "GET /api/jobs/hung"},
		{path: "/api/jobs/42", wantRoute: "GET /api/jobs/{id}"},
		{path: "/api/jobs/42/github", wantRoute: "GET /api/jobs/{id}/github"},
		{path: "/api/jobs/stats", wantRoute: "GET /api/jobs/stats"},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			t.Parallel()

			_, pattern := mux.Handler(httptest.NewRequest(http.MethodGet, tt.path, nil))
			if pattern != tt.wantRoute {
				t.Errorf("%s routed to %q, want %q", tt.path, pattern, tt.wantRoute)
			}
		})
	}
}

func TestHungHandler_GetJobGitHubStatus(t *testing.T) {
	t.Parallel()

	jobs := &mockJobsDB{jobs: []db.AdminJobEntry{openJob(42, time.Hour)}}
	checker := &mockJobChecker{byJob: map[int64]*GitHubJobStatus{
		42: {Status: "queued", RunnerName: ""},
	}}

	tests := []struct {
		name       string
		id         string
		checker    GitHubJobStatusChecker
		wantStatus int
		wantBody   string
	}{
		{name: "known job", id: "42", checker: checker, wantStatus: http.StatusOK, wantBody: "queued"},
		{name: "unknown job", id: "7", checker: checker, wantStatus: http.StatusNotFound},
		{name: "unparseable id", id: "abc", checker: checker, wantStatus: http.StatusBadRequest},
		{name: "no github client", id: "42", checker: nil, wantStatus: http.StatusServiceUnavailable},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			handler := NewHungHandler(jobs, tt.checker, NewAuthMiddleware(""))
			req := httptest.NewRequest(http.MethodGet, "/api/jobs/"+tt.id+"/github", nil)
			req.SetPathValue("id", tt.id)
			w := httptest.NewRecorder()
			handler.GetJobGitHubStatus(w, req)

			if w.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", w.Code, tt.wantStatus, w.Body.String())
			}
			if tt.wantBody != "" {
				var got GitHubJobStatusResponse
				if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
					t.Fatalf("decode: %v", err)
				}
				if got.Status != tt.wantBody {
					t.Errorf("status = %q, want %q", got.Status, tt.wantBody)
				}
			}
		})
	}
}
