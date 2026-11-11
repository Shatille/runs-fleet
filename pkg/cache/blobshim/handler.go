// Package blobshim translates the Azure Block Blob REST surface that the GitHub
// Actions v2 cache client speaks (stage block, commit block list, ranged get)
// into plain HTTP against an orchestrator-presigned S3 URL. It runs on the
// runner host; the v2 CacheService hands the client a /blob/<token> URL whose
// token is the (base64url-encoded) presigned S3 URL the orchestrator minted.
// The runner holds no S3 credentials — the presigned URL is the only capability.
package blobshim

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/Shavakan/runs-fleet/pkg/cache/wire"
	"github.com/Shavakan/runs-fleet/pkg/logging"
)

var shimLog = logging.WithComponent(logging.LogTypeCache, "blobshim")

// PathPrefix is the URL path under which the shim serves blob operations.
const PathPrefix = "/blob/"

const (
	msgBadTarget        = "bad blob target"
	msgUpstream         = "upstream error"
	msgStaging          = "staging error"
	msgNotFound         = "not found"
	msgUnsupportedComp  = "unsupported comp"
	msgMethodNotAllowed = "method not allowed"
	msgMissingBlockID   = "missing blockid"
	msgInvalidBlockList = "invalid block list"
	msgUnknownBlock     = "unknown block"
	msgInvalidBlockID   = "invalid block id"
)

// minPartSize is S3's smallest allowed multipart part, except for the last.
const minPartSize = 5 << 20

// ErrMultipartUnsupported is returned by a Multipart whose orchestrator has no
// multipart routes; the shim then stages blocks on disk as before.
var ErrMultipartUnsupported = errors.New("multipart upload unsupported")

// Multipart streams staged blocks to S3 as multipart parts through
// orchestrator-presigned URLs, so a committed blob never needs a disk copy.
type Multipart interface {
	CreateMultipart(ctx context.Context, key, version string) (uploadID string, err error)
	PresignPart(ctx context.Context, key, version, uploadID string, partNumber int32) (url string, err error)
	CompleteMultipart(ctx context.Context, key, version, uploadID string, parts []wire.MultipartPart, size int64) error
	AbortMultipart(ctx context.Context, key, version, uploadID string) error
}

// target is what a blob token carries: the presigned URL, plus the cache entry
// it belongs to when the token was minted for an upload.
type target struct {
	URL     string `json:"u"`
	Key     string `json:"k,omitempty"`
	Version string `json:"v,omitempty"`
}

// upload tracks one blob's streamed parts. An empty id means the blob is being
// staged on disk; decided records that the choice was made at its first block.
type upload struct {
	mu      sync.Mutex
	decided bool
	id      string
	parts   map[int]streamedPart
}

type streamedPart struct {
	blockID string
	part    wire.MultipartPart
	size    int64
}

// Handler serves the Azure Block Blob subset the v2 cache client uses and
// forwards the bytes to the presigned S3 URL encoded in the request path.
// Staged blocks stream to S3 as multipart parts when a Multipart is set and the
// blob qualifies; otherwise they are buffered under a per-blob directory and
// reassembled, in block-list order, on commit. Per-blob upload state lives
// from the first staged block until commit.
type Handler struct {
	client      *http.Client
	staging     string
	allowTarget func(string) bool
	onBytes     func(int64)
	multipart   Multipart
	minPartSize int64

	mu      sync.Mutex
	uploads map[string]*upload
}

// NewHandler returns a shim handler that forwards via client and buffers staged
// blocks under stagingDir. onBytes, if non-nil, is called with the object size on
// each successful store to S3 (commit or single-shot put) so callers can meter
// v2 cache write volume; it is not invoked for block staging.
func NewHandler(client *http.Client, stagingDir string, onBytes func(int64)) *Handler {
	if client == nil {
		client = http.DefaultClient
	}
	return &Handler{
		client:      client,
		staging:     stagingDir,
		allowTarget: allowedTarget,
		onBytes:     onBytes,
		minPartSize: minPartSize,
		uploads:     map[string]*upload{},
	}
}

// SetMultipart streams block uploads to S3 through m instead of staging them on disk.
func (h *Handler) SetMultipart(m Multipart) {
	h.multipart = m
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

// EncodeUploadTarget encodes an upload's presigned URL together with its cache
// key and version, which the shim needs to stream the blob as multipart parts.
func EncodeUploadTarget(presignedURL, key, version string) string {
	raw, _ := json.Marshal(target{URL: presignedURL, Key: key, Version: version})
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeTarget(token string) (target, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return target{}, fmt.Errorf("decode blob token: %w", err)
	}
	if !strings.HasPrefix(string(raw), "{") {
		return target{URL: string(raw)}, nil
	}
	var t target
	if err := json.Unmarshal(raw, &t); err != nil {
		return target{}, fmt.Errorf("decode blob token: %w", err)
	}
	return t, nil
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
		http.Error(w, msgNotFound, http.StatusNotFound)
		return
	}
	token := strings.TrimPrefix(r.URL.Path, PathPrefix)
	t, err := decodeTarget(token)
	if err != nil || !h.allowTarget(t.URL) {
		http.Error(w, msgBadTarget, http.StatusBadRequest)
		return
	}

	switch r.Method {
	case http.MethodPut:
		switch r.URL.Query().Get("comp") {
		case "block":
			h.stageBlock(w, r, token, t)
		case "blocklist":
			h.commitBlockList(w, r, token, t)
		case "":
			h.putBlob(w, r, t.URL)
		default:
			http.Error(w, msgUnsupportedComp, http.StatusBadRequest)
		}
	case http.MethodGet:
		h.getBlob(w, r, t.URL)
	case http.MethodHead:
		h.headBlob(w, r, t.URL)
	default:
		http.Error(w, msgMethodNotAllowed, http.StatusMethodNotAllowed)
	}
}

func (h *Handler) stageBlock(w http.ResponseWriter, r *http.Request, token string, t target) {
	blockID := r.URL.Query().Get("blockid")
	if blockID == "" {
		http.Error(w, msgMissingBlockID, http.StatusBadRequest)
		return
	}
	index, indexed := blockIndex(blockID)
	u := h.upload(token)
	u.mu.Lock()
	if !u.decided {
		u.decided = true
		if h.multipart != nil && t.Key != "" && indexed && r.ContentLength >= h.minPartSize {
			id, err := h.multipart.CreateMultipart(r.Context(), t.Key, t.Version)
			switch {
			case err == nil:
				u.id = id
				u.parts = map[int]streamedPart{}
			case !errors.Is(err, ErrMultipartUnsupported):
				shimLog.Warn(r.Context(), "multipart create failed; staging blocks on disk",
					slog.String("key", t.Key), slog.String("error", err.Error()))
			}
		}
	}
	uploadID := u.id
	u.mu.Unlock()

	if uploadID == "" {
		h.stageBlockOnDisk(w, r, token, blockID)
		return
	}
	if !indexed || r.ContentLength < 0 {
		http.Error(w, msgInvalidBlockID, http.StatusBadRequest)
		return
	}
	partNumber := int32(index + 1)
	partURL, err := h.multipart.PresignPart(r.Context(), t.Key, t.Version, uploadID, partNumber)
	if err != nil {
		shimLog.Warn(r.Context(), "multipart part presign failed",
			slog.String("key", t.Key), slog.Int("part", int(partNumber)), slog.String("error", err.Error()))
		http.Error(w, msgUpstream, http.StatusBadGateway)
		return
	}
	if !h.allowTarget(partURL) {
		shimLog.Warn(r.Context(), "multipart part URL rejected by target guard",
			slog.String("key", t.Key), slog.Int("part", int(partNumber)))
		http.Error(w, msgUpstream, http.StatusBadGateway)
		return
	}
	status, etag, err := h.put(r, partURL, r.Body, r.ContentLength)
	if err != nil || status/100 != 2 || etag == "" {
		shimLog.Warn(r.Context(), "multipart part upload failed",
			slog.String("key", t.Key), slog.Int("part", int(partNumber)), slog.Int("status", status), slog.Any("error", err))
		http.Error(w, msgUpstream, http.StatusBadGateway)
		return
	}
	u.mu.Lock()
	u.parts[index] = streamedPart{blockID: blockID, part: wire.MultipartPart{PartNumber: partNumber, ETag: etag}, size: r.ContentLength}
	u.mu.Unlock()
	w.WriteHeader(http.StatusCreated)
}

func (h *Handler) stageBlockOnDisk(w http.ResponseWriter, r *http.Request, token, blockID string) {
	dir := h.blockDir(token)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		http.Error(w, msgStaging, http.StatusInternalServerError)
		return
	}
	path := filepath.Join(dir, encodeBlockID(blockID))
	f, err := os.Create(path)
	if err != nil {
		http.Error(w, msgStaging, http.StatusInternalServerError)
		return
	}
	_, copyErr := io.Copy(f, r.Body)
	closeErr := f.Close()
	if copyErr != nil || closeErr != nil {
		_ = os.Remove(path)
		http.Error(w, msgStaging, http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

func (h *Handler) upload(token string) *upload {
	h.mu.Lock()
	defer h.mu.Unlock()
	u, ok := h.uploads[token]
	if !ok {
		u = &upload{}
		h.uploads[token] = u
	}
	return u
}

func (h *Handler) takeUpload(token string) *upload {
	h.mu.Lock()
	defer h.mu.Unlock()
	u := h.uploads[token]
	delete(h.uploads, token)
	return u
}

func (h *Handler) commitBlockList(w http.ResponseWriter, r *http.Request, token string, t target) {
	ids, err := parseBlockList(r.Body)
	if err != nil {
		http.Error(w, msgInvalidBlockList, http.StatusBadRequest)
		return
	}
	if u := h.takeUpload(token); u != nil {
		u.mu.Lock()
		streamed := u.id != ""
		u.mu.Unlock()
		if streamed {
			h.completeUpload(w, r, t, u, ids)
			return
		}
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
			http.Error(w, msgUnknownBlock, http.StatusBadRequest)
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

	status, err := h.forwardPut(r, t.URL, io.MultiReader(readers...), total)
	cleanup()
	_ = os.RemoveAll(dir) // always drop staged blocks, success or failure
	if err != nil || status/100 != 2 {
		http.Error(w, msgUpstream, http.StatusBadGateway)
		return
	}
	h.recordBytes(total)
	w.WriteHeader(http.StatusCreated)
}

// completeUpload assembles a streamed blob. S3 orders parts by part number, so
// the committed list must name every uploaded block in ascending index order.
func (h *Handler) completeUpload(w http.ResponseWriter, r *http.Request, t target, u *upload, ids []string) {
	u.mu.Lock()
	uploadID := u.id
	parts := make([]wire.MultipartPart, 0, len(ids))
	var total int64
	status, msg := 0, ""
	for i, id := range ids {
		index, ok := blockIndex(id)
		sp, staged := u.parts[index]
		switch {
		case !ok || !staged || sp.blockID != id:
			status, msg = http.StatusBadRequest, msgUnknownBlock
		case i > 0 && sp.part.PartNumber <= parts[i-1].PartNumber:
			status, msg = http.StatusBadRequest, msgInvalidBlockList
		}
		if status != 0 {
			break
		}
		parts = append(parts, sp.part)
		total += sp.size
	}
	u.mu.Unlock()

	if status == 0 {
		if err := h.multipart.CompleteMultipart(r.Context(), t.Key, t.Version, uploadID, parts, total); err != nil {
			shimLog.Warn(r.Context(), "multipart complete failed",
				slog.String("key", t.Key), slog.Int("parts", len(parts)), slog.String("error", err.Error()))
			status, msg = http.StatusBadGateway, msgUpstream
		}
	}
	if status != 0 {
		_ = h.multipart.AbortMultipart(r.Context(), t.Key, t.Version, uploadID)
		http.Error(w, msg, status)
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
	status, _, err := h.put(r, target, body, contentLength)
	return status, err
}

// put sends body to a presigned URL and returns the response status and ETag.
func (h *Handler) put(r *http.Request, target string, body io.Reader, contentLength int64) (int, string, error) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPut, target, body)
	if err != nil {
		return 0, "", err
	}
	req.ContentLength = contentLength
	resp, err := h.client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, resp.Header.Get("ETag"), nil
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
		http.Error(w, msgNotFound, http.StatusNotFound)
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

// blockIndex recovers the block's position from an @azure/storage-blob block
// ID: base64 of a 36-character UUID followed by the index zero-padded to 12
// digits. Indexes beyond S3's part-number ceiling are rejected.
func blockIndex(id string) (int, bool) {
	raw, err := base64.StdEncoding.DecodeString(id)
	if err != nil || len(raw) != 48 {
		return 0, false
	}
	n := 0
	for _, c := range raw[36:] {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	if n >= 10000 {
		return 0, false
	}
	return n, true
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
