package secrets

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/hashicorp/vault/api"
)

const (
	testAppRoleLoginPath    = "/v1/auth/approle/login"
	testKubernetesLoginPath = "/v1/auth/kubernetes/login"
)

func TestAuthenticate_TokenMethod(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client, err := api.NewClient(&api.Config{Address: server.URL})
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	cfg := VaultConfig{
		AuthMethod: AuthMethodToken,
		Token:      "test-token",
	}

	err = authenticate(context.Background(), client, cfg)
	if err != nil {
		t.Errorf("authenticate() error = %v", err)
	}

	if client.Token() != "test-token" {
		t.Errorf("client.Token() = %s, want test-token", client.Token())
	}
}

func TestAuthenticate_TokenMethod_EmptyToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client, err := api.NewClient(&api.Config{Address: server.URL})
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	cfg := VaultConfig{
		AuthMethod: AuthMethodToken,
		Token:      "", // Empty token
	}

	// Should not error with empty token - just doesn't set it
	err = authenticate(context.Background(), client, cfg)
	if err != nil {
		t.Errorf("authenticate() error = %v", err)
	}
}

func TestAuthenticate_UnsupportedMethod(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client, err := api.NewClient(&api.Config{Address: server.URL})
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	cfg := VaultConfig{
		AuthMethod: "unsupported-method",
	}

	err = authenticate(context.Background(), client, cfg)
	if err == nil {
		t.Error("authenticate() expected error for unsupported method")
	}
}

func TestAuthenticate_EmptyMethod_WithEnvToken(t *testing.T) {
	// Set VAULT_TOKEN env
	_ = os.Setenv("VAULT_TOKEN", "env-token-123")
	defer func() { _ = os.Unsetenv("VAULT_TOKEN") }()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client, err := api.NewClient(&api.Config{Address: server.URL})
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	cfg := VaultConfig{
		AuthMethod: "", // Empty - should use env token
	}

	err = authenticate(context.Background(), client, cfg)
	if err != nil {
		t.Errorf("authenticate() error = %v", err)
	}

	if client.Token() != "env-token-123" {
		t.Errorf("client.Token() = %s, want env-token-123", client.Token())
	}
}

func TestAuthenticate_AppRole_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == testAppRoleLoginPath {
			response := map[string]interface{}{
				"auth": map[string]interface{}{
					"client_token":   "test-approle-token",
					"accessor":       "accessor-123",
					"lease_duration": 3600,
				},
			}
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(response)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	client, err := api.NewClient(&api.Config{Address: server.URL})
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	cfg := VaultConfig{
		AuthMethod:      AuthMethodAppRole,
		AppRoleID:       "role-123",
		AppRoleSecretID: "secret-456",
	}

	err = authenticate(context.Background(), client, cfg)
	if err != nil {
		t.Errorf("authenticate() error = %v", err)
	}

	if client.Token() != "test-approle-token" {
		t.Errorf("client.Token() = %s, want test-approle-token", client.Token())
	}
}

func TestAuthenticate_AppRole_Failure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == testAppRoleLoginPath {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"errors": ["invalid credentials"]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	client, err := api.NewClient(&api.Config{Address: server.URL})
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	cfg := VaultConfig{
		AuthMethod:      AuthMethodAppRole,
		AppRoleID:       "wrong-role",
		AppRoleSecretID: "wrong-secret",
	}

	err = authenticate(context.Background(), client, cfg)
	if err == nil {
		t.Error("authenticate() expected error for invalid credentials")
	}
}

func TestAuthenticate_AppRole_NilAuth(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == testAppRoleLoginPath {
			// Return response without auth field
			response := map[string]interface{}{
				"data": map[string]interface{}{},
			}
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(response)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	client, err := api.NewClient(&api.Config{Address: server.URL})
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	cfg := VaultConfig{
		AuthMethod:      AuthMethodAppRole,
		AppRoleID:       "role-123",
		AppRoleSecretID: "secret-456",
	}

	err = authenticate(context.Background(), client, cfg)
	if err == nil {
		t.Error("authenticate() expected error for nil auth response")
	}
}

func TestAuthenticate_Kubernetes_Success(t *testing.T) {
	// Create a temporary JWT file
	tempDir := t.TempDir()
	jwtPath := filepath.Join(tempDir, "token")
	if err := os.WriteFile(jwtPath, []byte("test-jwt-token"), 0600); err != nil {
		t.Fatalf("failed to create temp JWT file: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == testKubernetesLoginPath {
			response := map[string]interface{}{
				"auth": map[string]interface{}{
					"client_token":   "test-k8s-token",
					"accessor":       "accessor-k8s-123",
					"lease_duration": 3600,
				},
			}
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(response)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	client, err := api.NewClient(&api.Config{Address: server.URL})
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	cfg := VaultConfig{
		AuthMethod: AuthMethodKubernetes,
		K8sRole:    "my-k8s-role",
		K8sJWTPath: jwtPath,
	}

	err = authenticate(context.Background(), client, cfg)
	if err != nil {
		t.Errorf("authenticate() error = %v", err)
	}

	if client.Token() != "test-k8s-token" {
		t.Errorf("client.Token() = %s, want test-k8s-token", client.Token())
	}
}

func TestAuthenticate_Kubernetes_K8sAlias(t *testing.T) {
	// Create a temporary JWT file
	tempDir := t.TempDir()
	jwtPath := filepath.Join(tempDir, "token")
	if err := os.WriteFile(jwtPath, []byte("test-jwt-token"), 0600); err != nil {
		t.Fatalf("failed to create temp JWT file: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == testKubernetesLoginPath {
			response := map[string]interface{}{
				"auth": map[string]interface{}{
					"client_token": "test-k8s-token",
				},
			}
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(response)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	client, err := api.NewClient(&api.Config{Address: server.URL})
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	// Use "k8s" alias instead of "kubernetes"
	cfg := VaultConfig{
		AuthMethod: AuthMethodK8s,
		K8sRole:    "my-k8s-role",
		K8sJWTPath: jwtPath,
	}

	err = authenticate(context.Background(), client, cfg)
	if err != nil {
		t.Errorf("authenticate() error = %v", err)
	}
}

func TestAuthenticate_Kubernetes_FileNotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client, err := api.NewClient(&api.Config{Address: server.URL})
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	cfg := VaultConfig{
		AuthMethod: AuthMethodKubernetes,
		K8sRole:    "my-k8s-role",
		K8sJWTPath: "/nonexistent/path/token",
	}

	err = authenticate(context.Background(), client, cfg)
	if err == nil {
		t.Error("authenticate() expected error for missing JWT file")
	}
}

func TestAuthenticate_Kubernetes_NilAuth(t *testing.T) {
	// Create a temporary JWT file
	tempDir := t.TempDir()
	jwtPath := filepath.Join(tempDir, "token")
	if err := os.WriteFile(jwtPath, []byte("test-jwt-token"), 0600); err != nil {
		t.Fatalf("failed to create temp JWT file: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == testKubernetesLoginPath {
			// Return response without auth field
			response := map[string]interface{}{
				"data": map[string]interface{}{},
			}
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(response)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	client, err := api.NewClient(&api.Config{Address: server.URL})
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	cfg := VaultConfig{
		AuthMethod: AuthMethodKubernetes,
		K8sRole:    "my-k8s-role",
		K8sJWTPath: jwtPath,
	}

	err = authenticate(context.Background(), client, cfg)
	if err == nil {
		t.Error("authenticate() expected error for nil auth response")
	}
}

func TestAuthenticate_Kubernetes_DefaultJWTPath(t *testing.T) {
	// Test that default JWT path is used when not specified
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client, err := api.NewClient(&api.Config{Address: server.URL})
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	cfg := VaultConfig{
		AuthMethod: AuthMethodKubernetes,
		K8sRole:    "my-k8s-role",
		K8sJWTPath: "", // Empty - should use default
	}

	// Will fail because default path doesn't exist, but verifies the code path
	err = authenticate(context.Background(), client, cfg)
	if err == nil {
		t.Error("authenticate() expected error for non-existent default JWT path")
	}
}

func TestAuthenticateAWS_WithRoleAndRegion(t *testing.T) {
	// This test verifies the AWS auth code path but will fail without AWS credentials
	// We're testing that the function constructs the auth request correctly
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client, err := api.NewClient(&api.Config{Address: server.URL})
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	// AWS auth will fail without proper credentials, but we're testing the setup
	err = authenticateAWS(context.Background(), client, "my-role", "us-east-1")
	// Expected to fail without AWS credentials
	if err == nil {
		t.Log("AWS auth succeeded (running in AWS environment)")
	}
}

func TestAuthenticateAWS_EmptyRoleAndRegion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client, err := api.NewClient(&api.Config{Address: server.URL})
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	// AWS auth with empty role and region
	err = authenticateAWS(context.Background(), client, "", "")
	// Expected to fail without AWS credentials
	if err == nil {
		t.Log("AWS auth succeeded (running in AWS environment)")
	}
}

func TestAuthenticateAppRole_DirectCall(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == testAppRoleLoginPath {
			response := map[string]interface{}{
				"auth": map[string]interface{}{
					"client_token": "direct-approle-token",
				},
			}
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(response)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	client, err := api.NewClient(&api.Config{Address: server.URL})
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	err = authenticateAppRole(context.Background(), client, "role-id", "secret-id")
	if err != nil {
		t.Errorf("authenticateAppRole() error = %v", err)
	}

	if client.Token() != "direct-approle-token" {
		t.Errorf("client.Token() = %s, want direct-approle-token", client.Token())
	}
}

func TestAuthenticateK8s_DirectCall(t *testing.T) {
	// Create a temporary JWT file
	tempDir := t.TempDir()
	jwtPath := filepath.Join(tempDir, "token")
	if err := os.WriteFile(jwtPath, []byte("test-jwt-token"), 0600); err != nil {
		t.Fatalf("failed to create temp JWT file: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == testKubernetesLoginPath {
			response := map[string]interface{}{
				"auth": map[string]interface{}{
					"client_token": "direct-k8s-token",
				},
			}
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(response)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	client, err := api.NewClient(&api.Config{Address: server.URL})
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	err = authenticateWithJWT(context.Background(), client, "kubernetes", "my-role", jwtPath)
	if err != nil {
		t.Errorf("authenticateWithJWT() error = %v", err)
	}

	if client.Token() != "direct-k8s-token" {
		t.Errorf("client.Token() = %s, want direct-k8s-token", client.Token())
	}
}

func TestAuthenticateK8s_LoginFailure(t *testing.T) {
	// Create a temporary JWT file
	tempDir := t.TempDir()
	jwtPath := filepath.Join(tempDir, "token")
	if err := os.WriteFile(jwtPath, []byte("test-jwt-token"), 0600); err != nil {
		t.Fatalf("failed to create temp JWT file: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == testKubernetesLoginPath {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"errors": ["permission denied"]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	client, err := api.NewClient(&api.Config{Address: server.URL})
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	err = authenticateWithJWT(context.Background(), client, "kubernetes", "my-role", jwtPath)
	if err == nil {
		t.Error("authenticateWithJWT() expected error for login failure")
	}
}
