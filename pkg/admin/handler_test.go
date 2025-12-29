package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Shavakan/runs-fleet/pkg/db"
)

type mockPoolDB struct {
	pools       map[string]*db.PoolConfig
	listErr     error
	getErr      error
	saveErr     error
	deleteErr   error
	deleteCalls []string
}

func newMockDB() *mockPoolDB {
	return &mockPoolDB{
		pools:       make(map[string]*db.PoolConfig),
		deleteCalls: make([]string, 0),
	}
}

func (m *mockPoolDB) ListPools(_ context.Context) ([]string, error) {
	if m.listErr != nil {
		return nil, m.listErr
	}
	names := make([]string, 0, len(m.pools))
	for name := range m.pools {
		names = append(names, name)
	}
	return names, nil
}

func (m *mockPoolDB) GetPoolConfig(_ context.Context, poolName string) (*db.PoolConfig, error) {
	if m.getErr != nil {
		return nil, m.getErr
	}
	return m.pools[poolName], nil
}

func (m *mockPoolDB) SavePoolConfig(_ context.Context, config *db.PoolConfig) error {
	if m.saveErr != nil {
		return m.saveErr
	}
	m.pools[config.PoolName] = config
	return nil
}

func (m *mockPoolDB) DeletePoolConfig(_ context.Context, poolName string) error {
	m.deleteCalls = append(m.deleteCalls, poolName)
	if m.deleteErr != nil {
		return m.deleteErr
	}
	delete(m.pools, poolName)
	return nil
}

func TestListPools(t *testing.T) {
	tests := []struct {
		name       string
		pools      map[string]*db.PoolConfig
		listErr    error
		wantStatus int
		wantCount  int
	}{
		{
			name:       "empty list",
			pools:      map[string]*db.PoolConfig{},
			wantStatus: http.StatusOK,
			wantCount:  0,
		},
		{
			name: "multiple pools",
			pools: map[string]*db.PoolConfig{
				"pool1": {PoolName: "pool1", DesiredRunning: 1},
				"pool2": {PoolName: "pool2", DesiredRunning: 2},
			},
			wantStatus: http.StatusOK,
			wantCount:  2,
		},
		{
			name:       "database error",
			listErr:    errors.New("db error"),
			wantStatus: http.StatusInternalServerError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockDB := newMockDB()
			mockDB.pools = tt.pools
			mockDB.listErr = tt.listErr

			h := NewHandler(mockDB)
			req := httptest.NewRequest(http.MethodGet, "/api/pools", nil)
			rec := httptest.NewRecorder()

			h.ListPools(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}

			if tt.wantStatus == http.StatusOK {
				var resp struct {
					Pools []PoolResponse `json:"pools"`
				}
				if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
					t.Fatalf("failed to decode response: %v", err)
				}
				if len(resp.Pools) != tt.wantCount {
					t.Errorf("pool count = %d, want %d", len(resp.Pools), tt.wantCount)
				}
			}
		})
	}
}

func TestGetPool(t *testing.T) {
	tests := []struct {
		name       string
		poolName   string
		pool       *db.PoolConfig
		getErr     error
		wantStatus int
	}{
		{
			name:     "existing pool",
			poolName: "test-pool",
			pool: &db.PoolConfig{
				PoolName:       "test-pool",
				InstanceType:   "c7g.xlarge",
				DesiredRunning: 2,
				DesiredStopped: 5,
				Arch:           "arm64",
			},
			wantStatus: http.StatusOK,
		},
		{
			name:       "pool not found",
			poolName:   "nonexistent",
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "database error",
			poolName:   "test-pool",
			getErr:     errors.New("db error"),
			wantStatus: http.StatusInternalServerError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockDB := newMockDB()
			if tt.pool != nil {
				mockDB.pools[tt.pool.PoolName] = tt.pool
			}
			mockDB.getErr = tt.getErr

			h := NewHandler(mockDB)

			mux := http.NewServeMux()
			mux.HandleFunc("GET /api/pools/{name}", h.GetPool)

			req := httptest.NewRequest(http.MethodGet, "/api/pools/"+tt.poolName, nil)
			rec := httptest.NewRecorder()

			mux.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}

			if tt.wantStatus == http.StatusOK {
				var resp PoolResponse
				if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
					t.Fatalf("failed to decode response: %v", err)
				}
				if resp.PoolName != tt.pool.PoolName {
					t.Errorf("pool_name = %q, want %q", resp.PoolName, tt.pool.PoolName)
				}
				if resp.InstanceType != tt.pool.InstanceType {
					t.Errorf("instance_type = %q, want %q", resp.InstanceType, tt.pool.InstanceType)
				}
			}
		})
	}
}

func TestCreatePool(t *testing.T) {
	tests := []struct {
		name       string
		body       PoolRequest
		existing   *db.PoolConfig
		saveErr    error
		wantStatus int
	}{
		{
			name: "create valid pool",
			body: PoolRequest{
				PoolName:       "new-pool",
				InstanceType:   "c7g.xlarge",
				DesiredRunning: 1,
				DesiredStopped: 3,
				Arch:           "arm64",
			},
			wantStatus: http.StatusCreated,
		},
		{
			name: "pool already exists",
			body: PoolRequest{
				PoolName:       "existing-pool",
				DesiredRunning: 1,
			},
			existing:   &db.PoolConfig{PoolName: "existing-pool"},
			wantStatus: http.StatusConflict,
		},
		{
			name: "missing pool name",
			body: PoolRequest{
				DesiredRunning: 1,
			},
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "invalid pool name",
			body: PoolRequest{
				PoolName: "invalid pool name!",
			},
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "negative desired_running",
			body: PoolRequest{
				PoolName:       "test-pool",
				DesiredRunning: -1,
			},
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "invalid arch",
			body: PoolRequest{
				PoolName: "test-pool",
				Arch:     "x86",
			},
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "database error",
			body: PoolRequest{
				PoolName:       "test-pool",
				DesiredRunning: 1,
			},
			saveErr:    errors.New("db error"),
			wantStatus: http.StatusInternalServerError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockDB := newMockDB()
			if tt.existing != nil {
				mockDB.pools[tt.existing.PoolName] = tt.existing
			}
			mockDB.saveErr = tt.saveErr

			h := NewHandler(mockDB)

			body, _ := json.Marshal(tt.body)
			req := httptest.NewRequest(http.MethodPost, "/api/pools", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()

			h.CreatePool(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d, body: %s", rec.Code, tt.wantStatus, rec.Body.String())
			}

			if tt.wantStatus == http.StatusCreated {
				if _, exists := mockDB.pools[tt.body.PoolName]; !exists {
					t.Error("pool was not saved to database")
				}
			}
		})
	}
}

func TestUpdatePool(t *testing.T) {
	tests := []struct {
		name       string
		poolName   string
		body       PoolRequest
		existing   *db.PoolConfig
		saveErr    error
		wantStatus int
	}{
		{
			name:     "update existing pool",
			poolName: "test-pool",
			body: PoolRequest{
				DesiredRunning: 5,
				DesiredStopped: 10,
			},
			existing: &db.PoolConfig{
				PoolName:       "test-pool",
				DesiredRunning: 2,
				DesiredStopped: 3,
			},
			wantStatus: http.StatusOK,
		},
		{
			name:     "pool not found",
			poolName: "nonexistent",
			body: PoolRequest{
				DesiredRunning: 1,
			},
			wantStatus: http.StatusNotFound,
		},
		{
			name:     "preserves ephemeral flag",
			poolName: "ephemeral-pool",
			body: PoolRequest{
				DesiredRunning: 3,
			},
			existing: &db.PoolConfig{
				PoolName:  "ephemeral-pool",
				Ephemeral: true,
			},
			wantStatus: http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockDB := newMockDB()
			if tt.existing != nil {
				mockDB.pools[tt.existing.PoolName] = tt.existing
			}
			mockDB.saveErr = tt.saveErr

			h := NewHandler(mockDB)

			mux := http.NewServeMux()
			mux.HandleFunc("PUT /api/pools/{name}", h.UpdatePool)

			body, _ := json.Marshal(tt.body)
			req := httptest.NewRequest(http.MethodPut, "/api/pools/"+tt.poolName, bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()

			mux.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}

			if tt.wantStatus == http.StatusOK && tt.existing != nil && tt.existing.Ephemeral {
				saved := mockDB.pools[tt.poolName]
				if !saved.Ephemeral {
					t.Error("ephemeral flag was not preserved")
				}
			}
		})
	}
}

func TestDeletePool(t *testing.T) {
	tests := []struct {
		name       string
		poolName   string
		existing   *db.PoolConfig
		deleteErr  error
		wantStatus int
	}{
		{
			name:     "delete ephemeral pool",
			poolName: "ephemeral-pool",
			existing: &db.PoolConfig{
				PoolName:  "ephemeral-pool",
				Ephemeral: true,
			},
			wantStatus: http.StatusNoContent,
		},
		{
			name:     "cannot delete non-ephemeral pool",
			poolName: "persistent-pool",
			existing: &db.PoolConfig{
				PoolName:  "persistent-pool",
				Ephemeral: false,
			},
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "pool not found",
			poolName:   "nonexistent",
			wantStatus: http.StatusNotFound,
		},
		{
			name:     "database error",
			poolName: "ephemeral-pool",
			existing: &db.PoolConfig{
				PoolName:  "ephemeral-pool",
				Ephemeral: true,
			},
			deleteErr:  errors.New("db error"),
			wantStatus: http.StatusInternalServerError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockDB := newMockDB()
			if tt.existing != nil {
				mockDB.pools[tt.existing.PoolName] = tt.existing
			}
			mockDB.deleteErr = tt.deleteErr

			h := NewHandler(mockDB)

			mux := http.NewServeMux()
			mux.HandleFunc("DELETE /api/pools/{name}", h.DeletePool)

			req := httptest.NewRequest(http.MethodDelete, "/api/pools/"+tt.poolName, nil)
			rec := httptest.NewRecorder()

			mux.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}

			if tt.wantStatus == http.StatusNoContent {
				if len(mockDB.deleteCalls) != 1 || mockDB.deleteCalls[0] != tt.poolName {
					t.Error("delete was not called correctly")
				}
			}
		})
	}
}

func TestValidatePoolRequest(t *testing.T) {
	h := NewHandler(nil)

	tests := []struct {
		name     string
		req      PoolRequest
		isCreate bool
		wantErr  bool
	}{
		{
			name:     "valid create request",
			req:      PoolRequest{PoolName: "valid-pool", DesiredRunning: 1},
			isCreate: true,
			wantErr:  false,
		},
		{
			name:     "pool name with underscore",
			req:      PoolRequest{PoolName: "valid_pool", DesiredRunning: 1},
			isCreate: true,
			wantErr:  false,
		},
		{
			name:     "pool name starting with number",
			req:      PoolRequest{PoolName: "1pool", DesiredRunning: 1},
			isCreate: true,
			wantErr:  false,
		},
		{
			name:     "pool name with spaces",
			req:      PoolRequest{PoolName: "invalid pool"},
			isCreate: true,
			wantErr:  true,
		},
		{
			name:     "pool name starting with hyphen",
			req:      PoolRequest{PoolName: "-invalid"},
			isCreate: true,
			wantErr:  true,
		},
		{
			name:     "cpu_min greater than cpu_max",
			req:      PoolRequest{PoolName: "test", CPUMin: 8, CPUMax: 4},
			isCreate: true,
			wantErr:  true,
		},
		{
			name:     "ram_min greater than ram_max",
			req:      PoolRequest{PoolName: "test", RAMMin: 16, RAMMax: 8},
			isCreate: true,
			wantErr:  true,
		},
		{
			name: "valid schedule",
			req: PoolRequest{
				PoolName: "test",
				Schedules: []ScheduleRequest{
					{Name: "business-hours", StartHour: 9, EndHour: 17, DaysOfWeek: []int{1, 2, 3, 4, 5}},
				},
			},
			isCreate: true,
			wantErr:  false,
		},
		{
			name: "invalid schedule hour",
			req: PoolRequest{
				PoolName: "test",
				Schedules: []ScheduleRequest{
					{Name: "invalid", StartHour: 25},
				},
			},
			isCreate: true,
			wantErr:  true,
		},
		{
			name: "invalid schedule day",
			req: PoolRequest{
				PoolName: "test",
				Schedules: []ScheduleRequest{
					{Name: "invalid", DaysOfWeek: []int{7}},
				},
			},
			isCreate: true,
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := h.validatePoolRequest(&tt.req, tt.isCreate)
			if (err != nil) != tt.wantErr {
				t.Errorf("validatePoolRequest() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestCreatePoolContentType(t *testing.T) {
	mockDB := newMockDB()
	h := NewHandler(mockDB)

	body := []byte(`{"pool_name": "test-pool"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/pools", bytes.NewReader(body))
	// No Content-Type header set
	rec := httptest.NewRecorder()

	h.CreatePool(rec, req)

	if rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnsupportedMediaType)
	}
}

func TestUpdatePoolContentType(t *testing.T) {
	mockDB := newMockDB()
	mockDB.pools["test-pool"] = &db.PoolConfig{PoolName: "test-pool"}
	h := NewHandler(mockDB)

	mux := http.NewServeMux()
	mux.HandleFunc("PUT /api/pools/{name}", h.UpdatePool)

	body := []byte(`{"desired_running": 1}`)
	req := httptest.NewRequest(http.MethodPut, "/api/pools/test-pool", bytes.NewReader(body))
	// No Content-Type header set
	rec := httptest.NewRecorder()

	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnsupportedMediaType)
	}
}
