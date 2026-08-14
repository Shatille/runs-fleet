package mirrorproxy

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	"github.com/aws/aws-sdk-go-v2/service/ecr/types"
)

type fakeECR struct {
	pages [][]types.PullThroughCacheRule
	rules []types.PullThroughCacheRule
	err   error
}

func (f fakeECR) GetAuthorizationToken(context.Context, *ecr.GetAuthorizationTokenInput, ...func(*ecr.Options)) (*ecr.GetAuthorizationTokenOutput, error) {
	return nil, errors.New("not used in discovery tests")
}

func (f fakeECR) DescribePullThroughCacheRules(_ context.Context, in *ecr.DescribePullThroughCacheRulesInput, _ ...func(*ecr.Options)) (*ecr.DescribePullThroughCacheRulesOutput, error) {
	if f.err != nil {
		return nil, f.err
	}
	if len(f.pages) == 0 {
		return &ecr.DescribePullThroughCacheRulesOutput{PullThroughCacheRules: f.rules}, nil
	}
	page := 0
	if in.NextToken != nil {
		_, _ = fmt.Sscanf(*in.NextToken, "page-%d", &page)
	}
	out := &ecr.DescribePullThroughCacheRulesOutput{PullThroughCacheRules: f.pages[page]}
	if page+1 < len(f.pages) {
		out.NextToken = aws.String(fmt.Sprintf("page-%d", page+1))
	}
	return out, nil
}

func rule(prefix, upstream string) types.PullThroughCacheRule {
	return types.PullThroughCacheRule{
		EcrRepositoryPrefix: aws.String(prefix),
		UpstreamRegistryUrl: aws.String(upstream),
	}
}

func TestDiscoverRules_MapsUpstreamsToPrefixes(t *testing.T) {
	client := fakeECR{rules: []types.PullThroughCacheRule{
		rule("docker-hub", "registry-1.docker.io"),
		rule("k8s", "registry.k8s.io"),
		rule("quay", "quay.io"),
	}}
	got, err := DiscoverRules(context.Background(), client)
	if err != nil {
		t.Fatalf("DiscoverRules() error = %v", err)
	}
	want := map[string]string{
		"docker.io":       "docker-hub",
		"registry.k8s.io": "k8s",
		"quay.io":         "quay",
	}
	for ns, prefix := range want {
		if got[ns] != prefix {
			t.Errorf("map[%q] = %q, want %q", ns, got[ns], prefix)
		}
	}
}

func TestDiscoverRules_DockerHubUpstreamNormalizesToDockerIO(t *testing.T) {
	client := fakeECR{rules: []types.PullThroughCacheRule{
		rule("hub-cache", "registry-1.docker.io"),
	}}
	got, err := DiscoverRules(context.Background(), client)
	if err != nil {
		t.Fatal(err)
	}
	if got["docker.io"] != "hub-cache" {
		t.Errorf("docker.io not normalized: %v", got)
	}
	if _, exists := got["registry-1.docker.io"]; exists {
		t.Errorf("raw registry-1.docker.io key must not appear: %v", got)
	}
}

func TestDiscoverRules_DuplicateUpstreamDedupsDeterministically(t *testing.T) {
	// Shortest prefix wins, then lexicographic — regardless of API order.
	for _, order := range [][]types.PullThroughCacheRule{
		{rule("cache/quay", "quay.io"), rule("quay", "quay.io")},
		{rule("quay", "quay.io"), rule("cache/quay", "quay.io")},
	} {
		got, err := DiscoverRules(context.Background(), fakeECR{rules: order})
		if err != nil {
			t.Fatal(err)
		}
		if got["quay.io"] != "quay" {
			t.Errorf("quay.io = %q, want shortest prefix 'quay' (order %v)", got["quay.io"], order)
		}
	}
}

func TestDiscoverRules_FollowsPagination(t *testing.T) {
	client := fakeECR{pages: [][]types.PullThroughCacheRule{
		{rule("docker-hub", "registry-1.docker.io")},
		{rule("quay", "quay.io")},
		{rule("k8s", "registry.k8s.io")},
	}}
	got, err := DiscoverRules(context.Background(), client)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Errorf("got %v, want all three pages followed", got)
	}
}

func TestDiscoverRules_ErrorPropagates(t *testing.T) {
	_, err := DiscoverRules(context.Background(), fakeECR{err: errors.New("denied")})
	if err == nil {
		t.Fatal("want error when describe fails")
	}
}

func TestDiscoverRules_SkipsIncompleteRules(t *testing.T) {
	client := fakeECR{rules: []types.PullThroughCacheRule{
		{EcrRepositoryPrefix: aws.String("orphan")},
		{UpstreamRegistryUrl: aws.String("quay.io")},
		rule("quay", "quay.io"),
	}}
	got, err := DiscoverRules(context.Background(), client)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got["quay.io"] != "quay" {
		t.Errorf("got %v, want only the complete rule", got)
	}
}
