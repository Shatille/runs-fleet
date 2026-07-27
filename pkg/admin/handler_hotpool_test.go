package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/Shavakan/runs-fleet/pkg/db"
)

func hpIntPtr(n int) *int { return &n }

// updatePoolReq drives UpdatePool and returns the response recorder plus the
// config the mock persisted.
func updatePoolReq(t *testing.T, h *Handler, mockDB *mockPoolDB, name string, body PoolRequest) (*httptest.ResponseRecorder, *db.PoolConfig) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /api/pools/{name}", h.UpdatePool)

	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPut, "/api/pools/"+name, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec, mockDB.pools[name]
}

// Override set / clear(null) / force-cold(&0) all round-trip through UpdatePool.
func TestUpdatePool_OverrideRoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		body       PoolRequest
		wantLinger *int
		wantMaxHot *int
	}{
		{
			name:       "set override",
			body:       PoolRequest{OverrideLingerMinutes: hpIntPtr(10), OverrideMaxHot: hpIntPtr(2)},
			wantLinger: hpIntPtr(10),
			wantMaxHot: hpIntPtr(2),
		},
		{
			name:       "force cold with &0",
			body:       PoolRequest{OverrideLingerMinutes: hpIntPtr(0)},
			wantLinger: hpIntPtr(0),
			wantMaxHot: nil,
		},
		{
			name:       "clear override (null)",
			body:       PoolRequest{},
			wantLinger: nil,
			wantMaxHot: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			mockDB := newMockDB()
			mockDB.pools["p"] = &db.PoolConfig{
				PoolName:              "p",
				OverrideLingerMinutes: hpIntPtr(99), // prior override that must be overwritten/cleared
			}
			h := NewHandler(mockDB, nil, NewAuthMiddleware(""), config.DefaultHotPoolCaps())

			rec, saved := updatePoolReq(t, h, mockDB, "p", tt.body)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			if !intPtrEqual(saved.OverrideLingerMinutes, tt.wantLinger) {
				t.Errorf("saved OverrideLingerMinutes = %v, want %v", fmtPtr(saved.OverrideLingerMinutes), fmtPtr(tt.wantLinger))
			}
			if !intPtrEqual(saved.OverrideMaxHot, tt.wantMaxHot) {
				t.Errorf("saved OverrideMaxHot = %v, want %v", fmtPtr(saved.OverrideMaxHot), fmtPtr(tt.wantMaxHot))
			}
		})
	}
}

// Override values above the configured cap are rejected.
func TestUpdatePool_OverrideBounds(t *testing.T) {
	t.Parallel()

	caps := config.HotPoolCaps{MaxLingerMinutes: 30, MaxHot: 3, MinJobsToActivate: 20, LookbackDays: 7, BurstGapMinutes: 20}

	tests := []struct {
		name string
		body PoolRequest
	}{
		{"linger over cap", PoolRequest{OverrideLingerMinutes: hpIntPtr(31)}},
		{"maxHot over cap", PoolRequest{OverrideMaxHot: hpIntPtr(4)}},
		{"negative linger", PoolRequest{OverrideLingerMinutes: hpIntPtr(-1)}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			mockDB := newMockDB()
			mockDB.pools["p"] = &db.PoolConfig{PoolName: "p"}
			h := NewHandler(mockDB, nil, NewAuthMiddleware(""), caps)

			rec, _ := updatePoolReq(t, h, mockDB, "p", tt.body)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 (bounds rejection); body = %s", rec.Code, rec.Body.String())
			}
		})
	}

	// At-cap is accepted.
	mockDB := newMockDB()
	mockDB.pools["p"] = &db.PoolConfig{PoolName: "p"}
	h := NewHandler(mockDB, nil, NewAuthMiddleware(""), caps)
	rec, _ := updatePoolReq(t, h, mockDB, "p", PoolRequest{OverrideLingerMinutes: hpIntPtr(30), OverrideMaxHot: hpIntPtr(3)})
	if rec.Code != http.StatusOK {
		t.Errorf("at-cap override status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
}

// A tuner-written auto_tune survives an admin pool save and is echoed read-only in
// the response; it is never taken from the request.
func TestUpdatePool_PreservesAutoTune(t *testing.T) {
	t.Parallel()

	rec := &db.AutoTuneRec{RecommendedLingerMinutes: 5, RecommendedMaxHot: 2, Reason: "tuned", JobCount: 40, TunedAt: time.Now()}
	mockDB := newMockDB()
	mockDB.pools["p"] = &db.PoolConfig{PoolName: "p", AutoTune: rec}
	h := NewHandler(mockDB, nil, NewAuthMiddleware(""), config.DefaultHotPoolCaps())

	// Request carries no auto_tune (it's not even a request field).
	resp, saved := updatePoolReq(t, h, mockDB, "p", PoolRequest{OverrideLingerMinutes: hpIntPtr(7)})
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.Code, resp.Body.String())
	}
	if saved.AutoTune == nil || *saved.AutoTune != *rec {
		t.Errorf("saved AutoTune = %+v, want preserved %+v", saved.AutoTune, rec)
	}

	var body PoolResponse
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.AutoTune == nil || body.AutoTune.RecommendedLingerMinutes != 5 {
		t.Errorf("response AutoTune = %+v, want the recommendation echoed", body.AutoTune)
	}
}

func TestPoolDiff_Overrides(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		old  *db.PoolConfig
		new  *db.PoolConfig
		want string
	}{
		{
			name: "unset -> N",
			old:  &db.PoolConfig{},
			new:  &db.PoolConfig{OverrideLingerMinutes: hpIntPtr(30)},
			want: "override_linger_minutes: unset -> 30",
		},
		{
			name: "N -> unset",
			old:  &db.PoolConfig{OverrideMaxHot: hpIntPtr(5)},
			new:  &db.PoolConfig{},
			want: "override_max_hot: 5 -> unset",
		},
		{
			name: "unset -> 0 (force cold)",
			old:  &db.PoolConfig{},
			new:  &db.PoolConfig{OverrideLingerMinutes: hpIntPtr(0)},
			want: "override_linger_minutes: unset -> 0",
		},
		{
			name: "no change when equal",
			old:  &db.PoolConfig{OverrideLingerMinutes: hpIntPtr(10)},
			new:  &db.PoolConfig{OverrideLingerMinutes: hpIntPtr(10)},
			want: "none",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := poolDiff(tt.old, tt.new); got != tt.want {
				t.Errorf("poolDiff() = %q, want %q", got, tt.want)
			}
		})
	}
}

// auto_tune is tuner-written, not an operator change, so it never appears in a diff.
func TestPoolDiff_IgnoresAutoTune(t *testing.T) {
	t.Parallel()

	old := &db.PoolConfig{PoolName: "p"}
	updated := &db.PoolConfig{PoolName: "p", AutoTune: &db.AutoTuneRec{RecommendedLingerMinutes: 5, Reason: "tuned"}}
	if got := poolDiff(old, updated); got != "none" {
		t.Errorf("poolDiff() = %q, want \"none\" (auto_tune not diffed)", got)
	}
}

func intPtrEqual(a, b *int) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func fmtPtr(p *int) string {
	if p == nil {
		return "nil"
	}
	return strconv.Itoa(*p)
}
