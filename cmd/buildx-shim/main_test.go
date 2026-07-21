package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Shavakan/runs-fleet/pkg/buildxshim"
)

const outcomeEngaged = "engaged"

type fakeFetcher struct {
	creds buildxshim.Credentials
	err   error
}

func (f fakeFetcher) FetchCredentials(context.Context) (buildxshim.Credentials, error) {
	return f.creds, f.err
}

func fullCreds() buildxshim.Credentials {
	return buildxshim.Credentials{AccessKeyID: "AKIA", SecretAccessKey: "sec", SessionToken: "tok"}
}

func TestPlan_InjectsWhenEligible(t *testing.T) {
	argv := []string{"buildx", "build", "--platform", "linux/arm64", "."}
	env := map[string]string{
		"RUNS_FLEET_BUILDKIT_CACHE_BUCKET": "b",
		"RUNS_FLEET_BUILDKIT_CACHE_REGION": "ap-northeast-1",
		"RUNS_FLEET_BUILDKIT_CACHE_PREFIX": "buildkit/o/r/",
		"BUILDX_BUILDER":                   "multiarch",
	}
	finalArgv, outcome := plan(context.Background(), argv, env, fakeFetcher{creds: fullCreds()})

	if outcome != outcomeEngaged {
		t.Fatalf("outcome = %q, want engaged", outcome)
	}
	// The plugin name argv[0] must be preserved; injected flags go after argv.
	if !reflect.DeepEqual(finalArgv[:len(argv)], argv) {
		t.Errorf("original argv not preserved: %v", finalArgv)
	}
	if len(finalArgv) <= len(argv) {
		t.Errorf("expected flags appended, got %v", finalArgv)
	}
}

func TestPlan_PassthroughWhenBucketAbsent(t *testing.T) {
	argv := []string{"buildx", "build", "."}
	finalArgv, outcome := plan(context.Background(), argv, map[string]string{}, fakeFetcher{creds: fullCreds()})
	if !reflect.DeepEqual(finalArgv, argv) {
		t.Errorf("expected byte-identical argv, got %v", finalArgv)
	}
	if outcome == outcomeEngaged {
		t.Error("must not engage without bucket env")
	}
}

func TestPlan_PassthroughOnCredsFailure(t *testing.T) {
	argv := []string{"buildx", "build", "."}
	env := map[string]string{
		"RUNS_FLEET_BUILDKIT_CACHE_BUCKET": "b",
		"RUNS_FLEET_BUILDKIT_CACHE_REGION": "ap-northeast-1",
		"RUNS_FLEET_BUILDKIT_CACHE_PREFIX": "buildkit/o/r/",
		"BUILDX_BUILDER":                   "multiarch",
	}
	finalArgv, outcome := plan(context.Background(), argv, env, fakeFetcher{err: context.DeadlineExceeded})
	if !reflect.DeepEqual(finalArgv, argv) {
		t.Errorf("expected passthrough argv on creds failure, got %v", finalArgv)
	}
	if outcome == outcomeEngaged {
		t.Error("must not engage when creds fetch fails")
	}
}

func TestPlan_MetadataHandshakeNeverFetchesCreds(t *testing.T) {
	argv := []string{"docker-cli-plugin-metadata"}
	env := map[string]string{
		"RUNS_FLEET_BUILDKIT_CACHE_BUCKET": "b",
		"RUNS_FLEET_BUILDKIT_CACHE_REGION": "ap-northeast-1",
		"RUNS_FLEET_BUILDKIT_CACHE_PREFIX": "buildkit/o/r/",
		"BUILDX_BUILDER":                   "multiarch",
	}
	// A fetcher that fails the test if called: metadata handshake is pure passthrough.
	finalArgv, outcome := plan(context.Background(), argv, env, panicFetcher{t})
	if !reflect.DeepEqual(finalArgv, argv) {
		t.Errorf("metadata handshake must be byte-identical, got %v", finalArgv)
	}
	if outcome == outcomeEngaged {
		t.Error("metadata handshake must never engage")
	}
}

type panicFetcher struct{ t *testing.T }

func (p panicFetcher) FetchCredentials(context.Context) (buildxshim.Credentials, error) {
	p.t.Error("creds fetch must not be called for non-build invocations")
	return buildxshim.Credentials{}, nil
}

func TestPlan_WritesOutcomeToEnvFile(t *testing.T) {
	dir := t.TempDir()
	outFile := filepath.Join(dir, "_rf_buildkit_cache_outcome")
	argv := []string{"buildx", "build", "--builder", "multiarch", "."}
	env := map[string]string{
		"RUNS_FLEET_BUILDKIT_CACHE_BUCKET":  "b",
		"RUNS_FLEET_BUILDKIT_CACHE_REGION":  "ap-northeast-1",
		"RUNS_FLEET_BUILDKIT_CACHE_PREFIX":  "buildkit/o/r/",
		"RUNS_FLEET_BUILDKIT_CACHE_OUTCOME": outFile,
	}
	_, outcome := plan(context.Background(), argv, env, fakeFetcher{creds: fullCreds()})
	recordOutcome(env, outcome)

	b, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("outcome file not written: %v", err)
	}
	if string(b) != "engaged\n" {
		t.Errorf("outcome file = %q, want engaged", string(b))
	}
}
