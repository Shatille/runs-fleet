package blobshim

import (
	"strings"
	"testing"
	"time"
)

func TestSignerRoundTrip(t *testing.T) {
	t.Parallel()

	s := NewSigner([]byte("instance-secret"))
	now := time.Unix(1_000_000, 0)
	exp := now.Add(15 * time.Minute)

	token := s.Sign(OpWrite, "caches/org/repo/v1/key", exp)
	op, key, err := s.Verify(token, now)
	if err != nil {
		t.Fatalf("Verify returned error: %v", err)
	}
	if op != OpWrite {
		t.Errorf("op = %q, want %q", op, OpWrite)
	}
	if key != "caches/org/repo/v1/key" {
		t.Errorf("key = %q, want caches/org/repo/v1/key", key)
	}
}

func TestSignerRejectsExpired(t *testing.T) {
	t.Parallel()

	s := NewSigner([]byte("secret"))
	exp := time.Unix(1_000_000, 0)
	token := s.Sign(OpRead, "caches/k", exp)

	if _, _, err := s.Verify(token, exp.Add(time.Second)); err == nil {
		t.Fatal("expected expiry error, got nil")
	}
}

func TestSignerRejectsTamperedSignature(t *testing.T) {
	t.Parallel()

	s := NewSigner([]byte("secret"))
	now := time.Unix(1_000_000, 0)
	token := s.Sign(OpWrite, "caches/k", now.Add(time.Minute))

	encoded, _, _ := strings.Cut(token, ".")
	tampered := encoded + "." + "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	if _, _, err := s.Verify(tampered, now); err == nil {
		t.Fatal("expected signature error, got nil")
	}
}

func TestSignerRejectsTamperedKey(t *testing.T) {
	t.Parallel()

	signed := NewSigner([]byte("secret"))
	now := time.Unix(1_000_000, 0)
	token := signed.Sign(OpWrite, "caches/org/repo/v1/key", now.Add(time.Minute))

	// A token signed with a different secret must not verify: a job cannot forge
	// a capability for an arbitrary S3 key.
	other := NewSigner([]byte("other-secret"))
	if _, _, err := other.Verify(token, now); err == nil {
		t.Fatal("expected signature mismatch across secrets, got nil")
	}
}

func TestSignerRejectsMalformed(t *testing.T) {
	t.Parallel()

	s := NewSigner([]byte("secret"))
	now := time.Unix(1_000_000, 0)
	for _, tok := range []string{"", "nodot", "a.b.c"} {
		if _, _, err := s.Verify(tok, now); err == nil {
			t.Errorf("Verify(%q) = nil error, want error", tok)
		}
	}
}
