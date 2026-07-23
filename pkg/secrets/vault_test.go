package secrets

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/hashicorp/vault/api"
)

func TestVaultStore_secretPath(t *testing.T) {
	t.Parallel()

	store := &VaultStore{
		kvMount:   "secret",
		basePath:  "runs-fleet/runners",
		kvVersion: 1,
	}

	tests := []struct {
		runnerID string
		wantPath string
	}{
		{"i-123456", "secret/runs-fleet/runners/i-123456"},
		{"abc-def", "secret/runs-fleet/runners/abc-def"},
	}

	for _, tt := range tests {
		t.Run(tt.runnerID, func(t *testing.T) {
			t.Parallel()

			path := store.secretPath(tt.runnerID)
			if path != tt.wantPath {
				t.Errorf("secretPath(%s) = %s, want %s", tt.runnerID, path, tt.wantPath)
			}
		})
	}
}

func TestVaultStore_secretPathCustomMount(t *testing.T) {
	t.Parallel()

	store := &VaultStore{
		kvMount:   "custom-kv",
		basePath:  "myapp/runners",
		kvVersion: 1,
	}

	path := store.secretPath("runner-1")
	expected := "custom-kv/myapp/runners/runner-1"
	if path != expected {
		t.Errorf("secretPath = %s, want %s", path, expected)
	}
}

func TestNewVaultStoreWithClient_defaults(t *testing.T) {
	t.Parallel()

	store := NewVaultStoreWithClient(nil, "", "", 0)

	if store.kvMount != "secret" {
		t.Errorf("kvMount = %s, want secret", store.kvMount)
	}
	if store.basePath != "runs-fleet/runners" {
		t.Errorf("basePath = %s, want runs-fleet/runners", store.basePath)
	}
	if store.kvVersion != 2 {
		t.Errorf("kvVersion = %d, want 2", store.kvVersion)
	}
}

func TestNewVaultStoreWithClient_customValues(t *testing.T) {
	t.Parallel()

	store := NewVaultStoreWithClient(nil, "custom-kv", "custom/path", 1)

	if store.kvMount != "custom-kv" {
		t.Errorf("kvMount = %s, want custom-kv", store.kvMount)
	}
	if store.basePath != "custom/path" {
		t.Errorf("basePath = %s, want custom/path", store.basePath)
	}
	if store.kvVersion != 1 {
		t.Errorf("kvVersion = %d, want 1", store.kvVersion)
	}
}

func TestVaultConfig_defaults(t *testing.T) {
	t.Parallel()

	cfg := VaultConfig{}

	if cfg.KVMount != "" {
		t.Errorf("default KVMount should be empty, got %s", cfg.KVMount)
	}
	if cfg.BasePath != "" {
		t.Errorf("default BasePath should be empty, got %s", cfg.BasePath)
	}
	if cfg.KVVersion != 0 {
		t.Errorf("default KVVersion should be 0 (auto), got %d", cfg.KVVersion)
	}
}

func TestIsNotFoundError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		err      error
		expected bool
	}{
		{
			name:     "nil error",
			err:      nil,
			expected: false,
		},
		{
			name:     "regular error",
			err:      fmt.Errorf("some error"),
			expected: false,
		},
		{
			name:     "404 response error",
			err:      &api.ResponseError{StatusCode: 404},
			expected: true,
		},
		{
			name:     "403 response error",
			err:      &api.ResponseError{StatusCode: 403},
			expected: false,
		},
		{
			name:     "500 response error",
			err:      &api.ResponseError{StatusCode: 500},
			expected: false,
		},
		{
			name:     "wrapped 404 error",
			err:      fmt.Errorf("wrapped: %w", &api.ResponseError{StatusCode: 404}),
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result := isNotFoundError(tt.err)
			if result != tt.expected {
				t.Errorf("isNotFoundError(%v) = %v, want %v", tt.err, result, tt.expected)
			}
		})
	}
}

func TestVaultStore_Close(_ *testing.T) {
	store := &VaultStore{}

	// Close with nil renewCancel should not panic
	store.Close()

	// Close with actual cancel function
	store.renewCancel = func() {}
	store.Close()
}

func TestVaultStore_Close_WithWaitGroup(t *testing.T) {
	t.Parallel()

	store := &VaultStore{}
	store.renewWg.Add(1)

	called := false
	store.renewCancel = func() {
		called = true
		store.renewWg.Done()
	}

	store.Close()

	if !called {
		t.Error("renewCancel was not called")
	}
}

// mockVaultServer creates a test server that simulates Vault API responses.
func mockVaultServer(handlers map[string]http.HandlerFunc) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if handler, ok := handlers[r.URL.Path]; ok {
			handler(w, r)
			return
		}
		// Default: return 404
		w.WriteHeader(http.StatusNotFound)
	}))
}

func TestVaultStore_Put(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		kvVersion int
		path      string
		response  string
	}{
		{"KVv2", 2, "/v1/secret/data/runs-fleet/runners/i-123", `{"data": {}}`},
		{"KVv1", 1, "/v1/secret/runs-fleet/runners/i-123", `{}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			putCalled := false
			var capturedPayload map[string]interface{}
			server := mockVaultServer(map[string]http.HandlerFunc{
				tt.path: func(w http.ResponseWriter, r *http.Request) {
					if r.Method != http.MethodPut && r.Method != http.MethodPost {
						t.Errorf("expected PUT or POST, got %s", r.Method)
					}
					putCalled = true
					var body map[string]interface{}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Errorf("failed to decode request body: %v", err)
					}
					// KVv2 wraps the payload under "data"; KVv1 sends it directly.
					if tt.kvVersion == 2 {
						if d, ok := body["data"].(map[string]interface{}); ok {
							capturedPayload = d
						}
					} else {
						capturedPayload = body
					}
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write([]byte(tt.response))
				},
			})
			defer server.Close()

			client, err := api.NewClient(&api.Config{Address: server.URL})
			if err != nil {
				t.Fatalf("failed to create client: %v", err)
			}
			client.SetToken("test-token")

			store := NewVaultStoreWithClient(client, "secret", "runs-fleet/runners", tt.kvVersion)

			config := &RunnerConfig{
				Org:        "testorg",
				Repo:       "testorg/testrepo",
				JITToken:   "token123",
				RunnerName: "runs-fleet-runner-unique-suffix-42",
			}

			err = store.Put(t.Context(), "i-123", config)
			if err != nil {
				t.Errorf("Put() error = %v", err)
			}

			if !putCalled {
				t.Error("Vault API was not called")
			}

			// Regression guard: every field the agent reads back must be in the
			// serialized payload. Missing runner_name caused all ephemeral runners
			// to register under the generic default name and trample each other
			// via `config.sh --replace`.
			if got, ok := capturedPayload["runner_name"].(string); !ok || got != config.RunnerName {
				t.Errorf("runner_name in Vault payload = %v, want %q", capturedPayload["runner_name"], config.RunnerName)
			}
		})
	}
}

func TestVaultStore_Get_KVv2(t *testing.T) {
	t.Parallel()

	expectedConfig := RunnerConfig{
		Org:      "testorg",
		Repo:     "testorg/testrepo",
		JITToken: "token123",
		Labels:   []string{"self-hosted"},
	}

	server := mockVaultServer(map[string]http.HandlerFunc{
		"/v1/secret/data/runs-fleet/runners/i-123": func(w http.ResponseWriter, _ *http.Request) {
			response := map[string]interface{}{
				"data": map[string]interface{}{
					"data": map[string]interface{}{
						"org":       expectedConfig.Org,
						"repo":      expectedConfig.Repo,
						"jit_token": expectedConfig.JITToken,
						"labels":    expectedConfig.Labels,
					},
				},
			}
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(response)
		},
	})
	defer server.Close()

	client, err := api.NewClient(&api.Config{Address: server.URL})
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}
	client.SetToken("test-token")

	store := NewVaultStoreWithClient(client, "secret", "runs-fleet/runners", 2)

	config, err := store.Get(t.Context(), "i-123")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}

	if config.Org != expectedConfig.Org {
		t.Errorf("Org = %s, want %s", config.Org, expectedConfig.Org)
	}
	if config.Repo != expectedConfig.Repo {
		t.Errorf("Repo = %s, want %s", config.Repo, expectedConfig.Repo)
	}
}

func TestVaultStore_Get_KVv1(t *testing.T) {
	t.Parallel()

	expectedConfig := RunnerConfig{
		Org:      "testorg",
		Repo:     "testorg/testrepo",
		JITToken: "token123",
	}

	server := mockVaultServer(map[string]http.HandlerFunc{
		"/v1/secret/runs-fleet/runners/i-123": func(w http.ResponseWriter, _ *http.Request) {
			response := map[string]interface{}{
				"data": map[string]interface{}{
					"org":       expectedConfig.Org,
					"repo":      expectedConfig.Repo,
					"jit_token": expectedConfig.JITToken,
				},
			}
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(response)
		},
	})
	defer server.Close()

	client, err := api.NewClient(&api.Config{Address: server.URL})
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}
	client.SetToken("test-token")

	store := NewVaultStoreWithClient(client, "secret", "runs-fleet/runners", 1)

	config, err := store.Get(t.Context(), "i-123")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}

	if config.Org != expectedConfig.Org {
		t.Errorf("Org = %s, want %s", config.Org, expectedConfig.Org)
	}
}

func TestVaultStore_Get_NotFound(t *testing.T) {
	t.Parallel()

	server := mockVaultServer(map[string]http.HandlerFunc{
		"/v1/secret/data/runs-fleet/runners/i-notfound": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		},
	})
	defer server.Close()

	client, err := api.NewClient(&api.Config{Address: server.URL})
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}
	client.SetToken("test-token")

	store := NewVaultStoreWithClient(client, "secret", "runs-fleet/runners", 2)

	_, err = store.Get(t.Context(), "i-notfound")
	if err == nil {
		t.Fatal("Get() expected error for non-existent secret")
	}
	if !errors.Is(err, ErrConfigNotFound) {
		t.Errorf("Get() error = %v, want errors.Is ErrConfigNotFound", err)
	}
}

func TestVaultStore_Get_NotFound_KVv1(t *testing.T) {
	t.Parallel()

	server := mockVaultServer(map[string]http.HandlerFunc{
		"/v1/secret/runs-fleet/runners/i-notfound": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		},
	})
	defer server.Close()

	client, err := api.NewClient(&api.Config{Address: server.URL})
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}
	client.SetToken("test-token")

	store := NewVaultStoreWithClient(client, "secret", "runs-fleet/runners", 1)

	_, err = store.Get(t.Context(), "i-notfound")
	if !errors.Is(err, ErrConfigNotFound) {
		t.Errorf("Get() error = %v, want errors.Is ErrConfigNotFound", err)
	}
}

func TestVaultStore_Delete(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		kvVersion int
		path      string
	}{
		{"KVv2", 2, "/v1/secret/metadata/runs-fleet/runners/i-123"},
		{"KVv1", 1, "/v1/secret/runs-fleet/runners/i-123"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			deleteCalled := false
			server := mockVaultServer(map[string]http.HandlerFunc{
				tt.path: func(w http.ResponseWriter, r *http.Request) {
					if r.Method != http.MethodDelete {
						t.Errorf("expected DELETE, got %s", r.Method)
					}
					deleteCalled = true
					w.WriteHeader(http.StatusNoContent)
				},
			})
			defer server.Close()

			client, err := api.NewClient(&api.Config{Address: server.URL})
			if err != nil {
				t.Fatalf("failed to create client: %v", err)
			}
			client.SetToken("test-token")

			store := NewVaultStoreWithClient(client, "secret", "runs-fleet/runners", tt.kvVersion)

			err = store.Delete(t.Context(), "i-123")
			if err != nil {
				t.Errorf("Delete() error = %v", err)
			}

			if !deleteCalled {
				t.Error("Delete API was not called")
			}
		})
	}
}

func TestVaultStore_Delete_NotFound_Ignored(t *testing.T) {
	t.Parallel()

	server := mockVaultServer(map[string]http.HandlerFunc{
		"/v1/secret/metadata/runs-fleet/runners/i-notfound": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		},
	})
	defer server.Close()

	client, err := api.NewClient(&api.Config{Address: server.URL})
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}
	client.SetToken("test-token")

	store := NewVaultStoreWithClient(client, "secret", "runs-fleet/runners", 2)

	// Delete should not return error for non-existent secrets
	err = store.Delete(t.Context(), "i-notfound")
	if err != nil {
		t.Errorf("Delete() should not error on 404, got: %v", err)
	}
}

func TestVaultStore_List_KVv2(t *testing.T) {
	t.Parallel()

	server := mockVaultServer(map[string]http.HandlerFunc{
		"/v1/secret/metadata/runs-fleet/runners": func(w http.ResponseWriter, r *http.Request) {
			// Vault LIST is sent as GET with X-Vault-Request header
			if r.Method != http.MethodGet {
				t.Errorf("expected GET (for LIST), got %s", r.Method)
			}
			response := map[string]interface{}{
				"data": map[string]interface{}{
					"keys": []string{"i-111", "i-222", "i-333"},
				},
			}
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(response)
		},
	})
	defer server.Close()

	client, err := api.NewClient(&api.Config{Address: server.URL})
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}
	client.SetToken("test-token")

	store := NewVaultStoreWithClient(client, "secret", "runs-fleet/runners", 2)

	ids, err := store.List(t.Context())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}

	if len(ids) != 3 {
		t.Errorf("List() returned %d items, want 3", len(ids))
	}
}

func TestVaultStore_List_Empty(t *testing.T) {
	t.Parallel()

	server := mockVaultServer(map[string]http.HandlerFunc{
		"/v1/secret/metadata/runs-fleet/runners": func(w http.ResponseWriter, _ *http.Request) {
			// Empty data response
			response := map[string]interface{}{
				"data": map[string]interface{}{
					"keys": []string{},
				},
			}
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(response)
		},
	})
	defer server.Close()

	client, err := api.NewClient(&api.Config{Address: server.URL})
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}
	client.SetToken("test-token")

	store := NewVaultStoreWithClient(client, "secret", "runs-fleet/runners", 2)

	ids, err := store.List(t.Context())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}

	if len(ids) != 0 {
		t.Errorf("List() returned %d items, want 0", len(ids))
	}
}

func TestVaultStore_List_SkipsDirectories(t *testing.T) {
	t.Parallel()

	server := mockVaultServer(map[string]http.HandlerFunc{
		"/v1/secret/metadata/runs-fleet/runners": func(w http.ResponseWriter, _ *http.Request) {
			response := map[string]interface{}{
				"data": map[string]interface{}{
					"keys": []string{"i-111", "subdir/", "i-222"},
				},
			}
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(response)
		},
	})
	defer server.Close()

	client, err := api.NewClient(&api.Config{Address: server.URL})
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}
	client.SetToken("test-token")

	store := NewVaultStoreWithClient(client, "secret", "runs-fleet/runners", 2)

	ids, err := store.List(t.Context())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}

	// Should skip "subdir/" and only return 2 items
	if len(ids) != 2 {
		t.Errorf("List() returned %d items, want 2 (excluding directory)", len(ids))
	}
}

func TestVaultStore_Put_Error(t *testing.T) {
	t.Parallel()

	server := mockVaultServer(map[string]http.HandlerFunc{
		"/v1/secret/data/runs-fleet/runners/i-error": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"errors": ["internal error"]}`))
		},
	})
	defer server.Close()

	client, err := api.NewClient(&api.Config{Address: server.URL})
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}
	client.SetToken("test-token")

	store := NewVaultStoreWithClient(client, "secret", "runs-fleet/runners", 2)

	config := &RunnerConfig{Org: "test"}
	err = store.Put(t.Context(), "i-error", config)
	if err == nil {
		t.Error("Put() expected error")
	}
}

func TestVaultStore_Delete_OtherError(t *testing.T) {
	t.Parallel()

	server := mockVaultServer(map[string]http.HandlerFunc{
		"/v1/secret/metadata/runs-fleet/runners/i-error": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"errors": ["permission denied"]}`))
		},
	})
	defer server.Close()

	client, err := api.NewClient(&api.Config{Address: server.URL})
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}
	client.SetToken("test-token")

	store := NewVaultStoreWithClient(client, "secret", "runs-fleet/runners", 2)

	err = store.Delete(t.Context(), "i-error")
	if err == nil {
		t.Error("Delete() expected error for 403")
	}
}

func TestVaultStore_detectKVVersion(t *testing.T) {
	t.Parallel()

	type pathResponse struct {
		status int
		body   string
	}

	tests := []struct {
		name        string
		responses   map[string]pathResponse
		wantVersion int
		wantErr     bool
	}{
		{
			name: "config endpoint returns v2 config fields",
			responses: map[string]pathResponse{
				"/v1/secret/config": {http.StatusOK, `{"data": {"max_versions": 10}}`},
			},
			wantVersion: 2,
		},
		{
			name: "config endpoint returns v1 secret data",
			responses: map[string]pathResponse{
				"/v1/secret/config": {http.StatusOK, `{"data": {"some_user_key": "value"}}`},
			},
			wantVersion: 1,
		},
		{
			name: "config endpoint 404 - KV v1",
			responses: map[string]pathResponse{
				"/v1/secret/config": {http.StatusNotFound, ""},
			},
			wantVersion: 1,
		},
		{
			name: "config 403, probe v1 accessible v2 denied - KV v1",
			responses: map[string]pathResponse{
				"/v1/secret/config":                      {http.StatusForbidden, `{"errors": ["permission denied"]}`},
				"/v1/secret/runs-fleet/runners":          {http.StatusOK, `{"data": {"keys": ["runner-1"]}}`},
				"/v1/secret/metadata/runs-fleet/runners": {http.StatusForbidden, `{"errors": ["permission denied"]}`},
			},
			wantVersion: 1,
		},
		{
			name: "config 403, probe v2 200 v1 404 - KV v2",
			responses: map[string]pathResponse{
				"/v1/secret/config":                      {http.StatusForbidden, `{"errors": ["permission denied"]}`},
				"/v1/secret/runs-fleet/runners":          {http.StatusNotFound, ""},
				"/v1/secret/metadata/runs-fleet/runners": {http.StatusOK, `{"data": {"keys": ["runner-1"]}}`},
			},
			wantVersion: 2,
		},
		{
			name: "config 403, probe v2 403 v1 404 - KV v2",
			responses: map[string]pathResponse{
				"/v1/secret/config":                      {http.StatusForbidden, `{"errors": ["permission denied"]}`},
				"/v1/secret/runs-fleet/runners":          {http.StatusNotFound, ""},
				"/v1/secret/metadata/runs-fleet/runners": {http.StatusForbidden, `{"errors": ["permission denied"]}`},
			},
			wantVersion: 2,
		},
		{
			name: "config 403, probe v1 403 v2 404 - KV v1",
			responses: map[string]pathResponse{
				"/v1/secret/config":                      {http.StatusForbidden, `{"errors": ["permission denied"]}`},
				"/v1/secret/runs-fleet/runners":          {http.StatusForbidden, `{"errors": ["permission denied"]}`},
				"/v1/secret/metadata/runs-fleet/runners": {http.StatusNotFound, ""},
			},
			wantVersion: 1,
		},
		{
			name: "config 403, both probes 403 - defaults to v1",
			responses: map[string]pathResponse{
				"/v1/secret/config":                      {http.StatusForbidden, `{"errors": ["permission denied"]}`},
				"/v1/secret/runs-fleet/runners":          {http.StatusForbidden, `{"errors": ["permission denied"]}`},
				"/v1/secret/metadata/runs-fleet/runners": {http.StatusForbidden, `{"errors": ["permission denied"]}`},
			},
			wantVersion: 1,
		},
		{
			name: "config 403, both probes 404 - defaults to v1",
			responses: map[string]pathResponse{
				"/v1/secret/config":                      {http.StatusForbidden, `{"errors": ["permission denied"]}`},
				"/v1/secret/runs-fleet/runners":          {http.StatusNotFound, ""},
				"/v1/secret/metadata/runs-fleet/runners": {http.StatusNotFound, ""},
			},
			wantVersion: 1,
		},
		{
			name: "config 403, probe returns 401 - error",
			responses: map[string]pathResponse{
				"/v1/secret/config":                      {http.StatusForbidden, `{"errors": ["permission denied"]}`},
				"/v1/secret/runs-fleet/runners":          {http.StatusUnauthorized, `{"errors": ["missing token"]}`},
				"/v1/secret/metadata/runs-fleet/runners": {http.StatusOK, `{"data": {"keys": []}}`},
			},
			wantErr: true,
		},
		{
			name: "config 403, probe returns 500 - error",
			responses: map[string]pathResponse{
				"/v1/secret/config":                      {http.StatusForbidden, `{"errors": ["permission denied"]}`},
				"/v1/secret/runs-fleet/runners":          {http.StatusOK, `{"data": {"keys": []}}`},
				"/v1/secret/metadata/runs-fleet/runners": {http.StatusInternalServerError, `{"errors": ["server error"]}`},
			},
			wantErr: true,
		},
		{
			name: "server error propagates",
			responses: map[string]pathResponse{
				"/v1/secret/config": {http.StatusInternalServerError, `{"errors": ["internal error"]}`},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			handlers := make(map[string]http.HandlerFunc)
			for path, resp := range tt.responses {
				resp := resp
				handlers[path] = func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(resp.status)
					if resp.body != "" {
						_, _ = w.Write([]byte(resp.body))
					}
				}
			}

			server := mockVaultServer(handlers)
			defer server.Close()

			client, err := api.NewClient(&api.Config{Address: server.URL})
			if err != nil {
				t.Fatalf("failed to create client: %v", err)
			}
			client.SetToken("test-token")
			store := &VaultStore{client: client, kvMount: "secret", basePath: "runs-fleet/runners"}

			version, err := store.detectKVVersion(t.Context())
			if tt.wantErr {
				if err == nil {
					t.Error("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if version != tt.wantVersion {
				t.Errorf("got version %d, want %d", version, tt.wantVersion)
			}
		})
	}
}

// fullyPopulatedRunnerConfig returns a RunnerConfig with every exported string and
// slice field set to a distinct sentinel and every bool set to boolVal. It uses
// reflection so a field added to the struct in the future is populated
// automatically, forcing the round-trip parity tests below to exercise it without a
// manual edit. boolVal is parameterized so callers can run the round-trip with
// bools at their zero value — a bool that erroneously gains omitempty vanishes from
// the payload when false, which the boolVal=true case alone cannot catch.
func fullyPopulatedRunnerConfig(t *testing.T, boolVal bool) *RunnerConfig {
	t.Helper()

	cfg := &RunnerConfig{}
	v := reflect.ValueOf(cfg).Elem()
	typ := v.Type()

	for i := 0; i < v.NumField(); i++ {
		field := v.Field(i)
		if !field.CanSet() {
			continue
		}
		switch field.Kind() {
		case reflect.String:
			field.SetString("sentinel-" + typ.Field(i).Name)
		case reflect.Bool:
			field.SetBool(boolVal)
		case reflect.Slice:
			if field.Type().Elem().Kind() == reflect.String {
				field.Set(reflect.ValueOf([]string{"sentinel-" + typ.Field(i).Name}))
			} else {
				t.Fatalf("unhandled slice element kind for field %s: %s", typ.Field(i).Name, field.Type().Elem().Kind())
			}
		default:
			t.Fatalf("unhandled field kind for %s: %s (extend fullyPopulatedRunnerConfig)", typ.Field(i).Name, field.Kind())
		}
	}

	return cfg
}

// statefulVaultServer simulates a single-secret Vault KV backend: it captures the
// payload written by Put and serves it back on Get, so a real Put→Get round-trip
// through VaultStore can be exercised. It supports both KV v1 and KV v2 path styles.
func statefulVaultServer(t *testing.T, kvVersion int, runnerID string) *httptest.Server {
	t.Helper()

	var putPath, getPath string
	if kvVersion == 2 {
		putPath = "/v1/secret/data/runs-fleet/runners/" + runnerID
		getPath = putPath
	} else {
		putPath = "/v1/secret/runs-fleet/runners/" + runnerID
		getPath = putPath
	}

	var stored map[string]interface{}

	handler := func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPut, http.MethodPost:
			var body map[string]interface{}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("failed to decode Put body: %v", err)
			}
			if kvVersion == 2 {
				if d, ok := body["data"].(map[string]interface{}); ok {
					stored = d
				} else {
					t.Errorf("KVv2 Put body missing data wrapper: %v", body)
				}
			} else {
				stored = body
			}
			w.WriteHeader(http.StatusOK)
			if kvVersion == 2 {
				_, _ = w.Write([]byte(`{"data": {}}`))
			} else {
				_, _ = w.Write([]byte(`{}`))
			}
		case http.MethodGet:
			if stored == nil {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			var response map[string]interface{}
			if kvVersion == 2 {
				response = map[string]interface{}{"data": map[string]interface{}{"data": stored}}
			} else {
				response = map[string]interface{}{"data": stored}
			}
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(response)
		default:
			t.Errorf("unexpected method %s", r.Method)
		}
	}

	return mockVaultServer(map[string]http.HandlerFunc{
		putPath: handler,
		getPath: handler,
	})
}

// TestVaultStore_PutGet_RoundTripParity is the drift guard: it writes a
// fully-populated RunnerConfig through Put and reads it back through Get, then
// asserts the result deep-equals the original for both KV v1 and v2. Because the
// input is populated by reflection over every exported field, any future field
// that Put drops (as buildkit_cache_* were before this fix) fails this test
// automatically — no manual test edit required.
func TestVaultStore_PutGet_RoundTripParity(t *testing.T) {
	t.Parallel()

	// bools=false exercises the zero-value path so a field that erroneously gains
	// omitempty (and would then vanish from the payload) is still caught.
	for _, bools := range []bool{true, false} {
		for _, kvVersion := range []int{1, 2} {
			name := fmt.Sprintf("KVv%d/bools=%t", kvVersion, bools)
			t.Run(name, func(t *testing.T) {
				t.Parallel()

				const runnerID = "i-123"
				server := statefulVaultServer(t, kvVersion, runnerID)
				defer server.Close()

				client, err := api.NewClient(&api.Config{Address: server.URL})
				if err != nil {
					t.Fatalf("failed to create client: %v", err)
				}
				client.SetToken("test-token")

				store := NewVaultStoreWithClient(client, "secret", "runs-fleet/runners", kvVersion)

				want := fullyPopulatedRunnerConfig(t, bools)
				if err = store.Put(t.Context(), runnerID, want); err != nil {
					t.Fatalf("Put() error = %v", err)
				}

				got, err := store.Get(t.Context(), runnerID)
				if err != nil {
					t.Fatalf("Get() error = %v", err)
				}

				if !reflect.DeepEqual(got, want) {
					t.Errorf("round-trip mismatch:\n got = %+v\nwant = %+v", got, want)
				}

				// Explicit per-field guard so a failure names the dropped field
				// rather than dumping the whole struct.
				gotVal := reflect.ValueOf(got).Elem()
				wantVal := reflect.ValueOf(want).Elem()
				for i := 0; i < gotVal.NumField(); i++ {
					fieldName := gotVal.Type().Field(i).Name
					if !reflect.DeepEqual(gotVal.Field(i).Interface(), wantVal.Field(i).Interface()) {
						t.Errorf("field %s lost in Put->Get: got %v, want %v",
							fieldName, gotVal.Field(i).Interface(), wantVal.Field(i).Interface())
					}
				}
			})
		}
	}
}

func TestVaultStore_detectKVVersion_ProbeNetworkError(t *testing.T) {
	t.Parallel()

	// Test that probe network failure propagates error
	server := mockVaultServer(map[string]http.HandlerFunc{
		"/v1/secret/config": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"errors": ["permission denied"]}`))
		},
	})

	client, err := api.NewClient(&api.Config{Address: server.URL})
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}
	client.SetToken("test-token")
	store := &VaultStore{client: client, kvMount: "secret", basePath: "runs-fleet/runners"}

	// Close server before probing to simulate network error
	server.Close()

	_, err = store.detectKVVersion(t.Context())
	if err == nil {
		t.Error("expected error for network failure during probe, got nil")
	}
}
