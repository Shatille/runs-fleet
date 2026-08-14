package mirrorproxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type staticTokens struct {
	token string
	err   error
}

func (s staticTokens) Token(context.Context) (string, error) { return s.token, s.err }

type upstreamCall struct {
	method string
	path   string
	query  string
	header http.Header
}

func newUpstream(t *testing.T, status int, body string, respHeader map[string]string) (*httptest.Server, *[]upstreamCall) {
	t.Helper()
	var calls []upstreamCall
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, upstreamCall{
			method: r.Method,
			path:   r.URL.Path,
			query:  r.URL.RawQuery,
			header: r.Header.Clone(),
		})
		for k, v := range respHeader {
			w.Header().Set(k, v)
		}
		w.WriteHeader(status)
		if r.Method != http.MethodHead {
			_, _ = io.WriteString(w, body)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func newTestHandler(t *testing.T, endpoint string, tokens TokenSource) *Handler {
	t.Helper()
	h, err := New(endpoint, tokens)
	if err != nil {
		t.Fatalf("New(%q) = %v", endpoint, err)
	}
	return h
}

const wantBasicAuth = "Basic tok"

func TestNew_RejectsBadEndpoints(t *testing.T) {
	for _, tc := range []string{
		"",
		"123456789012.dkr.ecr.ap-northeast-1.amazonaws.com/docker-hub",
		"ftp://host/ns",
		"https://host",
		"https://host/",
	} {
		if _, err := New(tc, staticTokens{token: "tok"}); err == nil {
			t.Errorf("New(%q) accepted, want error", tc)
		}
	}
}

func TestServeHTTP_RewritesManifestPathOntoNamespace(t *testing.T) {
	up, calls := newUpstream(t, http.StatusOK, `{"schemaVersion":2}`, map[string]string{
		"Docker-Content-Digest": "sha256:abc",
		"Content-Type":          "application/vnd.oci.image.manifest.v1+json",
	})
	h := newTestHandler(t, up.URL+"/docker-hub", staticTokens{token: "tok"})

	req := httptest.NewRequest(http.MethodGet, "/v2/library/mysql/manifests/8.0?ns=docker.io", nil)
	req.Header.Set("Accept", "application/vnd.oci.image.manifest.v1+json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if len(*calls) != 1 {
		t.Fatalf("upstream calls = %d, want 1", len(*calls))
	}
	got := (*calls)[0]
	if got.path != "/v2/docker-hub/library/mysql/manifests/8.0" {
		t.Errorf("upstream path = %q", got.path)
	}
	if got.query != "ns=docker.io" {
		t.Errorf("upstream query = %q", got.query)
	}
	if got.header.Get("Accept") != "application/vnd.oci.image.manifest.v1+json" {
		t.Errorf("Accept not forwarded, got %q", got.header.Get("Accept"))
	}
	if got.header.Get("Authorization") != wantBasicAuth {
		t.Errorf("Authorization = %q, want %q", got.header.Get("Authorization"), wantBasicAuth)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
	if rec.Header().Get("Docker-Content-Digest") != "sha256:abc" {
		t.Errorf("Docker-Content-Digest not relayed, got %q", rec.Header().Get("Docker-Content-Digest"))
	}
	if body := rec.Body.String(); body != `{"schemaVersion":2}` {
		t.Errorf("body = %q", body)
	}
}

func TestServeHTTP_NamespacedImageKeepsItsOwnPath(t *testing.T) {
	up, calls := newUpstream(t, http.StatusOK, "", nil)
	h := newTestHandler(t, up.URL+"/docker-hub", staticTokens{token: "tok"})

	req := httptest.NewRequest(http.MethodGet, "/v2/moby/buildkit/manifests/buildx-stable-1", nil)
	h.ServeHTTP(httptest.NewRecorder(), req)

	if got := (*calls)[0].path; got != "/v2/docker-hub/moby/buildkit/manifests/buildx-stable-1" {
		t.Errorf("upstream path = %q", got)
	}
}

func TestServeHTTP_PingGoesToRegistryRootNotNamespace(t *testing.T) {
	up, calls := newUpstream(t, http.StatusOK, "{}", nil)
	h := newTestHandler(t, up.URL+"/docker-hub", staticTokens{token: "tok"})

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v2/", nil))

	if got := (*calls)[0].path; got != "/v2/" {
		t.Errorf("ping path = %q, want /v2/", got)
	}
	if auth := (*calls)[0].header.Get("Authorization"); auth != wantBasicAuth {
		t.Errorf("ping Authorization = %q", auth)
	}
}

func TestServeHTTP_HeadIsForwardedAsHead(t *testing.T) {
	up, calls := newUpstream(t, http.StatusOK, "", map[string]string{"Docker-Content-Digest": "sha256:def"})
	h := newTestHandler(t, up.URL+"/docker-hub", staticTokens{token: "tok"})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodHead, "/v2/library/redis/manifests/7", nil))

	if got := (*calls)[0].method; got != http.MethodHead {
		t.Errorf("upstream method = %q", got)
	}
	if rec.Header().Get("Docker-Content-Digest") != "sha256:def" {
		t.Errorf("HEAD digest header not relayed")
	}
}

func TestServeHTTP_RejectsNonPullMethods(t *testing.T) {
	up, calls := newUpstream(t, http.StatusOK, "", nil)
	h := newTestHandler(t, up.URL+"/docker-hub", staticTokens{token: "tok"})

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, "/v2/library/mysql/blobs/uploads/", nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s status = %d, want 405", method, rec.Code)
		}
	}
	if len(*calls) != 0 {
		t.Errorf("non-pull methods reached upstream: %d calls", len(*calls))
	}
}

func TestServeHTTP_DotSegmentsNeverReachUpstream(t *testing.T) {
	// Any local process can reach the proxy, and the injected ECR token is
	// registry-wide — a ".." that survived to ECR would escape the
	// pull-through namespace. net/http preserves dot-segments on the wire, so
	// the proxy must refuse them itself.
	up, calls := newUpstream(t, http.StatusOK, "", nil)
	h := newTestHandler(t, up.URL+"/docker-hub", staticTokens{token: "tok"})

	for _, path := range []string{
		"/v2/../../other-repo/manifests/latest",
		"/v2/library/../../secrets/blobs/sha256:abc",
		"/v2/..",
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "http://mirror.invalid", nil)
		req.URL.Path = path
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", path, rec.Code)
		}
	}
	if len(*calls) != 0 {
		t.Errorf("dot-segment path reached upstream: %+v", *calls)
	}
}

func TestServeHTTP_NonV2PathIs404(t *testing.T) {
	up, calls := newUpstream(t, http.StatusOK, "", nil)
	h := newTestHandler(t, up.URL+"/docker-hub", staticTokens{token: "tok"})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz-not-registry", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	if len(*calls) != 0 {
		t.Errorf("non-v2 path reached upstream")
	}
}

func TestServeHTTP_TokenErrorIs502(t *testing.T) {
	up, calls := newUpstream(t, http.StatusOK, "", nil)
	h := newTestHandler(t, up.URL+"/docker-hub", staticTokens{err: errors.New("imds down")})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v2/library/mysql/manifests/8.0", nil))
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
	if len(*calls) != 0 {
		t.Errorf("token failure still reached upstream")
	}
}

func TestServeHTTP_UpstreamUnreachableIs502(t *testing.T) {
	up, _ := newUpstream(t, http.StatusOK, "", nil)
	endpoint := up.URL + "/docker-hub"
	up.Close()
	h := newTestHandler(t, endpoint, staticTokens{token: "tok"})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v2/library/mysql/manifests/8.0", nil))
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
}

func TestServeHTTP_RedirectIsPassedThroughNotFollowed(t *testing.T) {
	final := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("redirect target was fetched by the proxy; it must be passed through to the client")
	}))
	t.Cleanup(final.Close)
	up, _ := newUpstream(t, http.StatusTemporaryRedirect, "", map[string]string{"Location": final.URL + "/blob"})
	h := newTestHandler(t, up.URL+"/docker-hub", staticTokens{token: "tok"})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v2/library/mysql/blobs/sha256:abc", nil))

	if rec.Code != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d, want 307", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != final.URL+"/blob" {
		t.Errorf("Location = %q", loc)
	}
}

func TestServeHTTP_ClientAuthorizationIsReplacedNotForwarded(t *testing.T) {
	up, calls := newUpstream(t, http.StatusOK, "", nil)
	h := newTestHandler(t, up.URL+"/docker-hub", staticTokens{token: "tok"})

	req := httptest.NewRequest(http.MethodGet, "/v2/library/mysql/manifests/8.0", nil)
	req.Header.Set("Authorization", "Bearer client-supplied")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if got := (*calls)[0].header.Get("Authorization"); got != wantBasicAuth {
		t.Errorf("Authorization = %q, client value must be replaced", got)
	}
}

func TestServeHTTP_HopByHopHeadersAreDropped(t *testing.T) {
	up, calls := newUpstream(t, http.StatusOK, "", nil)
	h := newTestHandler(t, up.URL+"/docker-hub", staticTokens{token: "tok"})

	req := httptest.NewRequest(http.MethodGet, "/v2/library/mysql/manifests/8.0", nil)
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Proxy-Connection", "keep-alive")
	h.ServeHTTP(httptest.NewRecorder(), req)

	hdr := (*calls)[0].header
	if hdr.Get("Proxy-Connection") != "" {
		t.Errorf("hop-by-hop Proxy-Connection forwarded")
	}
}

func TestServeHTTP_UpstreamErrorStatusIsRelayed(t *testing.T) {
	up, _ := newUpstream(t, http.StatusNotFound, `{"errors":[{"code":"MANIFEST_UNKNOWN"}]}`, nil)
	h := newTestHandler(t, up.URL+"/docker-hub", staticTokens{token: "tok"})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v2/library/nosuch/manifests/1", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want upstream 404 relayed", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "MANIFEST_UNKNOWN") {
		t.Errorf("upstream error body not relayed: %q", rec.Body.String())
	}
}
