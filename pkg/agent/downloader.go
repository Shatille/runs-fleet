// Package agent provides GitHub Actions runner agent functionality.
package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

const (
	githubReleasesAPI = "https://api.github.com/repos/actions/runner/releases/latest"
	runnerDir         = "/opt/actions-runner"
)

// CacheClient provides S3 caching for runner binaries.
type CacheClient interface {
	CheckCache(ctx context.Context, version, arch string) (bool, string, error)
	DownloadFromCache(ctx context.Context, version, arch, destPath string) error
	UploadToCache(ctx context.Context, version, arch, sourcePath string) error
}

// Downloader handles downloading GitHub Actions runner binaries.
type Downloader struct {
	cache CacheClient
}

// NewDownloader creates a new Downloader instance.
func NewDownloader(cache CacheClient) *Downloader {
	return &Downloader{
		cache: cache,
	}
}

// Release represents GitHub release API response.
type Release struct {
	TagName string  `json:"tag_name"`
	Assets  []Asset `json:"assets"`
}

// Asset represents a release asset.
type Asset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

// DownloadRunner downloads and extracts the GitHub Actions runner binary.
// Returns the path to the extracted runner directory.
func (d *Downloader) DownloadRunner(ctx context.Context) (string, error) {
	arch := runtime.GOARCH
	if arch != "arm64" && arch != "amd64" {
		return "", fmt.Errorf("unsupported architecture: %s", arch)
	}

	// Map Go architecture names to GitHub runner asset names
	// GitHub uses "x64" for x86_64, but Go uses "amd64"
	githubArch := arch
	if arch == "amd64" {
		githubArch = "x64"
	}

	// Check cache first if available
	if d.cache != nil {
		cached, _, err := d.cache.CheckCache(ctx, "latest", arch)
		if err == nil && cached {
			if err := d.cache.DownloadFromCache(ctx, "latest", arch, runnerDir); err == nil {
				return runnerDir, nil
			}
		}
	}

	release, err := fetchLatestRelease(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to fetch latest release: %w", err)
	}

	assetName := fmt.Sprintf("actions-runner-linux-%s-%s.tar.gz", githubArch, release.TagName[1:])
	var downloadURL string
	for _, asset := range release.Assets {
		if asset.Name == assetName {
			downloadURL = asset.BrowserDownloadURL
			break
		}
	}

	if downloadURL == "" {
		return "", fmt.Errorf("no asset found for architecture %s", arch)
	}

	if err := os.MkdirAll(runnerDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create runner directory: %w", err)
	}

	tarballPath := filepath.Join(runnerDir, assetName)
	if err := downloadFile(ctx, downloadURL, tarballPath); err != nil {
		return "", fmt.Errorf("failed to download runner: %w", err)
	}

	if err := extractTarball(tarballPath, runnerDir); err != nil {
		return "", fmt.Errorf("failed to extract runner: %w", err)
	}

	if err := os.Remove(tarballPath); err != nil {
		return "", fmt.Errorf("failed to remove tarball: %w", err)
	}

	// Upload to cache if available
	if d.cache != nil {
		_ = d.cache.UploadToCache(ctx, release.TagName, arch, runnerDir)
		// Ignore cache upload errors - it's optional
	}

	return runnerDir, nil
}

func fetchLatestRelease(ctx context.Context) (*Release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, githubReleasesAPI, nil)
	if err != nil {
		return nil, err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API returned status %d", resp.StatusCode)
	}

	var release Release
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return nil, err
	}

	return &release, nil
}

func downloadFile(ctx context.Context, url, dest string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download failed with status %d", resp.StatusCode)
	}

	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer func() {
		_ = out.Close()
	}()

	if _, err := io.Copy(out, resp.Body); err != nil {
		return err
	}

	return nil
}

func extractTarball(tarballPath, destDir string) error {
	cmd := exec.Command("tar", "-xzf", tarballPath, "-C", destDir)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("tar extraction failed: %w, output: %s", err, string(output))
	}
	return nil
}

// VerifyChecksum verifies the SHA256 checksum of a file.
func VerifyChecksum(filePath, expectedChecksum string) error {
	file, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("failed to open file: %w", err)
	}
	defer func() {
		_ = file.Close()
	}()

	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return fmt.Errorf("failed to hash file: %w", err)
	}

	actualChecksum := hex.EncodeToString(hasher.Sum(nil))
	if actualChecksum != expectedChecksum {
		return fmt.Errorf("checksum mismatch: expected %s, got %s", expectedChecksum, actualChecksum)
	}

	return nil
}
