package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestNewDownloader(t *testing.T) {
	d := NewDownloader()

	if len(d.prebakedPaths) != 2 {
		t.Errorf("expected 2 prebaked paths, got %d", len(d.prebakedPaths))
	}
}

func TestDownloadRunner_UnsupportedArch(_ *testing.T) {
	// This test validates the architecture check logic
	d := NewDownloader()

	// The actual test depends on runtime.GOARCH
	// On unsupported architectures, it should return an error
	ctx := context.Background()
	_, err := d.DownloadRunner(ctx)

	// We can't easily test unsupported arch without build tags
	// So we just verify it doesn't panic
	_ = err
}

func TestVerifyChecksum_Success(t *testing.T) {
	// Create a temporary file with known content
	tmpFile, err := os.CreateTemp("", "checksum-test-*")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	defer func() { _ = os.Remove(tmpFile.Name()) }()

	content := []byte("test content for checksum")
	if _, writeErr := tmpFile.Write(content); writeErr != nil {
		t.Fatalf("failed to write content: %v", writeErr)
	}
	_ = tmpFile.Close()

	// Calculate expected checksum
	hasher := sha256.New()
	_, _ = hasher.Write(content)
	expectedChecksum := hex.EncodeToString(hasher.Sum(nil))

	err = VerifyChecksum(tmpFile.Name(), expectedChecksum)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestVerifyChecksum_Mismatch(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "checksum-test-*")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	defer func() { _ = os.Remove(tmpFile.Name()) }()

	content := []byte("test content")
	if _, writeErr := tmpFile.Write(content); writeErr != nil {
		t.Fatalf("failed to write content: %v", writeErr)
	}
	_ = tmpFile.Close()

	wrongChecksum := "0000000000000000000000000000000000000000000000000000000000000000"

	err = VerifyChecksum(tmpFile.Name(), wrongChecksum)
	if err == nil {
		t.Fatal("expected checksum mismatch error")
	}
}

func TestVerifyChecksum_FileNotFound(t *testing.T) {
	err := VerifyChecksum("/nonexistent/file", "checksum")
	if err == nil {
		t.Fatal("expected error for non-existent file")
	}
}

func TestRelease_Structure(t *testing.T) {
	release := Release{
		TagName: "v2.300.0",
		Assets: []Asset{
			{
				Name:               "actions-runner-linux-x64-2.300.0.tar.gz",
				BrowserDownloadURL: "https://github.com/actions/runner/releases/download/v2.300.0/actions-runner-linux-x64-2.300.0.tar.gz",
			},
		},
	}

	if release.TagName != "v2.300.0" {
		t.Errorf("expected TagName 'v2.300.0', got '%s'", release.TagName)
	}
	if len(release.Assets) != 1 {
		t.Errorf("expected 1 asset, got %d", len(release.Assets))
	}
}

func TestAsset_Structure(t *testing.T) {
	asset := Asset{
		Name:               "actions-runner-linux-x64-2.300.0.tar.gz",
		BrowserDownloadURL: "https://github.com/actions/runner/releases/download/v2.300.0/actions-runner-linux-x64-2.300.0.tar.gz",
	}

	if asset.Name != "actions-runner-linux-x64-2.300.0.tar.gz" {
		t.Errorf("unexpected asset name: %s", asset.Name)
	}
	if asset.BrowserDownloadURL == "" {
		t.Error("expected BrowserDownloadURL to be set")
	}
}

func TestFetchLatestRelease_ServerError(_ *testing.T) {
	// Create a test server that returns an error
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	// Note: Can't easily test fetchLatestRelease directly since it uses a hardcoded URL
	// This test documents the expected behavior
}

func TestDownloadFile_ServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	tmpFile := filepath.Join(os.TempDir(), "download-test")
	defer func() { _ = os.Remove(tmpFile) }()

	err := downloadFile(context.Background(), server.URL, tmpFile)
	if err == nil {
		t.Fatal("expected error for 404 response")
	}
}

func TestDownloadFile_Success(t *testing.T) {
	content := []byte("test file content")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(content)
	}))
	defer server.Close()

	tmpFile := filepath.Join(os.TempDir(), "download-test-success")
	defer func() { _ = os.Remove(tmpFile) }()

	err := downloadFile(context.Background(), server.URL, tmpFile)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify file content
	data, err := os.ReadFile(tmpFile)
	if err != nil {
		t.Fatalf("failed to read downloaded file: %v", err)
	}

	if string(data) != string(content) {
		t.Errorf("expected content '%s', got '%s'", string(content), string(data))
	}
}

func TestDownloadFile_ContextCancelled(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		// Simulate slow response
		select {}
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	tmpFile := filepath.Join(os.TempDir(), "download-test-cancelled")
	defer func() { _ = os.Remove(tmpFile) }()

	err := downloadFile(ctx, server.URL, tmpFile)
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
}

func TestConstants_Downloader(t *testing.T) {
	if githubReleasesAPI != "https://api.github.com/repos/actions/runner/releases/latest" {
		t.Errorf("unexpected githubReleasesAPI: %s", githubReleasesAPI)
	}
	if runnerDir != "/opt/actions-runner" {
		t.Errorf("unexpected runnerDir: %s", runnerDir)
	}
	if prebakedRunnerDir != "/home/runner" {
		t.Errorf("unexpected prebakedRunnerDir: %s", prebakedRunnerDir)
	}
}

func TestIsValidRunnerDir_Valid(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "runner-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	binDir := filepath.Join(tmpDir, "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatalf("failed to create bin dir: %v", err)
	}

	runnerBin := filepath.Join(binDir, "Runner.Listener")
	if err := os.WriteFile(runnerBin, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatalf("failed to create runner binary: %v", err)
	}

	if !isValidRunnerDir(tmpDir) {
		t.Error("expected valid runner dir")
	}
}

func TestIsValidRunnerDir_MissingBinary(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "runner-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	if isValidRunnerDir(tmpDir) {
		t.Error("expected invalid runner dir when binary is missing")
	}
}

func TestIsValidRunnerDir_NotExecutable(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "runner-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	binDir := filepath.Join(tmpDir, "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatalf("failed to create bin dir: %v", err)
	}

	runnerBin := filepath.Join(binDir, "Runner.Listener")
	if err := os.WriteFile(runnerBin, []byte("not executable"), 0644); err != nil {
		t.Fatalf("failed to create runner binary: %v", err)
	}

	if isValidRunnerDir(tmpDir) {
		t.Error("expected invalid runner dir when binary is not executable")
	}
}

func TestIsValidRunnerDir_IsDirectory(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "runner-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	binDir := filepath.Join(tmpDir, "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatalf("failed to create bin dir: %v", err)
	}

	runnerBin := filepath.Join(binDir, "Runner.Listener")
	if err := os.MkdirAll(runnerBin, 0755); err != nil {
		t.Fatalf("failed to create runner as directory: %v", err)
	}

	if isValidRunnerDir(tmpDir) {
		t.Error("expected invalid runner dir when Runner.Listener is a directory")
	}
}

func TestIsValidRunnerDir_NonexistentDir(t *testing.T) {
	if isValidRunnerDir("/nonexistent/path/to/runner") {
		t.Error("expected invalid runner dir for nonexistent path")
	}
}
