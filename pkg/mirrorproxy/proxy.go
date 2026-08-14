// Package mirrorproxy serves a local, pull-only Docker registry mirror that
// forwards Docker Hub requests onto an ECR pull-through cache endpoint.
// dockerd sends no credentials to a mirror and BuildKit cannot attach a path
// prefix to one, while ECR refuses anonymous pulls and namespaces cached
// images under the rule prefix — so something local has to translate. This is
// that translator: it rewrites /v2/<name>/... onto /v2/<prefix>/<name>/... and
// injects credentials fetched from the instance role, leaving no credential
// material on disk.
package mirrorproxy

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// TokenSource supplies the registry Authorization credential. ECR's
// GetAuthorizationToken output is already basic-auth formatted, so Token's
// result is sent as "Basic <token>" verbatim.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

// Handler is the mirror: GET/HEAD under /v2/, nothing else. namespaces maps
// the containerd-style ns query value (absent → docker.io) to a pull-through
// rule prefix.
type Handler struct {
	upstream   *url.URL
	namespaces map[string]string
	tokens     TokenSource
	client     *http.Client
	log        *slog.Logger
}

// New builds a Handler for an endpoint of the form
// http(s)://<registry-host>/<rule-prefix>. The path is required — it is the
// namespace every repository path is rewritten under.
func New(endpoint string, tokens TokenSource) (*Handler, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("invalid endpoint %q: %w", endpoint, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("endpoint %q must be an http(s) URL", endpoint)
	}
	prefix := strings.Trim(u.Path, "/")
	if u.Host == "" || prefix == "" {
		return nil, fmt.Errorf("endpoint %q must include a registry host and a pull-through rule prefix path", endpoint)
	}
	return &Handler{
		upstream:   &url.URL{Scheme: u.Scheme, Host: u.Host},
		namespaces: map[string]string{"docker.io": prefix},
		tokens:     tokens,
		client: &http.Client{
			// Relay blob 307s to the caller instead of following them, so
			// image bytes stream from object storage directly to dockerd.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
			Timeout: 5 * time.Minute,
		},
		log: slog.Default(),
	}, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "mirror is pull-only", http.StatusMethodNotAllowed)
		return
	}
	target, ok := h.rewrite(r.URL)
	if !ok {
		http.NotFound(w, r)
		return
	}

	token, err := h.tokens.Token(r.Context())
	if err != nil {
		// 5xx makes dockerd/BuildKit fall back to Docker Hub — the designed
		// degradation for every failure on this path.
		h.log.Warn("mirror credential fetch failed; client will fall back", "error", err)
		http.Error(w, "mirror credential unavailable", http.StatusBadGateway)
		return
	}

	req, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), nil)
	if err != nil {
		http.Error(w, "mirror request build failed", http.StatusBadGateway)
		return
	}
	copyHeaders(req.Header, r.Header)
	req.Header.Set("Authorization", "Basic "+token)

	resp, err := h.client.Do(req)
	if err != nil {
		h.log.Warn("mirror upstream unreachable; client will fall back", "target", target.Host, "error", err)
		http.Error(w, "mirror upstream unreachable", http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(w, resp.Body); err != nil {
		h.log.Warn("mirror response copy interrupted", "path", r.URL.Path, "error", err)
	}
}

// AddRules merges discovered ns→prefix mappings; the endpoint's explicit
// declaration wins on conflict.
func (h *Handler) AddRules(rules map[string]string) {
	for ns, prefix := range rules {
		if _, exists := h.namespaces[ns]; !exists {
			h.namespaces[ns] = prefix
		}
	}
}

// rewrite maps a mirror request onto the cache; refusals make the client
// fall back to the real registry. Dot-segments are refused because the port
// is reachable by any local process and the token is registry-wide, so ".."
// surviving to ECR would escape the namespace; ns is stripped because ECR is
// not itself a proxy.
func (h *Handler) rewrite(reqURL *url.URL) (*url.URL, bool) {
	path := reqURL.Path
	for _, seg := range strings.Split(path, "/") {
		if seg == ".." {
			return nil, false
		}
	}
	query := reqURL.Query()
	ns := query.Get("ns")
	if ns == "" {
		ns = "docker.io"
	}
	prefix, known := h.namespaces[ns]
	if !known {
		return nil, false
	}
	query.Del("ns")

	target := *h.upstream
	target.RawQuery = query.Encode()
	const apiRoot = "/v2/"
	switch {
	case path == "/v2" || path == apiRoot:
		target.Path = apiRoot
	case strings.HasPrefix(path, apiRoot):
		target.Path = apiRoot + prefix + strings.TrimPrefix(path, "/v2")
	default:
		return nil, false
	}
	return &target, true
}

// hopByHopHeaders per RFC 9110, plus Authorization: the client's credential is
// for the mirror, not ECR, and is replaced wholesale.
var hopByHopHeaders = map[string]bool{
	"Authorization":       true,
	"Connection":          true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Proxy-Connection":    true,
	"Te":                  true,
	"Trailer":             true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
}

func copyHeaders(dst, src http.Header) {
	for k, vs := range src {
		if hopByHopHeaders[http.CanonicalHeaderKey(k)] {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}
