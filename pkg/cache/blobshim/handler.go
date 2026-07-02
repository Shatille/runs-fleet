// Package blobshim translates the Azure Block Blob REST surface that the GitHub
// Actions v2 cache client speaks (stage block, commit block list, ranged get)
// into plain HTTP against an orchestrator-presigned S3 URL. It runs on the
// runner host; the v2 CacheService hands the client a /blob/<token> URL whose
// token is the (base64url-encoded) presigned S3 URL the orchestrator minted.
// The runner holds no S3 credentials — the presigned URL is the only capability.
package blobshim

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// PathPrefix is the URL path under which the shim serves blob operations.
const PathPrefix = "/blob/"

const (
	msgBadTarget = "bad blob target"
	msgUpstream  = "upstream error"
	msgStaging   = "staging error"
)

// Handler serves the Azure Block Blob subset the v2 cache client uses and
// forwards the bytes to the presigned S3 URL encoded in the request path.
// Staged blocks are buffered under a per-blob directory and reassembled, in
// block-list order, on commit. The handler is stateless across cache entries.
type Handler struct {
	client      *http.Client
	staging     string
	allowTarget func(string) bool
	onBytes     func(int64)
}

// NewHandler returns a shim handler that forwards via client and buffers staged
// blocks under stagingDir. onBytes, if non-nil, is called with the object size on
// each successful store to S3 (commit or single-shot put) so callers can meter
// v2 cache write volume; it is not invoked for block staging.
func NewHandler(client *http.Client, stagingDir string, onBytes func(int64)) *Handler {
	if client == nil {
		client = http.DefaultClient
	}
	return &Handler{client: client, staging: stagingDir, allowTarget: allowedTarget, onBytes: onBytes}
}

// recordBytes reports n bytes stored (nil-safe).
func (h *Handler) recordBytes(n int64) {
	if h.onBytes != nil && n > 0 {
		h.onBytes(n)
	}
}

// EncodeTarget encodes a presigned URL into a path-safe blob token.
func EncodeTarget(presignedURL string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(presignedURL))
}

func decodeTarget(token string) (string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return "", fmt.Errorf("decode blob token: %w", err)
	}
	return string(raw), nil
}

// allowedTarget guards the forward target, which is decoded from the
// client-controllable blob token: it must be an https URL to a public host.
// This stops the shim from being used as an SSRF relay to loopback/link-local
// (e.g. the instance metadata endpoint) or private addresses. The orchestrator
// only ever mints https S3 URLs whose host is a public name.
func allowedTarget(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return false
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
			return false
		}
	}
	return true
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.URL.Path, PathPrefix) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	token := strings.TrimPrefix(r.URL.Path, PathPrefix)
	target, err := decodeTarget(token)
	if err != nil || !h.allowTarget(target) {
		http.Error(w, msgBadTarget, http.StatusBadRequest)
		return
	}

	switch r.Method {
	case http.MethodPut:
		switch r.URL.Query().Get("comp") {
		case "block":
			h.stageBlock(w, r, token)
		case "blocklist":
			h.commitBlockList(w, r, token, target)
		case "":
			h.putBlob(w, r, target)
		default:
			http.Error(w, "unsupported comp", http.StatusBadRequest)
		}
	case http.MethodGet:
		h.getBlob(w, r, target)
	case http.MethodHead:
		h.headBlob(w, r, target)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *Handler) stageBlock(w http.ResponseWriter, r *http.Request, token string) {
	blockID := r.URL.Query().Get("blockid")
	if blockID == "" {
		http.Error(w, "missing blockid", http.StatusBadRequest)
		return
	}
	dir := h.blockDir(token)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		http.Error(w, msgStaging, http.StatusInternalServerError)
		return
	}
	f, err := os.Create(filepath.Join(dir, encodeBlockID(blockID)))
	if err != nil {
		http.Error(w, msgStaging, http.StatusInternalServerError)
		return
	}
	defer func() { _ = f.Close() }()
	if _, err := io.Copy(f, r.Body); err != nil {
		http.Error(w, msgStaging, http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

func (h *Handler) commitBlockList(w http.ResponseWriter, r *http.Request, token, target string) {
	ids, err := parseBlockList(r.Body)
	if err != nil {
		http.Error(w, "invalid block list", http.StatusBadRequest)
		return
	}
	dir := h.blockDir(token)
	files := make([]*os.File, 0, len(ids))
	readers := make([]io.Reader, 0, len(ids))
	var total int64
	cleanup := func() {
		for _, f := range files {
			_ = f.Close()
		}
	}
	for _, id := range ids {
		f, openErr := os.Open(filepath.Join(dir, encodeBlockID(id)))
		if openErr != nil {
			cleanup()
			http.Error(w, "unknown block", http.StatusBadRequest)
			return
		}
		info, statErr := f.Stat()
		if statErr != nil {
			_ = f.Close()
			cleanup()
			http.Error(w, msgStaging, http.StatusInternalServerError)
			return
		}
		total += info.Size()
		files = append(files, f)
		readers = append(readers, f)
	}

	status, err := h.forwardPut(r, target, io.MultiReader(readers...), total)
	cleanup()
	_ = os.RemoveAll(dir) // always drop staged blocks, success or failure
	if err != nil || status/100 != 2 {
		http.Error(w, msgUpstream, http.StatusBadGateway)
		return
	}
	h.recordBytes(total)
	w.WriteHeader(http.StatusCreated)
}

func (h *Handler) putBlob(w http.ResponseWriter, r *http.Request, target string) {
	// The Azure block-blob client always sets Content-Length on a single-shot
	// Put Blob, so r.ContentLength is known here. A chunked (-1) request would
	// be forwarded length-unknown and S3 would reject the presigned PUT (411),
	// surfacing as the 502 below.
	status, err := h.forwardPut(r, target, r.Body, r.ContentLength)
	if err != nil || status/100 != 2 {
		http.Error(w, msgUpstream, http.StatusBadGateway)
		return
	}
	// Only meter a known length. A chunked (-1) body has no declared size to
	// count; it is not the Azure client's normal single-shot path and S3 rejects
	// it upstream anyway, so skip rather than record a bogus size.
	if r.ContentLength >= 0 {
		h.recordBytes(r.ContentLength)
	}
	w.WriteHeader(http.StatusCreated)
}

// forwardPut PUTs body to the presigned target. contentLength must be set (-1
// is rejected by S3 presigned PUTs), so callers pass the known size.
func (h *Handler) forwardPut(r *http.Request, target string, body io.Reader, contentLength int64) (int, error) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPut, target, body)
	if err != nil {
		return 0, err
	}
	req.ContentLength = contentLength
	resp, err := h.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

func (h *Handler) getBlob(w http.ResponseWriter, r *http.Request, target string) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target, nil)
	if err != nil {
		http.Error(w, msgUpstream, http.StatusBadGateway)
		return
	}
	if rng := r.Header.Get("Range"); rng != "" {
		req.Header.Set("Range", rng)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		http.Error(w, msgUpstream, http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	copyHeader(w, resp, "Content-Length", "Content-Range", "ETag", "Content-Type", "Accept-Ranges", "Last-Modified")
	w.Header().Set("x-ms-blob-type", "BlockBlob")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// headBlob answers an Azure getProperties by issuing a 0-0 ranged GET to the
// presigned URL (S3 presigned GET URLs are signed for GET, not HEAD) and
// reporting the total size from the Content-Range.
func (h *Handler) headBlob(w http.ResponseWriter, r *http.Request, target string) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target, nil)
	if err != nil {
		http.Error(w, msgUpstream, http.StatusBadGateway)
		return
	}
	req.Header.Set("Range", "bytes=0-0")
	resp, err := h.client.Do(req)
	if err != nil {
		http.Error(w, msgUpstream, http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	// S3 answers a 0-0 ranged GET with 206 + Content-Range on a non-empty
	// object, and 416 on a zero-byte object (a valid, if rare, cache). Treat
	// 416 as size 0; any other non-2xx is a genuine miss.
	if resp.StatusCode != http.StatusRequestedRangeNotSatisfiable && resp.StatusCode/100 != 2 {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	size := int64(0)
	if resp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		size = headBlobSize(resp)
	}
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	copyHeader(w, resp, "ETag", "Content-Type", "Last-Modified")
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("x-ms-blob-type", "BlockBlob")
	w.WriteHeader(http.StatusOK)
}

func copyHeader(w http.ResponseWriter, resp *http.Response, keys ...string) {
	for _, k := range keys {
		if v := resp.Header.Get(k); v != "" {
			w.Header().Set(k, v)
		}
	}
}

// headBlobSize derives the object's total size from a 0-0 ranged GET response:
// the Content-Range total when present (a 206), else the Content-Length (a 200
// where the server ignored the range), defaulting to 0.
func headBlobSize(resp *http.Response) int64 {
	if total, ok := totalFromContentRange(resp.Header.Get("Content-Range")); ok {
		return total
	}
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		if n, err := strconv.ParseInt(cl, 10, 64); err == nil {
			return n
		}
	}
	return 0
}

// totalFromContentRange parses the total size out of "bytes 0-0/12345".
func totalFromContentRange(cr string) (int64, bool) {
	_, total, ok := strings.Cut(cr, "/")
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(total), 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

func (h *Handler) blockDir(token string) string {
	sum := sha256.Sum256([]byte(token))
	return filepath.Join(h.staging, hex.EncodeToString(sum[:]))
}

// encodeBlockID maps an Azure block ID (itself base64, may contain / + =) to a
// filesystem-safe filename.
func encodeBlockID(id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(id))
}

// blockList mirrors the Azure Put Block List XML body. Block IDs may appear
// under Committed, Uncommitted, or Latest; @azure/storage-blob emits Latest for
// freshly staged blocks. Order across elements is preserved.
type blockList struct {
	XMLName xml.Name `xml:"BlockList"`
	Blocks  []block  `xml:",any"`
}

type block struct {
	ID string `xml:",chardata"`
}

func parseBlockList(r io.Reader) ([]string, error) {
	var bl blockList
	if err := xml.NewDecoder(r).Decode(&bl); err != nil {
		return nil, fmt.Errorf("decode block list: %w", err)
	}
	ids := make([]string, 0, len(bl.Blocks))
	for _, b := range bl.Blocks {
		if id := strings.TrimSpace(b.ID); id != "" {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("empty block list")
	}
	return ids, nil
}
