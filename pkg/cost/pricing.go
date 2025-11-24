// Package cost provides cost reporting functionality for runs-fleet.
package cost

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/pricing"
	"github.com/aws/aws-sdk-go-v2/service/pricing/types"
)

// PricingAPI defines the AWS Pricing API operations.
type PricingAPI interface {
	GetProducts(ctx context.Context, params *pricing.GetProductsInput, optFns ...func(*pricing.Options)) (*pricing.GetProductsOutput, error)
}

// PriceFetcher fetches EC2 instance pricing from AWS Pricing API.
// It caches prices to avoid excessive API calls.
type PriceFetcher struct {
	client     PricingAPI
	region     string
	cache      map[string]float64
	cacheMu    sync.RWMutex
	cacheTime  time.Time
	cacheTTL   time.Duration
	useFallback bool
}

// NewPriceFetcher creates a new price fetcher.
// The AWS Pricing API is only available in us-east-1 and ap-south-1.
func NewPriceFetcher(cfg aws.Config, region string) *PriceFetcher {
	// Pricing API must be called from us-east-1 or ap-south-1
	pricingCfg := cfg.Copy()
	pricingCfg.Region = "us-east-1"

	return &PriceFetcher{
		client:      pricing.NewFromConfig(pricingCfg),
		region:      region,
		cache:       make(map[string]float64),
		cacheTTL:    24 * time.Hour, // Cache prices for 24 hours
		useFallback: false,
	}
}

// NewPriceFetcherWithClient creates a new price fetcher with an injected client for testing.
func NewPriceFetcherWithClient(client PricingAPI, region string) *PriceFetcher {
	return &PriceFetcher{
		client:      client,
		region:      region,
		cache:       make(map[string]float64),
		cacheTTL:    24 * time.Hour,
		useFallback: false,
	}
}

// GetPrice returns the hourly on-demand price for an instance type.
// It first checks the cache, then queries the AWS Pricing API, and falls back
// to hard-coded prices if the API is unavailable.
func (p *PriceFetcher) GetPrice(ctx context.Context, instanceType string) (float64, error) {
	// Check cache first
	p.cacheMu.RLock()
	if price, ok := p.cache[instanceType]; ok && time.Since(p.cacheTime) < p.cacheTTL {
		p.cacheMu.RUnlock()
		return price, nil
	}
	p.cacheMu.RUnlock()

	// If we've already failed to use the API, use fallback immediately
	if p.useFallback {
		return p.getFallbackPrice(instanceType), nil
	}

	// Try to fetch from AWS Pricing API
	price, err := p.fetchPriceFromAPI(ctx, instanceType)
	if err != nil {
		log.Printf("Warning: failed to fetch price from AWS Pricing API for %s: %v (using fallback)", instanceType, err)
		p.useFallback = true
		return p.getFallbackPrice(instanceType), nil
	}

	// Update cache
	p.cacheMu.Lock()
	p.cache[instanceType] = price
	p.cacheTime = time.Now()
	p.cacheMu.Unlock()

	return price, nil
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

// RefreshCache refreshes the price cache for all known instance types.
func (p *PriceFetcher) RefreshCache(ctx context.Context) error {
	knownTypes := []string{
		"t4g.micro", "t4g.small", "t4g.medium", "t4g.large", "t4g.xlarge", "t4g.2xlarge",
		"c7g.medium", "c7g.large", "c7g.xlarge", "c7g.2xlarge",
		"m7g.medium", "m7g.large", "m7g.xlarge", "m7g.2xlarge",
	}

	p.useFallback = false // Reset fallback flag to try API again

	for _, instanceType := range knownTypes {
		_, err := p.GetPrice(ctx, instanceType)
		if err != nil {
			log.Printf("Warning: failed to refresh price for %s: %v", instanceType, err)
		}
	}

	return nil
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

// getFallbackPrice returns the hard-coded fallback price for an instance type.
func (p *PriceFetcher) getFallbackPrice(instanceType string) float64 {
	if price, ok := instancePricing[instanceType]; ok {
		return price
	}
	// Default to t4g.medium price if unknown
	return 0.0336
}
