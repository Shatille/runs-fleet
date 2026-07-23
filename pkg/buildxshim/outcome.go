package buildxshim

import "os"

// EnvOutcomeFile names the env var the agent sets to tell the shim where to
// append its outcome. The shim reads it from its own environment (inherited
// from the runner .env), so it never has to guess the runner path.
const EnvOutcomeFile = "RUNS_FLEET_BUILDKIT_CACHE_OUTCOME"

// WriteOutcome appends a single outcome line to path. It is entirely
// best-effort: an empty path or any write error is swallowed so the shim can
// never fail a build over telemetry.
func WriteOutcome(path, outcome string) {
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	_, _ = f.WriteString(outcome + "\n")
}
