package mirrorproxy

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/ecr"
)

// tokenExpiryMargin refreshes ahead of ECR's ~12h expiry so a token is never
// handed out with only seconds of validity left mid-pull.
const tokenExpiryMargin = 5 * time.Minute

type fetchFunc func(ctx context.Context) (token string, expiresAt time.Time, err error)

type cachedTokenSource struct {
	fetch fetchFunc
	now   func() time.Time

	mu        sync.Mutex
	token     string
	expiresAt time.Time
}

func newCachedTokenSource(fetch fetchFunc, now func() time.Time) *cachedTokenSource {
	return &cachedTokenSource{fetch: fetch, now: now}
}

func (c *cachedTokenSource) Token(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && c.now().Before(c.expiresAt.Add(-tokenExpiryMargin)) {
		return c.token, nil
	}
	token, expiresAt, err := c.fetch(ctx)
	if err != nil {
		return "", err
	}
	c.token = token
	c.expiresAt = expiresAt
	return token, nil
}

type ecrAPI interface {
	GetAuthorizationToken(ctx context.Context, params *ecr.GetAuthorizationTokenInput, optFns ...func(*ecr.Options)) (*ecr.GetAuthorizationTokenOutput, error)
	DescribePullThroughCacheRules(ctx context.Context, params *ecr.DescribePullThroughCacheRulesInput, optFns ...func(*ecr.Options)) (*ecr.DescribePullThroughCacheRulesOutput, error)
}

// NewECRTokenSource exchanges the ambient AWS credentials (the instance role,
// in production) for registry basic-auth tokens, cached until shortly before
// expiry.
func NewECRTokenSource(client ecrAPI) TokenSource {
	return newCachedTokenSource(func(ctx context.Context) (string, time.Time, error) {
		out, err := client.GetAuthorizationToken(ctx, &ecr.GetAuthorizationTokenInput{})
		if err != nil {
			return "", time.Time{}, fmt.Errorf("ecr get-authorization-token: %w", err)
		}
		if len(out.AuthorizationData) == 0 || out.AuthorizationData[0].AuthorizationToken == nil {
			return "", time.Time{}, fmt.Errorf("ecr get-authorization-token returned no token")
		}
		data := out.AuthorizationData[0]
		expiresAt := time.Now().Add(time.Hour)
		if data.ExpiresAt != nil {
			expiresAt = *data.ExpiresAt
		}
		return *data.AuthorizationToken, expiresAt, nil
	}, time.Now)
}
