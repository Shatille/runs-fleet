package buildxshim

import (
	"reflect"
	"strings"
	"testing"
)

// creds used across injection cases.
func testCreds() Credentials {
	return Credentials{
		AccessKeyID:     "AKIA_TEST",
		SecretAccessKey: "secret_test",
		SessionToken:    "token_test",
	}
}

// enabledEnv is the baseline environment for an injection-eligible invocation:
// bucket present, opt-out unset, a non-default container builder selected.
func enabledEnv() map[string]string {
	return map[string]string{
		envCacheBucket:   "runs-fleet-cache",
		envCacheRegion:   "ap-northeast-1",
		envCachePrefix:   "buildkit/acme/widgets/",
		"BUILDX_BUILDER": "multiarch",
	}
}

func TestDecide_Passthrough(t *testing.T) {
	tests := []struct {
		name  string
		argv  []string
		env   map[string]string
		creds Credentials
	}{
		{
			name:  "metadata handshake returns nothing to inject",
			argv:  []string{"docker-cli-plugin-metadata"},
			env:   enabledEnv(),
			creds: testCreds(),
		},
		{
			name:  "non-build subcommand create",
			argv:  []string{"buildx", "create", "--name", "x"},
			env:   enabledEnv(),
			creds: testCreds(),
		},
		{
			name:  "non-build subcommand inspect",
			argv:  []string{"buildx", "inspect", "multiarch"},
			env:   enabledEnv(),
			creds: testCreds(),
		},
		{
			name:  "non-build subcommand ls",
			argv:  []string{"buildx", "ls"},
			env:   enabledEnv(),
			creds: testCreds(),
		},
		{
			name:  "non-build subcommand prune",
			argv:  []string{"buildx", "prune", "-f"},
			env:   enabledEnv(),
			creds: testCreds(),
		},
		{
			name:  "non-build subcommand version",
			argv:  []string{"buildx", "version"},
			env:   enabledEnv(),
			creds: testCreds(),
		},
		{
			name:  "non-build subcommand imagetools",
			argv:  []string{"buildx", "imagetools", "inspect", "alpine"},
			env:   enabledEnv(),
			creds: testCreds(),
		},
		{
			name:  "bake is not build",
			argv:  []string{"buildx", "bake"},
			env:   enabledEnv(),
			creds: testCreds(),
		},
		{
			name: "build with cache bucket env absent",
			argv: []string{"buildx", "build", "."},
			env: map[string]string{
				"BUILDX_BUILDER": "multiarch",
			},
			creds: testCreds(),
		},
		{
			name:  "build with existing --cache-from separate arg",
			argv:  []string{"buildx", "build", "--cache-from", "type=gha", "."},
			env:   enabledEnv(),
			creds: testCreds(),
		},
		{
			name:  "build with existing --cache-from equals form",
			argv:  []string{"buildx", "build", "--cache-from=type=gha", "."},
			env:   enabledEnv(),
			creds: testCreds(),
		},
		{
			name:  "build with existing --cache-to separate arg",
			argv:  []string{"buildx", "build", "--cache-to", "type=gha,mode=max", "."},
			env:   enabledEnv(),
			creds: testCreds(),
		},
		{
			name:  "build with existing --cache-to equals form",
			argv:  []string{"buildx", "build", "--cache-to=type=inline", "."},
			env:   enabledEnv(),
			creds: testCreds(),
		},
		{
			name: "opt-out RUNS_FLEET_BUILD_CACHE=off",
			argv: []string{"buildx", "build", "."},
			env: mergeEnv(enabledEnv(), map[string]string{
				envOptOut: "off",
			}),
			creds: testCreds(),
		},
		{
			name: "no builder in argv and BUILDX_BUILDER empty (docker driver)",
			argv: []string{"buildx", "build", "."},
			env: map[string]string{
				envCacheBucket: "runs-fleet-cache",
				envCacheRegion: "ap-northeast-1",
				envCachePrefix: "buildkit/acme/widgets/",
			},
			creds: testCreds(),
		},
		{
			name: "BUILDX_BUILDER=default (docker driver)",
			argv: []string{"buildx", "build", "."},
			env: map[string]string{
				envCacheBucket:   "runs-fleet-cache",
				envCacheRegion:   "ap-northeast-1",
				envCachePrefix:   "buildkit/acme/widgets/",
				"BUILDX_BUILDER": "default",
			},
			creds: testCreds(),
		},
		{
			name: "--builder default in argv (docker driver)",
			argv: []string{"buildx", "build", "--builder", "default", "."},
			env: map[string]string{
				envCacheBucket: "runs-fleet-cache",
				envCacheRegion: "ap-northeast-1",
				envCachePrefix: "buildkit/acme/widgets/",
			},
			creds: testCreds(),
		},
		{
			name:  "empty creds (IMDS fetch failed) suppresses injection",
			argv:  []string{"buildx", "build", "."},
			env:   enabledEnv(),
			creds: Credentials{},
		},
		{
			name:  "partial creds (no session token) suppresses injection",
			argv:  []string{"buildx", "build", "."},
			env:   enabledEnv(),
			creds: Credentials{AccessKeyID: "AKIA", SecretAccessKey: "s"},
		},
		{
			name: "cache bucket set but region missing",
			argv: []string{"buildx", "build", "."},
			env: map[string]string{
				envCacheBucket:   "runs-fleet-cache",
				envCachePrefix:   "buildkit/acme/widgets/",
				"BUILDX_BUILDER": "multiarch",
			},
			creds: testCreds(),
		},
		{
			name:  "empty argv",
			argv:  []string{},
			env:   enabledEnv(),
			creds: testCreds(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			extra, outcome := Decide(tt.argv, tt.env, tt.creds)
			if len(extra) != 0 {
				t.Errorf("expected no extra args (passthrough), got %v", extra)
			}
			if strings.HasPrefix(outcome, outcomeEngaged) {
				t.Errorf("expected non-engaged outcome, got %q", outcome)
			}
		})
	}
}

func TestDecide_InjectsCacheFlags(t *testing.T) {
	argv := []string{"buildx", "build", "--platform", "linux/arm64", "-t", "img:latest", "."}
	extra, outcome := Decide(argv, enabledEnv(), testCreds())

	if outcome != outcomeEngaged {
		t.Fatalf("outcome = %q, want %q", outcome, outcomeEngaged)
	}

	var cacheFrom, cacheTo string
	for i := 0; i < len(extra); i++ {
		switch extra[i] {
		case "--cache-from":
			cacheFrom = extra[i+1]
			i++
		case "--cache-to":
			cacheTo = extra[i+1]
			i++
		}
	}
	if cacheFrom == "" || cacheTo == "" {
		t.Fatalf("expected both --cache-from and --cache-to, got %v", extra)
	}

	for _, want := range []string{
		"type=s3",
		"region=ap-northeast-1",
		"bucket=runs-fleet-cache",
		"prefix=buildkit/acme/widgets/",
		"name=widgets-linux-arm64",
		"access_key_id=AKIA_TEST",
		"secret_access_key=secret_test",
		"session_token=token_test",
	} {
		if !strings.Contains(cacheFrom, want) {
			t.Errorf("cache-from missing %q: %s", want, cacheFrom)
		}
		if !strings.Contains(cacheTo, want) {
			t.Errorf("cache-to missing %q: %s", want, cacheTo)
		}
	}

	if strings.Contains(cacheFrom, "mode=max") {
		t.Errorf("cache-from must NOT contain mode=max: %s", cacheFrom)
	}
	if !strings.Contains(cacheTo, "mode=max") {
		t.Errorf("cache-to must contain mode=max: %s", cacheTo)
	}
}

func TestDecide_PlatformSlugFromArgvEqualsForm(t *testing.T) {
	argv := []string{"buildx", "build", "--platform=linux/amd64", "."}
	extra, outcome := Decide(argv, enabledEnv(), testCreds())
	if outcome != outcomeEngaged {
		t.Fatalf("outcome = %q, want engaged", outcome)
	}
	if !cacheAttrContains(extra, "name=widgets-linux-amd64") {
		t.Errorf("expected platform slug linux-amd64, got %v", extra)
	}
}

func TestDecide_PlatformSlugFirstOfMulti(t *testing.T) {
	argv := []string{"buildx", "build", "--platform", "linux/arm64,linux/amd64", "."}
	extra, outcome := Decide(argv, enabledEnv(), testCreds())
	if outcome != outcomeEngaged {
		t.Fatalf("outcome = %q, want engaged", outcome)
	}
	if !cacheAttrContains(extra, "name=widgets-linux-arm64") {
		t.Errorf("expected first platform slug linux-arm64, got %v", extra)
	}
}

func TestDecide_PlatformSlugFallsBackToRuntimeArch(t *testing.T) {
	argv := []string{"buildx", "build", "."}
	extra, outcome := Decide(argv, enabledEnv(), testCreds())
	if outcome != outcomeEngaged {
		t.Fatalf("outcome = %q, want engaged", outcome)
	}
	// runtimeArch is the shim's GOARCH; the name must include a non-empty slug.
	if !cacheAttrContains(extra, "name=widgets-"+runtimePlatformSlug()) {
		t.Errorf("expected runtime platform slug %q, got %v", runtimePlatformSlug(), extra)
	}
}

func TestDecide_BuilderFromArgvEnablesInjection(t *testing.T) {
	// No BUILDX_BUILDER env, but --builder names a non-default builder.
	env := map[string]string{
		envCacheBucket: "runs-fleet-cache",
		envCacheRegion: "ap-northeast-1",
		envCachePrefix: "buildkit/acme/widgets/",
	}
	argv := []string{"buildx", "build", "--builder", "multiarch", "."}
	extra, outcome := Decide(argv, env, testCreds())
	if outcome != outcomeEngaged {
		t.Fatalf("outcome = %q, want engaged", outcome)
	}
	if len(extra) == 0 {
		t.Error("expected injected flags when --builder names a non-default builder")
	}
}

func TestDecide_RepoSlugSanitizedFromPrefix(t *testing.T) {
	env := mergeEnv(enabledEnv(), map[string]string{
		envCachePrefix: "buildkit/Acme-Org/My.Weird_Repo/",
	})
	argv := []string{"buildx", "build", "--platform", "linux/arm64", "."}
	extra, outcome := Decide(argv, env, testCreds())
	if outcome != outcomeEngaged {
		t.Fatalf("outcome = %q, want engaged", outcome)
	}
	// repo part is the segment after org; sanitized to lowercase alnum + dashes.
	if !cacheAttrContains(extra, "name=my-weird-repo-linux-arm64") {
		t.Errorf("expected sanitized repo slug, got %v", extra)
	}
}

func TestDecide_BuildAliasFirstArg(t *testing.T) {
	// `docker build` aliases to the buildx plugin; docker may invoke the plugin
	// with "build" as argv[0] (no leading "buildx"). Injection must still work.
	argv := []string{"build", "--builder", "multiarch", "."}
	env := map[string]string{
		envCacheBucket: "runs-fleet-cache",
		envCacheRegion: "ap-northeast-1",
		envCachePrefix: "buildkit/acme/widgets/",
	}
	extra, outcome := Decide(argv, env, testCreds())
	if outcome != outcomeEngaged {
		t.Fatalf("outcome = %q, want engaged for build-alias argv, got extra=%v", extra, outcome)
	}
}

func TestDecide_BuildShortAlias(t *testing.T) {
	// buildx registers `b` as a short alias for `build`.
	for _, argv := range [][]string{
		{"b", "."},
		{"buildx", "b", "."},
	} {
		extra, outcome := Decide(argv, enabledEnv(), testCreds())
		if outcome != outcomeEngaged {
			t.Errorf("argv %v: outcome = %q, want engaged", argv, outcome)
		}
		if len(extra) == 0 {
			t.Errorf("argv %v: expected injected flags", argv)
		}
	}
}

func TestDecide_GlobalDockerFlagsBeforeSubcommand(t *testing.T) {
	// docker forwards its full os.Args[1:] to the plugin, so global docker
	// flags can precede the buildx/build tokens. Boolean flags and
	// value-taking flags (both separate-arg and = forms) must all be skipped.
	for _, argv := range [][]string{
		{"-D", "buildx", "build", "."},
		{"--debug", "buildx", "build", "."},
		{"--config", "/etc/docker-cfg", "buildx", "build", "."},
		{"--config=/etc/docker-cfg", "buildx", "build", "."},
		{"-H", "unix:///var/run/docker.sock", "buildx", "build", "."},
		{"--debug", "build", "."},
		{"-D", "b", "."},
	} {
		extra, outcome := Decide(argv, enabledEnv(), testCreds())
		if outcome != outcomeEngaged {
			t.Errorf("argv %v: outcome = %q, want engaged", argv, outcome)
		}
		if len(extra) == 0 {
			t.Errorf("argv %v: expected injected flags", argv)
		}
	}
}

func TestDecide_UnknownLeadingFlagFailsSafe(t *testing.T) {
	// A leading flag the shim does not recognize makes the subcommand position
	// ambiguous (the flag might consume the next token as its value). The only
	// safe reading is "not a build": under-inject, never risk injecting into a
	// non-build invocation.
	for _, argv := range [][]string{
		{"--future-flag", "buildx", "build", "."},
		{"--future-flag=x", "build", "."},
		{"-Z", "b", "."},
		{"--", "build", "."},
	} {
		extra, outcome := Decide(argv, enabledEnv(), testCreds())
		if len(extra) != 0 {
			t.Errorf("argv %v: expected passthrough, got %v", argv, extra)
		}
		if outcome == outcomeEngaged {
			t.Errorf("argv %v: must not engage on unknown leading flag", argv)
		}
	}
}

func TestDecide_KnownBoolFlagsBeforeSubcommand(t *testing.T) {
	for _, argv := range [][]string{
		{"--tlsverify", "buildx", "build", "."},
		{"--tls", "build", "."},
	} {
		_, outcome := Decide(argv, enabledEnv(), testCreds())
		if outcome != outcomeEngaged {
			t.Errorf("argv %v: outcome = %q, want engaged", argv, outcome)
		}
	}
}

func TestDecide_GlobalFlagsNonBuildStillPassthrough(t *testing.T) {
	for _, argv := range [][]string{
		{"-D", "buildx", "ls"},
		{"--config", "/etc/docker-cfg", "buildx", "inspect"},
		{"-D", "buildx", "prune", "-f"},
	} {
		extra, outcome := Decide(argv, enabledEnv(), testCreds())
		if len(extra) != 0 {
			t.Errorf("argv %v: expected passthrough, got %v", argv, extra)
		}
		if outcome == outcomeEngaged {
			t.Errorf("argv %v: must not engage", argv)
		}
	}
}

func cacheAttrContains(args []string, want string) bool {
	for _, a := range args {
		if strings.Contains(a, want) {
			return true
		}
	}
	return false
}

func mergeEnv(base, over map[string]string) map[string]string {
	out := make(map[string]string, len(base)+len(over))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range over {
		out[k] = v
	}
	return out
}

func TestDecide_ExtraArgsAreIndependentSlices(t *testing.T) {
	// Guard against the shim mutating the caller's argv; extraArgs must be a
	// fresh slice the caller appends to original argv.
	argv := []string{"buildx", "build", "."}
	extra, _ := Decide(argv, enabledEnv(), testCreds())
	if reflect.ValueOf(extra).Pointer() == reflect.ValueOf(argv).Pointer() {
		t.Error("extraArgs must not alias argv")
	}
}
