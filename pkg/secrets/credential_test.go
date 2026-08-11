package secrets

import (
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

// realisticJITConfig returns a blob with the same structure as GitHub's: base64
// of a JSON document whose bulk is high-entropy key material. The all-'A' fixture
// used elsewhere compresses far better than a real RSA key would, so pack/unpack
// assertions that depend on compression behaviour use this instead.
func realisticJITConfig(t *testing.T) string {
	t.Helper()

	key := make([]byte, 1024)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}

	inner, err := json.Marshal(map[string]string{
		".credentials_rsaparams": base64.StdEncoding.EncodeToString(key),
		".runner":                base64.StdEncoding.EncodeToString([]byte(`{"AgentId":"13771","AgentName":"runner"}`)),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	return base64.StdEncoding.EncodeToString(inner)
}

func TestPackCredential_RoundTripsRealisticJITConfig(t *testing.T) {
	t.Parallel()

	want := realisticJITConfig(t)

	packed, err := packCredential(&RunnerConfig{JITConfig: want})
	if err != nil {
		t.Fatalf("packCredential: %v", err)
	}

	var got RunnerConfig
	if err := unpackCredential(packed, &got); err != nil {
		t.Fatalf("unpackCredential: %v", err)
	}

	if got.JITConfig != want {
		t.Errorf("JIT config not restored: got %d chars, want %d", len(got.JITConfig), len(want))
	}
}

// A credential that is not valid base64 is stored verbatim rather than failing:
// GitHub's encoding is not a contract this package controls, and a credential
// that cannot be stored means a job that never runs.
func TestPackCredential_StoresNonBase64Verbatim(t *testing.T) {
	t.Parallel()

	want := "not!valid!base64!@#$%"

	packed, err := packCredential(&RunnerConfig{JITConfig: want})
	if err != nil {
		t.Fatalf("packCredential: %v", err)
	}

	var stored storedCredential
	if err := json.Unmarshal([]byte(packed), &stored); err != nil {
		t.Fatalf("unmarshal packed credential: %v", err)
	}
	if stored.Compressed {
		t.Error("non-base64 credential was marked compressed")
	}

	var got RunnerConfig
	if err := unpackCredential(packed, &got); err != nil {
		t.Fatalf("unpackCredential: %v", err)
	}
	if got.JITConfig != want {
		t.Errorf("JITConfig = %q, want %q", got.JITConfig, want)
	}
}

// Incompressible input must not grow the stored value. Random bytes gzip larger
// than their source, so the raw form has to win.
func TestPackCredential_SkipsCompressionWhenItDoesNotShrink(t *testing.T) {
	t.Parallel()

	noise := make([]byte, 64)
	if _, err := rand.Read(noise); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	want := base64.StdEncoding.EncodeToString(noise)

	packed, err := packCredential(&RunnerConfig{JITConfig: want})
	if err != nil {
		t.Fatalf("packCredential: %v", err)
	}

	var stored storedCredential
	if err := json.Unmarshal([]byte(packed), &stored); err != nil {
		t.Fatalf("unmarshal packed credential: %v", err)
	}
	if stored.Compressed {
		t.Error("compression was used despite not shrinking the value")
	}
	if len(stored.JITConfig) > len(want) {
		t.Errorf("stored credential grew: %d chars from %d", len(stored.JITConfig), len(want))
	}

	var got RunnerConfig
	if err := unpackCredential(packed, &got); err != nil {
		t.Fatalf("unpackCredential: %v", err)
	}
	if got.JITConfig != want {
		t.Errorf("JITConfig = %q, want %q", got.JITConfig, want)
	}
}

func TestPackCredential_RoundTripsTokenOnly(t *testing.T) {
	t.Parallel()

	packed, err := packCredential(&RunnerConfig{RegistrationToken: "AAAABBBBCCCC"})
	if err != nil {
		t.Fatalf("packCredential: %v", err)
	}

	var got RunnerConfig
	if err := unpackCredential(packed, &got); err != nil {
		t.Fatalf("unpackCredential: %v", err)
	}
	if got.RegistrationToken != "AAAABBBBCCCC" {
		t.Errorf("RegistrationToken = %q, want %q", got.RegistrationToken, "AAAABBBBCCCC")
	}
	if got.JITConfig != "" {
		t.Errorf("JITConfig = %q, want empty", got.JITConfig)
	}
}

// An empty credential round-trips to empty rather than to a spurious value; the
// caller distinguishes "no credential" from "some credential" by this field.
func TestPackCredential_RoundTripsEmpty(t *testing.T) {
	t.Parallel()

	packed, err := packCredential(&RunnerConfig{})
	if err != nil {
		t.Fatalf("packCredential: %v", err)
	}

	got := RunnerConfig{JITConfig: "stale", RegistrationToken: "stale"}
	if err := unpackCredential(packed, &got); err != nil {
		t.Fatalf("unpackCredential: %v", err)
	}
	if got.JITConfig != "" || got.RegistrationToken != "" {
		t.Errorf("stale credential survived: JITConfig=%q RegistrationToken=%q",
			got.JITConfig, got.RegistrationToken)
	}
}

// A rejected credential must leave the config untouched rather than half-applied:
// a config carrying a new token but a stale JIT config would register a runner
// against the wrong job.
func TestUnpackCredential_DoesNotPartiallyMutateOnError(t *testing.T) {
	t.Parallel()

	config := RunnerConfig{
		JITConfig:         "existing-jit",
		RegistrationToken: "existing-token",
	}

	payload := `{"jit_token":"replacement-token","jit_config":"!!!not base64!!!","compressed":true}`
	if err := unpackCredential(payload, &config); err == nil {
		t.Fatal("unpackCredential() accepted a malformed payload")
	}

	if config.RegistrationToken != "existing-token" {
		t.Errorf("RegistrationToken = %q, want it untouched at %q",
			config.RegistrationToken, "existing-token")
	}
	if config.JITConfig != "existing-jit" {
		t.Errorf("JITConfig = %q, want it untouched at %q", config.JITConfig, "existing-jit")
	}
}

func TestUnpackCredential_RejectsMalformedPayload(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		value string
	}{
		{"not json", "{not json"},
		{"compressed but not base64", `{"jit_config":"!!!not base64!!!","compressed":true}`},
		{"compressed but not gzip", `{"jit_config":"aGVsbG8gd29ybGQ=","compressed":true}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var config RunnerConfig
			if err := unpackCredential(tt.value, &config); err == nil {
				t.Error("unpackCredential() returned nil error for a malformed payload")
			}
		})
	}
}

// A gzip stream can inflate arbitrarily, and this value is read from a parameter
// store the agent trusts. An oversized credential must fail loudly: truncating it
// silently would hand the agent a mangled JIT config to register with, turning a
// detectable corruption into a runner that fails somewhere further downstream.
func TestUnpackCredential_RejectsOversizedCredential(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	writer := gzip.NewWriter(&buf)
	if _, err := writer.Write(bytes.Repeat([]byte("A"), credentialMaxDecodedBytes*4)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	payload, err := json.Marshal(storedCredential{
		JITConfig:  base64.StdEncoding.EncodeToString(buf.Bytes()),
		Compressed: true,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	config := RunnerConfig{}
	if unpackErr := unpackCredential(string(payload), &config); unpackErr == nil {
		t.Error("unpackCredential() accepted a credential exceeding the decompression bound")
	}
	if config.JITConfig != "" {
		t.Errorf("JITConfig = %d chars, want empty on a rejected credential", len(config.JITConfig))
	}
}

// A credential at the bound is legitimate and must still round-trip: the check
// rejects what exceeds the cap, not what reaches it.
func TestUnpackCredential_AcceptsCredentialAtTheBound(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	writer := gzip.NewWriter(&buf)
	if _, err := writer.Write(bytes.Repeat([]byte("A"), credentialMaxDecodedBytes)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	payload, err := json.Marshal(storedCredential{
		JITConfig:  base64.StdEncoding.EncodeToString(buf.Bytes()),
		Compressed: true,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var config RunnerConfig
	if unpackErr := unpackCredential(string(payload), &config); unpackErr != nil {
		t.Fatalf("unpackCredential rejected a credential at the bound: %v", unpackErr)
	}

	decoded, err := base64.StdEncoding.DecodeString(config.JITConfig)
	if err != nil {
		t.Fatalf("decode restored credential: %v", err)
	}
	if len(decoded) != credentialMaxDecodedBytes {
		t.Errorf("restored %d bytes, want %d", len(decoded), credentialMaxDecodedBytes)
	}
}

// The config half must never carry credential material, whatever the credential
// is. Asserted on the marshalled bytes so a future field that smuggles the
// credential into the plaintext parameter fails here.
func TestMarshalConfigHalf_OmitsCredentialsAndKeepsEverythingElse(t *testing.T) {
	t.Parallel()

	config := &RunnerConfig{
		Org:                 "Shavakan",
		Repo:                "Shavakan/runs-fleet",
		RunID:               "31454973402",
		RegistrationToken:   "secret-token",
		JITConfig:           realisticJITConfig(t),
		Labels:              []string{"runs-fleet", "cpu=4"},
		JobID:               "93666769901",
		CacheToken:          "cache-token",
		IsOrg:               true,
		BuildkitCachePrefix: "prefix",
	}

	encoded, err := marshalConfigHalf(config)
	if err != nil {
		t.Fatalf("marshalConfigHalf: %v", err)
	}

	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("config half is not valid JSON: %v", err)
	}

	for _, key := range []string{"jit_token", "jit_config"} {
		if _, present := decoded[key]; present {
			t.Errorf("config half carries %q", key)
		}
	}
	if strings.Contains(string(encoded), config.JITConfig) {
		t.Error("config half contains the JIT config verbatim")
	}
	if strings.Contains(string(encoded), config.RegistrationToken) {
		t.Error("config half contains the registration token verbatim")
	}

	for _, key := range []string{"org", "repo", "run_id", "labels", "job_id", "cache_token", "is_org"} {
		if _, present := decoded[key]; !present {
			t.Errorf("config half lost %q", key)
		}
	}
}

// marshalConfigHalf must not mutate the config it is handed: the caller still
// needs the credential fields to build the credential parameter.
func TestMarshalConfigHalf_DoesNotMutateInput(t *testing.T) {
	t.Parallel()

	config := &RunnerConfig{
		JITConfig:         "jit",
		RegistrationToken: "token",
	}

	if _, err := marshalConfigHalf(config); err != nil {
		t.Fatalf("marshalConfigHalf: %v", err)
	}

	if config.JITConfig != "jit" || config.RegistrationToken != "token" {
		t.Errorf("marshalConfigHalf mutated its input: JITConfig=%q RegistrationToken=%q",
			config.JITConfig, config.RegistrationToken)
	}
}
