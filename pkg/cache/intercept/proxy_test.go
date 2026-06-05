package intercept

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

const (
	cachePath    = cacheServicePrefix + "CreateCacheEntry"
	artifactPath = artifactServicePrefix + "CreateArtifact"
)

func setupProxy(t *testing.T, enabled bool) (*Proxy, *bool) {
	t.Helper()
	upstreamHit := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamHit = true
		_, _ = io.WriteString(w, "UPSTREAM")
	}))
	t.Cleanup(upstream.Close)

	local := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "LOCAL")
	})
	u, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream: %v", err)
	}
	return NewProxy(local, u, nil, func() bool { return enabled }), &upstreamHit
}

func serve(t *testing.T, p *Proxy, path string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}"))
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	body, _ := io.ReadAll(rec.Body)
	return string(body)
}

func TestEngagedCacheServedLocally(t *testing.T) {
	t.Parallel()

	p, upstreamHit := setupProxy(t, true)
	if body := serve(t, p, cachePath); body != "LOCAL" {
		t.Errorf("cache body = %q, want LOCAL", body)
	}
	if *upstreamHit {
		t.Error("upstream was hit for a cache request while engaged")
	}
}

func TestEngagedArtifactsPassThrough(t *testing.T) {
	t.Parallel()

	p, upstreamHit := setupProxy(t, true)
	if body := serve(t, p, artifactPath); body != "UPSTREAM" {
		t.Errorf("artifact body = %q, want UPSTREAM", body)
	}
	if !*upstreamHit {
		t.Error("artifact request was not passed through to upstream")
	}
}

func TestDisengagedCachePassesThrough(t *testing.T) {
	t.Parallel()

	// Fail-open: when the local cache is not engaged, even cache requests go to
	// GitHub so the job still gets a (real) cache rather than a broken one.
	p, upstreamHit := setupProxy(t, false)
	if body := serve(t, p, cachePath); body != "UPSTREAM" {
		t.Errorf("cache body = %q, want UPSTREAM (passthrough)", body)
	}
	if !*upstreamHit {
		t.Error("cache request was not passed through when disengaged")
	}
}
