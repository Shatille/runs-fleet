package buildxshim

import (
	"reflect"
	"testing"
)

const wantEngagedCreate = "engaged:create"

func TestIsCreate(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		want bool
	}{
		{"plain create", []string{"buildx", "create", "--name", "x"}, true},
		{"create with leading global flag", []string{"--debug", "buildx", "create"}, true},
		{"build", []string{"buildx", "build", "."}, false},
		{"inspect", []string{"buildx", "inspect"}, false},
		{"metadata handshake", []string{"docker-cli-plugin-metadata"}, false},
		{"empty", nil, false},
		{"unknown leading flag stays ambiguous", []string{"--mystery", "create"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsCreate(tc.argv); got != tc.want {
				t.Errorf("IsCreate(%v) = %v, want %v", tc.argv, got, tc.want)
			}
		})
	}
}

func TestDecideCreate_InjectsBuilderConfig(t *testing.T) {
	argv := []string{"buildx", "create", "--name", "builder-abc", "--driver", "docker-container", "--bootstrap", "--use"}
	extra, outcome := DecideCreate(argv, "/opt/runs-fleet/buildkitd.toml")

	if outcome != wantEngagedCreate {
		t.Fatalf("outcome = %q, want engaged:create", outcome)
	}
	want := []string{"--buildkitd-config", "/opt/runs-fleet/buildkitd.toml"}
	if !reflect.DeepEqual(extra, want) {
		t.Errorf("extra = %v, want %v", extra, want)
	}
}

func TestDecideCreate_InjectsWhenDriverOmitted(t *testing.T) {
	// buildx create defaults to the docker-container driver, so an absent
	// --driver flag is injection-eligible.
	extra, outcome := DecideCreate([]string{"buildx", "create", "--name", "x"}, "/cfg/buildkitd.toml")
	if outcome != wantEngagedCreate || len(extra) != 2 {
		t.Errorf("extra = %v outcome = %q", extra, outcome)
	}
}

func TestDecideCreate_SkipsWithoutBakedConfig(t *testing.T) {
	extra, outcome := DecideCreate([]string{"buildx", "create", "--name", "x"}, "")
	if outcome != "skipped:no-builder-config" {
		t.Errorf("outcome = %q", outcome)
	}
	if extra != nil {
		t.Errorf("extra = %v, want nil", extra)
	}
}

func TestDecideCreate_SkipsWhenUserSuppliedConfig(t *testing.T) {
	cases := []struct {
		name string
		argv []string
	}{
		{"buildkitd-config value form", []string{"buildx", "create", "--buildkitd-config", "/mine.toml"}},
		{"buildkitd-config inline-eq form", []string{"buildx", "create", "--buildkitd-config=/mine.toml"}},
		{"buildkitd-config-inline", []string{"buildx", "create", "--buildkitd-config-inline", "debug = true"}},
		{"deprecated config alias", []string{"buildx", "create", "--config", "/mine.toml"}},
		{"deprecated config alias eq", []string{"buildx", "create", "--config=/mine.toml"}},
		{"deprecated config-inline alias", []string{"buildx", "create", "--config-inline=debug = true"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			extra, outcome := DecideCreate(tc.argv, "/cfg/buildkitd.toml")
			if outcome != "skipped:user-config" {
				t.Errorf("outcome = %q, want skipped:user-config", outcome)
			}
			if extra != nil {
				t.Errorf("extra = %v, want nil", extra)
			}
		})
	}
}

func TestDecideCreate_DockerGlobalConfigFlagIsNotUserConfig(t *testing.T) {
	// docker's pre-subcommand --config names the CLI config DIRECTORY — a
	// different flag that must not suppress injection.
	argv := []string{"--config", "/home/u/.docker", "buildx", "create", "--name", "x"}
	extra, outcome := DecideCreate(argv, "/cfg/buildkitd.toml")
	if outcome != wantEngagedCreate {
		t.Errorf("outcome = %q, want engaged:create", outcome)
	}
	if len(extra) != 2 {
		t.Errorf("extra = %v", extra)
	}
}

func TestDecideCreate_SkipsNonContainerDrivers(t *testing.T) {
	for _, argv := range [][]string{
		{"buildx", "create", "--driver", "remote", "tcp://host:1234"},
		{"buildx", "create", "--driver=kubernetes"},
		{"buildx", "create", "--driver", "docker"},
	} {
		extra, outcome := DecideCreate(argv, "/cfg/buildkitd.toml")
		if outcome != "skipped:driver" {
			t.Errorf("argv %v: outcome = %q, want skipped:driver", argv, outcome)
		}
		if extra != nil {
			t.Errorf("argv %v: extra = %v, want nil", argv, extra)
		}
	}
}

func TestDecideCreate_ExplicitContainerDriverInjects(t *testing.T) {
	extra, outcome := DecideCreate([]string{"buildx", "create", "--driver=docker-container"}, "/cfg/buildkitd.toml")
	if outcome != wantEngagedCreate || len(extra) != 2 {
		t.Errorf("extra = %v outcome = %q", extra, outcome)
	}
}
