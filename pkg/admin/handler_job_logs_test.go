package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/Shavakan/runs-fleet/pkg/db"
)

type mockLogsS3 struct {
	objects   []types.Object
	err       error
	gotPrefix []string
}

func (m *mockLogsS3) ListObjectsV2(_ context.Context, params *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	m.gotPrefix = append(m.gotPrefix, *params.Prefix)
	if m.err != nil {
		return nil, m.err
	}
	var matched []types.Object
	for _, o := range m.objects {
		if len(*params.Prefix) <= len(*o.Key) && (*o.Key)[:len(*params.Prefix)] == *params.Prefix {
			matched = append(matched, o)
		}
	}
	return &s3.ListObjectsV2Output{Contents: matched}, nil
}

type mockLogsPresigner struct {
	err error
}

func (m *mockLogsPresigner) PresignGetObject(_ context.Context, params *s3.GetObjectInput, _ ...func(*s3.PresignOptions)) (*v4.PresignedHTTPRequest, error) {
	if m.err != nil {
		return nil, m.err
	}
	return &v4.PresignedHTTPRequest{URL: "https://example.invalid/" + *params.Key + "?signed=1"}, nil
}

func s3obj(key string, size int64) types.Object {
	k := key
	when := time.Date(2026, 8, 18, 10, 11, 12, 0, time.UTC)
	return types.Object{Key: &k, Size: &size, LastModified: &when}
}

func logsHandler(t *testing.T, jobs *mockJobsDB, s3api LogsS3API, presigner LogsPresignAPI, audit AuditDB) *JobsHandler {
	t.Helper()
	h := NewJobsHandler(jobs, NewAuthMiddleware(""), "")
	if s3api != nil {
		h.SetLogSource(s3api, presigner, "test-bucket", "")
	}
	if audit != nil {
		h.SetAuditDB(audit)
	}
	return h
}

func TestGetJobLogs_ReturnsPresignedURLs(t *testing.T) {
	jobs := &mockJobsDB{jobs: []db.AdminJobEntry{{JobID: 99, RunID: 42, InstanceID: "i-abc"}}}
	s3api := &mockLogsS3{objects: []types.Object{
		s3obj("runner-logs/42/99/i-abc/Worker_a.log.gz", 1234),
		s3obj("runner-logs/42/99/i-abc/Runner_a.log.gz", 567),
	}}
	h := logsHandler(t, jobs, s3api, &mockLogsPresigner{}, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/jobs/99/logs", nil)
	req.SetPathValue("id", "99")
	rec := httptest.NewRecorder()
	h.GetJobLogs(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var got jobLogsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Logs) != 2 {
		t.Fatalf("returned %d logs, want 2", len(got.Logs))
	}
	if got.ExpiresInSeconds != int(presignExpiry.Seconds()) {
		t.Errorf("ExpiresInSeconds = %d, want %d", got.ExpiresInSeconds, int(presignExpiry.Seconds()))
	}
	for _, l := range got.Logs {
		if l.URL == "" {
			t.Errorf("log %q has no presigned URL", l.Name)
		}
	}
}

func TestGetJobLogs_ServiceUnavailableWhenSourceUnset(t *testing.T) {
	jobs := &mockJobsDB{jobs: []db.AdminJobEntry{{JobID: 99, RunID: 42}}}
	h := logsHandler(t, jobs, nil, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/jobs/99/logs", nil)
	req.SetPathValue("id", "99")
	rec := httptest.NewRecorder()
	h.GetJobLogs(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 when no log source is configured", rec.Code)
	}
}

func TestGetJobLogs_NotFoundForUnknownJob(t *testing.T) {
	h := logsHandler(t, &mockJobsDB{}, &mockLogsS3{}, &mockLogsPresigner{}, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/jobs/12345/logs", nil)
	req.SetPathValue("id", "12345")
	rec := httptest.NewRecorder()
	h.GetJobLogs(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestGetJobLogs_BadRequestForNonNumericID(t *testing.T) {
	h := logsHandler(t, &mockJobsDB{}, &mockLogsS3{}, &mockLogsPresigner{}, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/jobs/abc/logs", nil)
	req.SetPathValue("id", "abc")
	rec := httptest.NewRecorder()
	h.GetJobLogs(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// A job whose agent had no job_id wrote its logs under unknown-job. The
// per-job prefix misses, so the handler must fall back to the run prefix and
// keep only this instance's objects.
func TestGetJobLogs_FallsBackToRunPrefixFilteredByInstance(t *testing.T) {
	jobs := &mockJobsDB{jobs: []db.AdminJobEntry{{JobID: 99, RunID: 42, InstanceID: "i-abc"}}}
	s3api := &mockLogsS3{objects: []types.Object{
		s3obj("runner-logs/42/unknown-job/i-abc/Worker_a.log.gz", 10),
		s3obj("runner-logs/42/unknown-job/i-other/Worker_b.log.gz", 20),
	}}
	h := logsHandler(t, jobs, s3api, &mockLogsPresigner{}, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/jobs/99/logs", nil)
	req.SetPathValue("id", "99")
	rec := httptest.NewRecorder()
	h.GetJobLogs(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var got jobLogsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Logs) != 1 {
		t.Fatalf("returned %d logs, want only this instance's 1: %+v", len(got.Logs), got.Logs)
	}
	if len(s3api.gotPrefix) != 2 {
		t.Errorf("listed prefixes %v, want the per-job prefix then the run fallback", s3api.gotPrefix)
	}
}

func TestGetJobLogs_BadGatewayOnListError(t *testing.T) {
	jobs := &mockJobsDB{jobs: []db.AdminJobEntry{{JobID: 99, RunID: 42}}}
	h := logsHandler(t, jobs, &mockLogsS3{err: errors.New("AccessDenied")}, &mockLogsPresigner{}, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/jobs/99/logs", nil)
	req.SetPathValue("id", "99")
	rec := httptest.NewRecorder()
	h.GetJobLogs(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
}

// Runner logs are pre-masking and may carry secret material, so every read has
// to be attributable.
func TestGetJobLogs_RecordsAudit(t *testing.T) {
	jobs := &mockJobsDB{jobs: []db.AdminJobEntry{{JobID: 99, RunID: 42, InstanceID: "i-abc"}}}
	s3api := &mockLogsS3{objects: []types.Object{s3obj("runner-logs/42/99/i-abc/Worker_a.log.gz", 10)}}
	audit := &mockAuditDB{}
	h := logsHandler(t, jobs, s3api, &mockLogsPresigner{}, audit)

	req := httptest.NewRequest(http.MethodGet, "/api/jobs/99/logs", nil)
	req.SetPathValue("id", "99")
	h.GetJobLogs(httptest.NewRecorder(), req)

	if len(audit.entries) != 1 {
		t.Fatalf("recorded %d audit entries, want 1", len(audit.entries))
	}
	if audit.entries[0].Action != auditActionJobLogsView {
		t.Errorf("audit action = %q, want %q", audit.entries[0].Action, auditActionJobLogsView)
	}
	if audit.entries[0].Target != "99" {
		t.Errorf("audit target = %q, want the job id", audit.entries[0].Target)
	}
}

func TestGetJobLogs_EmptyListIsOKWithNoLogs(t *testing.T) {
	jobs := &mockJobsDB{jobs: []db.AdminJobEntry{{JobID: 99, RunID: 42, InstanceID: "i-abc"}}}
	h := logsHandler(t, jobs, &mockLogsS3{}, &mockLogsPresigner{}, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/jobs/99/logs", nil)
	req.SetPathValue("id", "99")
	rec := httptest.NewRecorder()
	h.GetJobLogs(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 with an empty list", rec.Code)
	}
	var got jobLogsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Logs) != 0 {
		t.Errorf("returned %d logs, want 0", len(got.Logs))
	}
}
