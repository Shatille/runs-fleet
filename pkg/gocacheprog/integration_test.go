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

type integrationFixture struct {
	goBin, root, helper, mod string
}

func newIntegrationFixture(t *testing.T) integrationFixture {
	t.Helper()
	if testing.Short() {
		t.Skip("builds the helper and runs the real toolchain")
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
	return integrationFixture{goBin: goBin, root: root, helper: helper, mod: mod}
}

func (f integrationFixture) env(goCache string, extra ...string) []string {
	return append(append(hostEnv(),
		"GOCACHE="+goCache,
		"GOTOOLCHAIN=local",
		"GOPROXY=off",
		"GOSUMDB=off",
		"GOFLAGS=",
	), extra...)
}

func (f integrationFixture) build(t *testing.T, env []string) string {
	t.Helper()
	cmd := exec.Command(f.goBin, "build", "-x", "-o", filepath.Join(f.root, "hello-bin"), ".")
	cmd.Dir = f.mod
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return string(out)
}

func compiled(buildLog string) bool {
	return strings.Contains(buildLog, "/compile ")
}

func TestIntegration_RealGoBuildHitsOnSecondRun(t *testing.T) {
	f := newIntegrationFixture(t)

	outcome := filepath.Join(f.root, "outcome")
	env := f.env(filepath.Join(f.root, "gocache"),
		"GOCACHEPROG="+f.helper,
		gocacheprog.EnvDir+"="+filepath.Join(f.root, "l1"),
		gocacheprog.EnvOutcome+"="+outcome,
	)
	for range 2 {
		f.build(t, env)
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

func TestIntegration_PlainGoReusesWhatTheHelperStored(t *testing.T) {
	f := newIntegrationFixture(t)
	goCache := filepath.Join(f.root, "gocache")

	if log := f.build(t, f.env(goCache, "GOCACHEPROG="+f.helper)); !compiled(log) {
		t.Fatalf("first build compiled nothing:\n%s", log)
	}
	records, err := filepath.Glob(filepath.Join(goCache, "*", "*-a"))
	if err != nil || len(records) == 0 {
		t.Fatalf("helper stored no records under GOCACHE %s (err %v)", goCache, err)
	}

	if log := f.build(t, f.env(goCache)); compiled(log) {
		t.Fatalf("go without the helper recompiled instead of reusing the helper's entries:\n%s", log)
	}
}

func TestIntegration_HelperCoexistsWithGoRunExecutableEntries(t *testing.T) {
	f := newIntegrationFixture(t)
	goCache := filepath.Join(f.root, "gocache")
	goRun := func(env []string) {
		t.Helper()
		cmd := exec.Command(f.goBin, "run", ".")
		cmd.Dir = f.mod
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err != nil || !strings.Contains(string(out), "hello") {
			t.Fatalf("go run: %v\n%s", err, out)
		}
	}

	goRun(f.env(goCache))
	executables, err := filepath.Glob(filepath.Join(goCache, "*", "*-d", "*"))
	if err != nil || len(executables) == 0 {
		t.Skipf("this toolchain cached no executable entry (err %v)", err)
	}

	outcome := filepath.Join(f.root, "outcome")
	withHelper := f.env(goCache, "GOCACHEPROG="+f.helper, gocacheprog.EnvOutcome+"="+outcome)
	f.build(t, withHelper)
	goRun(withHelper)
	goRun(f.env(goCache))

	sums, err := gocacheprog.ReadOutcomes(outcome)
	if err != nil || len(sums) == 0 {
		t.Fatalf("outcomes: %v, err %v", sums, err)
	}
	for _, sum := range sums {
		if sum.Errors != 0 {
			t.Fatalf("helper reported errors over a cache holding executables: %+v", sum)
		}
	}
	for _, exe := range executables {
		if _, serr := os.Stat(exe); serr != nil {
			t.Fatalf("helper disturbed go's cached executable: %v", serr)
		}
	}
}

func TestIntegration_HelperServesWhatPlainGoStored(t *testing.T) {
	f := newIntegrationFixture(t)
	goCache := filepath.Join(f.root, "gocache")

	if log := f.build(t, f.env(goCache)); !compiled(log) {
		t.Fatalf("first build compiled nothing:\n%s", log)
	}

	outcome := filepath.Join(f.root, "outcome")
	log := f.build(t, f.env(goCache, "GOCACHEPROG="+f.helper, gocacheprog.EnvOutcome+"="+outcome))
	if compiled(log) {
		t.Fatalf("go with the helper recompiled instead of reusing go's own entries:\n%s", log)
	}
	sums, err := gocacheprog.ReadOutcomes(outcome)
	if err != nil || len(sums) == 0 {
		t.Fatalf("outcomes: %v, err %v", sums, err)
	}
	last := sums[len(sums)-1]
	if last.HitsLocal == 0 || last.Errors != 0 {
		t.Fatalf("helper outcome = %+v, want local hits and no errors", last)
	}
	t.Logf("helper outcome over go's own cache: %+v", last)
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
