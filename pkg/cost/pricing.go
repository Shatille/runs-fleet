// Package cost provides cost reporting functionality for runs-fleet.
package cost

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/logging"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/pricing"
	"github.com/aws/aws-sdk-go-v2/service/pricing/types"
)

var pricingLog = logging.WithComponent(logging.LogTypeCost, "pricing")

// PricingAPI defines the AWS Pricing API operations.
type PricingAPI interface {
	GetProducts(ctx context.Context, params *pricing.GetProductsInput, optFns ...func(*pricing.Options)) (*pricing.GetProductsOutput, error)
}

// PriceFetcher fetches EC2 instance pricing from AWS Pricing API.
// It caches prices to avoid excessive API calls.
type PriceFetcher struct {
	client    PricingAPI
	region    string
	cache     map[string]float64
	cacheMu   sync.RWMutex
	cacheTime time.Time
	cacheTTL  time.Duration
	// useFallback is read/written from concurrent GetPrice callers (the admin
	// cost page prices jobs per request against a shared fetcher), so it is
	// atomic rather than guarded by cacheMu.
	useFallback atomic.Bool
	// fallbackUntil is when the latch lapses, as unix nanoseconds. The latch
	// exists so a failing API is not hammered once per priced instance, but it
	// must expire: nothing cleared it in any live path, so a single failure at
	// startup pinned the process to estimates for its whole lifetime, and the
	// latch suppressed the warning too, so the silence read as health.
	fallbackUntil atomic.Int64
	// retryAfter is atomic for the same reason as its siblings: a shared fetcher
	// is read concurrently by per-request cost pricing.
	retryAfter atomic.Int64
}

// fallbackRetryAfter is how long a failed Pricing API call suppresses further
// attempts. Short enough that a newly-granted permission or a transient outage
// is picked up within one cost-report cycle.
const fallbackRetryAfter = 5 * time.Minute

// NewPriceFetcher creates a new price fetcher.
// The AWS Pricing API is only available in us-east-1 and ap-south-1.
func NewPriceFetcher(cfg aws.Config, region string) *PriceFetcher {
	// Pricing API must be called from us-east-1 or ap-south-1
	pricingCfg := cfg.Copy()
	pricingCfg.Region = "us-east-1"

	return &PriceFetcher{
		client:   pricing.NewFromConfig(pricingCfg),
		region:   region,
		cache:    make(map[string]float64),
		cacheTTL: 24 * time.Hour, // Cache prices for 24 hours
	}
}

// NewPriceFetcherWithClient creates a new price fetcher with an injected client for testing.
func NewPriceFetcherWithClient(client PricingAPI, region string) *PriceFetcher {
	return &PriceFetcher{
		client:   client,
		region:   region,
		cache:    make(map[string]float64),
		cacheTTL: 24 * time.Hour,
	}
}

// GetPrice returns the hourly on-demand price for an instance type.
// It first checks the cache, then queries the AWS Pricing API, and falls back
// to hard-coded prices if the API is unavailable.
func (p *PriceFetcher) GetPrice(ctx context.Context, instanceType string) (float64, error) {
	p.cacheMu.RLock()
	if price, ok := p.cache[instanceType]; ok && time.Since(p.cacheTime) < p.cacheTTL {
		p.cacheMu.RUnlock()
		return price, nil
	}
	p.cacheMu.RUnlock()

	// If we've already failed to use the API, use fallback until the latch lapses.
	if p.inFallback() {
		return p.getFallbackPrice(instanceType), nil
	}

	price, err := p.fetchPriceFromAPI(ctx, instanceType)
	if err != nil {
		pricingLog.Warn(ctx, "pricing api fetch failed, using fallback",
			slog.String(logging.KeyInstanceType, instanceType),
			slog.String("error", err.Error()))
		p.enterFallback()
		return p.getFallbackPrice(instanceType), nil
	}

	p.cacheMu.Lock()
	p.cache[instanceType] = price
	p.cacheTime = time.Now()
	p.cacheMu.Unlock()

	return price, nil
}

// inFallback reports whether the fallback latch is still in force.
func (p *PriceFetcher) inFallback() bool {
	if !p.useFallback.Load() {
		return false
	}
	if time.Now().UnixNano() >= p.fallbackUntil.Load() {
		p.useFallback.Store(false)
		return false
	}
	return true
}

// enterFallback latches the fallback for the retry window.
func (p *PriceFetcher) enterFallback() {
	window := time.Duration(p.retryAfter.Load())
	if window == 0 {
		window = fallbackRetryAfter
	}
	p.fallbackUntil.Store(time.Now().Add(window).UnixNano())
	p.useFallback.Store(true)
}

// SetFallbackRetryAfter overrides how long a Pricing API failure suppresses
// retries. A non-positive duration expires the latch immediately, which is how
// tests exercise recovery without waiting out the real window.
func (p *PriceFetcher) SetFallbackRetryAfter(d time.Duration) {
	p.retryAfter.Store(int64(d))
	if d <= 0 {
		p.fallbackUntil.Store(0)
	}
}

// GetPricing returns a map of instance type to hourly prices.
// This is useful for batch operations.
func (p *PriceFetcher) GetPricing(ctx context.Context, instanceTypes []string) map[string]float64 {
	prices := make(map[string]float64)
	for _, instanceType := range instanceTypes {
		price, _ := p.GetPrice(ctx, instanceType)
		prices[instanceType] = price
	}
	return prices
}

// fetchPriceFromAPI queries the AWS Pricing API for EC2 instance pricing.
func (p *PriceFetcher) fetchPriceFromAPI(ctx context.Context, instanceType string) (float64, error) {
	input := &pricing.GetProductsInput{
		ServiceCode: aws.String("AmazonEC2"),
		Filters: []types.Filter{
			{
				Type:  types.FilterTypeTermMatch,
				Field: aws.String("instanceType"),
				Value: aws.String(instanceType),
			},
			{
				Type:  types.FilterTypeTermMatch,
				Field: aws.String("location"),
				Value: aws.String(p.regionToLocation(p.region)),
			},
			{
				Type:  types.FilterTypeTermMatch,
				Field: aws.String("operatingSystem"),
				Value: aws.String("Linux"),
			},
			{
				Type:  types.FilterTypeTermMatch,
				Field: aws.String("preInstalledSw"),
				Value: aws.String("NA"),
			},
			{
				Type:  types.FilterTypeTermMatch,
				Field: aws.String("tenancy"),
				Value: aws.String("Shared"),
			},
			{
				Type:  types.FilterTypeTermMatch,
				Field: aws.String("capacitystatus"),
				Value: aws.String("Used"),
			},
		},
		MaxResults: aws.Int32(1),
	}

	output, err := p.client.GetProducts(ctx, input)
	if err != nil {
		return 0, fmt.Errorf("GetProducts failed: %w", err)
	}

	if len(output.PriceList) == 0 {
		return 0, fmt.Errorf("no pricing data found for %s in %s", instanceType, p.region)
	}

	// Parse the price from the JSON response
	return p.parsePriceFromProduct(output.PriceList[0])
}

// parsePriceFromProduct extracts the hourly price from the AWS Pricing API response.
func (p *PriceFetcher) parsePriceFromProduct(priceJSON string) (float64, error) {
	var product map[string]interface{}
	if err := json.Unmarshal([]byte(priceJSON), &product); err != nil {
		return 0, fmt.Errorf("failed to parse price JSON: %w", err)
	}

	// Navigate the complex pricing structure
	terms, ok := product["terms"].(map[string]interface{})
	if !ok {
		return 0, fmt.Errorf("missing terms in pricing data")
	}

	onDemand, ok := terms["OnDemand"].(map[string]interface{})
	if !ok {
		return 0, fmt.Errorf("missing OnDemand terms")
	}

	// Get the first (and usually only) SKU
	for _, skuData := range onDemand {
		sku, ok := skuData.(map[string]interface{})
		if !ok {
			continue
		}

		priceDimensions, ok := sku["priceDimensions"].(map[string]interface{})
		if !ok {
			continue
		}

		for _, dimData := range priceDimensions {
			dim, ok := dimData.(map[string]interface{})
			if !ok {
				continue
			}

			pricePerUnit, ok := dim["pricePerUnit"].(map[string]interface{})
			if !ok {
				continue
			}

			if usd, ok := pricePerUnit["USD"].(string); ok {
				price, err := strconv.ParseFloat(usd, 64)
				if err != nil {
					return 0, fmt.Errorf("failed to parse USD price: %w", err)
				}
				return price, nil
			}
		}
	}

	return 0, fmt.Errorf("could not find USD price in pricing data")
}

// regionToLocation converts AWS region codes to location names used by the Pricing API.
func (p *PriceFetcher) regionToLocation(region string) string {
	regionMap := map[string]string{
		"us-east-1":      "US East (N. Virginia)",
		"us-east-2":      "US East (Ohio)",
		"us-west-1":      "US West (N. California)",
		"us-west-2":      "US West (Oregon)",
		"eu-west-1":      "EU (Ireland)",
		"eu-west-2":      "EU (London)",
		"eu-west-3":      "EU (Paris)",
		"eu-central-1":   "EU (Frankfurt)",
		"eu-north-1":     "EU (Stockholm)",
		"ap-northeast-1": "Asia Pacific (Tokyo)",
		"ap-northeast-2": "Asia Pacific (Seoul)",
		"ap-northeast-3": "Asia Pacific (Osaka)",
		"ap-southeast-1": "Asia Pacific (Singapore)",
		"ap-southeast-2": "Asia Pacific (Sydney)",
		"ap-south-1":     "Asia Pacific (Mumbai)",
		"sa-east-1":      "South America (Sao Paulo)",
		"ca-central-1":   "Canada (Central)",
	}

	if loc, ok := regionMap[region]; ok {
		return loc
	}
	return "US East (N. Virginia)" // Default to us-east-1
}

// getFallbackPrice returns the estimated price for an instance type when the
// Pricing API is unavailable. It shares GetInstancePrice's family/vCPU
// derivation so both paths estimate identically.
func (p *PriceFetcher) getFallbackPrice(instanceType string) float64 {
	return GetInstancePrice(instanceType)
}
