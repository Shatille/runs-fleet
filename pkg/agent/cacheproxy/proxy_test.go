package cacheproxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// stubOrchestrator answers the v1 reserve endpoint with a presigned PUT URL.
func stubOrchestrator(t *testing.T) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/_apis/artifactcache/caches" {
			w.Header().Set("Location", "https://s3.example/presigned-put?sig=abc")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"cacheId":1,"cacheKey":"caches/org/repo/v1/k"}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(ts.Close)
	return ts
}

func startProxy(t *testing.T, orchURL string) *Proxy {
	t.Helper()
	p, err := New(Config{
		OrchestratorBaseURL: orchURL,
		CacheToken:          "tok",
		ResultsHost:         "results.test",
		ListenAddr:          "127.0.0.1:0",
		StagingDir:          t.TempDir(),
		HTTPClient:          http.DefaultClient,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Avoid real DNS; a non-loopback TEST-NET addr (RFC 5737) passes the
	// loopback guard. The reverse-proxy upstream isn't exercised here.
	p.resolver = func(context.Context, string) ([]string, error) { return []string{"203.0.113.10"}, nil }
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop(context.Background()) })
	return p
}

// caClient is an HTTPS client that trusts only the proxy's per-instance CA —
// exactly the trust NODE_EXTRA_CA_CERTS gives the real cache client.
func caClient(t *testing.T, p *Proxy) *http.Client {
	t.Helper()
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(p.CACertPEM()) {
		t.Fatal("CA PEM not appended")
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}
}

func TestProxyServesCacheServiceOverTLS(t *testing.T) {
	t.Parallel()

	orch := stubOrchestrator(t)
	p := startProxy(t, orch.URL)
	client := caClient(t, p)

	url := "https://" + p.Addr() + "/twirp/github.actions.results.api.v1.CacheService/CreateCacheEntry"
	resp, err := client.Post(url, "application/json", strings.NewReader(`{"key":"k","version":"v1"}`))
	if err != nil {
		t.Fatalf("post over TLS: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		OK              bool   `json:"ok"`
		SignedUploadURL string `json:"signedUploadUrl"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.OK {
		t.Error("ok = false")
	}
	// The client is handed a blob-shim URL on the (pinned) results host, not the
	// raw presigned S3 URL.
	if !strings.HasPrefix(body.SignedUploadURL, "https://results.test/blob/") {
		t.Errorf("signedUploadUrl = %q, want a results.test /blob/ URL", body.SignedUploadURL)
	}
}

func TestStartFailsWhenResultsHostUnresolvable(t *testing.T) {
	t.Parallel()

	p, err := New(Config{OrchestratorBaseURL: "https://orch.invalid", ListenAddr: "127.0.0.1:0", StagingDir: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	p.resolver = func(context.Context, string) ([]string, error) { return nil, errors.New("no such host") }
	if err := p.Start(context.Background()); err == nil {
		t.Error("Start should fail (so the agent fails open) when the results host can't be resolved")
	}
}

func TestStartRefusesLoopbackResolution(t *testing.T) {
	t.Parallel()

	// Simulates a stale /etc/hosts pin from a prior boot resolving the results
	// host to loopback; Start must refuse so the agent fails open.
	p, err := New(Config{OrchestratorBaseURL: "https://orch.invalid", ListenAddr: "127.0.0.1:0", StagingDir: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	p.resolver = func(context.Context, string) ([]string, error) { return []string{"127.0.0.1"}, nil }
	if err := p.Start(context.Background()); err == nil {
		t.Error("Start should refuse a loopback-resolved results host (stale pin)")
	}
}

func TestNewRequiresOrchestratorURL(t *testing.T) {
	t.Parallel()

	if _, err := New(Config{}); err == nil {
		t.Error("New should require OrchestratorBaseURL")
	}
}
