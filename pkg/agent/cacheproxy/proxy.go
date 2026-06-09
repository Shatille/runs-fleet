// Package cacheproxy assembles and runs the on-host transparent cache
// interceptor on a runner. It wires the v2 CacheService (backed by the
// orchestrator's presign endpoint), the Azure-block-blob→S3 shim, and the
// selective reverse proxy behind a TLS listener whose leaf is signed by a
// per-instance CA. The agent engages it by pinning the results host to this
// listener (/etc/hosts) and trusting the CA (NODE_EXTRA_CA_CERTS + system
// store); if anything fails the runner is left talking to GitHub directly.
package cacheproxy

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"sync/atomic"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/agent/tlsca"
	"github.com/Shavakan/runs-fleet/pkg/cache/blobshim"
	"github.com/Shavakan/runs-fleet/pkg/cache/cachev2"
	"github.com/Shavakan/runs-fleet/pkg/cache/intercept"
)

// DefaultResultsHost is the GitHub Actions v2 results host the cache client
// talks to (confirmed live). The interceptor pins and terminates this host.
const DefaultResultsHost = "results-receiver.actions.githubusercontent.com"

// DefaultListenAddr is where the interceptor listens; the results host is
// pinned here via /etc/hosts. Binding 443 requires CAP_NET_BIND_SERVICE on the
// agent binary (granted in the AMI).
const DefaultListenAddr = "127.0.0.1:443"

// Config configures the on-host cache proxy.
type Config struct {
	// OrchestratorBaseURL is the orchestrator's base URL (RunnerConfig.CacheURL);
	// the presigner calls its v1 cache endpoints to mint presigned S3 URLs.
	OrchestratorBaseURL string
	// CacheToken is the runner's HMAC cache token (RunnerConfig.CacheToken).
	CacheToken string
	// ResultsHost overrides DefaultResultsHost.
	ResultsHost string
	// ListenAddr overrides DefaultListenAddr (tests use 127.0.0.1:0).
	ListenAddr string
	// StagingDir buffers staged blocks during uploads.
	StagingDir string
	// HTTPClient is used by the presigner and shim; defaults to a sane client.
	HTTPClient *http.Client
}

// Proxy is the running on-host cache interceptor.
type Proxy struct {
	cfg      Config
	ca       *tlsca.CA
	server   *http.Server
	listener net.Listener
	healthy  atomic.Bool
	// resolver resolves the real results-host IP before the /etc/hosts pin is
	// installed; overridable in tests.
	resolver func(ctx context.Context, host string) ([]string, error)
}

// New builds the proxy and its per-instance CA. It does not listen yet.
func New(cfg Config) (*Proxy, error) {
	if cfg.OrchestratorBaseURL == "" {
		return nil, fmt.Errorf("OrchestratorBaseURL is required")
	}
	if cfg.ResultsHost == "" {
		cfg.ResultsHost = DefaultResultsHost
	}
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = DefaultListenAddr
	}
	if cfg.StagingDir == "" {
		cfg.StagingDir = os.TempDir()
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 0}
	}
	ca, err := tlsca.NewCA()
	if err != nil {
		return nil, fmt.Errorf("generate CA: %w", err)
	}
	return &Proxy{cfg: cfg, ca: ca, resolver: net.DefaultResolver.LookupHost}, nil
}

// CACertPEM returns the per-instance CA certificate for NODE_EXTRA_CA_CERTS and
// the system trust store.
func (p *Proxy) CACertPEM() []byte { return p.ca.CertPEM() }

// Start resolves the real results-host IP (before any pin is installed), then
// starts the TLS listener and marks the proxy healthy so it begins serving the
// cache locally. The reverse proxy dials the resolved real IP, so artifacts and
// other traffic still reach GitHub despite the pin.
func (p *Proxy) Start(ctx context.Context) error {
	addrs, err := p.resolver(ctx, p.cfg.ResultsHost)
	if err != nil || len(addrs) == 0 {
		return fmt.Errorf("resolve results host %q: %w", p.cfg.ResultsHost, err)
	}
	// Guard against a stale /etc/hosts pin (e.g. a prior agent SIGKILLed before
	// teardown on a reused warm-pool instance): if the results host already
	// resolves to loopback, pinning would point the reverse proxy at our own
	// listener and loop all passthrough traffic. Refuse → the agent fails open.
	ip := net.ParseIP(addrs[0]) // addrs is non-empty: checked above
	if ip == nil || ip.IsLoopback() {
		return fmt.Errorf("results host %q resolved to %q (loopback/invalid); refusing to pin", p.cfg.ResultsHost, addrs[0])
	}
	transport := intercept.PinnedTransport(p.cfg.ResultsHost, net.JoinHostPort(addrs[0], "443"))

	presigner := cachev2.NewHTTPPresigner(p.cfg.HTTPClient, p.cfg.OrchestratorBaseURL, p.cfg.CacheToken)
	cacheMux := http.NewServeMux()
	cachev2.NewServiceV2(presigner, "https://"+p.cfg.ResultsHost, nil).RegisterRoutes(cacheMux)
	shim := blobshim.NewHandler(p.cfg.HTTPClient, p.cfg.StagingDir)
	upstream := &url.URL{Scheme: "https", Host: p.cfg.ResultsHost}
	handler := intercept.NewInterceptor(cacheMux, shim, upstream, transport, p.healthy.Load)

	leaf, err := p.ca.IssueLeaf([]string{p.cfg.ResultsHost, "*.actions.githubusercontent.com", "127.0.0.1", "localhost"})
	if err != nil {
		return fmt.Errorf("issue leaf: %w", err)
	}
	ln, err := net.Listen("tcp", p.cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", p.cfg.ListenAddr, err)
	}
	p.listener = ln
	p.server = &http.Server{
		Handler:           handler,
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{leaf}, MinVersion: tls.VersionTLS12},
		ReadHeaderTimeout: 15 * time.Second,
	}
	p.healthy.Store(true)
	go func() {
		// If the listener fails after startup, disengage so the interceptor
		// stops claiming traffic it can't serve (fail-open).
		if err := p.server.ServeTLS(ln, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "cacheproxy: TLS server stopped: %v\n", err)
			p.healthy.Store(false)
		}
	}()
	return nil
}

// Addr is the actual listen address (useful when ListenAddr used port 0).
func (p *Proxy) Addr() string {
	if p.listener == nil {
		return ""
	}
	return p.listener.Addr().String()
}

// Stop disengages (so in-flight requests pass through) and shuts the listener.
func (p *Proxy) Stop(ctx context.Context) error {
	p.healthy.Store(false)
	if p.server == nil {
		return nil
	}
	return p.server.Shutdown(ctx)
}
