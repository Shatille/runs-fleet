package buildxshim

import "strings"

// DefaultBuilderConfigFile is where the runner AMI bakes the BuildKit mirror
// configuration when the deployment declared an ECR pull-through cache
// endpoint. Its presence is the create-path injection's activation signal,
// the same file-gated pattern as DefaultRealPathFile.
const DefaultBuilderConfigFile = "/opt/runs-fleet/buildkitd.toml"

// EnvBuilderConfigFile overrides the baked config path, mirroring
// RUNS_FLEET_BUILDKIT_REAL_PLUGIN's escape-hatch role.
const EnvBuilderConfigFile = "RUNS_FLEET_BUILDKIT_BUILDER_CONFIG"

// createConfigFlags mark a user-supplied buildkitd configuration on `buildx
// create`; --config/--config-inline are the deprecated aliases buildx still
// accepts. Distinct from docker's global pre-subcommand --config (the CLI
// config directory), which splitSubcommand consumes before these are scanned.
var createConfigFlags = map[string]bool{
	"--buildkitd-config":        true,
	"--buildkitd-config-inline": true,
	"--config":                  true,
	"--config-inline":           true,
}

// IsCreate reports whether argv is a `buildx create` invocation. Ambiguous
// argv reads as not-create — under-inject, never inject on a guess.
func IsCreate(argv []string) bool {
	if len(argv) == 0 || argv[0] == "docker-cli-plugin-metadata" {
		return false
	}
	return subcommand(argv) == "create"
}

// DecideCreate returns the flags to append to a `buildx create` invocation so
// the new builder picks up the baked BuildKit mirror configuration, or nil
// with a skip outcome. builderConfigPath is the resolved config file ("" when
// none is baked); resolution stays with the caller so this remains I/O-free
// like Decide. Skips when the deployment never opted in, when the user
// supplied their own config, or when --driver names anything but
// docker-container (an absent --driver means docker-container, create's
// default).
func DecideCreate(argv []string, builderConfigPath string) (extraArgs []string, outcome string) {
	if builderConfigPath == "" {
		return nil, outcomeSkipped + ":no-builder-config"
	}
	rest := afterSubcommand(argv)
	if hasCreateConfigFlag(rest) {
		return nil, OutcomeSkippedUserConfig
	}
	if driver, ok := driverFromArgs(rest); ok && driver != driverDockerContainer {
		return nil, outcomeSkipped + ":driver"
	}
	return []string{"--buildkitd-config", builderConfigPath}, outcomeEngaged + ":create"
}

func afterSubcommand(argv []string) []string {
	_, rest := splitSubcommand(argv)
	return rest
}

func hasCreateConfigFlag(args []string) bool {
	for _, a := range args {
		name, _, _ := cutFlag(a)
		if createConfigFlags[name] {
			return true
		}
	}
	return false
}

func cutFlag(token string) (name, value string, inline bool) {
	return strings.Cut(token, "=")
}

func driverFromArgs(args []string) (string, bool) {
	for i := 0; i < len(args); i++ {
		name, value, inlineValue := cutFlag(args[i])
		if name != "--driver" {
			continue
		}
		if inlineValue {
			return value, true
		}
		if i+1 < len(args) {
			return args[i+1], true
		}
	}
	return "", false
}
