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
	jobs  []db.AdminJobEntry
	stats *db.AdminJobStats
	err   error
}

func (m *mockJobsDB) ListJobsForAdmin(_ context.Context, _ db.AdminJobFilter) ([]db.AdminJobEntry, int, error) {
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

func (m *mockJobsDB) GetJobStatsForAdmin(_ context.Context, _ time.Time) (*db.AdminJobStats, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.stats, nil
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

	t.Run("with auth header", func(t *testing.T) {
		t.Parallel()

		req := httptest.NewRequest("GET", "/api/jobs", nil)
		req.Header.Set(HeaderUser, "test-user")
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
