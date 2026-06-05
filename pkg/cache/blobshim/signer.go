// Package blobshim translates the Azure Block Blob REST surface that the
// GitHub Actions v2 cache client speaks (stage block, commit block list,
// ranged get) into S3 operations, so cache archives land in the runs-fleet
// bucket. It runs on the runner host; the v2 CacheService hands the client
// signed URLs that point here.
package blobshim

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Operation is the access a capability token grants.
type Operation string

const (
	// OpRead permits ranged GET/HEAD of a blob.
	OpRead Operation = "r"
	// OpWrite permits staging blocks and committing a blob.
	OpWrite Operation = "w"
)

// Signer issues and verifies capability tokens for blob URLs. The runner runs
// untrusted job code on the same host as the shim, so a blob URL must be an
// unforgeable, expiring capability bound to one S3 key and one operation.
type Signer struct {
	secret []byte
}

// NewSigner returns a Signer backed by the given HMAC secret. The secret is
// generated per instance and never leaves the host.
func NewSigner(secret []byte) *Signer {
	return &Signer{secret: secret}
}

type tokenPayload struct {
	Op  Operation `json:"op"`
	Key string    `json:"key"`
	Exp int64     `json:"exp"`
}

// Sign returns a URL-path-safe capability token granting op on s3Key until exp.
func (s *Signer) Sign(op Operation, s3Key string, exp time.Time) string {
	payload := tokenPayload{Op: op, Key: s3Key, Exp: exp.Unix()}
	raw, _ := json.Marshal(payload)
	encoded := base64.RawURLEncoding.EncodeToString(raw)
	return encoded + "." + s.mac(encoded)
}

// Verify validates a token's signature and expiry against now, returning the
// granted operation and S3 key.
func (s *Signer) Verify(token string, now time.Time) (Operation, string, error) {
	encoded, sig, ok := strings.Cut(token, ".")
	if !ok {
		return "", "", fmt.Errorf("malformed token")
	}
	if !hmac.Equal([]byte(sig), []byte(s.mac(encoded))) {
		return "", "", fmt.Errorf("invalid token signature")
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return "", "", fmt.Errorf("invalid token encoding: %w", err)
	}
	var payload tokenPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", "", fmt.Errorf("invalid token payload: %w", err)
	}
	if now.Unix() > payload.Exp {
		return "", "", fmt.Errorf("token expired")
	}
	if payload.Op != OpRead && payload.Op != OpWrite {
		return "", "", fmt.Errorf("unknown operation %q", payload.Op)
	}
	return payload.Op, payload.Key, nil
}

func (s *Signer) mac(encoded string) string {
	h := hmac.New(sha256.New, s.secret)
	h.Write([]byte(encoded))
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}
