package cachev2

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/Shavakan/runs-fleet/pkg/cache/wire"
)

// HTTPPresigner implements Presigner by calling the orchestrator's existing v1
// cache server, which holds the S3 credentials and mints presigned URLs. The
// runner authenticates with its HMAC cache token and never touches S3 directly,
// preserving the v1 isolation property (each presigned URL is scoped by the
// orchestrator to one key).
type HTTPPresigner struct {
	client  *http.Client
	baseURL string
	token   string
}

// NewHTTPPresigner returns a Presigner that calls the orchestrator at baseURL,
// authenticating with cacheToken (the runner's provisioned HMAC token).
func NewHTTPPresigner(client *http.Client, baseURL, cacheToken string) *HTTPPresigner {
	if client == nil {
		client = http.DefaultClient
	}
	return &HTTPPresigner{client: client, baseURL: strings.TrimRight(baseURL, "/"), token: cacheToken}
}

// PresignUpload reserves a cache entry on the orchestrator and returns the
// presigned PUT URL it mints (the v1 Location header).
func (p *HTTPPresigner) PresignUpload(ctx context.Context, key, version string) (string, error) {
	body, err := json.Marshal(wire.ReserveCacheRequest{Key: key, Version: version})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/_apis/artifactcache/caches", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", contentTypeJSON)
	p.authenticate(req)

	resp, err := p.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("reserve cache entry: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("reserve cache entry: status %d", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if loc == "" {
		return "", fmt.Errorf("reserve cache entry: missing Location header")
	}
	return loc, nil
}

// PresignDownload looks up keys at version on the orchestrator and returns the
// presigned GET URL plus matched (full S3) key, or found=false on a 204 miss.
func (p *HTTPPresigner) PresignDownload(ctx context.Context, keys []string, version string) (string, string, bool, error) {
	q := url.Values{}
	q.Set("keys", strings.Join(keys, ","))
	q.Set("version", version)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+"/_apis/artifactcache/cache?"+q.Encode(), nil)
	if err != nil {
		return "", "", false, err
	}
	p.authenticate(req)

	resp, err := p.client.Do(req)
	if err != nil {
		return "", "", false, fmt.Errorf("get cache entry: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusNoContent:
		_, _ = io.Copy(io.Discard, resp.Body)
		return "", "", false, nil
	case http.StatusOK:
		var gr wire.GetCacheResponse
		if err := json.NewDecoder(resp.Body).Decode(&gr); err != nil {
			return "", "", false, fmt.Errorf("decode get cache response: %w", err)
		}
		return gr.ArchiveLocation, gr.CacheKey, true, nil
	default:
		_, _ = io.Copy(io.Discard, resp.Body)
		return "", "", false, fmt.Errorf("get cache entry: status %d", resp.StatusCode)
	}
}

func (p *HTTPPresigner) authenticate(req *http.Request) {
	if p.token != "" {
		req.Header.Set(wire.CacheTokenHeader, p.token)
	}
}
