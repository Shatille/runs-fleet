package buildxshim

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// BuildxState carries file-derived buildx builder state resolved by the shim
// before calling Decide, keeping Decide itself I/O-free.
type BuildxState struct {
	// CurrentBuilder is the builder name from the buildx store's current file,
	// or "" when unset, unreadable, or recorded for a different docker
	// endpoint/context than the effective one — buildx itself ignores the
	// current file in that case, so trusting it could inject into a
	// default-driver build and break it.
	CurrentBuilder string
	// Drivers maps user-created builder instance names to their driver
	// (docker-container, kubernetes, remote), from the store's instances dir.
	// Names absent here are unknown, stale, or context-backed docker-driver
	// builders — all ineligible for cache export.
	Drivers map[string]string
}

const (
	defaultDockerEndpoint = "unix:///var/run/docker.sock"
	defaultContextName    = "default"
)

// LoadBuildxState reads the buildx store (BUILDX_CONFIG > DOCKER_CONFIG/buildx
// > HOME/.docker/buildx) best-effort. Every failure yields zero values, which
// Decide treats as "no eligible builder" (passthrough).
func LoadBuildxState(env map[string]string) BuildxState {
	confDir := buildxConfigDir(env)
	if confDir == "" {
		return BuildxState{}
	}
	return BuildxState{
		CurrentBuilder: loadCurrent(filepath.Join(confDir, "current"), env),
		Drivers:        loadDrivers(filepath.Join(confDir, "instances")),
	}
}

func buildxConfigDir(env map[string]string) string {
	if d := env["BUILDX_CONFIG"]; d != "" {
		return d
	}
	if d := env["DOCKER_CONFIG"]; d != "" {
		return filepath.Join(d, "buildx")
	}
	if h := env["HOME"]; h != "" {
		return filepath.Join(h, ".docker", "buildx")
	}
	return ""
}

// loadCurrent returns the current-builder name only when the record is for the
// effective docker endpoint and no non-default docker context is selected. Any
// mismatch or read/parse failure returns "".
func loadCurrent(path string, env map[string]string) string {
	if ctx := env["DOCKER_CONTEXT"]; ctx != "" && ctx != defaultContextName {
		return ""
	}
	if configCurrentContext(env) != "" {
		return ""
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var cur struct {
		Key  string `json:"Key"`
		Name string `json:"Name"`
	}
	if err := json.Unmarshal(b, &cur); err != nil {
		return ""
	}
	endpoint := env["DOCKER_HOST"]
	if endpoint == "" {
		endpoint = defaultDockerEndpoint
	}
	if cur.Key != endpoint {
		return ""
	}
	return cur.Name
}

// configCurrentContext returns docker config.json's currentContext when it
// selects a non-default context, "" otherwise (including on any read error).
func configCurrentContext(env map[string]string) string {
	dir := env["DOCKER_CONFIG"]
	if dir == "" {
		if h := env["HOME"]; h != "" {
			dir = filepath.Join(h, ".docker")
		}
	}
	if dir == "" {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		return ""
	}
	var cfg struct {
		CurrentContext string `json:"currentContext"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return ""
	}
	if cfg.CurrentContext == defaultContextName {
		return ""
	}
	return cfg.CurrentContext
}

func loadDrivers(instancesDir string) map[string]string {
	entries, err := os.ReadDir(instancesDir)
	if err != nil {
		return nil
	}
	drivers := make(map[string]string, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(instancesDir, e.Name()))
		if err != nil {
			continue
		}
		var ng struct {
			Driver string `json:"Driver"`
		}
		if err := json.Unmarshal(b, &ng); err != nil || ng.Driver == "" {
			continue
		}
		drivers[e.Name()] = ng.Driver
	}
	if len(drivers) == 0 {
		return nil
	}
	return drivers
}
