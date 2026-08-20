package buildxshim

import "testing"

const (
	userConfigPath   = "/tmp/u.toml"
	mergedConfigPath = "/tmp/merged.toml"
)

func TestMirrorAddrFromConfig(t *testing.T) {
	got := MirrorAddrFromConfig(`[registry."docker.io"]
  mirrors = ["172.17.0.1:8989"]
[registry."172.17.0.1:8989"]
  http = true
`)
	if got != "172.17.0.1:8989" {
		t.Errorf("MirrorAddrFromConfig() = %q, want %q", got, "172.17.0.1:8989")
	}
}

func TestMirrorAddrFromConfigReturnsEmptyWhenAbsent(t *testing.T) {
	for _, in := range []string{"", "debug = true\n", `[registry."docker.io"]` + "\n"} {
		if got := MirrorAddrFromConfig(in); got != "" {
			t.Errorf("MirrorAddrFromConfig(%q) = %q, want empty", in, got)
		}
	}
}

// The case this exists for: devsisters/docker-setup-buildx-action writes the
// host's primary-ENI address, which the mirror does not bind. BuildKit answers
// a refused mirror by pulling from Docker Hub, so the build silently loses the
// pull-through cache and eventually dies on a Hub 429.
func TestRewriteMirrorHostsRedirectsTheEniAddressToTheBridge(t *testing.T) {
	userConfig := `debug = true

[registry."docker.io"]
  mirrors = ["172.21.138.36:8989"]

[registry."172.21.138.36:8989"]
  http = true
`
	local := map[string]bool{"172.21.138.36": true, "127.0.0.1": true}

	got, n := RewriteMirrorHosts(userConfig, "172.17.0.1:8989", local)
	if n != 2 {
		t.Errorf("rewrote %d addresses, want 2 (the mirrors entry and its registry table)", n)
	}
	want := `debug = true

[registry."docker.io"]
  mirrors = ["172.17.0.1:8989"]

[registry."172.17.0.1:8989"]
  http = true
`
	if got != want {
		t.Errorf("RewriteMirrorHosts() =\n%s\nwant\n%s", got, want)
	}
}

// A loopback mirror in a hand-written config is the same bug: dockerd can
// reach it but a bridge-network buildkitd cannot.
func TestRewriteMirrorHostsRedirectsLoopback(t *testing.T) {
	got, n := RewriteMirrorHosts(`mirrors = ["127.0.0.1:8989"]`, "172.17.0.1:8989", map[string]bool{"127.0.0.1": true})
	if n != 1 || got != `mirrors = ["172.17.0.1:8989"]` {
		t.Errorf("RewriteMirrorHosts() = %q (n=%d), want the bridge address", got, n)
	}
}

// Only host-local addresses are redirected. A registry that genuinely lives
// elsewhere on the same port is somebody else's service, not our mirror.
func TestRewriteMirrorHostsLeavesRemoteHostsAlone(t *testing.T) {
	in := `mirrors = ["registry.example.com:8989"]
[registry."10.9.9.9:8989"]
  http = true
`
	got, n := RewriteMirrorHosts(in, "172.17.0.1:8989", map[string]bool{"172.21.138.36": true, "127.0.0.1": true})
	if n != 0 || got != in {
		t.Errorf("RewriteMirrorHosts() rewrote a remote host: %q (n=%d)", got, n)
	}
}

// A different port is a different service; only our mirror's port is ours to
// redirect.
func TestRewriteMirrorHostsLeavesOtherPortsAlone(t *testing.T) {
	in := `mirrors = ["127.0.0.1:5000"]`
	got, n := RewriteMirrorHosts(in, "172.17.0.1:8989", map[string]bool{"127.0.0.1": true})
	if n != 0 || got != in {
		t.Errorf("RewriteMirrorHosts() rewrote a different port: %q (n=%d)", got, n)
	}
}

// Already correct, so nothing to do — and reporting 0 lets the caller leave
// argv untouched rather than writing a pointless temp file.
func TestRewriteMirrorHostsIsANoOpWhenAlreadyTheBridge(t *testing.T) {
	in := `mirrors = ["172.17.0.1:8989"]`
	got, n := RewriteMirrorHosts(in, "172.17.0.1:8989", map[string]bool{"172.17.0.1": true})
	if n != 0 || got != in {
		t.Errorf("RewriteMirrorHosts() = %q (n=%d), want an unchanged no-op", got, n)
	}
}

func TestRewriteMirrorHostsHandlesAnEmptyMirrorAddr(t *testing.T) {
	in := `mirrors = ["127.0.0.1:8989"]`
	got, n := RewriteMirrorHosts(in, "", map[string]bool{"127.0.0.1": true})
	if n != 0 || got != in {
		t.Errorf("RewriteMirrorHosts() with no mirror addr = %q (n=%d), want unchanged", got, n)
	}
}

func TestUserConfigFlagFindsSeparateValue(t *testing.T) {
	flag, value, inline, ok := UserConfigFlag([]string{"create", "--driver", "docker-container", "--buildkitd-config", userConfigPath})
	if !ok || flag != "--buildkitd-config" || value != userConfigPath || inline {
		t.Errorf("UserConfigFlag() = (%q,%q,%v,%v)", flag, value, inline, ok)
	}
}

func TestUserConfigFlagFindsEqualsForm(t *testing.T) {
	flag, value, inline, ok := UserConfigFlag([]string{"create", "--buildkitd-config=/tmp/u.toml"})
	if !ok || flag != "--buildkitd-config" || value != userConfigPath || inline {
		t.Errorf("UserConfigFlag() = (%q,%q,%v,%v)", flag, value, inline, ok)
	}
}

func TestUserConfigFlagReportsInlineVariants(t *testing.T) {
	_, value, inline, ok := UserConfigFlag([]string{"create", "--buildkitd-config-inline", "debug = true\n"})
	if !ok || !inline || value != "debug = true\n" {
		t.Errorf("UserConfigFlag() = (%q,%v,%v), want the inline TOML", value, inline, ok)
	}
}

func TestUserConfigFlagAbsent(t *testing.T) {
	if _, _, _, ok := UserConfigFlag([]string{"create", "--driver", "docker-container"}); ok {
		t.Error("UserConfigFlag() ok = true, want false when no config flag is present")
	}
}

func TestReplaceFlagValueRewritesBothForms(t *testing.T) {
	got := ReplaceFlagValue([]string{"buildx", "create", "--buildkitd-config", userConfigPath, "--use"}, "--buildkitd-config", mergedConfigPath)
	want := []string{"buildx", "create", "--buildkitd-config", mergedConfigPath, "--use"}
	assertArgv(t, got, want)

	got = ReplaceFlagValue([]string{"buildx", "create", "--buildkitd-config=/tmp/u.toml"}, "--buildkitd-config", mergedConfigPath)
	want = []string{"buildx", "create", "--buildkitd-config=/tmp/merged.toml"}
	assertArgv(t, got, want)
}

// The shim must never mutate the slice its caller still holds.
func TestReplaceFlagValueDoesNotMutateInput(t *testing.T) {
	in := []string{"buildx", "create", "--buildkitd-config", userConfigPath}
	_ = ReplaceFlagValue(in, "--buildkitd-config", mergedConfigPath)
	if in[3] != userConfigPath {
		t.Errorf("input argv mutated: %v", in)
	}
}

func assertArgv(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("argv = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("argv[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
