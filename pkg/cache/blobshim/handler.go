package blobshim

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// PathPrefix is the URL path under which the shim serves blob operations.
// The v2 CacheService issues URLs of the form <base><PathPrefix><token>.
const PathPrefix = "/blob/"

const (
	msgForbidden = "forbidden"
	msgNotFound  = "not found"
	msgStaging   = "staging error"
)

// Handler serves the Azure Block Blob REST subset the v2 cache client uses and
// translates it to ObjectStore operations. Staged blocks are buffered under a
// per-key directory and reassembled, in block-list order, on commit.
type Handler struct {
	store   ObjectStore
	signer  *Signer
	staging string
	now     func() time.Time
}

// NewHandler returns a shim handler backed by store, validating capability
// tokens with signer and buffering staged blocks under stagingDir.
func NewHandler(store ObjectStore, signer *Signer, stagingDir string) *Handler {
	return &Handler{store: store, signer: signer, staging: stagingDir, now: time.Now}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.URL.Path, PathPrefix) {
		http.Error(w, msgNotFound, http.StatusNotFound)
		return
	}
	token := strings.TrimPrefix(r.URL.Path, PathPrefix)
	op, s3Key, err := h.signer.Verify(token, h.now())
	if err != nil {
		http.Error(w, msgForbidden, http.StatusForbidden)
		return
	}

	switch r.Method {
	case http.MethodPut:
		if op != OpWrite {
			http.Error(w, msgForbidden, http.StatusForbidden)
			return
		}
		switch r.URL.Query().Get("comp") {
		case "block":
			h.stageBlock(w, r, s3Key)
		case "blocklist":
			h.commitBlockList(w, r, s3Key)
		case "":
			h.putBlob(w, r, s3Key)
		default:
			http.Error(w, "unsupported comp", http.StatusBadRequest)
		}
	case http.MethodGet:
		if op != OpRead {
			http.Error(w, msgForbidden, http.StatusForbidden)
			return
		}
		h.getBlob(w, r, s3Key)
	case http.MethodHead:
		if op != OpRead {
			http.Error(w, msgForbidden, http.StatusForbidden)
			return
		}
		h.headBlob(w, r, s3Key)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *Handler) stageBlock(w http.ResponseWriter, r *http.Request, s3Key string) {
	blockID := r.URL.Query().Get("blockid")
	if blockID == "" {
		http.Error(w, "missing blockid", http.StatusBadRequest)
		return
	}
	dir := h.blockDir(s3Key)
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

func (h *Handler) commitBlockList(w http.ResponseWriter, r *http.Request, s3Key string) {
	ids, err := parseBlockList(r.Body)
	if err != nil {
		http.Error(w, "invalid block list", http.StatusBadRequest)
		return
	}
	dir := h.blockDir(s3Key)
	files := make([]*os.File, 0, len(ids))
	readers := make([]io.Reader, 0, len(ids))
	cleanup := func() {
		for _, f := range files {
			_ = f.Close()
		}
	}
	for _, id := range ids {
		f, err := os.Open(filepath.Join(dir, encodeBlockID(id)))
		if err != nil {
			cleanup()
			http.Error(w, "unknown block", http.StatusBadRequest)
			return
		}
		files = append(files, f)
		readers = append(readers, f)
	}

	if err := h.store.Put(r.Context(), s3Key, io.MultiReader(readers...)); err != nil {
		cleanup()
		http.Error(w, "upload error", http.StatusInternalServerError)
		return
	}
	cleanup()
	_ = os.RemoveAll(dir)
	w.WriteHeader(http.StatusCreated)
}

func (h *Handler) putBlob(w http.ResponseWriter, r *http.Request, s3Key string) {
	if err := h.store.Put(r.Context(), s3Key, r.Body); err != nil {
		http.Error(w, "upload error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

func (h *Handler) getBlob(w http.ResponseWriter, r *http.Request, s3Key string) {
	res, err := h.store.Get(r.Context(), s3Key, r.Header.Get("Range"))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			http.Error(w, msgNotFound, http.StatusNotFound)
			return
		}
		http.Error(w, "read error", http.StatusInternalServerError)
		return
	}
	defer func() { _ = res.Body.Close() }()
	setBlobHeaders(w, res.Info)
	if res.Partial {
		w.Header().Set("Content-Range", res.ContentRange)
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.WriteHeader(http.StatusOK)
	}
	_, _ = io.Copy(w, res.Body)
}

func (h *Handler) headBlob(w http.ResponseWriter, r *http.Request, s3Key string) {
	info, err := h.store.Head(r.Context(), s3Key)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			http.Error(w, msgNotFound, http.StatusNotFound)
			return
		}
		http.Error(w, "read error", http.StatusInternalServerError)
		return
	}
	setBlobHeaders(w, *info)
	w.WriteHeader(http.StatusOK)
}

func setBlobHeaders(w http.ResponseWriter, info ObjectInfo) {
	w.Header().Set("Content-Length", fmt.Sprintf("%d", info.Size))
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("x-ms-blob-type", "BlockBlob")
	if info.ETag != "" {
		w.Header().Set("ETag", info.ETag)
	}
	ct := info.ContentType
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
}

func (h *Handler) blockDir(s3Key string) string {
	sum := sha256.Sum256([]byte(s3Key))
	return filepath.Join(h.staging, hex.EncodeToString(sum[:]))
}

// encodeBlockID maps an Azure block ID (itself base64, may contain / + =) to a
// filesystem-safe filename.
func encodeBlockID(id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(id))
}

// blockList mirrors the Azure Put Block List XML body. Block IDs may appear
// under Committed, Uncommitted, or Latest; the @azure/storage-blob client emits
// Latest for freshly staged blocks. Order across elements is preserved.
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
		id := strings.TrimSpace(b.ID)
		if id != "" {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("empty block list")
	}
	return ids, nil
}
