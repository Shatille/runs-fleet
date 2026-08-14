package mirrorproxy

import (
	"context"
	"errors"
	"testing"
	"time"
)

const testToken = "tok"

func TestCachedTokenSource_FetchesOnceWhileValid(t *testing.T) {
	now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	fetches := 0
	src := newCachedTokenSource(func(context.Context) (string, time.Time, error) {
		fetches++
		return "tok-1", now.Add(12 * time.Hour), nil
	}, func() time.Time { return now })

	for i := 0; i < 3; i++ {
		tok, err := src.Token(context.Background())
		if err != nil {
			t.Fatalf("Token() error = %v", err)
		}
		if tok != "tok-1" {
			t.Fatalf("Token() = %q", tok)
		}
	}
	if fetches != 1 {
		t.Errorf("fetches = %d, want 1", fetches)
	}
}

func TestCachedTokenSource_RefetchesInsideExpiryMargin(t *testing.T) {
	base := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	now := base
	fetches := 0
	src := newCachedTokenSource(func(context.Context) (string, time.Time, error) {
		fetches++
		return testToken, now.Add(12 * time.Hour), nil
	}, func() time.Time { return now })

	if _, err := src.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	now = base.Add(12*time.Hour - tokenExpiryMargin + time.Second)
	if _, err := src.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fetches != 2 {
		t.Errorf("fetches = %d, want refetch inside the expiry margin", fetches)
	}
}

func TestCachedTokenSource_FetchErrorPropagatesAndIsNotCached(t *testing.T) {
	now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	fetches := 0
	fail := true
	src := newCachedTokenSource(func(context.Context) (string, time.Time, error) {
		fetches++
		if fail {
			return "", time.Time{}, errors.New("throttled")
		}
		return testToken, now.Add(time.Hour), nil
	}, func() time.Time { return now })

	if _, err := src.Token(context.Background()); err == nil {
		t.Fatal("want error from failing fetch")
	}
	fail = false
	tok, err := src.Token(context.Background())
	if err != nil {
		t.Fatalf("Token() after recovery = %v", err)
	}
	if tok != testToken {
		t.Fatalf("Token() = %q", tok)
	}
	if fetches != 2 {
		t.Errorf("fetches = %d, want the error not cached", fetches)
	}
}
