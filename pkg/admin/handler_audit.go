package admin

import (
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/db"
)

// AuditEntryResponse represents an audit log entry in API responses.
type AuditEntryResponse struct {
	ID        string         `json:"id"`
	User      string         `json:"user"`
	Action    string         `json:"action"`
	Target    string         `json:"target,omitempty"`
	Result    string         `json:"result"`
	Details   map[string]any `json:"details,omitempty"`
	ClientIP  string         `json:"client_ip,omitempty"`
	Timestamp time.Time      `json:"timestamp"`
}

// AuditLogsResponse is the response body for GET /api/audit-logs.
type AuditLogsResponse struct {
	Entries []AuditEntryResponse `json:"entries"`
	Limit   int                  `json:"limit"`
	Offset  int                  `json:"offset"`
}

// ListAuditLogs handles GET /api/audit-logs. Query params: user, action,
// since, until (RFC3339; unparseable values are silently ignored, matching
// ListJobs's since handling), limit (default 50, max 100), offset.
func (h *Handler) ListAuditLogs(w http.ResponseWriter, r *http.Request) {
	if h.auditDB == nil || !h.auditDB.HasAuditTable() {
		h.writeError(w, http.StatusServiceUnavailable, "Audit table not configured", "")
		return
	}

	q := r.URL.Query()
	filter := db.AuditFilter{
		User:   q.Get("user"),
		Action: q.Get("action"),
		Limit:  50,
	}

	if since := q.Get("since"); since != "" {
		if t, err := time.Parse(time.RFC3339, since); err == nil {
			filter.Since = t
		}
	}
	if until := q.Get("until"); until != "" {
		if t, err := time.Parse(time.RFC3339, until); err == nil {
			filter.Until = t
		}
	}
	if limit := q.Get("limit"); limit != "" {
		if n, err := strconv.Atoi(limit); err == nil && n > 0 && n <= 100 {
			filter.Limit = n
		}
	}
	if offset := q.Get("offset"); offset != "" {
		if n, err := strconv.Atoi(offset); err == nil && n >= 0 {
			filter.Offset = n
		}
	}

	entries, err := h.auditDB.ListAuditLogs(r.Context(), filter)
	if err != nil {
		adminLog.Error(r.Context(), "failed to list audit logs", slog.String("error", err.Error()))
		h.writeError(w, http.StatusInternalServerError, "Failed to list audit logs", err.Error())
		return
	}

	resp := AuditLogsResponse{
		Entries: make([]AuditEntryResponse, len(entries)),
		Limit:   filter.Limit,
		Offset:  filter.Offset,
	}
	for i, e := range entries {
		resp.Entries[i] = AuditEntryResponse{
			ID:        e.ID,
			User:      e.User,
			Action:    e.Action,
			Target:    e.Target,
			Result:    e.Result,
			Details:   e.Details,
			ClientIP:  e.ClientIP,
			Timestamp: e.Timestamp,
		}
	}

	h.writeJSON(w, http.StatusOK, resp)
}
