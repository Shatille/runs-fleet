package mirrorproxy

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/service/ecr"
)

// DiscoverRules maps the registry's pull-through cache rules to an ns-host →
// rule-prefix table. Duplicate upstreams dedup deterministically (shortest
// prefix, then lexicographic) so this table and the bake-time buildkitd.toml
// generation can never disagree.
func DiscoverRules(ctx context.Context, client ecrAPI) (map[string]string, error) {
	rules := make(map[string]string)
	input := &ecr.DescribePullThroughCacheRulesInput{}
	for {
		out, err := client.DescribePullThroughCacheRules(ctx, input)
		if err != nil {
			return nil, fmt.Errorf("ecr describe-pull-through-cache-rules: %w", err)
		}
		for _, r := range out.PullThroughCacheRules {
			if r.EcrRepositoryPrefix == nil || r.UpstreamRegistryUrl == nil {
				continue
			}
			ns := normalizeUpstream(*r.UpstreamRegistryUrl)
			prefix := *r.EcrRepositoryPrefix
			if existing, ok := rules[ns]; ok && !prefixWins(prefix, existing) {
				continue
			}
			rules[ns] = prefix
		}
		if out.NextToken == nil {
			return rules, nil
		}
		input.NextToken = out.NextToken
	}
}

func normalizeUpstream(upstream string) string {
	if upstream == "registry-1.docker.io" {
		return "docker.io"
	}
	return upstream
}

func prefixWins(candidate, incumbent string) bool {
	if len(candidate) != len(incumbent) {
		return len(candidate) < len(incumbent)
	}
	return candidate < incumbent
}
