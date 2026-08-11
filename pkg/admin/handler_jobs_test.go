package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/db"
)

type mockJobsDB struct {
	jobs       []db.AdminJobEntry
	stats      *db.AdminJobStats
	err        error
	gotFilter  db.AdminJobFilter
	gotStalled time.Time
}

func (m *mockJobsDB) ListJobsForAdmin(_ context.Context, filter db.AdminJobFilter) ([]db.AdminJobEntry, int, error) {
	m.gotFilter = filter
	if m.err != nil {
		return nil, 0, m.err
	}
	return m.jobs, len(m.jobs), nil
}

func (m *mockJobsDB) GetJobForAdmin(_ context.Context, jobID int64) (*db.AdminJobEntry, error) {
	if m.err != nil {
		return nil, m.err
	}
	for _, j := range m.jobs {
		if j.JobID == jobID {
			return &j, nil
		}
	}
	return nil, nil
}

func (m *mockJobsDB) GetJobStatsForAdmin(_ context.Context, _, stalledBefore time.Time) (*db.AdminJobStats, error) {
	m.gotStalled = stalledBefore
	if m.err != nil {
		return nil, m.err
	}
	return m.stats, nil
}

func TestJobEntryToResponse_ElapsedAndStalled(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 11, 11, 0, 0, 0, time.UTC)
	const staleAfter = 15 * time.Minute

	tests := []struct {
		name        string
		entry       db.AdminJobEntry
		wantElapsed int
		wantStalled bool
	}{
		{
			name: "the production hang: running, started three hours ago, never completed",
			entry: db.AdminJobEntry{
				Status:    string(db.JobStatusRunning),
				CreatedAt: now.Add(-3 * time.Hour),
				StartedAt: now.Add(-3 * time.Hour),
			},
			wantElapsed: 10800,
			wantStalled: true,
		},
		{
			name: "no started_at falls back to created_at",
			entry: db.AdminJobEntry{
				Status:    string(db.JobStatusLaunched),
				CreatedAt: now.Add(-20 * time.Minute),
			},
			wantElapsed: 1200,
			wantStalled: true,
		},
		{
			name: "a stranded requeued record is stalled like any other open record",
			entry: db.AdminJobEntry{
				Status:    string(db.JobStatusRequeued),
				CreatedAt: now.Add(-40 * time.Minute),
			},
			wantElapsed: 2400,
			wantStalled: true,
		},
		{
			name: "young enough that the watchdog has not had its turn",
			entry: db.AdminJobEntry{
				Status:    string(db.JobStatusRunning),
				CreatedAt: now.Add(-time.Minute),
				StartedAt: now.Add(-time.Minute),
			},
			wantElapsed: 60,
			wantStalled: false,
		},
		{
			name: "a completed record reports no elapsed however old it is",
			entry: db.AdminJobEntry{
				Status:      string(db.JobStatusSuccess),
				CreatedAt:   now.Add(-9 * time.Hour),
				StartedAt:   now.Add(-9 * time.Hour),
				CompletedAt: now.Add(-8 * time.Hour),
			},
		},
		{
			name: "a timestamp ahead of the server clock reports nothing rather than a fabricated age",
			entry: db.AdminJobEntry{
				Status:    string(db.JobStatusRunning),
				CreatedAt: now.Add(time.Minute),
				StartedAt: now.Add(time.Minute),
			},
		},
		{
			name:  "no timestamps at all",
			entry: db.AdminJobEntry{Status: string(db.JobStatusRunning)},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := jobEntryToResponse(tt.entry, now, staleAfter)
			if got.ElapsedSeconds != tt.wantElapsed {
				t.Errorf("ElapsedSeconds = %d, want %d", got.ElapsedSeconds, tt.wantElapsed)
			}
			if got.Stalled != tt.wantStalled {
				t.Errorf("Stalled = %v, want %v", got.Stalled, tt.wantStalled)
			}
		})
	}
}

func TestJobsHandler_ListJobs_StaleFilter(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		query         string
		wantStatus    int
		wantStale     bool
		wantStaleSpan time.Duration
	}{
		{
			name:       "no stale param leaves the filter unbounded",
			query:      "",
			wantStatus: http.StatusOK,
		},
		{
			name:          "stale=true applies the default window",
			query:         "?stale=true",
			wantStatus:    http.StatusOK,
			wantStale:     true,
			wantStaleSpan: defaultStaleAfter,
		},
		{
			name:          "stale_minutes overrides the window",
			query:         "?stale=true&stale_minutes=60",
			wantStatus:    http.StatusOK,
			wantStale:     true,
			wantStaleSpan: time.Hour,
		},
		{
			name:       "a non-numeric window is rejected rather than silently defaulted",
			query:      "?stale=true&stale_minutes=soon",
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "an out-of-range window is rejected",
			query:      "?stale=true&stale_minutes=0",
			wantStatus: http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			mockDB := &mockJobsDB{}
			handler := NewJobsHandler(mockDB, NewAuthMiddleware(""), "")

			before := time.Now()
			req := httptest.NewRequest(http.MethodGet, "/api/jobs"+tt.query, nil)
			w := httptest.NewRecorder()
			handler.ListJobs(w, req)
			after := time.Now()

			if w.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", w.Code, tt.wantStatus, w.Body.String())
			}
			if tt.wantStatus != http.StatusOK {
				return
			}

			got := mockDB.gotFilter.StaleBefore
			if got.IsZero() == tt.wantStale {
				t.Fatalf("StaleBefore zero = %v, want stale = %v", got.IsZero(), tt.wantStale)
			}
			if !tt.wantStale {
				return
			}
			if got.Before(before.Add(-tt.wantStaleSpan-time.Minute)) || got.After(after.Add(-tt.wantStaleSpan)) {
				t.Errorf("StaleBefore = %v, want roughly now-%v", got, tt.wantStaleSpan)
			}
		})
	}
}

func TestJobsHandler_GetJobStats_ReportsStalled(t *testing.T) {
	t.Parallel()

	mockDB := &mockJobsDB{stats: &db.AdminJobStats{Total: 10, Running: 4, Stalled: 2}}
	handler := NewJobsHandler(mockDB, NewAuthMiddleware(""), "")

	w := httptest.NewRecorder()
	handler.GetJobStats(w, httptest.NewRequest(http.MethodGet, "/api/jobs/stats", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var resp JobStatsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Stalled != 2 {
		t.Errorf("Stalled = %d, want 2", resp.Stalled)
	}
	if mockDB.gotStalled.IsZero() {
		t.Error("GetJobStatsForAdmin was passed a zero cutoff, so Stalled could never be counted")
	}
}

func TestJobsHandler_ListJobs(t *testing.T) {
	t.Parallel()

	now := time.Now()
	mockDB := &mockJobsDB{
		jobs: []db.AdminJobEntry{
			{
				JobID:     123,
				Repo:      "org/repo",
				Pool:      "default",
				Status:    string(db.JobStatusRunning),
				TraceID:   "0102030405060708090a0b0c0d0e0f10",
				CreatedAt: now,
			},
			{
				JobID:     124,
				Repo:      "org/repo2",
				Pool:      "arm64",
				Status:    string(db.JobStatusCompleted),
				CreatedAt: now.Add(-time.Hour),
			},
		},
	}

	auth := NewAuthMiddleware("")
	handler := NewJobsHandler(mockDB, auth, "")

	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest("GET", "/api/jobs", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}

	var resp struct {
		Jobs   []JobResponse `json:"jobs"`
		Total  int           `json:"total"`
		Limit  int           `json:"limit"`
		Offset int           `json:"offset"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if len(resp.Jobs) != 2 {
		t.Errorf("expected 2 jobs, got %d", len(resp.Jobs))
	}
	if resp.Total != 2 {
		t.Errorf("expected total 2, got %d", resp.Total)
	}
	if resp.Jobs[0].TraceID != "0102030405060708090a0b0c0d0e0f10" {
		t.Errorf("expected trace_id on first job, got %q", resp.Jobs[0].TraceID)
	}
	if resp.Jobs[1].TraceID != "" {
		t.Errorf("expected empty trace_id on second job, got %q", resp.Jobs[1].TraceID)
	}
}

func TestJobsHandler_InvalidStatusFilter(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		status string
		want   int
	}{
		{"valid status running", "running", http.StatusOK},
		{"valid status completed", "completed", http.StatusOK},
		{"valid status orphaned", "orphaned", http.StatusOK},
		{"invalid status", "bogus", http.StatusBadRequest},
		{"invalid status pending", "pending", http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			mockDB := &mockJobsDB{jobs: []db.AdminJobEntry{}}
			auth := NewAuthMiddleware("")
			handler := NewJobsHandler(mockDB, auth, "")

			mux := http.NewServeMux()
			handler.RegisterRoutes(mux)

			req := httptest.NewRequest("GET", "/api/jobs?status="+tt.status, nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			if rec.Code != tt.want {
				t.Errorf("status=%q: got status %d, want %d", tt.status, rec.Code, tt.want)
			}
		})
	}
}

func TestJobsHandler_GetJob(t *testing.T) {
	t.Parallel()

	now := time.Now()
	mockDB := &mockJobsDB{
		jobs: []db.AdminJobEntry{
			{
				JobID:     123,
				Repo:      "org/repo",
				Pool:      "default",
				Status:    string(db.JobStatusRunning),
				CreatedAt: now,
			},
		},
	}

	auth := NewAuthMiddleware("")
	handler := NewJobsHandler(mockDB, auth, "")

	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	t.Run("existing job", func(t *testing.T) {
		t.Parallel()

		req := httptest.NewRequest("GET", "/api/jobs/123", nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("expected status 200, got %d", rec.Code)
		}

		var job JobResponse
		if err := json.NewDecoder(rec.Body).Decode(&job); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}

		if job.JobID != 123 {
			t.Errorf("expected job ID 123, got %d", job.JobID)
		}
	})

	t.Run("job not found", func(t *testing.T) {
		t.Parallel()

		req := httptest.NewRequest("GET", "/api/jobs/999", nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Errorf("expected status 404, got %d", rec.Code)
		}
	})

	t.Run("invalid job ID", func(t *testing.T) {
		t.Parallel()

		req := httptest.NewRequest("GET", "/api/jobs/invalid", nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("expected status 400, got %d", rec.Code)
		}
	})
}

func TestJobsHandler_GetJobStats(t *testing.T) {
	t.Parallel()

	mockDB := &mockJobsDB{
		stats: &db.AdminJobStats{
			Total:       100,
			Completed:   80,
			Failed:      5,
			Running:     10,
			Requeued:    5,
			WarmPoolHit: 60,
		},
	}

	auth := NewAuthMiddleware("")
	handler := NewJobsHandler(mockDB, auth, "")

	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest("GET", "/api/jobs/stats", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}

	var stats JobStatsResponse
	if err := json.NewDecoder(rec.Body).Decode(&stats); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if stats.Total != 100 {
		t.Errorf("expected total 100, got %d", stats.Total)
	}
	if stats.Completed != 80 {
		t.Errorf("expected completed 80, got %d", stats.Completed)
	}
	if stats.HitRate != 0.75 {
		t.Errorf("expected hit rate 0.75, got %f", stats.HitRate)
	}
}

func TestJobsHandler_WithAuth(t *testing.T) {
	t.Parallel()

	mockDB := &mockJobsDB{
		jobs: []db.AdminJobEntry{},
	}

	auth := NewAuthMiddleware("require-auth")
	handler := NewJobsHandler(mockDB, auth, "")

	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	t.Run("without auth header", func(t *testing.T) {
		t.Parallel()

		req := httptest.NewRequest("GET", "/api/jobs", nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Errorf("expected status 401, got %d", rec.Code)
		}
	})

	t.Run("with valid session", func(t *testing.T) {
		t.Parallel()

		token, err := GenerateSessionCookie("require-auth", SessionClaims{
			Username:  "test-user",
			ExpiresAt: time.Now().Add(time.Hour).Unix(),
		})
		if err != nil {
			t.Fatalf("setup: GenerateSessionCookie() error = %v", err)
		}

		req := httptest.NewRequest("GET", "/api/jobs", nil)
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("expected status 200, got %d", rec.Code)
		}
	})
}

func TestJobsHandler_GetTraceURL(t *testing.T) {
	t.Parallel()

	t.Run("configured trace URL", func(t *testing.T) {
		t.Parallel()

		mockDB := &mockJobsDB{}
		auth := NewAuthMiddleware("")
		handler := NewJobsHandler(mockDB, auth, "https://jaeger.example.com/trace/")

		mux := http.NewServeMux()
		handler.RegisterRoutes(mux)

		req := httptest.NewRequest("GET", "/api/config/trace-url", nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("expected status 200, got %d", rec.Code)
		}

		var resp map[string]string
		if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}

		if resp["trace_url"] != "https://jaeger.example.com/trace/" {
			t.Errorf("expected trace_url https://jaeger.example.com/trace/, got %q", resp["trace_url"])
		}
	})

	t.Run("unconfigured trace URL", func(t *testing.T) {
		t.Parallel()

		mockDB := &mockJobsDB{}
		auth := NewAuthMiddleware("")
		handler := NewJobsHandler(mockDB, auth, "")

		mux := http.NewServeMux()
		handler.RegisterRoutes(mux)

		req := httptest.NewRequest("GET", "/api/config/trace-url", nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("expected status 200, got %d", rec.Code)
		}

		var resp map[string]string
		if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}

		if resp["trace_url"] != "" {
			t.Errorf("expected empty trace_url, got %q", resp["trace_url"])
		}
	})
}
