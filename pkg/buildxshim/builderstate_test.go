package buildxshim

import (
	"os"
	"path/filepath"
	"testing"
)

func writeState(t *testing.T, confDir, currentJSON string, instances map[string]string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(confDir, "instances"), 0o755); err != nil {
		t.Fatal(err)
	}
	if currentJSON != "" {
		if err := os.WriteFile(filepath.Join(confDir, "current"), []byte(currentJSON), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for name, content := range instances {
		if err := os.WriteFile(filepath.Join(confDir, "instances", name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLoadBuildxState_CurrentAndDrivers(t *testing.T) {
	home := t.TempDir()
	confDir := filepath.Join(home, ".docker", "buildx")
	writeState(t, confDir,
		`{"Key":"unix:///var/run/docker.sock","Name":"builder-abc","Global":false}`,
		map[string]string{
			"builder-abc": `{"Name":"builder-abc","Driver":"docker-container"}`,
			"k8s-builder": `{"Name":"k8s-builder","Driver":"kubernetes"}`,
		})

	state := LoadBuildxState(map[string]string{"HOME": home})
	if state.CurrentBuilder != "builder-abc" {
		t.Errorf("CurrentBuilder = %q, want builder-abc", state.CurrentBuilder)
	}
	if state.Drivers["builder-abc"] != driverDockerContainer {
		t.Errorf("Drivers[builder-abc] = %q, want docker-container", state.Drivers["builder-abc"])
	}
	if state.Drivers["k8s-builder"] != "kubernetes" {
		t.Errorf("Drivers[k8s-builder] = %q, want kubernetes", state.Drivers["k8s-builder"])
	}
}

func TestLoadBuildxState_BuildxConfigPrecedesDockerConfig(t *testing.T) {
	buildxDir := t.TempDir()
	dockerCfg := t.TempDir()
	writeState(t, buildxDir, `{"Key":"unix:///var/run/docker.sock","Name":"from-buildx-config"}`,
		map[string]string{"from-buildx-config": `{"Driver":"docker-container"}`})
	writeState(t, filepath.Join(dockerCfg, "buildx"), `{"Key":"unix:///var/run/docker.sock","Name":"from-docker-config"}`,
		map[string]string{"from-docker-config": `{"Driver":"docker-container"}`})

	state := LoadBuildxState(map[string]string{
		"BUILDX_CONFIG": buildxDir,
		"DOCKER_CONFIG": dockerCfg,
	})
	if state.CurrentBuilder != "from-buildx-config" {
		t.Errorf("CurrentBuilder = %q, want from-buildx-config", state.CurrentBuilder)
	}
}

func TestLoadBuildxState_DockerConfigPrecedesHome(t *testing.T) {
	dockerCfg := t.TempDir()
	home := t.TempDir()
	writeState(t, filepath.Join(dockerCfg, "buildx"), `{"Key":"unix:///var/run/docker.sock","Name":"from-docker-config"}`,
		map[string]string{"from-docker-config": `{"Driver":"docker-container"}`})
	writeState(t, filepath.Join(home, ".docker", "buildx"), `{"Key":"unix:///var/run/docker.sock","Name":"from-home"}`,
		map[string]string{"from-home": `{"Driver":"docker-container"}`})

	state := LoadBuildxState(map[string]string{
		"DOCKER_CONFIG": dockerCfg,
		"HOME":          home,
	})
	if state.CurrentBuilder != "from-docker-config" {
		t.Errorf("CurrentBuilder = %q, want from-docker-config", state.CurrentBuilder)
	}
}

func TestLoadBuildxState_CurrentUntrustedOnEndpointMismatch(t *testing.T) {
	// The current file records the docker endpoint it was set on. If the
	// effective endpoint differs (DOCKER_HOST points elsewhere), buildx ignores
	// the current file — so must we, or we could inject into a default-driver
	// build and break it.
	home := t.TempDir()
	confDir := filepath.Join(home, ".docker", "buildx")
	writeState(t, confDir, `{"Key":"unix:///var/run/docker.sock","Name":"builder-abc"}`,
		map[string]string{"builder-abc": `{"Driver":"docker-container"}`})

	state := LoadBuildxState(map[string]string{
		"HOME":        home,
		"DOCKER_HOST": "tcp://10.0.0.5:2376",
	})
	if state.CurrentBuilder != "" {
		t.Errorf("CurrentBuilder = %q, want empty on endpoint mismatch", state.CurrentBuilder)
	}
	// The drivers map is endpoint-independent (named builders are global).
	if state.Drivers["builder-abc"] != driverDockerContainer {
		t.Errorf("Drivers must still load on endpoint mismatch")
	}
}

func TestLoadBuildxState_CurrentUntrustedOnNonDefaultContext(t *testing.T) {
	// A non-default docker context (DOCKER_CONTEXT env or config.json
	// currentContext) changes the effective endpoint in ways we don't resolve;
	// fail toward not trusting the current file.
	home := t.TempDir()
	confDir := filepath.Join(home, ".docker", "buildx")
	writeState(t, confDir, `{"Key":"unix:///var/run/docker.sock","Name":"builder-abc"}`,
		map[string]string{"builder-abc": `{"Driver":"docker-container"}`})

	state := LoadBuildxState(map[string]string{
		"HOME":           home,
		"DOCKER_CONTEXT": "remote-ctx",
	})
	if state.CurrentBuilder != "" {
		t.Errorf("CurrentBuilder = %q, want empty under DOCKER_CONTEXT", state.CurrentBuilder)
	}

	if err := os.WriteFile(filepath.Join(home, ".docker", "config.json"),
		[]byte(`{"currentContext":"remote-ctx"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	state = LoadBuildxState(map[string]string{"HOME": home})
	if state.CurrentBuilder != "" {
		t.Errorf("CurrentBuilder = %q, want empty under config.json currentContext", state.CurrentBuilder)
	}
}

func TestLoadBuildxState_MissingOrMalformedIsZero(t *testing.T) {
	state := LoadBuildxState(map[string]string{"HOME": t.TempDir()})
	if state.CurrentBuilder != "" || len(state.Drivers) != 0 {
		t.Errorf("expected zero state for missing dirs, got %+v", state)
	}

	home := t.TempDir()
	confDir := filepath.Join(home, ".docker", "buildx")
	writeState(t, confDir, `{not json`, map[string]string{"bad": `{not json`})
	state = LoadBuildxState(map[string]string{"HOME": home})
	if state.CurrentBuilder != "" {
		t.Errorf("CurrentBuilder = %q, want empty for malformed current", state.CurrentBuilder)
	}
	if _, ok := state.Drivers["bad"]; ok {
		t.Error("malformed instance file must not produce a driver entry")
	}
}

func TestLoadBuildxState_NoHomeIsZero(t *testing.T) {
	state := LoadBuildxState(map[string]string{})
	if state.CurrentBuilder != "" || len(state.Drivers) != 0 {
		t.Errorf("expected zero state with no config env, got %+v", state)
	}
}
