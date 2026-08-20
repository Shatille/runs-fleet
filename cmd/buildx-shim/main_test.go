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

func capableLoader() func() buildxshim.BuildxState {
	return func() buildxshim.BuildxState {
		return buildxshim.BuildxState{Drivers: map[string]string{"multiarch": "docker-container"}}
	}
}

func mustNotLoadState(t *testing.T) func() buildxshim.BuildxState {
	return func() buildxshim.BuildxState {
		t.Error("buildx state must not be loaded for this invocation")
		return buildxshim.BuildxState{}
	}
}

func mustNotResolveConfig(t *testing.T) func() string {
	return func() string {
		t.Error("builder config must not be resolved for this invocation")
		return ""
	}
}

func TestPlan_InjectsWhenEligible(t *testing.T) {
	argv := []string{"buildx", "build", "--platform", "linux/arm64", "."}
	env := map[string]string{
		"RUNS_FLEET_BUILDKIT_CACHE_BUCKET": "b",
		"RUNS_FLEET_BUILDKIT_CACHE_REGION": "ap-northeast-1",
		"RUNS_FLEET_BUILDKIT_CACHE_PREFIX": "buildkit/o/r/",
		"BUILDX_BUILDER":                   "multiarch",
	}
	finalArgv, outcome := plan(context.Background(), argv, env, fakeFetcher{creds: fullCreds()}, capableLoader(), mustNotResolveConfig(t))

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
	finalArgv, outcome := plan(context.Background(), argv, map[string]string{}, fakeFetcher{creds: fullCreds()}, mustNotLoadState(t), mustNotResolveConfig(t))
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
	finalArgv, outcome := plan(context.Background(), argv, env, fakeFetcher{err: context.DeadlineExceeded}, capableLoader(), mustNotResolveConfig(t))
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
	// A fetcher and state loader that fail the test if called: the metadata
	// handshake is pure passthrough and touches neither IMDS nor the filesystem.
	finalArgv, outcome := plan(context.Background(), argv, env, panicFetcher{t}, mustNotLoadState(t), mustNotResolveConfig(t))
	if !reflect.DeepEqual(finalArgv, argv) {
		t.Errorf("metadata handshake must be byte-identical, got %v", finalArgv)
	}
	if outcome == outcomeEngaged {
		t.Error("metadata handshake must never engage")
	}
}

func TestPlan_CreateInjectsBuilderConfigWithoutCredsOrState(t *testing.T) {
	argv := []string{"buildx", "create", "--name", "builder-xyz", "--driver", "docker-container", "--use"}
	// Creds and builder state must never be touched on the create path: the
	// injection depends only on the baked config file.
	finalArgv, outcome := plan(context.Background(), argv, map[string]string{}, panicFetcher{t}, mustNotLoadState(t),
		func() string { return "/opt/runs-fleet/buildkitd.toml" })
	if outcome != "engaged:create" {
		t.Fatalf("outcome = %q, want engaged:create", outcome)
	}
	if !reflect.DeepEqual(finalArgv[:len(argv)], argv) {
		t.Errorf("original argv must be preserved as prefix, got %v", finalArgv)
	}
	want := append(append([]string{}, argv...), "--buildkitd-config", "/opt/runs-fleet/buildkitd.toml")
	if !reflect.DeepEqual(finalArgv, want) {
		t.Errorf("finalArgv = %v, want %v", finalArgv, want)
	}
}

func TestPlan_CreateWithoutBakedConfigIsByteIdenticalPassthrough(t *testing.T) {
	argv := []string{"buildx", "create", "--name", "builder-xyz"}
	finalArgv, outcome := plan(context.Background(), argv, map[string]string{}, panicFetcher{t}, mustNotLoadState(t),
		func() string { return "" })
	if !reflect.DeepEqual(finalArgv, argv) {
		t.Errorf("expected byte-identical argv, got %v", finalArgv)
	}
	if outcome != "skipped:no-builder-config" {
		t.Errorf("outcome = %q", outcome)
	}
}

func TestResolveBuilderConfig(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "buildkitd.toml")

	if got := resolveBuilderConfig(map[string]string{"RUNS_FLEET_BUILDKIT_BUILDER_CONFIG": cfg}); got != "" {
		t.Errorf("missing file resolved to %q, want empty", got)
	}
	if err := os.WriteFile(cfg, []byte("[registry.\"docker.io\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := resolveBuilderConfig(map[string]string{"RUNS_FLEET_BUILDKIT_BUILDER_CONFIG": cfg}); got != cfg {
		t.Errorf("resolved %q, want %q", got, cfg)
	}
}

type panicFetcher struct{ t *testing.T }

func (p panicFetcher) FetchCredentials(context.Context) (buildxshim.Credentials, error) {
	p.t.Error("creds fetch must not be called for non-build invocations")
	return buildxshim.Credentials{}, nil
}

type crashFetcher struct{}

func (crashFetcher) FetchCredentials(context.Context) (buildxshim.Credentials, error) {
	panic("injected decision-path panic")
}

func setEligibleEnv(t *testing.T) {
	t.Helper()
	t.Setenv("RUNS_FLEET_BUILDKIT_CACHE_BUCKET", "b")
	t.Setenv("RUNS_FLEET_BUILDKIT_CACHE_REGION", "ap-northeast-1")
	t.Setenv("RUNS_FLEET_BUILDKIT_CACHE_PREFIX", "buildkit/o/r/")
	t.Setenv("RUNS_FLEET_BUILD_CACHE", "")
	t.Setenv("BUILDX_BUILDER", "multiarch")
	// safePlan resolves the builder driver from real buildx state files, so
	// provision an isolated store naming multiarch as a container builder.
	confDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(confDir, "instances"), 0o755); err != nil {
		t.Fatal(err)
	}
	instance := filepath.Join(confDir, "instances", "multiarch")
	if err := os.WriteFile(instance, []byte(`{"Driver":"docker-container"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BUILDX_CONFIG", confDir)
}

func TestSafePlan_PanicRecoversToOriginalArgv(t *testing.T) {
	setEligibleEnv(t)
	argv := []string{"buildx", "build", "."}
	got := safePlan(context.Background(), argv, crashFetcher{})
	if !reflect.DeepEqual(got, argv) {
		t.Errorf("safePlan after panic = %v, want original argv %v", got, argv)
	}
}

func TestSafePlan_NormalPathInjects(t *testing.T) {
	setEligibleEnv(t)
	argv := []string{"buildx", "build", "."}
	got := safePlan(context.Background(), argv, fakeFetcher{creds: fullCreds()})
	if len(got) <= len(argv) {
		t.Fatalf("expected flags appended, got %v", got)
	}
	if !reflect.DeepEqual(got[:len(argv)], argv) {
		t.Errorf("original argv not preserved: %v", got)
	}
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
	_, outcome := plan(context.Background(), argv, env, fakeFetcher{creds: fullCreds()}, capableLoader(), mustNotResolveConfig(t))
	recordOutcome(env, outcome)

	b, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("outcome file not written: %v", err)
	}
	if string(b) != "engaged\n" {
		t.Errorf("outcome file = %q, want engaged", string(b))
	}
}

// A workflow that brings its own buildkitd config is skipped by DecideCreate,
// so the address it guessed is whatever it guessed. On this host that address
// is unreachable from a bridge-network buildkitd, and BuildKit answers an
// unreachable mirror with an unauthenticated Docker Hub pull rather than an
// error — so the config has to be redirected onto the address the proxy bound.
func TestPlan_CreateRedirectsUserConfigOntoTheBoundMirror(t *testing.T) {
	dir := t.TempDir()
	ours := filepath.Join(dir, "buildkitd.toml")
	if err := os.WriteFile(ours, []byte("[registry.\"docker.io\"]\n  mirrors = [\"172.17.0.1:8989\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	userCfg := filepath.Join(dir, "user.toml")
	if err := os.WriteFile(userCfg, []byte("debug = true\n[registry.\"docker.io\"]\n  mirrors = [\"127.0.0.1:8989\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	argv := []string{"buildx", "create", "--driver", "docker-container", "--buildkitd-config", userCfg}
	finalArgv, outcome := plan(context.Background(), argv, map[string]string{}, panicFetcher{t}, mustNotLoadState(t),
		func() string { return ours })

	if outcome != buildxshim.OutcomeEngagedUserConfig {
		t.Fatalf("outcome = %q, want %q", outcome, buildxshim.OutcomeEngagedUserConfig)
	}
	redirected := finalArgv[len(finalArgv)-1]
	if redirected == userCfg {
		t.Fatal("argv still points at the user's original config")
	}
	got, err := os.ReadFile(redirected)
	if err != nil {
		t.Fatal(err)
	}
	want := "debug = true\n[registry.\"docker.io\"]\n  mirrors = [\"172.17.0.1:8989\"]\n"
	if string(got) != want {
		t.Errorf("redirected config =\n%s\nwant\n%s", got, want)
	}
}

// Nothing to redirect must stay byte-identical: the shim never rewrites a
// config it has no reason to touch.
func TestPlan_CreateLeavesAnUnrelatedUserConfigAlone(t *testing.T) {
	dir := t.TempDir()
	ours := filepath.Join(dir, "buildkitd.toml")
	if err := os.WriteFile(ours, []byte("[registry.\"docker.io\"]\n  mirrors = [\"172.17.0.1:8989\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	userCfg := filepath.Join(dir, "user.toml")
	if err := os.WriteFile(userCfg, []byte("debug = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	argv := []string{"buildx", "create", "--buildkitd-config", userCfg}
	finalArgv, outcome := plan(context.Background(), argv, map[string]string{}, panicFetcher{t}, mustNotLoadState(t),
		func() string { return ours })

	if !reflect.DeepEqual(finalArgv, argv) {
		t.Errorf("expected byte-identical argv, got %v", finalArgv)
	}
	if outcome != buildxshim.OutcomeSkippedUserConfig {
		t.Errorf("outcome = %q, want %q", outcome, buildxshim.OutcomeSkippedUserConfig)
	}
}

// An unreadable user config must degrade to passthrough, never fail the build.
func TestPlan_CreateWithUnreadableUserConfigPassesThrough(t *testing.T) {
	dir := t.TempDir()
	ours := filepath.Join(dir, "buildkitd.toml")
	if err := os.WriteFile(ours, []byte("mirrors = [\"172.17.0.1:8989\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	argv := []string{"buildx", "create", "--buildkitd-config", filepath.Join(dir, "nope.toml")}
	finalArgv, outcome := plan(context.Background(), argv, map[string]string{}, panicFetcher{t}, mustNotLoadState(t),
		func() string { return ours })

	if !reflect.DeepEqual(finalArgv, argv) {
		t.Errorf("expected byte-identical argv, got %v", finalArgv)
	}
	if outcome != buildxshim.OutcomeSkippedUserConfig {
		t.Errorf("outcome = %q", outcome)
	}
}
