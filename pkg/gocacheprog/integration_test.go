package gocacheprog_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Shavakan/runs-fleet/pkg/gocacheprog"
)

const helloMain = `package main

import "fmt"

func main() { fmt.Println("hello") }
`

func TestIntegration_RealGoBuildHitsOnSecondRun(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the helper and runs the real toolchain twice")
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go not on PATH")
	}

	root := t.TempDir()
	helper := filepath.Join(root, "runs-fleet-gocacheprog")
	build := exec.Command(goBin, "build", "-o", helper, "github.com/Shavakan/runs-fleet/cmd/gocacheprog")
	build.Env = hostEnv()
	if out, berr := build.CombinedOutput(); berr != nil {
		t.Fatalf("build helper: %v\n%s", berr, out)
	}

	mod := filepath.Join(root, "hello")
	if merr := os.MkdirAll(mod, 0o755); merr != nil {
		t.Fatal(merr)
	}
	if werr := os.WriteFile(filepath.Join(mod, "go.mod"), []byte("module hello\n\ngo 1.26\n"), 0o644); werr != nil {
		t.Fatal(werr)
	}
	if werr := os.WriteFile(filepath.Join(mod, "main.go"), []byte(helloMain), 0o644); werr != nil {
		t.Fatal(werr)
	}

	outcome := filepath.Join(root, "outcome")
	env := append(hostEnv(),
		"GOCACHE="+filepath.Join(root, "gocache"),
		"GOCACHEPROG="+helper,
		"GOTOOLCHAIN=local",
		"GOPROXY=off",
		"GOSUMDB=off",
		"GOFLAGS=",
		gocacheprog.EnvDir+"="+filepath.Join(root, "l1"),
		gocacheprog.EnvOutcome+"="+outcome,
	)
	for run := 1; run <= 2; run++ {
		cmd := exec.Command(goBin, "build", "-o", filepath.Join(root, "hello-bin"), ".")
		cmd.Dir = mod
		cmd.Env = env
		if out, rerr := cmd.CombinedOutput(); rerr != nil {
			t.Fatalf("run %d: %v\n%s", run, rerr, out)
		}
	}

	sums, err := gocacheprog.ReadOutcomes(outcome)
	if err != nil {
		t.Fatal(err)
	}
	if len(sums) < 2 {
		t.Fatalf("outcome file has %d lines, want one per go invocation (>= 2)", len(sums))
	}
	first, last := sums[0], sums[len(sums)-1]
	if first.Puts == 0 {
		t.Fatalf("first run stored nothing: %+v", first)
	}
	if last.HitsLocal == 0 {
		t.Fatalf("second run hit nothing: %+v", last)
	}
	if first.Errors != 0 || last.Errors != 0 {
		t.Fatalf("helper reported errors: first %+v, last %+v", first, last)
	}
}

func hostEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "GOCACHEPROG=") || strings.HasPrefix(kv, "GOFLAGS=") || strings.HasPrefix(kv, "RUNS_FLEET_GOCACHE_") {
			continue
		}
		env = append(env, kv)
	}
	return env
}
