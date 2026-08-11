package secrets

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

// ssmStandardTierMaxChars is the hard ceiling AWS enforces on a Standard-tier
// parameter value. Exceeding it fails the PutParameter call outright, which is
// what stranded every cold-start dispatch once JIT configs began carrying a
// 2048-bit RSA private key.
const ssmStandardTierMaxChars = 4096

const testLegacyConfigPath = "/runs-fleet/runners/i-legacy/config"

// syntheticJITConfig returns a blob shaped like GitHub's encoded JIT config:
// base64 of a JSON document whose bulk is an RSA private key. Real blobs measure
// ~4100 chars, just past the Standard-tier ceiling, so the fixture is sized to
// reproduce that overflow rather than approximate it.
func syntheticJITConfig() string {
	return strings.Repeat("A", 4108)
}

// recordingSSMStore retains what Put wrote and reports the tags of every Put, so
// tests can assert on the stored layout rather than on call arguments alone.
func recordingSSMStore(prefix string) (*SSMStore, map[string]string, map[string][]string) {
	stored := map[string]string{}
	tagsByPath := map[string][]string{}

	mock := &mockSSMClient{
		putFunc: func(_ context.Context, params *ssm.PutParameterInput, _ ...func(*ssm.Options)) (*ssm.PutParameterOutput, error) {
			path := aws.ToString(params.Name)
			stored[path] = aws.ToString(params.Value)
			tags := make([]string, 0, len(params.Tags))
			for _, tag := range params.Tags {
				tags = append(tags, aws.ToString(tag.Key)+"="+aws.ToString(tag.Value))
			}
			tagsByPath[path] = tags
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

	return NewSSMStoreWithClient(mock, prefix), stored, tagsByPath
}

func configWithJIT() *RunnerConfig {
	return &RunnerConfig{
		Org:                 "Shavakan",
		Repo:                "Shavakan/rami-code-review",
		RunID:               "31454973402",
		JITConfig:           syntheticJITConfig(),
		Labels:              []string{"runs-fleet", "cpu=4"},
		RunnerName:          "runs-fleet-runner-rami-code-review-cpu4-769901-abcde",
		JobID:               "93666769901",
		CacheURL:            "https://runs-fleet-cache.s3.ap-northeast-1.amazonaws.com",
		TerminationQueueURL: "https://sqs.ap-northeast-1.amazonaws.com/759793954216/runs-fleet-termination",
		CreatedAt:           "2026-08-11T03:17:53Z",
	}
}

// SSM rejects PutParameter outright when Tags and Overwrite are both set:
// "tags and overwrite can't be used together". The mocks accept anything, so
// only this assertion stands between that constraint and a dispatch outage.
func TestSSMStore_Put_NeverCombinesTagsWithOverwrite(t *testing.T) {
	t.Parallel()

	var violations []string
	mock := &mockSSMClient{
		putFunc: func(_ context.Context, params *ssm.PutParameterInput, _ ...func(*ssm.Options)) (*ssm.PutParameterOutput, error) {
			if len(params.Tags) > 0 && aws.ToBool(params.Overwrite) {
				violations = append(violations, aws.ToString(params.Name))
			}
			return &ssm.PutParameterOutput{}, nil
		},
	}
	store := NewSSMStoreWithClient(mock, "")

	if err := store.Put(context.Background(), "i-123456", configWithJIT()); err != nil {
		t.Fatalf("Put() error = %v", err)
	}

	if len(violations) > 0 {
		t.Errorf("PutParameter sent Tags together with Overwrite for %v; SSM rejects this", violations)
	}
}

// Overwrite is what lets a runner ID be reused without an intervening Delete, so
// every parameter Put writes must set it.
func TestSSMStore_Put_SetsOverwriteOnEveryParameter(t *testing.T) {
	t.Parallel()

	missing := []string{}
	mock := &mockSSMClient{
		putFunc: func(_ context.Context, params *ssm.PutParameterInput, _ ...func(*ssm.Options)) (*ssm.PutParameterOutput, error) {
			if !aws.ToBool(params.Overwrite) {
				missing = append(missing, aws.ToString(params.Name))
			}
			return &ssm.PutParameterOutput{}, nil
		},
	}
	store := NewSSMStoreWithClient(mock, "")

	if err := store.Put(context.Background(), "i-123456", configWithJIT()); err != nil {
		t.Fatalf("Put() error = %v", err)
	}

	if len(missing) > 0 {
		t.Errorf("PutParameter omitted Overwrite for %v", missing)
	}
}

// The managed and job-id tags are what the housekeeping sweep and cost reporting
// select on, so dropping them silently would break both. Since they cannot ride
// on the Put itself, they must be applied separately.
func TestSSMStore_Put_TagsTheConfigParameter(t *testing.T) {
	t.Parallel()

	tagged := map[string][]string{}
	mock := &mockSSMClient{
		addTagsFunc: func(_ context.Context, params *ssm.AddTagsToResourceInput, _ ...func(*ssm.Options)) (*ssm.AddTagsToResourceOutput, error) {
			values := make([]string, 0, len(params.Tags))
			for _, tag := range params.Tags {
				values = append(values, aws.ToString(tag.Key)+"="+aws.ToString(tag.Value))
			}
			tagged[aws.ToString(params.ResourceId)] = values
			return &ssm.AddTagsToResourceOutput{}, nil
		},
	}
	store := NewSSMStoreWithClient(mock, "")

	config := configWithJIT()
	if err := store.Put(context.Background(), "i-123456", config); err != nil {
		t.Fatalf("Put() error = %v", err)
	}

	tags, ok := tagged["/runs-fleet/runners/i-123456/config"]
	if !ok {
		t.Fatalf("config parameter was never tagged; tagged: %v", tagged)
	}

	want := map[string]bool{
		"runs-fleet:managed=true":           false,
		"runs-fleet:job-id=" + config.JobID: false,
	}
	for _, tag := range tags {
		if _, expected := want[tag]; expected {
			want[tag] = true
		}
	}
	for tag, seen := range want {
		if !seen {
			t.Errorf("config parameter missing tag %q; got %v", tag, tags)
		}
	}
}

// Tagging is best-effort: the tags drive housekeeping and cost attribution, but a
// runner whose config is stored yet untagged still boots and runs its job. Failing
// the dispatch over them would trade a working runner for a reporting detail.
func TestSSMStore_Put_SurvivesTaggingFailure(t *testing.T) {
	t.Parallel()

	mock := &mockSSMClient{
		addTagsFunc: func(_ context.Context, _ *ssm.AddTagsToResourceInput, _ ...func(*ssm.Options)) (*ssm.AddTagsToResourceOutput, error) {
			return nil, errors.New("AccessDeniedException")
		},
	}
	store := NewSSMStoreWithClient(mock, "")

	if err := store.Put(context.Background(), "i-123456", configWithJIT()); err != nil {
		t.Errorf("Put() error = %v; tagging is best-effort and must not fail the dispatch", err)
	}
}

// A single parameter carrying the whole config exceeds the Standard-tier ceiling
// once a JIT config is present. Splitting the credential out is what keeps both
// halves on the free tier; without it the store must be billed as Advanced.
func TestSSMStore_Put_EveryParameterFitsStandardTier(t *testing.T) {
	t.Parallel()

	store, stored, _ := recordingSSMStore("")

	if err := store.Put(context.Background(), "i-123456", configWithJIT()); err != nil {
		t.Fatalf("Put() error = %v", err)
	}

	if len(stored) == 0 {
		t.Fatal("Put() wrote no parameters")
	}

	for path, value := range stored {
		if len(value) > ssmStandardTierMaxChars {
			t.Errorf("parameter %s is %d chars, exceeds Standard-tier limit of %d",
				path, len(value), ssmStandardTierMaxChars)
		}
	}
}

// The credential must never ride in the same parameter as the plaintext config:
// that is the boundary RunnerConfig documents, and it is what lets the config
// half stay small enough to remain on the free tier.
func TestSSMStore_Put_SplitsCredentialFromConfig(t *testing.T) {
	t.Parallel()

	store, stored, _ := recordingSSMStore("")
	config := configWithJIT()

	if err := store.Put(context.Background(), "i-123456", config); err != nil {
		t.Fatalf("Put() error = %v", err)
	}

	configValue, ok := stored["/runs-fleet/runners/i-123456/config"]
	if !ok {
		t.Fatalf("no config parameter written; got paths %v", pathsOf(stored))
	}
	if _, ok := stored["/runs-fleet/runners/i-123456/credential"]; !ok {
		t.Fatalf("no credential parameter written; got paths %v", pathsOf(stored))
	}

	if strings.Contains(configValue, config.JITConfig) {
		t.Error("config parameter contains the raw JIT config; credential must be stored separately")
	}
	if !strings.Contains(configValue, config.JobID) {
		t.Error("config parameter lost job_id")
	}
}

// Round-tripping is the contract that matters: whatever layout Put chooses, Get
// must reassemble the identical struct the caller handed in.
func TestSSMStore_PutGet_RoundTripsJITConfig(t *testing.T) {
	t.Parallel()

	store, _, _ := recordingSSMStore("")
	want := configWithJIT()

	if err := store.Put(context.Background(), "i-123456", want); err != nil {
		t.Fatalf("Put() error = %v", err)
	}

	got, err := store.Get(context.Background(), "i-123456")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}

	if got.JITConfig != want.JITConfig {
		t.Errorf("JITConfig round-trip failed: got %d chars, want %d chars",
			len(got.JITConfig), len(want.JITConfig))
	}
	if got.JobID != want.JobID {
		t.Errorf("JobID = %q, want %q", got.JobID, want.JobID)
	}
	if got.Repo != want.Repo {
		t.Errorf("Repo = %q, want %q", got.Repo, want.Repo)
	}
	if len(got.Labels) != len(want.Labels) {
		t.Errorf("Labels = %v, want %v", got.Labels, want.Labels)
	}
}

// A token-only config carries no oversized field, but must still round-trip
// through whatever split layout Put writes.
func TestSSMStore_PutGet_RoundTripsRegistrationToken(t *testing.T) {
	t.Parallel()

	store, _, _ := recordingSSMStore("")
	want := &RunnerConfig{
		Org:               "Shavakan",
		Repo:              "Shavakan/runs-fleet",
		RegistrationToken: "AAAAABBBBBCCCCCDDDDD",
		JobID:             "job-1",
	}

	if err := store.Put(context.Background(), "i-999", want); err != nil {
		t.Fatalf("Put() error = %v", err)
	}

	got, err := store.Get(context.Background(), "i-999")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}

	if got.RegistrationToken != want.RegistrationToken {
		t.Errorf("RegistrationToken = %q, want %q", got.RegistrationToken, want.RegistrationToken)
	}
	if got.JITConfig != "" {
		t.Errorf("JITConfig = %q, want empty", got.JITConfig)
	}
}

// Delete must remove every parameter Put wrote. A surviving credential is a live
// registration credential left in the store after its instance is gone.
func TestSSMStore_Delete_RemovesCredentialToo(t *testing.T) {
	t.Parallel()

	store, stored, _ := recordingSSMStore("")

	if err := store.Put(context.Background(), "i-123456", configWithJIT()); err != nil {
		t.Fatalf("Put() error = %v", err)
	}
	if err := store.Delete(context.Background(), "i-123456"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}

	if len(stored) != 0 {
		t.Errorf("Delete() left parameters behind: %v", pathsOf(stored))
	}
}

// Deleting a config that never had a credential parameter is the warm-pool case
// and must not surface as an error.
func TestSSMStore_Delete_ToleratesMissingCredential(t *testing.T) {
	t.Parallel()

	store, stored, _ := recordingSSMStore("")

	if err := store.Put(context.Background(), "i-777", &RunnerConfig{
		Org:               "Shavakan",
		RegistrationToken: "tok",
	}); err != nil {
		t.Fatalf("Put() error = %v", err)
	}
	delete(stored, "/runs-fleet/runners/i-777/credential")

	if err := store.Delete(context.Background(), "i-777"); err != nil {
		t.Errorf("Delete() error = %v, want nil for a missing credential", err)
	}
}

// Put writes two parameters, so a crash between them can leave a credential with
// no config. List is housekeeping's only source of runner IDs, so a credential it
// cannot see is a live registration credential stranded in the store forever.
func TestSSMStore_List_ReportsOrphanedCredential(t *testing.T) {
	t.Parallel()

	mock := &mockSSMClient{
		getParametersByPath: func(_ context.Context, _ *ssm.GetParametersByPathInput, _ ...func(*ssm.Options)) (*ssm.GetParametersByPathOutput, error) {
			return &ssm.GetParametersByPathOutput{
				Parameters: []ssmtypes.Parameter{
					{Name: aws.String("/runs-fleet/runners/i-orphan/credential")},
				},
			}, nil
		},
	}
	store := NewSSMStoreWithClient(mock, "")

	ids, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}

	if len(ids) != 1 || ids[0] != "i-orphan" {
		t.Errorf("List() = %v, want [i-orphan]; a credential without its config is invisible to housekeeping", ids)
	}
}

// Delete removes the config first, so the only reachable crash state is a lone
// credential — which List reports, letting the sweep finish the job.
func TestSSMStore_Delete_RemovesConfigBeforeCredential(t *testing.T) {
	t.Parallel()

	var order []string
	mock := &mockSSMClient{
		deleteFunc: func(_ context.Context, params *ssm.DeleteParameterInput, _ ...func(*ssm.Options)) (*ssm.DeleteParameterOutput, error) {
			order = append(order, aws.ToString(params.Name))
			return &ssm.DeleteParameterOutput{}, nil
		},
	}
	store := NewSSMStoreWithClient(mock, "")

	if err := store.Delete(context.Background(), "i-123456"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}

	if len(order) != 2 {
		t.Fatalf("Delete() issued %d deletes, want 2: %v", len(order), order)
	}
	if !strings.HasSuffix(order[0], "/config") {
		t.Errorf("Delete() removed %s first, want the config parameter", order[0])
	}
}

// List enumerates runners by their config parameter. The credential parameter
// shares the runner's path prefix, so a naive scan would report each runner
// twice and make housekeeping act on a phantom.
func TestSSMStore_List_DoesNotDoubleCountSplitParameters(t *testing.T) {
	t.Parallel()

	mock := &mockSSMClient{
		getParametersByPath: func(_ context.Context, _ *ssm.GetParametersByPathInput, _ ...func(*ssm.Options)) (*ssm.GetParametersByPathOutput, error) {
			return &ssm.GetParametersByPathOutput{
				Parameters: []ssmtypes.Parameter{
					{Name: aws.String("/runs-fleet/runners/i-111/config")},
					{Name: aws.String("/runs-fleet/runners/i-111/credential")},
					{Name: aws.String("/runs-fleet/runners/i-222/config")},
					{Name: aws.String("/runs-fleet/runners/i-222/credential")},
				},
			}, nil
		},
	}
	store := NewSSMStoreWithClient(mock, "")

	ids, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}

	if len(ids) != 2 {
		t.Fatalf("List() = %v (%d entries), want 2 unique runner IDs", ids, len(ids))
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			t.Errorf("List() returned %s more than once", id)
		}
		seen[id] = true
	}
}

// The credential parameter is a registration credential. It must be encrypted at
// rest and must never be echoed into a resource tag, where it would be readable
// by anyone with DescribeTags.
func TestSSMStore_Put_CredentialIsSecureAndUntagged(t *testing.T) {
	t.Parallel()

	stored := map[string]ssmtypes.ParameterType{}
	tagValues := map[string][]string{}

	mock := &mockSSMClient{
		putFunc: func(_ context.Context, params *ssm.PutParameterInput, _ ...func(*ssm.Options)) (*ssm.PutParameterOutput, error) {
			path := aws.ToString(params.Name)
			stored[path] = params.Type
			values := make([]string, 0, len(params.Tags))
			for _, tag := range params.Tags {
				values = append(values, aws.ToString(tag.Value))
			}
			tagValues[path] = values
			return &ssm.PutParameterOutput{}, nil
		},
	}
	store := NewSSMStoreWithClient(mock, "")
	config := configWithJIT()

	if err := store.Put(context.Background(), "i-123456", config); err != nil {
		t.Fatalf("Put() error = %v", err)
	}

	credPath := "/runs-fleet/runners/i-123456/credential"
	if stored[credPath] != ssmtypes.ParameterTypeSecureString {
		t.Errorf("credential parameter type = %v, want SecureString", stored[credPath])
	}

	for path, values := range tagValues {
		for _, value := range values {
			if strings.Contains(value, config.JITConfig) {
				t.Errorf("parameter %s tags leak the JIT config", path)
			}
		}
	}
}

// Put must not leave a stale credential from a previous occupant of the same
// runner ID: a token-only config following a JIT config would otherwise read
// back with the old JIT blob still attached and bind the runner to a dead job.
func TestSSMStore_Put_ClearsStaleCredential(t *testing.T) {
	t.Parallel()

	store, _, _ := recordingSSMStore("")

	if err := store.Put(context.Background(), "i-123456", configWithJIT()); err != nil {
		t.Fatalf("Put() with JIT error = %v", err)
	}
	if err := store.Put(context.Background(), "i-123456", &RunnerConfig{
		Org:               "Shavakan",
		RegistrationToken: "fresh-token",
	}); err != nil {
		t.Fatalf("Put() with token error = %v", err)
	}

	got, err := store.Get(context.Background(), "i-123456")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.JITConfig != "" {
		t.Errorf("JITConfig = %d chars, want empty; stale credential survived rewrite", len(got.JITConfig))
	}
	if got.RegistrationToken != "fresh-token" {
		t.Errorf("RegistrationToken = %q, want %q", got.RegistrationToken, "fresh-token")
	}
}

// A config parameter carrying neither a credential parameter nor an inline
// credential must surface as a real error rather than a runner that boots with
// nothing to register with and hangs.
func TestSSMStore_Get_MissingCredentialEntirelyIsAnError(t *testing.T) {
	t.Parallel()

	store, stored, _ := recordingSSMStore("")

	if err := store.Put(context.Background(), "i-123456", configWithJIT()); err != nil {
		t.Fatalf("Put() error = %v", err)
	}
	delete(stored, "/runs-fleet/runners/i-123456/credential")

	if _, err := store.Get(context.Background(), "i-123456"); err == nil {
		t.Error("Get() returned nil error when no credential existed in either layout")
	}
}

// The orchestrator deploys in minutes; agents turn over only as the AMI rebakes,
// so for the length of a rollout both layouts are live in production at once.
// Reading a legacy config — credential inline, no credential parameter — must
// succeed, or every instance booted from an older AMI fails to register.
func TestSSMStore_Get_ReadsLegacyInlineLayout(t *testing.T) {
	t.Parallel()

	legacy := configWithJIT()
	legacyJSON, err := json.Marshal(legacy)
	if err != nil {
		t.Fatalf("marshal legacy config: %v", err)
	}

	mock := &mockSSMClient{
		getFunc: func(_ context.Context, params *ssm.GetParameterInput, _ ...func(*ssm.Options)) (*ssm.GetParameterOutput, error) {
			if aws.ToString(params.Name) == testLegacyConfigPath {
				return &ssm.GetParameterOutput{
					Parameter: &ssmtypes.Parameter{Value: aws.String(string(legacyJSON))},
				}, nil
			}
			return nil, &ssmtypes.ParameterNotFound{}
		},
	}
	store := NewSSMStoreWithClient(mock, "")

	got, err := store.Get(context.Background(), "i-legacy")
	if err != nil {
		t.Fatalf("Get() on legacy layout error = %v", err)
	}
	if got.JITConfig != legacy.JITConfig {
		t.Errorf("legacy JITConfig lost: got %d chars, want %d",
			len(got.JITConfig), len(legacy.JITConfig))
	}
	if got.JobID != legacy.JobID {
		t.Errorf("JobID = %q, want %q", got.JobID, legacy.JobID)
	}
}

// The legacy token-only config is the warm-pool shape and must read back too.
func TestSSMStore_Get_ReadsLegacyInlineToken(t *testing.T) {
	t.Parallel()

	legacyJSON, err := json.Marshal(&RunnerConfig{
		Org:               "Shavakan",
		Repo:              "Shavakan/runs-fleet",
		RegistrationToken: "legacy-token",
		JobID:             "job-legacy",
	})
	if err != nil {
		t.Fatalf("marshal legacy config: %v", err)
	}

	mock := &mockSSMClient{
		getFunc: func(_ context.Context, params *ssm.GetParameterInput, _ ...func(*ssm.Options)) (*ssm.GetParameterOutput, error) {
			if aws.ToString(params.Name) == testLegacyConfigPath {
				return &ssm.GetParameterOutput{
					Parameter: &ssmtypes.Parameter{Value: aws.String(string(legacyJSON))},
				}, nil
			}
			return nil, &ssmtypes.ParameterNotFound{}
		},
	}
	store := NewSSMStoreWithClient(mock, "")

	got, err := store.Get(context.Background(), "i-legacy")
	if err != nil {
		t.Fatalf("Get() on legacy token layout error = %v", err)
	}
	if got.RegistrationToken != "legacy-token" {
		t.Errorf("RegistrationToken = %q, want %q", got.RegistrationToken, "legacy-token")
	}
}

// Some SSM clients surface a missing parameter as an untyped error string rather
// than the typed ParameterNotFound. The legacy fallback must trigger on both, or
// the rollout window fails against whichever shape production returns.
func TestSSMStore_Get_LegacyFallbackOnUntypedNotFound(t *testing.T) {
	t.Parallel()

	legacy := configWithJIT()
	legacyJSON, err := json.Marshal(legacy)
	if err != nil {
		t.Fatalf("marshal legacy config: %v", err)
	}

	mock := &mockSSMClient{
		getFunc: func(_ context.Context, params *ssm.GetParameterInput, _ ...func(*ssm.Options)) (*ssm.GetParameterOutput, error) {
			if aws.ToString(params.Name) == testLegacyConfigPath {
				return &ssm.GetParameterOutput{
					Parameter: &ssmtypes.Parameter{Value: aws.String(string(legacyJSON))},
				}, nil
			}
			return nil, errors.New("ParameterNotFound: parameter does not exist")
		},
	}
	store := NewSSMStoreWithClient(mock, "")

	got, err := store.Get(context.Background(), "i-legacy")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.JITConfig != legacy.JITConfig {
		t.Error("legacy fallback did not trigger on untyped ParameterNotFound")
	}
}

// The split parameter wins when both layouts are present: a stale legacy config
// left over from a previous write must not shadow the current credential.
func TestSSMStore_Get_PrefersSplitCredentialOverInline(t *testing.T) {
	t.Parallel()

	inline := configWithJIT()
	inline.JITConfig = strings.Repeat("S", 64)
	inlineJSON, err := json.Marshal(inline)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}

	current := configWithJIT()
	credential, err := packCredential(current)
	if err != nil {
		t.Fatalf("packCredential: %v", err)
	}

	mock := &mockSSMClient{
		getFunc: func(_ context.Context, params *ssm.GetParameterInput, _ ...func(*ssm.Options)) (*ssm.GetParameterOutput, error) {
			switch aws.ToString(params.Name) {
			case "/runs-fleet/runners/i-both/config":
				return &ssm.GetParameterOutput{
					Parameter: &ssmtypes.Parameter{Value: aws.String(string(inlineJSON))},
				}, nil
			case "/runs-fleet/runners/i-both/credential":
				return &ssm.GetParameterOutput{
					Parameter: &ssmtypes.Parameter{Value: aws.String(credential)},
				}, nil
			}
			return nil, &ssmtypes.ParameterNotFound{}
		},
	}
	store := NewSSMStoreWithClient(mock, "")

	got, err := store.Get(context.Background(), "i-both")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.JITConfig != current.JITConfig {
		t.Errorf("stale inline credential shadowed the split parameter: got %d chars, want %d",
			len(got.JITConfig), len(current.JITConfig))
	}
}

func pathsOf(stored map[string]string) []string {
	paths := make([]string, 0, len(stored))
	for path := range stored {
		paths = append(paths, path)
	}
	return paths
}

// Guards the marshalled config half against silently regaining the credential:
// a future field that carries it would push the config parameter back toward the
// ceiling and undo the split.
func TestSSMStore_ConfigHalfExcludesCredentialFields(t *testing.T) {
	t.Parallel()

	store, stored, _ := recordingSSMStore("")

	if err := store.Put(context.Background(), "i-123456", configWithJIT()); err != nil {
		t.Fatalf("Put() error = %v", err)
	}

	var decoded map[string]json.RawMessage
	if err := json.Unmarshal([]byte(stored["/runs-fleet/runners/i-123456/config"]), &decoded); err != nil {
		t.Fatalf("config parameter is not valid JSON: %v", err)
	}

	for _, key := range []string{"jit_config", "jit_token"} {
		if _, present := decoded[key]; present {
			t.Errorf("config parameter carries %q; credentials belong in the credential parameter", key)
		}
	}
}
