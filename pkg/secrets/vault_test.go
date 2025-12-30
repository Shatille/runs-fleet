package secrets

import (
	"testing"
)

func TestVaultStore_secretPath(t *testing.T) {
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
			path := store.secretPath(tt.runnerID)
			if path != tt.wantPath {
				t.Errorf("secretPath(%s) = %s, want %s", tt.runnerID, path, tt.wantPath)
			}
		})
	}
}

func TestVaultStore_secretPathCustomMount(t *testing.T) {
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
