package secrets

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hashicorp/vault/api"
)

const (
	reloginRunnerID = "i-123"
	testStaleToken  = "t1"
	testFreshToken  = "t2"
)

type fakeVault struct {
	mu          sync.Mutex
	valid       map[string]bool
	lookupTTL   int
	renewLease  int
	kvRequests  atomic.Int32
	renewCalls  atomic.Int32
	lookupCalls atomic.Int32
}

func newFakeVault(t *testing.T, validTokens ...string) (*fakeVault, *httptest.Server) {
	t.Helper()
	fv := &fakeVault{valid: map[string]bool{}, lookupTTL: 7200, renewLease: 7200}
	for _, tok := range validTokens {
		fv.valid[tok] = true
	}
	server := httptest.NewServer(http.HandlerFunc(fv.serve))
	t.Cleanup(server.Close)
	return fv, server
}

func (fv *fakeVault) serve(w http.ResponseWriter, r *http.Request) {
	fv.mu.Lock()
	ok := fv.valid[r.Header.Get("X-Vault-Token")]
	lookupTTL, renewLease := fv.lookupTTL, fv.renewLease
	fv.mu.Unlock()

	switch r.URL.Path {
	case "/v1/auth/token/lookup-self":
		fv.lookupCalls.Add(1)
	case "/v1/auth/token/renew-self":
		fv.renewCalls.Add(1)
	default:
		fv.kvRequests.Add(1)
	}

	if !ok {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"errors":["permission denied","invalid token"]}`))
		return
	}

	switch r.URL.Path {
	case "/v1/auth/token/lookup-self":
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"ttl": lookupTTL, "renewable": true}})
	case "/v1/auth/token/renew-self":
		_ = json.NewEncoder(w).Encode(map[string]any{"auth": map[string]any{
			"client_token": r.Header.Get("X-Vault-Token"), "lease_duration": renewLease, "renewable": true,
		}})
	case "/v1/secret/data/runs-fleet/runners/" + reloginRunnerID:
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"data": map[string]any{"org": "testorg"}}})
			return
		}
		_, _ = w.Write([]byte(`{"data": {}}`))
	case "/v1/secret/metadata/runs-fleet/runners/" + reloginRunnerID:
		w.WriteHeader(http.StatusNoContent)
	case "/v1/secret/metadata/runs-fleet/runners":
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"keys": []string{reloginRunnerID}}})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

type loginStub struct {
	calls atomic.Int32
	mu    sync.Mutex
	next  []loginResult
}

type loginResult struct {
	token string
	err   error
}

func (l *loginStub) login(_ context.Context) (string, error) {
	l.calls.Add(1)
	l.mu.Lock()
	defer l.mu.Unlock()
	res := l.next[0]
	if len(l.next) > 1 {
		l.next = l.next[1:]
	}
	return res.token, res.err
}

func newReloginStore(t *testing.T, server *httptest.Server, token string, stub *loginStub) *VaultStore {
	t.Helper()
	client, err := api.NewClient(&api.Config{Address: server.URL})
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}
	client.SetToken(token)
	store := NewVaultStoreWithClient(client, "secret", "runs-fleet/runners", 2)
	if stub != nil {
		store.login = stub.login
	}
	return store
}

func TestVaultStore_RevokedToken_OperationsLogInAgain(t *testing.T) {
	t.Parallel()

	ops := map[string]func(*VaultStore) error{
		"Get": func(s *VaultStore) error { _, err := s.Get(t.Context(), reloginRunnerID); return err },
		"Put": func(s *VaultStore) error {
			return s.Put(t.Context(), reloginRunnerID, &RunnerConfig{Org: "testorg"})
		},
		"Delete": func(s *VaultStore) error { return s.Delete(t.Context(), reloginRunnerID) },
		"List":   func(s *VaultStore) error { _, err := s.List(t.Context()); return err },
	}

	for name, op := range ops {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, server := newFakeVault(t, testFreshToken)
			stub := &loginStub{next: []loginResult{{token: testFreshToken}}}
			store := newReloginStore(t, server, testStaleToken, stub)

			if err := op(store); err != nil {
				t.Fatalf("%s() error = %v, want success after re-login", name, err)
			}
			if got := stub.calls.Load(); got != 1 {
				t.Errorf("login calls = %d, want 1", got)
			}
			if got := store.client.Token(); got != testFreshToken {
				t.Errorf("client token = %q, want t2", got)
			}
		})
	}
}

func TestVaultStore_RevokedToken_StaticTokenPassesThrough(t *testing.T) {
	t.Parallel()
	fv, server := newFakeVault(t)
	store := newReloginStore(t, server, testStaleToken, nil)

	_, err := store.Get(t.Context(), reloginRunnerID)
	if !isPermissionDenied(err) {
		t.Fatalf("Get() error = %v, want 403", err)
	}
	if got := fv.kvRequests.Load(); got != 1 {
		t.Errorf("KV requests = %d, want 1 (no retry without a login method)", got)
	}
}

func TestVaultStore_PersistentDenial_RetriesOnce(t *testing.T) {
	t.Parallel()
	fv, server := newFakeVault(t)
	stub := &loginStub{next: []loginResult{{token: testFreshToken}}}
	store := newReloginStore(t, server, testStaleToken, stub)

	_, err := store.Get(t.Context(), reloginRunnerID)
	if !isPermissionDenied(err) {
		t.Fatalf("Get() error = %v, want 403", err)
	}
	if got := stub.calls.Load(); got != 1 {
		t.Errorf("login calls = %d, want 1", got)
	}
	if got := fv.kvRequests.Load(); got != 2 {
		t.Errorf("KV requests = %d, want 2 (one retry)", got)
	}
}

func TestVaultStore_FailedRelogin_KeepsTokenAndRetriesNextCall(t *testing.T) {
	t.Parallel()
	_, server := newFakeVault(t, testFreshToken)
	loginErr := errors.New("vault unreachable")
	stub := &loginStub{next: []loginResult{{err: loginErr}, {token: testFreshToken}}}
	store := newReloginStore(t, server, testStaleToken, stub)

	_, err := store.Get(t.Context(), reloginRunnerID)
	if !errors.Is(err, loginErr) {
		t.Fatalf("first Get() error = %v, want it to carry the login error", err)
	}
	if got := store.client.Token(); got != testStaleToken {
		t.Errorf("client token after failed login = %q, want t1 unchanged", got)
	}

	if _, err := store.Get(t.Context(), reloginRunnerID); err != nil {
		t.Fatalf("second Get() error = %v, want success", err)
	}
	if got := stub.calls.Load(); got != 2 {
		t.Errorf("login calls = %d, want 2", got)
	}
}

func TestVaultStore_ConcurrentDenials_SingleLogin(t *testing.T) {
	t.Parallel()
	_, server := newFakeVault(t, testFreshToken)
	stub := &loginStub{next: []loginResult{{token: testFreshToken}}}
	store := newReloginStore(t, server, testStaleToken, stub)

	const workers = 16
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for range workers {
		wg.Go(func() {
			if _, err := store.Get(t.Context(), reloginRunnerID); err != nil {
				errs <- err
			}
		})
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("Get() error = %v", err)
	}
	if got := stub.calls.Load(); got != 1 {
		t.Errorf("login calls = %d, want 1", got)
	}
}

func TestVaultStore_MaybeRenewToken(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		valid      []string
		lookupTTL  int
		renewLease int
		wantLogins int32
		wantToken  string
	}{
		{"healthy token is not renewed", []string{testStaleToken, testFreshToken}, 7200, 7200, 0, testStaleToken},
		{"renewal extends the token", []string{testStaleToken, testFreshToken}, 1800, 3600, 0, testStaleToken},
		{"renewal capped by max TTL logs in again", []string{testStaleToken, testFreshToken}, 300, 120, 1, testFreshToken},
		{"revoked token logs in again", []string{testFreshToken}, 7200, 7200, 1, testFreshToken},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fv, server := newFakeVault(t, tt.valid...)
			fv.lookupTTL, fv.renewLease = tt.lookupTTL, tt.renewLease
			stub := &loginStub{next: []loginResult{{token: testFreshToken}}}
			store := newReloginStore(t, server, testStaleToken, stub)

			if err := store.maybeRenewToken(t.Context()); err != nil {
				t.Fatalf("maybeRenewToken() error = %v", err)
			}
			if got := stub.calls.Load(); got != tt.wantLogins {
				t.Errorf("login calls = %d, want %d", got, tt.wantLogins)
			}
			if got := store.client.Token(); got != tt.wantToken {
				t.Errorf("client token = %q, want %q", got, tt.wantToken)
			}
		})
	}
}

func TestVaultStore_MaybeRenewToken_StaticTokenRevoked(t *testing.T) {
	t.Parallel()
	_, server := newFakeVault(t)
	store := newReloginStore(t, server, testStaleToken, nil)

	if err := store.maybeRenewToken(t.Context()); err == nil {
		t.Fatal("maybeRenewToken() error = nil, want lookup failure")
	}
}

func TestReloginFunc_StaticTokenHasNoLogin(t *testing.T) {
	tests := []struct {
		name     string
		cfg      VaultConfig
		envToken string
		wantNil  bool
	}{
		{"token method", VaultConfig{AuthMethod: AuthMethodToken, Token: "x"}, "", true},
		{"env token fallback", VaultConfig{}, "from-env", true},
		{"kubernetes", VaultConfig{AuthMethod: AuthMethodKubernetes}, "", false},
		{"aws", VaultConfig{AuthMethod: AuthMethodAWS}, "", false},
		{"approle", VaultConfig{AuthMethod: AuthMethodAppRole}, "", false},
		{"default aws", VaultConfig{}, "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("VAULT_TOKEN", tt.envToken)
			client, err := api.NewClient(api.DefaultConfig())
			if err != nil {
				t.Fatalf("failed to create client: %v", err)
			}
			if got := reloginFunc(client, tt.cfg) == nil; got != tt.wantNil {
				t.Errorf("reloginFunc() nil = %v, want %v", got, tt.wantNil)
			}
		})
	}
}

func TestVaultStore_Relogin_SurvivesCallerCancellation(t *testing.T) {
	t.Parallel()
	_, server := newFakeVault(t, testFreshToken)
	store := newReloginStore(t, server, testStaleToken, nil)
	store.login = func(ctx context.Context) (string, error) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		return testFreshToken, nil
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if err := store.relogin(ctx, testStaleToken); err != nil {
		t.Fatalf("relogin() error = %v, want the shared login to ignore one caller's cancellation", err)
	}
	if got := store.client.Token(); got != testFreshToken {
		t.Errorf("client token = %q, want t2", got)
	}
}

func TestVaultStore_RevokedToken_MissingSecretStillNotFound(t *testing.T) {
	t.Parallel()
	_, server := newFakeVault(t, testFreshToken)
	stub := &loginStub{next: []loginResult{{token: testFreshToken}}}
	store := newReloginStore(t, server, testStaleToken, stub)

	_, err := store.Get(t.Context(), "i-missing")
	if !errors.Is(err, ErrConfigNotFound) {
		t.Fatalf("Get() error = %v, want ErrConfigNotFound after re-login", err)
	}
	if got := stub.calls.Load(); got != 1 {
		t.Errorf("login calls = %d, want 1", got)
	}
}

type recordedAuthFailure struct{ backend, reason string }

type authFailureRecorder struct {
	mu       sync.Mutex
	failures []recordedAuthFailure
}

func (r *authFailureRecorder) PublishSecretsAuthFailure(_ context.Context, backend, reason string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failures = append(r.failures, recordedAuthFailure{backend, reason})
	return nil
}

func (r *authFailureRecorder) recorded() []recordedAuthFailure {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recordedAuthFailure(nil), r.failures...)
}

func TestVaultStore_FailedRelogin_PublishesMetric(t *testing.T) {
	t.Parallel()
	_, server := newFakeVault(t)
	stub := &loginStub{next: []loginResult{{err: errors.New("vault unreachable")}}}
	store := newReloginStore(t, server, testStaleToken, stub)
	rec := &authFailureRecorder{}
	store.metrics = rec

	_, _ = store.Get(t.Context(), reloginRunnerID)

	want := []recordedAuthFailure{{"vault", "relogin"}}
	if got := rec.recorded(); !reflect.DeepEqual(got, want) {
		t.Errorf("recorded failures = %v, want %v", got, want)
	}
}

func TestVaultStore_CheckToken_PublishesRenewFailure(t *testing.T) {
	t.Parallel()
	_, server := newFakeVault(t)
	store := newReloginStore(t, server, testStaleToken, nil)
	rec := &authFailureRecorder{}
	store.metrics = rec

	store.checkToken(t.Context())

	want := []recordedAuthFailure{{"vault", "renew"}}
	if got := rec.recorded(); !reflect.DeepEqual(got, want) {
		t.Errorf("recorded failures = %v, want %v", got, want)
	}
}

func TestVaultStore_CheckToken_HealthyTokenPublishesNothing(t *testing.T) {
	t.Parallel()
	_, server := newFakeVault(t, testStaleToken)
	store := newReloginStore(t, server, testStaleToken, nil)
	rec := &authFailureRecorder{}
	store.metrics = rec

	store.checkToken(t.Context())

	if got := rec.recorded(); len(got) != 0 {
		t.Errorf("recorded failures = %v, want none", got)
	}
}

func TestVaultStore_CheckToken_FailedReloginCountedOnce(t *testing.T) {
	t.Parallel()
	_, server := newFakeVault(t)
	stub := &loginStub{next: []loginResult{{err: errors.New("vault unreachable")}}}
	store := newReloginStore(t, server, testStaleToken, stub)
	rec := &authFailureRecorder{}
	store.metrics = rec

	store.checkToken(t.Context())

	want := []recordedAuthFailure{{"vault", "relogin"}}
	if got := rec.recorded(); !reflect.DeepEqual(got, want) {
		t.Errorf("recorded failures = %v, want %v", got, want)
	}
}
