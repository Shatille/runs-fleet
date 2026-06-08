package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAppendRunnerEnv(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	if err := os.WriteFile(envPath, []byte("RUNNER_ALLOW_RUNASROOT=1\n"), 0644); err != nil {
		t.Fatal(err)
	}

	r := &Registrar{}
	if err := r.AppendRunnerEnv(dir, "NODE_EXTRA_CA_CERTS", "/opt/ca.pem"); err != nil {
		t.Fatalf("AppendRunnerEnv: %v", err)
	}

	data, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if !strings.Contains(got, "NODE_EXTRA_CA_CERTS=/opt/ca.pem") {
		t.Errorf("appended var missing: %q", got)
	}
	if !strings.Contains(got, "RUNNER_ALLOW_RUNASROOT=1") {
		t.Error("existing .env content not preserved")
	}
}
