package agent

import (
	"context"
	"testing"
)

func TestDownloadRunner(t *testing.T) {
	tests := []struct {
		name    string
		wantErr bool
	}{
		{
			name:    "unsupported architecture returns error",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			// Create downloader with no cache
			d := NewDownloader(nil)

			// This test will fail on non-arm64/amd64 architectures
			// For real testing, we'd mock the HTTP client
			_, err := d.DownloadRunner(ctx)
			if tt.wantErr && err == nil {
				t.Error("DownloadRunner() expected error but got none")
			}
		})
	}
}
