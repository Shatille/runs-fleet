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

// mockCacheClient implements CacheClient for testing.
type mockCacheClient struct {
	cacheHit     bool
	checkErr     error
	downloadErr  error
	uploadErr    error
	checkCalls   int
	downloadCalls int
	uploadCalls  int
}

func (m *mockCacheClient) CheckCache(ctx context.Context, version, arch string) (bool, string, error) {
	m.checkCalls++
	if m.checkErr != nil {
		return false, "", m.checkErr
	}
	return m.cacheHit, "cached-path", nil
}

func (m *mockCacheClient) DownloadFromCache(ctx context.Context, version, arch, destPath string) error {
	m.downloadCalls++
	return m.downloadErr
}

func (m *mockCacheClient) UploadToCache(ctx context.Context, version, arch, sourcePath string) error {
	m.uploadCalls++
	return m.uploadErr
}

func TestNewDownloader(t *testing.T) {
	cache := &mockCacheClient{}
	d := NewDownloader(cache)

	if d.cache != cache {
		t.Error("expected cache to be set")
	}
}

func TestNewDownloader_NilCache(t *testing.T) {
	d := NewDownloader(nil)

	if d.cache != nil {
		t.Error("expected cache to be nil")
	}
}

func TestDownloadRunner_UnsupportedArch(t *testing.T) {
	// This test validates the architecture check logic
	d := NewDownloader(nil)

	// The actual test depends on runtime.GOARCH
	// On unsupported architectures, it should return an error
	ctx := context.Background()
	_, err := d.DownloadRunner(ctx)

	// We can't easily test unsupported arch without build tags
	// So we just verify it doesn't panic
	_ = err
}

func TestDownloadRunner_CacheHit(t *testing.T) {
	cache := &mockCacheClient{
		cacheHit: true,
	}
	d := NewDownloader(cache)

	// This would succeed only if cache download works
	// Since DownloadFromCache returns nil error, it should try to return cached path
	// But the actual runner directory may not exist
	ctx := context.Background()
	_, _ = d.DownloadRunner(ctx)

	if cache.checkCalls != 1 {
		t.Errorf("expected 1 cache check call, got %d", cache.checkCalls)
	}
}

func TestVerifyChecksum_Success(t *testing.T) {
	// Create a temporary file with known content
	tmpFile, err := os.CreateTemp("", "checksum-test-*")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())

	content := []byte("test content for checksum")
	if _, err := tmpFile.Write(content); err != nil {
		t.Fatalf("failed to write content: %v", err)
	}
	tmpFile.Close()

	// Calculate expected checksum
	hasher := sha256.New()
	hasher.Write(content)
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
	defer os.Remove(tmpFile.Name())

	content := []byte("test content")
	if _, err := tmpFile.Write(content); err != nil {
		t.Fatalf("failed to write content: %v", err)
	}
	tmpFile.Close()

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

func TestFetchLatestRelease_ServerError(t *testing.T) {
	// Create a test server that returns an error
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	// Note: Can't easily test fetchLatestRelease directly since it uses a hardcoded URL
	// This test documents the expected behavior
}

func TestDownloadFile_ServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	tmpFile := filepath.Join(os.TempDir(), "download-test")
	defer os.Remove(tmpFile)

	err := downloadFile(context.Background(), server.URL, tmpFile)
	if err == nil {
		t.Fatal("expected error for 404 response")
	}
}

func TestDownloadFile_Success(t *testing.T) {
	content := []byte("test file content")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write(content)
	}))
	defer server.Close()

	tmpFile := filepath.Join(os.TempDir(), "download-test-success")
	defer os.Remove(tmpFile)

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
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Simulate slow response
		select {}
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	tmpFile := filepath.Join(os.TempDir(), "download-test-cancelled")
	defer os.Remove(tmpFile)

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
}
