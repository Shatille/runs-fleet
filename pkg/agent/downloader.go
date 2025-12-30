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
	prebakedRunnerDir = "/home/runner"
)

// Downloader handles downloading GitHub Actions runner binaries.
type Downloader struct {
	prebakedPaths     []string     // Paths to check for pre-baked runner (for testing)
	skipPrebakedCheck bool         // Skip pre-baked runner check (for testing)
	HTTPClient        *http.Client // HTTP client to use (nil uses http.DefaultClient)
	releasesURL       string       // URL for GitHub releases API (empty uses default)
}

// NewDownloader creates a new Downloader instance.
func NewDownloader() *Downloader {
	return &Downloader{
		prebakedPaths: []string{runnerDir, prebakedRunnerDir},
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

// isValidRunnerDir checks if a directory contains a valid GitHub Actions runner.
func isValidRunnerDir(dir string) bool {
	runnerBin := filepath.Join(dir, "bin", "Runner.Listener")
	info, err := os.Stat(runnerBin)
	if err != nil {
		return false
	}
	return !info.IsDir() && info.Mode()&0100 != 0
}

// DownloadRunner downloads and extracts the GitHub Actions runner binary.
// Returns the path to the extracted runner directory.
// Checks for pre-baked runner in AMI/Docker image first to avoid unnecessary downloads.
func (d *Downloader) DownloadRunner(ctx context.Context) (string, error) {
	arch := runtime.GOARCH
	if arch != "arm64" && arch != "amd64" {
		return "", fmt.Errorf("unsupported architecture: %s", arch)
	}

	// Check for pre-baked runner (AMI: /opt/actions-runner, Docker: /home/runner)
	if !d.skipPrebakedCheck {
		for _, path := range d.prebakedPaths {
			if isValidRunnerDir(path) {
				return path, nil
			}
		}
	}

	// Map Go architecture names to GitHub runner asset names
	// GitHub uses "x64" for x86_64, but Go uses "amd64"
	githubArch := arch
	if arch == "amd64" {
		githubArch = "x64"
	}

	release, err := d.fetchLatestRelease(ctx)
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

	return runnerDir, nil
}

func (d *Downloader) fetchLatestRelease(ctx context.Context) (*Release, error) {
	url := d.releasesURL
	if url == "" {
		url = githubReleasesAPI
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	client := d.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}

	resp, err := client.Do(req)
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
