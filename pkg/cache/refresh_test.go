package cache

import (
	"testing"

	"github.com/Shavakan/runs-fleet/pkg/cache/wire"
)

func TestRefreshEligible(t *testing.T) {
	t.Parallel()

	tests := []struct {
		key  string
		want bool
	}{
		{"setup-go-Linux-arm64-ubuntu24-go-1.26.3-abc", true},
		{"setup-go-", true},
		{"Linux-go-abc", false},
		{"setup-node-Linux-x64-npm-abc", false},
		{"go-build-Linux-ARM64-abc", false},
		{"", false},
	}
	for _, tc := range tests {
		t.Run(tc.key, func(t *testing.T) {
			t.Parallel()
			if got := refreshEligible(tc.key); got != tc.want {
				t.Errorf("refreshEligible(%q) = %v, want %v", tc.key, got, tc.want)
			}
		})
	}
}

func TestMarkUnmarkRefresh(t *testing.T) {
	t.Parallel()

	const key = "setup-go-x"
	marked := markRefresh(key)
	if marked != key+wire.RefreshMarker {
		t.Fatalf("markRefresh(%q) = %q, want %q", key, marked, key+wire.RefreshMarker)
	}
	if again := markRefresh(marked); again != marked {
		t.Errorf("markRefresh is not idempotent: %q -> %q", marked, again)
	}
	if got := unmarkRefresh(marked); got != key {
		t.Errorf("unmarkRefresh(%q) = %q, want %q", marked, got, key)
	}
	if got := unmarkRefresh(key); got != key {
		t.Errorf("unmarkRefresh(%q) = %q, want unchanged", key, got)
	}
	const foreign = "deps-abc~refresh"
	if got := unmarkRefresh(foreign); got != foreign {
		t.Errorf("unmarkRefresh(%q) = %q, want a non-eligible key left alone", foreign, got)
	}
}
