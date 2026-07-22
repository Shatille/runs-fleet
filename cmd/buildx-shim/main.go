// Command buildx-shim shadows the docker-buildx CLI plugin on runs-fleet
// runners to add transparent S3 layer caching. It ALWAYS execs the real buildx
// plugin; the only thing it ever does first is optionally append cache flags to
// argv. Every uncertainty, error, or ambiguous invocation is a pure passthrough
// with the original argv, so the shim can never break a build.
package main

import (
	"context"
	"os"
	"syscall"

	"github.com/Shavakan/runs-fleet/pkg/buildxshim"
)

func main() {
	// argv[0] is the plugin binary path; the CLI-plugin protocol passes the
	// subcommand and flags as os.Args[1:].
	pluginArgs := os.Args[1:]
	finalArgs := safePlan(context.Background(), pluginArgs, buildxshim.NewIMDSClient())

	realPlugin := buildxshim.DiscoverRealPlugin(
		firstNonEmpty(os.Getenv("RUNS_FLEET_BUILDKIT_REAL_PLUGIN"), buildxshim.DefaultRealPathFile),
		buildxshim.DefaultPluginSearch,
	)
	if realPlugin == "" {
		// Nothing found: exec the canonical packaged path so the failure text is
		// docker's own ("exec format" / "no such file"), never shim-invented.
		realPlugin = buildxshim.DefaultPluginSearch[0]
	}

	execArgv := append([]string{realPlugin}, finalArgs...)
	// Replace this process with the real plugin. On success this never returns.
	if err := syscall.Exec(realPlugin, execArgv, os.Environ()); err != nil {
		// Mirror docker's own "plugin not found" behavior as closely as we can
		// without inventing new error semantics: exit non-zero, message to stderr.
		_, _ = os.Stderr.WriteString("docker-buildx: " + err.Error() + "\n")
		os.Exit(1)
	}
}

// safePlan runs the whole decision path under a last-resort recover for the
// never-break-a-build invariant: if anything in it panics, the real plugin is
// still exec'd with the ORIGINAL argv (pure passthrough), so a shim bug
// degrades to unmodified behavior rather than a failed build.
func safePlan(ctx context.Context, argv []string, fetcher buildxshim.CredsFetcher) (finalArgs []string) {
	finalArgs = argv
	defer func() {
		if r := recover(); r != nil {
			_, _ = os.Stderr.WriteString("docker-buildx shim: recovered, passing through\n")
			finalArgs = argv
		}
	}()
	env := environ()
	loadState := func() buildxshim.BuildxState { return buildxshim.LoadBuildxState(env) }
	args, outcome := plan(ctx, argv, env, fetcher, loadState)
	recordOutcome(env, outcome)
	return args
}

// plan decides the final argv to exec and the outcome for telemetry. It fetches
// credentials only when the invocation is otherwise injection-eligible, so
// non-build invocations (and the metadata handshake) never touch IMDS; Decide
// likewise consults loadState only past its cheap gates.
func plan(ctx context.Context, argv []string, env map[string]string, fetcher buildxshim.CredsFetcher, loadState func() buildxshim.BuildxState) (finalArgv []string, outcome string) {
	// Fast passthrough decision without creds first: Decide will tell us
	// not-build / disabled / opt-out / user-flags / no-cache-builder before we
	// ever reach IMDS. We pass empty creds; if Decide would otherwise engage it
	// returns failed:no-creds, which is our signal to fetch.
	_, pre := buildxshim.Decide(argv, env, buildxshim.Credentials{}, loadState)
	if pre != buildxshim.OutcomeNeedsCreds {
		// Terminal decision that needs no creds (passthrough of every kind).
		return argv, pre
	}

	creds, err := fetcher.FetchCredentials(ctx)
	if err != nil {
		return argv, "failed:imds"
	}
	extra, outcome := buildxshim.Decide(argv, env, creds, loadState)
	if len(extra) == 0 {
		return argv, outcome
	}
	return append(append([]string{}, argv...), extra...), outcome
}

// recordOutcome appends the outcome to the file named by the agent-injected env
// var. Best-effort; never fails the build.
func recordOutcome(env map[string]string, outcome string) {
	buildxshim.WriteOutcome(env[buildxshim.EnvOutcomeFile], outcome)
}

func environ() map[string]string {
	out := make(map[string]string)
	for _, kv := range os.Environ() {
		for i := 0; i < len(kv); i++ {
			if kv[i] == '=' {
				out[kv[:i]] = kv[i+1:]
				break
			}
		}
	}
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
