package cacheproxy

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// stubHelper replaces runHelper with a recorder for the duration of a test.
func stubHelper(t *testing.T) *struct {
	stdin []byte
	args  []string
	err   error
} {
	t.Helper()
	rec := &struct {
		stdin []byte
		args  []string
		err   error
	}{}
	old := runHelper
	runHelper = func(stdin []byte, args ...string) error {
		rec.stdin = stdin
		rec.args = args
		return rec.err
	}
	t.Cleanup(func() { runHelper = old })
	return rec
}

func TestEngageInvokesHelperWithEngageArgsAndCAOnStdin(t *testing.T) {
	rec := stubHelper(t)
	host := "results-receiver.actions.githubusercontent.com"
	pem := []byte("CA-PEM")

	if err := EngageCacheTrustAndPin(host, pem); err != nil {
		t.Fatalf("engage: %v", err)
	}
	if len(rec.args) != 2 || rec.args[0] != "engage" || rec.args[1] != host {
		t.Errorf("args = %v, want [engage %s]", rec.args, host)
	}
	if string(rec.stdin) != "CA-PEM" {
		t.Errorf("stdin = %q, want the CA PEM", rec.stdin)
	}
}

func TestDisengageInvokesHelperWithDisengageArgs(t *testing.T) {
	rec := stubHelper(t)
	host := "results-receiver.actions.githubusercontent.com"

	if err := DisengageCache(host); err != nil {
		t.Fatalf("disengage: %v", err)
	}
	if len(rec.args) != 2 || rec.args[0] != "disengage" || rec.args[1] != host {
		t.Errorf("args = %v, want [disengage %s]", rec.args, host)
	}
	if len(rec.stdin) != 0 {
		t.Errorf("disengage should send no stdin, got %q", rec.stdin)
	}
}

func TestEngagePropagatesHelperError(t *testing.T) {
	rec := stubHelper(t)
	rec.err = errors.New("boom")

	if err := EngageCacheTrustAndPin("results-receiver.actions.githubusercontent.com", []byte("x")); err == nil {
		t.Fatal("expected the helper error to propagate (so the caller fails open)")
	}
}

func TestWriteCACert(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := WriteCACert(path, []byte("PEMDATA")); err != nil {
		t.Fatalf("write: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "PEMDATA" {
		t.Errorf("read back = %q, %v", data, err)
	}
}
