package intercept

import "testing"

func TestClassify(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		path string
		want Target
	}{
		{"cache create", "/twirp/github.actions.results.api.v1.CacheService/CreateCacheEntry", TargetCacheService},
		{"cache download url", "/twirp/github.actions.results.api.v1.CacheService/GetCacheEntryDownloadURL", TargetCacheService},
		{"artifact service passes through", "/twirp/github.actions.results.api.v1.ArtifactService/CreateArtifact", TargetOther},
		{"oidc passes through", "/oidc/token", TargetOther},
		{"empty path", "", TargetOther},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := Classify(tt.path); got != tt.want {
				t.Errorf("Classify(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

func TestIsArtifactService(t *testing.T) {
	t.Parallel()

	if !IsArtifactService("/twirp/github.actions.results.api.v1.ArtifactService/CreateArtifact") {
		t.Error("artifact path not recognized")
	}
	if IsArtifactService("/twirp/github.actions.results.api.v1.CacheService/CreateCacheEntry") {
		t.Error("cache path misclassified as artifact")
	}
}
