// Package agent implements the GitHub Actions runner agent for EC2 instances.
package agent

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	// GitHubRunnerReleasesURL is the GitHub API endpoint for runner releases.
	GitHubRunnerReleasesURL = "https://api.github.com/repos/actions/runner/releases/latest"
	// DefaultRunnerDir is where the runner is extracted.
	DefaultRunnerDir = "/opt/actions-runner"
	// MinDiskSpaceBytes is the minimum disk space required (1GB).
	MinDiskSpaceBytes = 1 * 1024 * 1024 * 1024
)

// CacheClient provides S3 caching operations for runner binaries.
type CacheClient interface {
	CheckCache(ctx context.Context, version, arch string) (string, bool, error)
	CacheRunner(ctx context.Context, version, arch, localPath string) error
	GetCachedRunner(ctx context.Context, version, arch, destPath string) error
}

// ReleaseAsset represents a runner release asset from GitHub API.
type ReleaseAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
}

// Release represents a GitHub Actions runner release.
type Release struct {
	TagName string         `json:"tag_name"`
	Assets  []ReleaseAsset `json:"assets"`
}

// Downloader handles downloading and extracting GitHub Actions runner.
type Downloader struct {
	cacheClient CacheClient
	httpClient  *http.Client
	runnerDir   string
}

// NewDownloader creates a new runner downloader with optional S3 cache.
func NewDownloader(cache CacheClient) *Downloader {
	return &Downloader{
		cacheClient: cache,
		httpClient: &http.Client{
			Timeout: 10 * time.Minute,
		},
		runnerDir: DefaultRunnerDir,
	}
}

// DownloadRunner downloads and extracts the GitHub Actions runner.
// Returns the path to the runner directory.
func (d *Downloader) DownloadRunner(ctx context.Context) (string, error) {
	// Check disk space
	if err := d.checkDiskSpace(d.runnerDir); err != nil {
		return "", err
	}

	// Determine architecture
	arch := d.getArchitecture()

	// Get latest release info
	release, err := d.getLatestRelease(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to get latest release: %w", err)
	}

	version := strings.TrimPrefix(release.TagName, "v")

	// Check S3 cache first
	if d.cacheClient != nil {
		if _, cached, err := d.cacheClient.CheckCache(ctx, version, arch); err == nil && cached {
			// Download from cache
			tarballPath := filepath.Join(os.TempDir(), fmt.Sprintf("actions-runner-%s-%s.tar.gz", version, arch))
			if err := d.cacheClient.GetCachedRunner(ctx, version, arch, tarballPath); err == nil {
				if err := d.extractRunner(tarballPath, d.runnerDir); err == nil {
					os.Remove(tarballPath)
					return d.runnerDir, nil
				}
			}
		}
	}

	// Find the appropriate asset
	asset, err := d.findAsset(release, arch)
	if err != nil {
		return "", err
	}

	// Download the runner tarball
	tarballPath := filepath.Join(os.TempDir(), asset.Name)
	if err := d.downloadWithRetry(ctx, asset.BrowserDownloadURL, tarballPath, 3); err != nil {
		return "", fmt.Errorf("failed to download runner: %w", err)
	}
	defer os.Remove(tarballPath)

	// Extract the tarball
	if err := d.extractRunner(tarballPath, d.runnerDir); err != nil {
		return "", fmt.Errorf("failed to extract runner: %w", err)
	}

	// Cache to S3 for future use
	if d.cacheClient != nil {
		// Re-download to cache since we already deleted it
		cachePath := filepath.Join(os.TempDir(), fmt.Sprintf("cache-%s", asset.Name))
		if err := d.downloadWithRetry(ctx, asset.BrowserDownloadURL, cachePath, 1); err == nil {
			if err := d.cacheClient.CacheRunner(ctx, version, arch, cachePath); err != nil {
				// Log but don't fail - caching is optional
				fmt.Printf("Warning: failed to cache runner: %v\n", err)
			}
			os.Remove(cachePath)
		}
	}

	return d.runnerDir, nil
}

// getArchitecture returns the architecture string for the runner binary.
func (d *Downloader) getArchitecture() string {
	switch runtime.GOARCH {
	case "arm64":
		return "linux-arm64"
	default:
		return "linux-x64"
	}
}

// getLatestRelease fetches the latest runner release from GitHub.
func (d *Downloader) getLatestRelease(ctx context.Context) (*Release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, GitHubRunnerReleasesURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	req.Header.Set("User-Agent", "runs-fleet-agent")

	resp, err := d.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch release: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	var release Release
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return nil, fmt.Errorf("failed to decode release: %w", err)
	}

	return &release, nil
}

// findAsset finds the runner asset for the specified architecture.
func (d *Downloader) findAsset(release *Release, arch string) (*ReleaseAsset, error) {
	for _, asset := range release.Assets {
		if strings.Contains(asset.Name, arch) && strings.HasSuffix(asset.Name, ".tar.gz") {
			return &asset, nil
		}
	}
	return nil, fmt.Errorf("no runner asset found for architecture: %s", arch)
}

// downloadWithRetry downloads a file with retry logic.
func (d *Downloader) downloadWithRetry(ctx context.Context, url, destPath string, maxRetries int) error {
	var lastErr error
	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(1<<uint(attempt)) * time.Second
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return ctx.Err()
			}
		}

		if err := d.download(ctx, url, destPath); err != nil {
			lastErr = err
			continue
		}
		return nil
	}
	return fmt.Errorf("download failed after %d attempts: %w", maxRetries, lastErr)
}

// download downloads a file from URL to destPath.
func (d *Downloader) download(ctx context.Context, url, destPath string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("User-Agent", "runs-fleet-agent")

	resp, err := d.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to download: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	// Create destination directory
	if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}

	// Create temp file
	tmpFile, err := os.CreateTemp(filepath.Dir(destPath), "download-*")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer func() {
		tmpFile.Close()
		os.Remove(tmpPath)
	}()

	// Download to temp file
	if _, err := io.Copy(tmpFile, resp.Body); err != nil {
		return fmt.Errorf("failed to write file: %w", err)
	}
	tmpFile.Close()

	// Move to final destination
	if err := os.Rename(tmpPath, destPath); err != nil {
		return fmt.Errorf("failed to rename file: %w", err)
	}

	return nil
}

// extractRunner extracts the runner tarball to the destination directory.
func (d *Downloader) extractRunner(tarballPath, destDir string) error {
	// Create destination directory
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}

	// Open tarball
	file, err := os.Open(tarballPath)
	if err != nil {
		return fmt.Errorf("failed to open tarball: %w", err)
	}
	defer file.Close()

	// Create gzip reader
	gzReader, err := gzip.NewReader(file)
	if err != nil {
		return fmt.Errorf("failed to create gzip reader: %w", err)
	}
	defer gzReader.Close()

	// Create tar reader
	tarReader := tar.NewReader(gzReader)

	// Extract files
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("failed to read tar header: %w", err)
		}

		targetPath := filepath.Join(destDir, header.Name)

		// Validate path to prevent directory traversal
		if !strings.HasPrefix(targetPath, filepath.Clean(destDir)+string(os.PathSeparator)) {
			return fmt.Errorf("invalid file path: %s", header.Name)
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(targetPath, os.FileMode(header.Mode)); err != nil {
				return fmt.Errorf("failed to create directory: %w", err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
				return fmt.Errorf("failed to create parent directory: %w", err)
			}
			outFile, err := os.OpenFile(targetPath, os.O_CREATE|os.O_RDWR|os.O_TRUNC, os.FileMode(header.Mode))
			if err != nil {
				return fmt.Errorf("failed to create file: %w", err)
			}
			if _, err := io.Copy(outFile, tarReader); err != nil {
				outFile.Close()
				return fmt.Errorf("failed to extract file: %w", err)
			}
			outFile.Close()
		case tar.TypeSymlink:
			// Validate symlink target to prevent path traversal attacks
			// Resolve the symlink target relative to the symlink's directory
			linkDir := filepath.Dir(targetPath)
			resolvedTarget := filepath.Join(linkDir, header.Linkname)
			resolvedTarget = filepath.Clean(resolvedTarget)

			// Ensure the resolved target stays within the destination directory
			if !strings.HasPrefix(resolvedTarget, filepath.Clean(destDir)+string(os.PathSeparator)) &&
				resolvedTarget != filepath.Clean(destDir) {
				return fmt.Errorf("symlink target escapes extraction directory: %s -> %s", header.Name, header.Linkname)
			}

			if err := os.Symlink(header.Linkname, targetPath); err != nil {
				// Ignore symlink errors on some systems
				fmt.Printf("Warning: failed to create symlink: %v\n", err)
			}
		}
	}

	// Set execute permissions on key files
	for _, file := range []string{"run.sh", "config.sh", "bin/Runner.Listener", "bin/Runner.Worker"} {
		path := filepath.Join(destDir, file)
		if _, err := os.Stat(path); err == nil {
			os.Chmod(path, 0755)
		}
	}

	// Create work directory
	workDir := filepath.Join(destDir, "_work")
	if err := os.MkdirAll(workDir, 0755); err != nil {
		return fmt.Errorf("failed to create work directory: %w", err)
	}

	return nil
}

// checkDiskSpace verifies sufficient disk space is available.
//
// PLATFORM SUPPORT:
//   - Linux: Full support via syscall.Statfs
//   - macOS/Darwin: Full support via syscall.Statfs
//   - Windows/other: Stub implementation with warning (assumes 10GB available)
//
// For production deployments on EC2, Linux instances are recommended.
func (d *Downloader) checkDiskSpace(path string) error {
	// Create the directory if it doesn't exist to check its filesystem
	if err := os.MkdirAll(path, 0755); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}

	// Use syscall to get disk space info
	// NOTE: On non-Linux platforms, statfs returns dummy values (see statfs_other.go)
	var stat syscallStatfs
	if err := statfs(path, &stat); err != nil {
		// If we can't check, proceed anyway
		return nil
	}

	available := stat.Bavail * uint64(stat.Bsize)
	if available < MinDiskSpaceBytes {
		return fmt.Errorf("insufficient disk space: %d bytes available, need %d", available, MinDiskSpaceBytes)
	}

	return nil
}

// VerifyChecksum verifies SHA256 checksum of a file.
func VerifyChecksum(filePath, expectedHash string) error {
	file, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("failed to open file: %w", err)
	}
	defer file.Close()

	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return fmt.Errorf("failed to hash file: %w", err)
	}

	actualHash := hex.EncodeToString(hasher.Sum(nil))
	if actualHash != expectedHash {
		return fmt.Errorf("checksum mismatch: expected %s, got %s", expectedHash, actualHash)
	}

	return nil
}
