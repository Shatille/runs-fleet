package main

import (
	"context"
	"errors"
	"testing"
)

func TestResolveRegionPrefersExplicitConfig(t *testing.T) {
	got, err := resolveRegion(context.Background(), "ap-northeast-1", func(context.Context) (string, error) {
		t.Fatal("IMDS must not be consulted when the config already carries a region")
		return "", nil
	})
	if err != nil {
		t.Fatalf("resolveRegion() error = %v", err)
	}
	if got != "ap-northeast-1" {
		t.Errorf("resolveRegion() = %q, want %q", got, "ap-northeast-1")
	}
}

func TestResolveRegionFallsBackToIMDS(t *testing.T) {
	got, err := resolveRegion(context.Background(), "", func(context.Context) (string, error) {
		return "us-west-2", nil
	})
	if err != nil {
		t.Fatalf("resolveRegion() error = %v", err)
	}
	if got != "us-west-2" {
		t.Errorf("resolveRegion() = %q, want %q", got, "us-west-2")
	}
}

func TestResolveRegionFailsWhenRegionUnresolvable(t *testing.T) {
	_, err := resolveRegion(context.Background(), "", func(context.Context) (string, error) {
		return "", errors.New("imds unreachable")
	})
	if err == nil {
		t.Fatal("resolveRegion() error = nil, want an error so the proxy refuses to serve a mirror that can only 502")
	}
}

func TestResolveRegionFailsOnEmptyIMDSRegion(t *testing.T) {
	_, err := resolveRegion(context.Background(), "", func(context.Context) (string, error) {
		return "", nil
	})
	if err == nil {
		t.Fatal("resolveRegion() error = nil, want an error when IMDS yields no region")
	}
}
