package intercept

import (
	"net/http"
	"net/http/httputil"
	"net/url"
)

// Proxy is the on-host interceptor's request router. It terminates TLS for the
// GitHub Actions results host and, when local caching is engaged, serves the
// cache service from S3 while reverse-proxying everything else (artifacts,
// OIDC) to the real results host unchanged.
//
// Engagement is gated by `enabled`: until the agent confirms the local cache is
// healthy it returns false and every request — cache included — passes through
// to GitHub. That is the fail-open default: a broken interceptor yields a cache
// miss against GitHub's own cache, never a failed job.
type Proxy struct {
	local    http.Handler
	upstream http.Handler
	enabled  func() bool
}

// NewProxy routes cache-service requests to local and everything else to the
// real results host at upstream. transport, when non-nil, dials upstream
// directly (production injects a dialer that resolves the real GitHub IP,
// bypassing the host pin that points the results host at this listener).
func NewProxy(local http.Handler, upstream *url.URL, transport http.RoundTripper, enabled func() bool) *Proxy {
	rp := httputil.NewSingleHostReverseProxy(upstream)
	if transport != nil {
		rp.Transport = transport
	}
	return &Proxy{local: local, upstream: rp, enabled: enabled}
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if p.enabled() && Classify(r.URL.Path) == TargetCacheService {
		p.local.ServeHTTP(w, r)
		return
	}
	p.upstream.ServeHTTP(w, r)
}
