package validation

import (
	"testing"
)

func TestValidateK8sJWTPath(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{
			name:    "valid standard serviceaccount token",
			path:    "/var/run/secrets/kubernetes.io/serviceaccount/token",
			wantErr: false,
		},
		{
			name:    "valid nested path under secrets",
			path:    "/var/run/secrets/custom/jwt",
			wantErr: false,
		},
		{
			name:    "empty path",
			path:    "",
			wantErr: true,
		},
		{
			name:    "relative path",
			path:    "secrets/token",
			wantErr: true,
		},
		{
			name:    "relative path with dot prefix",
			path:    "./var/run/secrets/token",
			wantErr: true,
		},
		{
			name:    "absolute path outside allowed prefix",
			path:    "/etc/passwd",
			wantErr: true,
		},
		{
			name:    "absolute path under /var but not secrets",
			path:    "/var/log/syslog",
			wantErr: true,
		},
		{
			name:    "path traversal via dot-dot",
			path:    "/var/run/secrets/../../etc/passwd",
			wantErr: true,
		},
		{
			name:    "path traversal at end",
			path:    "/var/run/secrets/kubernetes.io/serviceaccount/../../..",
			wantErr: true,
		},
		{
			name:    "prefix directory only with trailing slash",
			path:    "/var/run/secrets/",
			wantErr: true,
		},
		{
			name:    "prefix directory without trailing slash",
			path:    "/var/run/secrets",
			wantErr: true,
		},
		{
			name:    "double slashes in path",
			path:    "/var/run/secrets//kubernetes.io/serviceaccount/token",
			wantErr: false,
		},
		{
			name:    "trailing dot in path",
			path:    "/var/run/secrets/kubernetes.io/serviceaccount/token/.",
			wantErr: false,
		},
		{
			name:    "path with trailing slash on valid file",
			path:    "/var/run/secrets/kubernetes.io/serviceaccount/",
			wantErr: false,
		},
		{
			name:    "path that is prefix substring but not under it",
			path:    "/var/run/secrets-other/token",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateK8sJWTPath(tt.path)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateK8sJWTPath(%q) error = %v, wantErr %v", tt.path, err, tt.wantErr)
			}
		})
	}
}
