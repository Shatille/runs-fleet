package intercept

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/Shavakan/runs-fleet/pkg/cache/blobshim"
)

const (
	cachePath    = cacheServicePrefix + "CreateCacheEntry"
	artifactPath = artifactServicePrefix + "CreateArtifact"
	upstreamBody = "UPSTREAM"
)

func handlerWriting(s string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, s) })
}

func setup(t *testing.T, healthy bool) (*Interceptor, *bool) {
	t.Helper()
	upstreamHit := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamHit = true
		_, _ = io.WriteString(w, upstreamBody)
	}))
	t.Cleanup(upstream.Close)
	u, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream: %v", err)
	}
	i := NewInterceptor(handlerWriting("CACHE"), handlerWriting("BLOB"), u, nil, func() bool { return healthy })
	return i, &upstreamHit
}

func serve(t *testing.T, i *Interceptor, path string) (string, int) {
	t.Helper()
	rec := httptest.NewRecorder()
	i.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}")))
	body, _ := io.ReadAll(rec.Body)
	return string(body), rec.Code
}

func TestEngagedCacheAndBlobServedLocally(t *testing.T) {
	t.Parallel()

	i, upstreamHit := setup(t, true)
	if body, _ := serve(t, i, cachePath); body != "CACHE" {
		t.Errorf("cache body = %q, want CACHE", body)
	}
	if body, _ := serve(t, i, blobshim.PathPrefix+"token"); body != "BLOB" {
		t.Errorf("blob body = %q, want BLOB", body)
	}
	if *upstreamHit {
		t.Error("upstream hit for locally-served request while engaged")
	}
}

func TestEngagedArtifactsPassThrough(t *testing.T) {
	t.Parallel()

	i, upstreamHit := setup(t, true)
	if body, _ := serve(t, i, artifactPath); body != upstreamBody {
		t.Errorf("artifact body = %q, want UPSTREAM", body)
	}
	if !*upstreamHit {
		t.Error("artifact request was not passed through")
	}
}

func TestDisengagedEverythingPassesThrough(t *testing.T) {
	t.Parallel()

	i, upstreamHit := setup(t, false)
	// Fail-open: even cache and blob requests go to GitHub when not engaged.
	for _, p := range []string{cachePath, blobshim.PathPrefix + "token"} {
		if body, _ := serve(t, i, p); body != upstreamBody {
			t.Errorf("path %s body = %q, want UPSTREAM (passthrough)", p, body)
		}
	}
	if !*upstreamHit {
		t.Error("requests were not passed through when disengaged")
	}
}

func TestNilHealthyFailsOpen(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(handlerWriting(upstreamBody))
	t.Cleanup(upstream.Close)
	u, _ := url.Parse(upstream.URL)
	i := NewInterceptor(handlerWriting("CACHE"), handlerWriting("BLOB"), u, nil, nil)
	if body, _ := serve(t, i, cachePath); body != upstreamBody {
		t.Errorf("nil healthy should pass through, got %q", body)
	}
}

func TestPinnedTransportDialsPinnedAddr(t *testing.T) {
	t.Parallel()

	// A server on a real loopback addr stands in for "real GitHub".
	realSrv := httptest.NewServer(handlerWriting("REAL"))
	t.Cleanup(realSrv.Close)
	realHost := strings.TrimPrefix(realSrv.URL, "http://") // 127.0.0.1:PORT

	// Reverse-proxy to the pinned hostname; the transport must redirect the dial
	// to the real addr instead of trying to resolve "results.example".
	transport := PinnedTransport("results.example", realHost)
	u, _ := url.Parse("http://results.example")
	i := NewInterceptor(handlerWriting("CACHE"), handlerWriting("BLOB"), u, transport, func() bool { return true })

	rec := httptest.NewRecorder()
	i.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, artifactPath, nil))
	if body, _ := io.ReadAll(rec.Body); string(body) != "REAL" {
		t.Errorf("body = %q, want REAL (dialed pinned addr)", body)
	}
}

func TestPinnedTransportLeavesOtherHosts(t *testing.T) {
	t.Parallel()

	// A non-pinned host must be dialed at its own address, not redirected to the
	// pinned addr. Point the pin at a dead port, then dial a live server by its
	// real address: success proves the dial was not hijacked.
	live := httptest.NewServer(handlerWriting("LIVE"))
	t.Cleanup(live.Close)
	liveAddr := strings.TrimPrefix(live.URL, "http://")

	tr := PinnedTransport("results.example", "127.0.0.1:1") // pinned host → dead port
	conn, err := tr.DialContext(context.Background(), "tcp", liveAddr)
	if err != nil {
		t.Fatalf("non-pinned dial to %s failed: %v", liveAddr, err)
	}
	_ = conn.Close()
}
