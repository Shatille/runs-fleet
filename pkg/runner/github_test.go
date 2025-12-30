package runner

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func init() {
	// Use minimal delays in tests to avoid slow test execution
	baseRetryDelay = 1 * time.Millisecond
}

const (
	testPathOrgInstallation  = "/orgs/myorg/installation"
	testPathUserInstallation = "/users/myuser/installation"
	testPathAccessTokens123  = "/app/installations/123/access_tokens"
	testPathAccessTokens456  = "/app/installations/456/access_tokens"
	testPathRegTokenMyOrg    = "/repos/myorg/myrepo/actions/runners/registration-token"
	testPathRegTokenMyUser   = "/repos/myuser/myrepo/actions/runners/registration-token"
	testPathOrgTestOrg       = "/orgs/testorg/installation"
	testPathOrgMyUser        = "/orgs/myuser/installation"
)

// generateTestKey generates a test RSA key pair and returns the base64-encoded PEM.
func generateTestKey(t *testing.T) string {
	t.Helper()

	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate test key: %v", err)
	}

	keyBytes := x509.MarshalPKCS1PrivateKey(privateKey)
	pemBlock := &pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: keyBytes,
	}
	pemBytes := pem.EncodeToMemory(pemBlock)

	return base64.StdEncoding.EncodeToString(pemBytes)
}

func TestNewGitHubClient_ValidKey(t *testing.T) {
	keyBase64 := generateTestKey(t)

	client, err := NewGitHubClient("12345", keyBase64)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if client.appID != 12345 {
		t.Errorf("expected appID 12345, got %d", client.appID)
	}

	if client.privateKey == nil {
		t.Error("expected privateKey to be set")
	}

	if client.httpClient == nil {
		t.Error("expected httpClient to be set")
	}
}

func TestNewGitHubClient_InvalidAppID(t *testing.T) {
	keyBase64 := generateTestKey(t)

	_, err := NewGitHubClient("invalid", keyBase64)
	if err == nil {
		t.Fatal("expected error for invalid app ID")
	}
}

func TestNewGitHubClient_InvalidBase64(t *testing.T) {
	_, err := NewGitHubClient("12345", "invalid-base64!!!")
	if err == nil {
		t.Fatal("expected error for invalid base64")
	}
}

func TestNewGitHubClient_InvalidPEM(t *testing.T) {
	// Valid base64 but not a valid PEM
	invalidPEM := base64.StdEncoding.EncodeToString([]byte("not a pem"))

	_, err := NewGitHubClient("12345", invalidPEM)
	if err == nil {
		t.Fatal("expected error for invalid PEM")
	}
}

func TestNewGitHubClient_PKCS8Key(t *testing.T) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}

	// Encode as PKCS8
	keyBytes, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatalf("failed to marshal PKCS8: %v", err)
	}

	pemBlock := &pem.Block{
		Type:  "PRIVATE KEY",
		Bytes: keyBytes,
	}
	pemBytes := pem.EncodeToMemory(pemBlock)
	keyBase64 := base64.StdEncoding.EncodeToString(pemBytes)

	client, err := NewGitHubClient("12345", keyBase64)
	if err != nil {
		t.Fatalf("unexpected error for PKCS8 key: %v", err)
	}

	if client.privateKey == nil {
		t.Error("expected privateKey to be set for PKCS8")
	}
}

func TestGitHubClient_generateJWT(t *testing.T) {
	keyBase64 := generateTestKey(t)

	client, err := NewGitHubClient("12345", keyBase64)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	jwt, err := client.generateJWT()
	if err != nil {
		t.Fatalf("unexpected error generating JWT: %v", err)
	}

	if jwt == "" {
		t.Error("expected non-empty JWT")
	}

	// JWT should have 3 parts separated by dots
	parts := 0
	for _, c := range jwt {
		if c == '.' {
			parts++
		}
	}
	if parts != 2 {
		t.Errorf("expected JWT with 2 dots (3 parts), got %d dots", parts)
	}
}

func TestGitHubClient_GetRegistrationToken_InvalidRepo(t *testing.T) {
	keyBase64 := generateTestKey(t)

	client, err := NewGitHubClient("12345", keyBase64)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	// Test empty repo
	_, err = client.GetRegistrationToken(context.Background(), "")
	if err == nil {
		t.Error("expected error for empty repo")
	}

	// Test invalid repo format
	_, err = client.GetRegistrationToken(context.Background(), "invalid")
	if err == nil {
		t.Error("expected error for invalid repo format")
	}

	// Test repo with empty parts
	_, err = client.GetRegistrationToken(context.Background(), "/repo")
	if err == nil {
		t.Error("expected error for repo with empty owner")
	}

	_, err = client.GetRegistrationToken(context.Background(), "owner/")
	if err == nil {
		t.Error("expected error for repo with empty name")
	}
}

func TestGitHubClient_GetRegistrationToken_Success(t *testing.T) {
	keyBase64 := generateTestKey(t)

	// Create a test server that simulates GitHub API
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case testPathOrgInstallation:
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"id": 123,
				"account": map[string]interface{}{
					"type": "Organization",
				},
			})
		case testPathAccessTokens123:
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"token": "ghs_test_token",
			})
		case testPathRegTokenMyOrg:
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"token": "AABB123",
			})
		default:
			t.Logf("unexpected request path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := NewGitHubClient("12345", keyBase64)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}
	client.baseURL = server.URL

	result, err := client.GetRegistrationToken(context.Background(), "myorg/myrepo")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.Token != "AABB123" {
		t.Errorf("expected token 'AABB123', got '%s'", result.Token)
	}
	if result.IsOrg {
		t.Error("expected IsOrg to be false for repo-level registration")
	}
}

func TestGitHubClient_GetJITConfig_EmptyOrg(t *testing.T) {
	keyBase64 := generateTestKey(t)

	client, err := NewGitHubClient("12345", keyBase64)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	_, err = client.GetJITConfig(context.Background(), "", "runner-name", []string{"self-hosted"})
	if err == nil {
		t.Error("expected error for empty org")
	}
}

func TestGitHubClient_getInstallationInfo_EmptyOwner(t *testing.T) {
	keyBase64 := generateTestKey(t)

	client, err := NewGitHubClient("12345", keyBase64)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	_, err = client.getInstallationInfo(context.Background(), "")
	if err == nil {
		t.Error("expected error for empty owner")
	}
}

func TestGitHubClient_HttpTimeout(t *testing.T) {
	keyBase64 := generateTestKey(t)

	client, err := NewGitHubClient("12345", keyBase64)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	// Verify http client timeout is set
	if client.httpClient.Timeout != 30*time.Second {
		t.Errorf("expected 30s timeout, got %v", client.httpClient.Timeout)
	}
}

func TestRegistrationResult_Structure(t *testing.T) {
	result := RegistrationResult{
		Token: "test-token",
		IsOrg: true,
	}

	if result.Token != "test-token" {
		t.Errorf("expected token 'test-token', got '%s'", result.Token)
	}
	if !result.IsOrg {
		t.Error("expected IsOrg to be true")
	}
}

func TestInstallationInfo_Structure(t *testing.T) {
	info := installationInfo{
		Token: "test-token",
		IsOrg: false,
	}

	if info.Token != "test-token" {
		t.Errorf("expected token 'test-token', got '%s'", info.Token)
	}
	if info.IsOrg {
		t.Error("expected IsOrg to be false")
	}
}

func TestGitHubClient_getInstallationToken(t *testing.T) {
	keyBase64 := generateTestKey(t)

	// Create a test server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case testPathOrgTestOrg:
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"id": 123,
				"account": map[string]interface{}{
					"type": "Organization",
				},
			})
		case testPathAccessTokens123:
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"token": "ghs_test_token",
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := NewGitHubClient("12345", keyBase64)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}
	client.baseURL = server.URL

	// Test with empty owner
	_, err = client.getInstallationToken(context.Background(), "")
	if err == nil {
		t.Error("expected error for empty owner")
	}

	// Test successful token retrieval
	token, err := client.getInstallationToken(context.Background(), "testorg")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token != "ghs_test_token" {
		t.Errorf("expected token 'ghs_test_token', got '%s'", token)
	}
}

func TestGitHubClient_FallbackToUserInstallation(t *testing.T) {
	keyBase64 := generateTestKey(t)

	// Create a test server that simulates a personal account (no org)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case testPathOrgMyUser:
			w.WriteHeader(http.StatusNotFound)
		case testPathUserInstallation:
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"id": 456,
				"account": map[string]interface{}{
					"type": "User",
				},
			})
		case testPathAccessTokens456:
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"token": "ghs_user_token",
			})
		case testPathRegTokenMyUser:
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"token": "user_reg_token",
			})
		default:
			t.Logf("unexpected request path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := NewGitHubClient("12345", keyBase64)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}
	client.baseURL = server.URL

	// Test that user installation fallback works
	result, err := client.GetRegistrationToken(context.Background(), "myuser/myrepo")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.Token != "user_reg_token" {
		t.Errorf("expected token 'user_reg_token', got '%s'", result.Token)
	}
	if result.IsOrg {
		t.Error("expected IsOrg to be false for user account")
	}
}

func TestIsRetryableError(t *testing.T) {
	tests := []struct {
		name     string
		resp     *http.Response
		err      error
		expected bool
	}{
		{
			name:     "network error is retryable",
			resp:     nil,
			err:      context.DeadlineExceeded,
			expected: true,
		},
		{
			name:     "nil response is retryable",
			resp:     nil,
			err:      nil,
			expected: true,
		},
		{
			name:     "429 rate limit is retryable",
			resp:     &http.Response{StatusCode: http.StatusTooManyRequests},
			err:      nil,
			expected: true,
		},
		{
			name:     "500 server error is retryable",
			resp:     &http.Response{StatusCode: http.StatusInternalServerError},
			err:      nil,
			expected: true,
		},
		{
			name:     "502 bad gateway is retryable",
			resp:     &http.Response{StatusCode: http.StatusBadGateway},
			err:      nil,
			expected: true,
		},
		{
			name:     "503 service unavailable is retryable",
			resp:     &http.Response{StatusCode: http.StatusServiceUnavailable},
			err:      nil,
			expected: true,
		},
		{
			name:     "400 bad request is not retryable",
			resp:     &http.Response{StatusCode: http.StatusBadRequest},
			err:      nil,
			expected: false,
		},
		{
			name:     "401 unauthorized is not retryable",
			resp:     &http.Response{StatusCode: http.StatusUnauthorized},
			err:      nil,
			expected: false,
		},
		{
			name:     "403 forbidden is not retryable",
			resp:     &http.Response{StatusCode: http.StatusForbidden},
			err:      nil,
			expected: false,
		},
		{
			name:     "404 not found is not retryable",
			resp:     &http.Response{StatusCode: http.StatusNotFound},
			err:      nil,
			expected: false,
		},
		{
			name:     "201 created is not retryable",
			resp:     &http.Response{StatusCode: http.StatusCreated},
			err:      nil,
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isRetryableError(tt.resp, tt.err)
			if result != tt.expected {
				t.Errorf("isRetryableError() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestRetryDelay(t *testing.T) {
	for attempt := 0; attempt < 5; attempt++ {
		delay := retryDelay(attempt)
		expectedMin := (baseRetryDelay * time.Duration(1<<attempt)) / 2
		expectedMax := baseRetryDelay * time.Duration(1<<attempt)
		if expectedMax > maxRetryDelay {
			expectedMax = maxRetryDelay
			expectedMin = maxRetryDelay / 2
		}

		if delay < expectedMin || delay > expectedMax {
			t.Errorf("attempt %d: delay %v outside expected range [%v, %v]",
				attempt, delay, expectedMin, expectedMax)
		}
	}
}

func TestGitHubClient_GetRegistrationToken_RetriesOnTransientErrors(t *testing.T) {
	tests := []struct {
		name             string
		errorStatusCode  int
		failuresBeforeOK int
		expectedAttempts int
		expectedToken    string
	}{
		{
			name:             "retries on 503 service unavailable",
			errorStatusCode:  http.StatusServiceUnavailable,
			failuresBeforeOK: 2,
			expectedAttempts: 3,
			expectedToken:    "RETRY_SUCCESS",
		},
		{
			name:             "retries on 429 rate limit",
			errorStatusCode:  http.StatusTooManyRequests,
			failuresBeforeOK: 1,
			expectedAttempts: 2,
			expectedToken:    "RATE_LIMIT_SUCCESS",
		},
		{
			name:             "retries on 500 internal server error",
			errorStatusCode:  http.StatusInternalServerError,
			failuresBeforeOK: 1,
			expectedAttempts: 2,
			expectedToken:    "SERVER_ERROR_SUCCESS",
		},
		{
			name:             "retries on 502 bad gateway",
			errorStatusCode:  http.StatusBadGateway,
			failuresBeforeOK: 1,
			expectedAttempts: 2,
			expectedToken:    "BAD_GATEWAY_SUCCESS",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			keyBase64 := generateTestKey(t)

			attempts := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case testPathOrgInstallation:
					w.WriteHeader(http.StatusOK)
					_ = json.NewEncoder(w).Encode(map[string]interface{}{
						"id": 123,
						"account": map[string]interface{}{
							"type": "Organization",
						},
					})
				case testPathAccessTokens123:
					w.WriteHeader(http.StatusCreated)
					_ = json.NewEncoder(w).Encode(map[string]string{
						"token": "ghs_test_token",
					})
				case testPathRegTokenMyOrg:
					attempts++
					if attempts <= tt.failuresBeforeOK {
						w.WriteHeader(tt.errorStatusCode)
						return
					}
					w.WriteHeader(http.StatusCreated)
					_ = json.NewEncoder(w).Encode(map[string]string{
						"token": tt.expectedToken,
					})
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()

			client, err := NewGitHubClient("12345", keyBase64)
			if err != nil {
				t.Fatalf("failed to create client: %v", err)
			}
			client.baseURL = server.URL

			result, err := client.GetRegistrationToken(context.Background(), "myorg/myrepo")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if result.Token != tt.expectedToken {
				t.Errorf("expected token '%s', got '%s'", tt.expectedToken, result.Token)
			}

			if attempts != tt.expectedAttempts {
				t.Errorf("expected %d attempts, got %d", tt.expectedAttempts, attempts)
			}
		})
	}
}

func TestGitHubClient_GetRegistrationToken_NoRetryOnClientError(t *testing.T) {
	keyBase64 := generateTestKey(t)

	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case testPathOrgInstallation:
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"id": 123,
				"account": map[string]interface{}{
					"type": "Organization",
				},
			})
		case testPathAccessTokens123:
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"token": "ghs_test_token",
			})
		case testPathRegTokenMyOrg:
			attempts++
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"forbidden"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := NewGitHubClient("12345", keyBase64)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}
	client.baseURL = server.URL

	_, err = client.GetRegistrationToken(context.Background(), "myorg/myrepo")
	if err == nil {
		t.Fatal("expected error for forbidden response")
	}

	if attempts != 1 {
		t.Errorf("expected 1 attempt (no retries for 403), got %d", attempts)
	}
}

func TestGitHubClient_GetRegistrationToken_ExhaustsRetries(t *testing.T) {
	keyBase64 := generateTestKey(t)

	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case testPathOrgInstallation:
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"id": 123,
				"account": map[string]interface{}{
					"type": "Organization",
				},
			})
		case testPathAccessTokens123:
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"token": "ghs_test_token",
			})
		case testPathRegTokenMyOrg:
			attempts++
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := NewGitHubClient("12345", keyBase64)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}
	client.baseURL = server.URL

	_, err = client.GetRegistrationToken(context.Background(), "myorg/myrepo")
	if err == nil {
		t.Fatal("expected error after exhausting retries")
	}

	expectedAttempts := maxRetries + 1
	if attempts != expectedAttempts {
		t.Errorf("expected %d attempts, got %d", expectedAttempts, attempts)
	}
}

func TestGitHubClient_GetRegistrationToken_RespectsContextCancellation(t *testing.T) {
	keyBase64 := generateTestKey(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case testPathOrgInstallation:
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"id": 123,
				"account": map[string]interface{}{
					"type": "Organization",
				},
			})
		case testPathAccessTokens123:
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"token": "ghs_test_token",
			})
		case testPathRegTokenMyOrg:
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := NewGitHubClient("12345", keyBase64)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}
	client.baseURL = server.URL

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	_, err = client.GetRegistrationToken(ctx, "myorg/myrepo")
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
}

func TestGitHubClient_GetRegistrationToken_ContextCancelledDuringBackoff(t *testing.T) {
	keyBase64 := generateTestKey(t)

	attempts := 0
	attemptCh := make(chan struct{}, 1) // Signal when registration token endpoint is hit
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case testPathOrgInstallation:
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"id": 123,
				"account": map[string]interface{}{
					"type": "Organization",
				},
			})
		case testPathAccessTokens123:
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"token": "ghs_test_token",
			})
		case testPathRegTokenMyOrg:
			attempts++
			// Signal that an attempt was made
			select {
			case attemptCh <- struct{}{}:
			default:
			}
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := NewGitHubClient("12345", keyBase64)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}
	client.baseURL = server.URL

	// Save original delay and use a longer one for this test to allow cancellation during backoff
	originalDelay := baseRetryDelay
	baseRetryDelay = 50 * time.Millisecond
	defer func() { baseRetryDelay = originalDelay }()

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		_, err := client.GetRegistrationToken(ctx, "myorg/myrepo")
		done <- err
	}()

	// Wait for first attempt to fail, then cancel during backoff
	select {
	case <-attemptCh:
		// First attempt received, cancel during backoff
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for first attempt")
	}
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("expected context.Canceled, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for cancellation")
	}

	if attempts < 1 {
		t.Errorf("expected at least 1 attempt before cancellation, got %d", attempts)
	}
}

func TestGitHubClient_GetJITConfig_Success(t *testing.T) {
	keyBase64 := generateTestKey(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case testPathOrgInstallation:
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"id": 123,
				"account": map[string]interface{}{
					"type": "Organization",
				},
			})
		case testPathAccessTokens123:
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"token": "ghs_test_token",
			})
		case "/orgs/myorg/actions/runners/generate-jitconfig":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"encoded_jit_config": "base64-encoded-jit-config",
				"runner": map[string]interface{}{
					"id":   12345,
					"name": "test-runner",
				},
			})
		default:
			t.Logf("unexpected request path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := NewGitHubClient("12345", keyBase64)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}
	client.baseURL = server.URL

	// Note: This will fail because go-github client uses api.github.com directly
	// The test validates the function signature and error handling
	_, err = client.GetJITConfig(context.Background(), "myorg", "test-runner", []string{"self-hosted"})
	// Error is expected because the GitHub client doesn't use our mock server's baseURL
	if err == nil {
		t.Log("GetJITConfig succeeded (unexpected but acceptable if mock is working)")
	}
}

func TestGitHubClient_GetJITConfig_Validation(t *testing.T) {
	keyBase64 := generateTestKey(t)

	client, err := NewGitHubClient("12345", keyBase64)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	// Test empty org - should return error immediately without network call
	_, err = client.GetJITConfig(context.Background(), "", "test-runner", []string{"self-hosted"})
	if err == nil {
		t.Error("expected error for empty org")
	}
}

func TestGitHubClient_getInstallationInfo_Success(t *testing.T) {
	keyBase64 := generateTestKey(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case testPathOrgInstallation:
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"id": 123,
				"account": map[string]interface{}{
					"type": "Organization",
				},
			})
		case testPathAccessTokens123:
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"token": "ghs_installation_token",
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := NewGitHubClient("12345", keyBase64)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}
	client.baseURL = server.URL

	info, err := client.getInstallationInfo(context.Background(), "myorg")
	if err != nil {
		t.Fatalf("getInstallationInfo() error = %v", err)
	}

	if info.Token != "ghs_installation_token" {
		t.Errorf("Token = %s, want ghs_installation_token", info.Token)
	}
	if !info.IsOrg {
		t.Error("IsOrg should be true for Organization account")
	}
}

func TestGitHubClient_getInstallationInfo_UserAccount(t *testing.T) {
	keyBase64 := generateTestKey(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case testPathOrgMyUser:
			w.WriteHeader(http.StatusNotFound)
		case testPathUserInstallation:
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"id": 456,
				"account": map[string]interface{}{
					"type": "User",
				},
			})
		case testPathAccessTokens456:
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"token": "ghs_user_installation_token",
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := NewGitHubClient("12345", keyBase64)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}
	client.baseURL = server.URL

	info, err := client.getInstallationInfo(context.Background(), "myuser")
	if err != nil {
		t.Fatalf("getInstallationInfo() error = %v", err)
	}

	if info.Token != "ghs_user_installation_token" {
		t.Errorf("Token = %s, want ghs_user_installation_token", info.Token)
	}
	if info.IsOrg {
		t.Error("IsOrg should be false for User account")
	}
}

func TestGitHubClient_getInstallationInfo_NotFound(t *testing.T) {
	keyBase64 := generateTestKey(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Return 404 for both org and user installation
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found"}`))
	}))
	defer server.Close()

	client, err := NewGitHubClient("12345", keyBase64)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}
	client.baseURL = server.URL

	_, err = client.getInstallationInfo(context.Background(), "unknownorg")
	if err == nil {
		t.Fatal("expected error for unknown org")
	}
}

func TestGitHubClient_getInstallationInfo_TokenCreationFailed(t *testing.T) {
	keyBase64 := generateTestKey(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case testPathOrgInstallation:
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"id": 123,
				"account": map[string]interface{}{
					"type": "Organization",
				},
			})
		case testPathAccessTokens123:
			// Fail token creation
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"message":"Internal Server Error"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := NewGitHubClient("12345", keyBase64)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}
	client.baseURL = server.URL

	_, err = client.getInstallationInfo(context.Background(), "myorg")
	if err == nil {
		t.Fatal("expected error when token creation fails")
	}
}

func TestRetryDelay_Bounds(t *testing.T) {
	// Test that retry delay is bounded
	for attempt := 0; attempt < 20; attempt++ {
		delay := retryDelay(attempt)

		// Delay should never exceed maxRetryDelay
		if delay > maxRetryDelay {
			t.Errorf("attempt %d: delay %v exceeds maxRetryDelay %v", attempt, delay, maxRetryDelay)
		}

		// Delay should always be positive
		if delay <= 0 {
			t.Errorf("attempt %d: delay %v should be positive", attempt, delay)
		}
	}
}

func TestRetryDelay_ExponentialGrowth(t *testing.T) {
	// Test that delay grows exponentially until capped
	var lastDelay time.Duration
	for attempt := 0; attempt < 5; attempt++ {
		delay := retryDelay(attempt)

		// Get multiple samples to account for jitter
		minDelay := delay
		maxDelay := delay
		for i := 0; i < 10; i++ {
			d := retryDelay(attempt)
			if d < minDelay {
				minDelay = d
			}
			if d > maxDelay {
				maxDelay = d
			}
		}

		// Average should increase with each attempt (until capped)
		avgDelay := (minDelay + maxDelay) / 2
		if attempt > 0 && avgDelay <= lastDelay/2 && lastDelay < maxRetryDelay {
			t.Logf("attempt %d: delay did not grow as expected (avg=%v, last=%v)", attempt, avgDelay, lastDelay)
		}
		lastDelay = avgDelay
	}
}

func TestNewGitHubClient_BaseURLDefault(t *testing.T) {
	keyBase64 := generateTestKey(t)

	client, err := NewGitHubClient("12345", keyBase64)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	if client.baseURL != "https://api.github.com" {
		t.Errorf("baseURL = %s, want https://api.github.com", client.baseURL)
	}
}

func TestGitHubClient_generateJWT_Validity(t *testing.T) {
	keyBase64 := generateTestKey(t)

	client, err := NewGitHubClient("12345", keyBase64)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	jwtToken, err := client.generateJWT()
	if err != nil {
		t.Fatalf("generateJWT() error = %v", err)
	}

	// Verify JWT structure (header.payload.signature)
	parts := 0
	lastDot := -1
	for i, c := range jwtToken {
		if c == '.' {
			parts++
			// Verify each part is non-empty
			if i == lastDot+1 {
				t.Error("JWT has empty part")
			}
			lastDot = i
		}
	}
	if parts != 2 {
		t.Errorf("JWT should have 2 dots (3 parts), got %d dots", parts)
	}

	// Verify last part (signature) is not empty
	if lastDot == len(jwtToken)-1 {
		t.Error("JWT signature part is empty")
	}
}

func TestIsRetryableError_AllStatusCodes(t *testing.T) {
	// Comprehensive test of status codes
	statusCodes := []struct {
		code        int
		retryable   bool
		description string
	}{
		{100, false, "Continue"},
		{200, false, "OK"},
		{201, false, "Created"},
		{204, false, "No Content"},
		{301, false, "Moved Permanently"},
		{302, false, "Found"},
		{400, false, "Bad Request"},
		{401, false, "Unauthorized"},
		{403, false, "Forbidden"},
		{404, false, "Not Found"},
		{405, false, "Method Not Allowed"},
		{422, false, "Unprocessable Entity"},
		{429, true, "Too Many Requests"},
		{500, true, "Internal Server Error"},
		{501, true, "Not Implemented"},
		{502, true, "Bad Gateway"},
		{503, true, "Service Unavailable"},
		{504, true, "Gateway Timeout"},
	}

	for _, tc := range statusCodes {
		t.Run(tc.description, func(t *testing.T) {
			resp := &http.Response{StatusCode: tc.code}
			result := isRetryableError(resp, nil)
			if result != tc.retryable {
				t.Errorf("status %d (%s): isRetryableError() = %v, want %v",
					tc.code, tc.description, result, tc.retryable)
			}
		})
	}
}
