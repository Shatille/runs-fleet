// Package validation provides shared validation utilities.
package validation

import (
	"fmt"
	"path/filepath"
	"strings"
)

// ValidateK8sJWTPath validates the Kubernetes JWT token path for security.
// It ensures the path is absolute and under /var/run/secrets/ to prevent
// path traversal attacks.
func ValidateK8sJWTPath(path string) error {
	if path == "" {
		return fmt.Errorf("VAULT_K8S_JWT_PATH cannot be empty for Kubernetes auth")
	}
	if !filepath.IsAbs(path) {
		return fmt.Errorf("VAULT_K8S_JWT_PATH must be an absolute path, got %q", path)
	}
	cleanPath := filepath.Clean(path)
	if !strings.HasPrefix(cleanPath, "/var/run/secrets/") {
		return fmt.Errorf("VAULT_K8S_JWT_PATH must be under /var/run/secrets/, got %q", path)
	}
	return nil
}
