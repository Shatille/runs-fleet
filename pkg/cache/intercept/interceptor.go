package intercept

import (
	"context"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/cache/blobshim"
)

// Interceptor is the on-host request router. It terminates TLS for the GitHub
// Actions results host (which is pinned to this listener via /etc/hosts) and
// routes by path: the v2 cache service and the blob shim are served locally;
// everything else — crucially the artifacts service, which shares the host,
// plus OIDC — is reverse-proxied to the real results host unchanged.
//
// Engagement is gated by `healthy`: until the agent confirms the local cache is
// ready it returns false and every request, cache included, passes through to
// GitHub. That is the fail-open default — a broken interceptor yields a cache
// miss against GitHub's own cache, never a failed job.
type Interceptor struct {
	cacheService http.Handler
	blobShim     http.Handler
	upstream     http.Handler
	healthy      func() bool
}

// NewInterceptor routes CacheService RPCs to cacheService and blob ops to
// blobShim, reverse-proxying everything else to the real results host at
// upstream. transport, when non-nil, is used for the reverse proxy; production
// injects one that dials the real GitHub IP (see PinnedTransport), bypassing
// the /etc/hosts pin that points the results host at this listener.
func NewInterceptor(cacheService, blobShim http.Handler, upstream *url.URL, transport http.RoundTripper, healthy func() bool) *Interceptor {
	rp := httputil.NewSingleHostReverseProxy(upstream)
	if transport != nil {
		rp.Transport = transport
	}
	// Return 502 on an unreachable upstream without the stdlib's stderr noise; a
	// genuine connectivity failure surfaces as the job's own artifact error.
	rp.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) {
		w.WriteHeader(http.StatusBadGateway)
	}
	return &Interceptor{cacheService: cacheService, blobShim: blobShim, upstream: rp, healthy: healthy}
}

func (i *Interceptor) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if i.engaged() {
		switch {
		case Classify(r.URL.Path) == TargetCacheService:
			i.cacheService.ServeHTTP(w, r)
			return
		case strings.HasPrefix(r.URL.Path, blobshim.PathPrefix):
			i.blobShim.ServeHTTP(w, r)
			return
		}
	}
	i.upstream.ServeHTTP(w, r)
}

// engaged also guards the local handlers being non-nil so a misconstructed
// interceptor fails open (passes through) rather than panicking mid-request.
func (i *Interceptor) engaged() bool {
	return i.healthy != nil && i.healthy() && i.cacheService != nil && i.blobShim != nil
}

// PinnedTransport returns an HTTP transport that dials addr (host:port) for any
// connection whose host is pinHost, and resolves everything else normally. The
// reverse proxy uses it so requests to the results host reach the real GitHub
// endpoint even though /etc/hosts points that hostname at this listener.
func PinnedTransport(pinHost, addr string) *http.Transport {
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	// Explicit base (the net/http DefaultTransport defaults) rather than cloning
	// http.DefaultTransport, so this is deterministic and immune to a swapped
	// DefaultTransport. TLS to the upstream still verifies against system roots.
	return &http.Transport{
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			if host, _, err := net.SplitHostPort(address); err == nil && host == pinHost {
				address = addr
			}
			return dialer.DialContext(ctx, network, address)
		},
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
}
