package secrets

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
	"github.com/hashicorp/vault/api"
)

// The two backends deliberately store different layouts: SSM splits the
// credential into its own parameter to stay under a 4096-character cap, while
// Vault has no such cap and keeps the config flat. Both are deployed — SSM in the
// upstream account, Vault on forked environments — and one agent binary reads
// whichever it is pointed at. These tests hold both to the same Store contract so
// the layouts cannot drift into shapes only one backend's agent can read.

// backendFactory builds a Store backed by an in-memory fake of its real API.
// Cleanup is registered on t, so each subtest gets an isolated backend.
type backendFactory struct {
	name string
	new  func(t *testing.T) Store
}

func allBackends() []backendFactory {
	return []backendFactory{
		{"SSM", newFakeSSMStore},
		{"Vault/KVv1", func(t *testing.T) Store { return newFakeVaultStore(t, 1) }},
		{"Vault/KVv2", func(t *testing.T) Store { return newFakeVaultStore(t, 2) }},
	}
}

func newFakeSSMStore(t *testing.T) Store {
	t.Helper()

	stored := map[string]string{}
	mock := &mockSSMClient{
		putFunc: func(_ context.Context, params *ssm.PutParameterInput, _ ...func(*ssm.Options)) (*ssm.PutParameterOutput, error) {
			// The real API rejects this pairing; a fake that accepts it lets a
			// dispatch-wide outage pass as a green test.
			if len(params.Tags) > 0 && aws.ToBool(params.Overwrite) {
				return nil, &ssmtypes.ParameterLimitExceeded{}
			}
			stored[aws.ToString(params.Name)] = aws.ToString(params.Value)
			return &ssm.PutParameterOutput{}, nil
		},
		getFunc: func(_ context.Context, params *ssm.GetParameterInput, _ ...func(*ssm.Options)) (*ssm.GetParameterOutput, error) {
			value, ok := stored[aws.ToString(params.Name)]
			if !ok {
				return nil, &ssmtypes.ParameterNotFound{}
			}
			return &ssm.GetParameterOutput{
				Parameter: &ssmtypes.Parameter{Value: aws.String(value)},
			}, nil
		},
		deleteFunc: func(_ context.Context, params *ssm.DeleteParameterInput, _ ...func(*ssm.Options)) (*ssm.DeleteParameterOutput, error) {
			path := aws.ToString(params.Name)
			if _, ok := stored[path]; !ok {
				return nil, &ssmtypes.ParameterNotFound{}
			}
			delete(stored, path)
			return &ssm.DeleteParameterOutput{}, nil
		},
	}

	return NewSSMStoreWithClient(mock, "")
}

// newFakeVaultStore serves one runner's secret from memory across every path the
// store may address, so Put/Get/Delete exercise the real client rather than a
// single hard-coded route.
func newFakeVaultStore(t *testing.T, kvVersion int) Store {
	t.Helper()

	stored := map[string]map[string]interface{}{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := runnerIDFromVaultPath(r.URL.Path)
		if key == "" {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		switch r.Method {
		case http.MethodPut, http.MethodPost:
			var body map[string]interface{}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode Vault put body: %v", err)
			}
			if kvVersion == 2 {
				data, ok := body["data"].(map[string]interface{})
				if !ok {
					t.Errorf("KVv2 put body missing data wrapper: %v", body)
				}
				stored[key] = data
			} else {
				stored[key] = body
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data": {}}`))

		case http.MethodGet:
			data, ok := stored[key]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			response := map[string]interface{}{"data": data}
			if kvVersion == 2 {
				response = map[string]interface{}{"data": map[string]interface{}{"data": data}}
			}
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(response)

		case http.MethodDelete:
			delete(stored, key)
			w.WriteHeader(http.StatusNoContent)

		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(server.Close)

	client, err := api.NewClient(&api.Config{Address: server.URL})
	if err != nil {
		t.Fatalf("failed to create Vault client: %v", err)
	}
	client.SetToken("test-token")

	return NewVaultStoreWithClient(client, "secret", "runs-fleet/runners", kvVersion)
}

// runnerIDFromVaultPath reduces every KV path shape (v1, v2 data, v2 metadata) to
// the runner ID, so one in-memory map serves them all.
func runnerIDFromVaultPath(path string) string {
	idx := strings.LastIndex(path, "/runners/")
	if idx == -1 {
		return ""
	}
	return strings.TrimPrefix(path[idx+len("/runners/"):], "/")
}

// A JIT config carries a 2048-bit RSA key and measures ~4100 characters — past
// the cap that broke SSM. Whatever layout a backend chooses, the credential the
// agent reads back must be the one the orchestrator minted, byte for byte.
func TestBackendParity_RoundTripsOversizedCredential(t *testing.T) {
	t.Parallel()

	for _, backend := range allBackends() {
		t.Run(backend.name, func(t *testing.T) {
			t.Parallel()

			store := backend.new(t)
			want := configWithJIT()

			if err := store.Put(t.Context(), "i-123456", want); err != nil {
				t.Fatalf("Put() error = %v", err)
			}

			got, err := store.Get(t.Context(), "i-123456")
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}

			if got.JITConfig != want.JITConfig {
				t.Errorf("JITConfig round-trip failed: got %d chars, want %d",
					len(got.JITConfig), len(want.JITConfig))
			}
			if got.RunnerName != want.RunnerName {
				t.Errorf("RunnerName = %q, want %q", got.RunnerName, want.RunnerName)
			}
			if got.JobID != want.JobID {
				t.Errorf("JobID = %q, want %q", got.JobID, want.JobID)
			}
			if got.Repo != want.Repo {
				t.Errorf("Repo = %q, want %q", got.Repo, want.Repo)
			}
		})
	}
}

// The warm-pool shape: a registration token and no JIT config. A backend that
// conflates "absent credential" with "empty string" would register a runner with
// an empty token and fail at config.sh instead of here.
func TestBackendParity_RoundTripsRegistrationToken(t *testing.T) {
	t.Parallel()

	for _, backend := range allBackends() {
		t.Run(backend.name, func(t *testing.T) {
			t.Parallel()

			store := backend.new(t)
			want := &RunnerConfig{
				Org:               "Shavakan",
				Repo:              "Shavakan/runs-fleet",
				RegistrationToken: "AAAAABBBBBCCCCCDDDDD",
				JobID:             "job-1",
				Labels:            []string{"runs-fleet", "cpu=4"},
			}

			if err := store.Put(t.Context(), "i-123456", want); err != nil {
				t.Fatalf("Put() error = %v", err)
			}

			got, err := store.Get(t.Context(), "i-123456")
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}

			if got.RegistrationToken != want.RegistrationToken {
				t.Errorf("RegistrationToken = %q, want %q", got.RegistrationToken, want.RegistrationToken)
			}
			if got.JITConfig != "" {
				t.Errorf("JITConfig = %q, want empty", got.JITConfig)
			}
		})
	}
}

// Every exported field must survive every backend. Driven by reflection so a
// field added later is covered without editing this test — the failure mode being
// guarded against is a backend that silently drops a field the agent depends on.
func TestBackendParity_CarriesEveryField(t *testing.T) {
	t.Parallel()

	for _, backend := range allBackends() {
		t.Run(backend.name, func(t *testing.T) {
			t.Parallel()

			store := backend.new(t)
			want := fullyPopulatedRunnerConfig(t, true)

			if err := store.Put(t.Context(), "i-123456", want); err != nil {
				t.Fatalf("Put() error = %v", err)
			}

			got, err := store.Get(t.Context(), "i-123456")
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}

			assertRunnerConfigsEqual(t, got, want)
		})
	}
}

// Zero values are the case a naive omitempty breaks: a field that vanishes from
// the payload reads back as the type's zero, which for IsOrg silently flips
// org-level registration off.
func TestBackendParity_CarriesZeroValuedFields(t *testing.T) {
	t.Parallel()

	for _, backend := range allBackends() {
		t.Run(backend.name, func(t *testing.T) {
			t.Parallel()

			store := backend.new(t)
			want := fullyPopulatedRunnerConfig(t, false)

			if err := store.Put(t.Context(), "i-123456", want); err != nil {
				t.Fatalf("Put() error = %v", err)
			}

			got, err := store.Get(t.Context(), "i-123456")
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}

			assertRunnerConfigsEqual(t, got, want)
		})
	}
}

// Delete must remove everything Put wrote. On SSM that spans two parameters, so a
// backend that forgets one leaves a live registration credential behind after the
// instance it was minted for is gone.
func TestBackendParity_DeleteRemovesTheConfig(t *testing.T) {
	t.Parallel()

	for _, backend := range allBackends() {
		t.Run(backend.name, func(t *testing.T) {
			t.Parallel()

			store := backend.new(t)

			if err := store.Put(t.Context(), "i-123456", configWithJIT()); err != nil {
				t.Fatalf("Put() error = %v", err)
			}
			if err := store.Delete(t.Context(), "i-123456"); err != nil {
				t.Fatalf("Delete() error = %v", err)
			}

			if _, err := store.Get(t.Context(), "i-123456"); err == nil {
				t.Error("Get() succeeded after Delete; the config outlived its runner")
			}
		})
	}
}

// Reusing a runner ID without an intervening Delete is what pool reassignment
// does. A backend that refuses the second write, or that leaves the previous
// occupant's credential in place, binds the runner to a job that is already gone.
func TestBackendParity_OverwritesAPreviousOccupant(t *testing.T) {
	t.Parallel()

	for _, backend := range allBackends() {
		t.Run(backend.name, func(t *testing.T) {
			t.Parallel()

			store := backend.new(t)

			if err := store.Put(t.Context(), "i-123456", configWithJIT()); err != nil {
				t.Fatalf("first Put() error = %v", err)
			}

			second := &RunnerConfig{
				Org:               "Shavakan",
				Repo:              "Shavakan/runs-fleet",
				RegistrationToken: "second-occupant-token",
				JobID:             "job-2",
			}
			if err := store.Put(t.Context(), "i-123456", second); err != nil {
				t.Fatalf("second Put() error = %v", err)
			}

			got, err := store.Get(t.Context(), "i-123456")
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}

			if got.JITConfig != "" {
				t.Errorf("JITConfig = %d chars, want empty; the previous occupant's credential survived",
					len(got.JITConfig))
			}
			if got.RegistrationToken != second.RegistrationToken {
				t.Errorf("RegistrationToken = %q, want %q", got.RegistrationToken, second.RegistrationToken)
			}
			if got.JobID != second.JobID {
				t.Errorf("JobID = %q, want %q", got.JobID, second.JobID)
			}
		})
	}
}

// A runner ID that was never written must report ErrConfigNotFound rather than an
// opaque failure: callers branch on it to tell a not-yet-assigned warm-pool
// instance from a backend that is actually broken.
func TestBackendParity_MissingConfigIsNotFound(t *testing.T) {
	t.Parallel()

	for _, backend := range allBackends() {
		t.Run(backend.name, func(t *testing.T) {
			t.Parallel()

			store := backend.new(t)

			_, err := store.Get(t.Context(), "i-never-written")
			if err == nil {
				t.Fatal("Get() on a missing config returned nil error")
			}
			if !errors.Is(err, ErrConfigNotFound) {
				t.Errorf("Get() error = %v, want one wrapping ErrConfigNotFound", err)
			}
		})
	}
}

// Deleting a config that is not there is how the cleanup paths converge when a
// sweep and a termination handler both fire; neither should surface an error.
func TestBackendParity_DeleteIsIdempotent(t *testing.T) {
	t.Parallel()

	for _, backend := range allBackends() {
		t.Run(backend.name, func(t *testing.T) {
			t.Parallel()

			store := backend.new(t)

			if err := store.Delete(t.Context(), "i-never-written"); err != nil {
				t.Errorf("Delete() on a missing config error = %v, want nil", err)
			}

			if err := store.Put(t.Context(), "i-123456", configWithJIT()); err != nil {
				t.Fatalf("Put() error = %v", err)
			}
			if err := store.Delete(t.Context(), "i-123456"); err != nil {
				t.Fatalf("first Delete() error = %v", err)
			}
			if err := store.Delete(t.Context(), "i-123456"); err != nil {
				t.Errorf("second Delete() error = %v, want nil", err)
			}
		})
	}
}

// The credential is base64 and the config JSON travels as a string through both
// backends. A byte-exact comparison is what catches an encoder that normalises
// padding or re-wraps the value.
func TestBackendParity_PreservesCredentialBytes(t *testing.T) {
	t.Parallel()

	for _, backend := range allBackends() {
		t.Run(backend.name, func(t *testing.T) {
			t.Parallel()

			store := backend.new(t)
			want := configWithJIT()
			want.JITConfig = strings.Repeat("aA1+/=", 700)

			if err := store.Put(t.Context(), "i-123456", want); err != nil {
				t.Fatalf("Put() error = %v", err)
			}

			got, err := store.Get(t.Context(), "i-123456")
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}

			if got.JITConfig != want.JITConfig {
				t.Errorf("credential altered in transit: got %d chars, want %d",
					len(got.JITConfig), len(want.JITConfig))
			}
		})
	}
}

func assertRunnerConfigsEqual(t *testing.T, got, want *RunnerConfig) {
	t.Helper()

	gotJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal got: %v", err)
	}
	wantJSON, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal want: %v", err)
	}

	if string(gotJSON) != string(wantJSON) {
		t.Errorf("round trip changed the config\n got: %s\nwant: %s", gotJSON, wantJSON)
	}
}
