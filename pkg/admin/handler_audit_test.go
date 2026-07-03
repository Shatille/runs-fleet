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

func TestListAuditLogs_NotConfigured(t *testing.T) {
	t.Parallel()

	h := NewHandler(newMockDB(), nil, NewAuthMiddleware(""))

	req := httptest.NewRequest(http.MethodGet, "/api/audit-logs", nil)
	rec := httptest.NewRecorder()

	h.ListAuditLogs(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}

// TestListAuditLogs_TableUnsetOnConfiguredClient covers the production
// wiring shape: main.go always passes the shared *db.Client as AuditDB
// regardless of whether RUNS_FLEET_AUDIT_TABLE is set, so a nil AuditDB
// never occurs -- HasAuditTable is the real presence signal.
func TestListAuditLogs_TableUnsetOnConfiguredClient(t *testing.T) {
	t.Parallel()

	auditDB := &mockAuditDB{tableUnset: true}
	h := NewHandler(newMockDB(), auditDB, NewAuthMiddleware(""))

	req := httptest.NewRequest(http.MethodGet, "/api/audit-logs", nil)
	rec := httptest.NewRecorder()

	h.ListAuditLogs(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}

func TestListAuditLogs_ReturnsEntries(t *testing.T) {
	t.Parallel()

	now := time.Now()
	auditDB := &mockAuditDB{
		listResult: []db.AuditEntry{
			{ID: "01A", User: "dave", Action: "pool.create", Target: "default", Result: "success", Timestamp: now},
		},
	}
	h := NewHandler(newMockDB(), auditDB, NewAuthMiddleware(""))

	req := httptest.NewRequest(http.MethodGet, "/api/audit-logs", nil)
	rec := httptest.NewRecorder()

	h.ListAuditLogs(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp AuditLogsResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Entries) != 1 || resp.Entries[0].User != "dave" {
		t.Errorf("entries = %+v, want one entry for dave", resp.Entries)
	}
	if resp.Limit != 50 || resp.Offset != 0 {
		t.Errorf("limit/offset = %d/%d, want 50/0 defaults", resp.Limit, resp.Offset)
	}
}

func TestListAuditLogs_QueryParams(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		query      string
		wantFilter db.AuditFilter
	}{
		{
			name:       "user and action filters",
			query:      "user=dave&action=pool.create",
			wantFilter: db.AuditFilter{User: "dave", Action: "pool.create", Limit: 50},
		},
		{
			// Matches ListJobs's exact bounds check (n > 0 && n <= 100):
			// out-of-range values are rejected outright, not clamped.
			name:       "limit exceeding max is rejected, default kept",
			query:      "limit=500",
			wantFilter: db.AuditFilter{Limit: 50},
		},
		{
			name:       "limit ignored if non-positive",
			query:      "limit=0",
			wantFilter: db.AuditFilter{Limit: 50},
		},
		{
			name:       "offset applied",
			query:      "offset=20",
			wantFilter: db.AuditFilter{Limit: 50, Offset: 20},
		},
		{
			name:       "negative offset ignored",
			query:      "offset=-5",
			wantFilter: db.AuditFilter{Limit: 50, Offset: 0},
		},
		{
			name:       "unparseable since/until silently ignored",
			query:      "since=not-a-date&until=also-not-a-date",
			wantFilter: db.AuditFilter{Limit: 50},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			auditDB := &mockAuditDB{}
			h := NewHandler(newMockDB(), auditDB, NewAuthMiddleware(""))

			req := httptest.NewRequest(http.MethodGet, "/api/audit-logs?"+tt.query, nil)
			rec := httptest.NewRecorder()
			h.ListAuditLogs(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
			}
			got := auditDB.gotFilter
			if got.User != tt.wantFilter.User || got.Action != tt.wantFilter.Action ||
				got.Limit != tt.wantFilter.Limit || got.Offset != tt.wantFilter.Offset {
				t.Errorf("filter = %+v, want %+v", got, tt.wantFilter)
			}
		})
	}
}

func TestListAuditLogs_SinceUntilParsed(t *testing.T) {
	t.Parallel()

	since := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)

	auditDB := &mockAuditDB{}
	h := NewHandler(newMockDB(), auditDB, NewAuthMiddleware(""))

	req := httptest.NewRequest(http.MethodGet, "/api/audit-logs?since="+since.Format(time.RFC3339)+"&until="+until.Format(time.RFC3339), nil)
	rec := httptest.NewRecorder()
	h.ListAuditLogs(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if !auditDB.gotFilter.Since.Equal(since) {
		t.Errorf("Since = %v, want %v", auditDB.gotFilter.Since, since)
	}
	if !auditDB.gotFilter.Until.Equal(until) {
		t.Errorf("Until = %v, want %v", auditDB.gotFilter.Until, until)
	}
}

func TestListAuditLogs_DBError(t *testing.T) {
	t.Parallel()

	auditDB := &mockAuditDB{listErr: context.DeadlineExceeded}
	h := NewHandler(newMockDB(), auditDB, NewAuthMiddleware(""))

	req := httptest.NewRequest(http.MethodGet, "/api/audit-logs", nil)
	rec := httptest.NewRecorder()
	h.ListAuditLogs(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
}
