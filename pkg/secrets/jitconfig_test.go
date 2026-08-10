package secrets

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

// statefulSSMStore returns a store whose backing mock actually retains what Put
// wrote, so a Put->Get round trip exercises the real serialization path. It also
// records the tags of the last Put for the credential-hygiene assertion.
func statefulSSMStore(prefix string) (*SSMStore, *[]string) {
	stored := map[string]string{}
	lastTags := &[]string{}

	mock := &mockSSMClient{
		putFunc: func(_ context.Context, params *ssm.PutParameterInput, _ ...func(*ssm.Options)) (*ssm.PutParameterOutput, error) {
			stored[aws.ToString(params.Name)] = aws.ToString(params.Value)
			tags := make([]string, 0, len(params.Tags))
			for _, tag := range params.Tags {
				tags = append(tags, aws.ToString(tag.Key)+"="+aws.ToString(tag.Value))
			}
			*lastTags = tags
			return &ssm.PutParameterOutput{}, nil
		},
		getFunc: func(_ context.Context, params *ssm.GetParameterInput, _ ...func(*ssm.Options)) (*ssm.GetParameterOutput, error) {
			value, ok := stored[aws.ToString(params.Name)]
			if !ok {
				return nil, errors.New("ParameterNotFound")
			}
			return &ssm.GetParameterOutput{
				Parameter: &ssmtypes.Parameter{Value: aws.String(value)},
			}, nil
		},
	}

	return NewSSMStoreWithClient(mock, prefix), lastTags
}

// envVarForField maps a RunnerConfig field to the environment variable EnvStore
// reads it from. Kept as data (not derived) so a field whose env name does not
// follow the mechanical convention is stated explicitly rather than guessed.
var envVarForField = map[string]string{
	"Org":                 "RUNS_FLEET_ORG",
	"Repo":                "RUNS_FLEET_REPO",
	"RunID":               "RUNS_FLEET_RUN_ID",
	"JITToken":            "RUNS_FLEET_JIT_TOKEN",
	"JITConfig":           "RUNS_FLEET_JIT_CONFIG",
	"Labels":              "RUNS_FLEET_LABELS",
	"RunnerGroup":         "RUNS_FLEET_RUNNER_GROUP",
	"RunnerName":          "RUNS_FLEET_RUNNER_NAME",
	"JobID":               "RUNS_FLEET_JOB_ID",
	"CacheToken":          "RUNS_FLEET_CACHE_TOKEN",
	"CacheURL":            "RUNS_FLEET_CACHE_URL",
	"TerminationQueueURL": "RUNS_FLEET_TERMINATION_QUEUE_URL",
	"IsOrg":               "RUNS_FLEET_IS_ORG",
	"BuildkitCacheBucket": "RUNS_FLEET_BUILDKIT_CACHE_BUCKET",
	"BuildkitCacheRegion": "RUNS_FLEET_BUILDKIT_CACHE_REGION",
	"BuildkitCachePrefix": "RUNS_FLEET_BUILDKIT_CACHE_PREFIX",
}

// fieldsEnvStoreCannotCarry are RunnerConfig fields EnvStore intentionally does
// not source from the environment. CreatedAt is stamped by the orchestrator when
// it writes a config to a real backend; the env backend has no writer, so there
// is nothing to stamp.
var fieldsEnvStoreCannotCarry = map[string]bool{
	"CreatedAt": true,
}

// TestEnvStore_CarriesEveryRunnerConfigField is the drift guard for the one
// backend that enumerates fields by hand. SSMStore.Put and VaultStore.Put both
// marshal the whole struct, so a new field round-trips there automatically —
// EnvStore does not, and would silently drop it (leaving the agent to behave as
// though the feature were disabled).
//
// Reflection over every exported field means a future field is caught here
// without editing this test: it fails as "no env var mapping", forcing a
// deliberate choice between wiring it up or listing it as intentionally absent.
func TestEnvStore_CarriesEveryRunnerConfigField(t *testing.T) {
	typ := reflect.TypeOf(RunnerConfig{})

	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		if fieldsEnvStoreCannotCarry[name] {
			continue
		}

		t.Run(name, func(t *testing.T) {
			envVar, ok := envVarForField[name]
			if !ok {
				t.Fatalf("RunnerConfig.%s has no env var mapping: either wire it into "+
					"EnvStore.Get and add it to envVarForField, or declare it in "+
					"fieldsEnvStoreCannotCarry with a reason", name)
			}

			// EnvStore.Get refuses to build a config without an org and without a
			// registration credential, so both preconditions are satisfied for
			// every case; the field under test then overwrites its own var.
			t.Setenv("RUNS_FLEET_ORG", "sentinel-Org")
			t.Setenv("RUNS_FLEET_JIT_TOKEN", "sentinel-JITToken")

			field := reflect.ValueOf(RunnerConfig{}).Field(i)
			var want string
			switch field.Kind() {
			case reflect.String:
				want = "sentinel-" + name
			case reflect.Bool:
				want = "true"
			case reflect.Slice:
				want = "sentinel-" + name
			default:
				t.Fatalf("unhandled field kind %s for %s (extend this test)", field.Kind(), name)
			}
			t.Setenv(envVar, want)

			got, err := NewEnvStore().Get(t.Context(), "i-ignored")
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}

			gotField := reflect.ValueOf(got).Elem().Field(i)
			switch gotField.Kind() {
			case reflect.String:
				if gotField.String() != want {
					t.Errorf("RunnerConfig.%s = %q, want %q (env %s dropped)",
						name, gotField.String(), want, envVar)
				}
			case reflect.Bool:
				if !gotField.Bool() {
					t.Errorf("RunnerConfig.%s = false, want true (env %s dropped)", name, envVar)
				}
			case reflect.Slice:
				if gotField.Len() == 0 {
					t.Errorf("RunnerConfig.%s is empty, want %q (env %s dropped)", name, want, envVar)
				}
			}
		})
	}
}

// TestSSMStore_PutGet_RoundTripParity mirrors the Vault drift guard for SSM.
// SSM marshals the whole struct today, so this pins that property rather than
// fixing a bug: a future hand-rolled field map would fail here.
func TestSSMStore_PutGet_RoundTripParity(t *testing.T) {
	for _, bools := range []bool{true, false} {
		name := "bools=false"
		if bools {
			name = "bools=true"
		}
		t.Run(name, func(t *testing.T) {
			const runnerID = "i-123"

			store, _ := statefulSSMStore("/runs-fleet/runners")

			want := fullyPopulatedRunnerConfig(t, bools)
			if err := store.Put(t.Context(), runnerID, want); err != nil {
				t.Fatalf("Put() error = %v", err)
			}

			got, err := store.Get(t.Context(), runnerID)
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}

			if !reflect.DeepEqual(got, want) {
				t.Errorf("round-trip mismatch:\n got = %+v\nwant = %+v", got, want)
			}

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

// The JIT config registers a runner, so it must never appear in a resource tag —
// tags are readable by anything with ec2/ssm describe rights.
func TestSSMStore_Put_DoesNotTagTheJITConfig(t *testing.T) {
	store, lastTags := statefulSSMStore("/runs-fleet/runners")

	cfg := &RunnerConfig{
		Org:       "myorg",
		JobID:     "123",
		JITConfig: "SUPERSECRETJITBLOB",
	}
	if err := store.Put(t.Context(), "i-123", cfg); err != nil {
		t.Fatalf("Put() error = %v", err)
	}

	for _, tag := range *lastTags {
		if strings.Contains(tag, "SUPERSECRETJITBLOB") {
			t.Errorf("tag %q leaks the JIT config", tag)
		}
	}
}
