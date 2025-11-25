package cost_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/cost"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/pricing"
	"github.com/aws/aws-sdk-go-v2/service/pricing/types"
)

type mockPricingClient struct {
	getProductsFunc func(ctx context.Context, params *pricing.GetProductsInput, optFns ...func(*pricing.Options)) (*pricing.GetProductsOutput, error)
}

func (m *mockPricingClient) GetProducts(ctx context.Context, params *pricing.GetProductsInput, optFns ...func(*pricing.Options)) (*pricing.GetProductsOutput, error) {
	if m.getProductsFunc != nil {
		return m.getProductsFunc(ctx, params, optFns...)
	}
	return &pricing.GetProductsOutput{}, nil
}

func TestPriceFetcher_GetPrice_FromAPI(t *testing.T) {
	mockClient := &mockPricingClient{
		getProductsFunc: func(_ context.Context, params *pricing.GetProductsInput, _ ...func(*pricing.Options)) (*pricing.GetProductsOutput, error) {
			// Verify the filters are correct
			if *params.ServiceCode != "AmazonEC2" {
				t.Errorf("Expected ServiceCode 'AmazonEC2', got '%s'", *params.ServiceCode)
			}

			// Return a valid pricing response
			priceJSON := `{
				"terms": {
					"OnDemand": {
						"SKU123": {
							"priceDimensions": {
								"DIM123": {
									"pricePerUnit": {
										"USD": "0.0420"
									}
								}
							}
						}
					}
				}
			}`

			return &pricing.GetProductsOutput{
				PriceList: []string{priceJSON},
			}, nil
		},
	}

	pf := cost.NewPriceFetcherWithClient(mockClient, "us-east-1")

	price, err := pf.GetPrice(context.Background(), "t4g.medium")
	if err != nil {
		t.Fatalf("GetPrice() error = %v", err)
	}

	if price != 0.0420 {
		t.Errorf("GetPrice() = %v, want 0.0420", price)
	}
}

func TestPriceFetcher_GetPrice_APIError_FallsBackToHardcoded(t *testing.T) {
	mockClient := &mockPricingClient{
		getProductsFunc: func(_ context.Context, _ *pricing.GetProductsInput, _ ...func(*pricing.Options)) (*pricing.GetProductsOutput, error) {
			return nil, errors.New("API error")
		},
	}

	pf := cost.NewPriceFetcherWithClient(mockClient, "us-east-1")

	price, err := pf.GetPrice(context.Background(), "t4g.medium")
	if err != nil {
		t.Fatalf("GetPrice() error = %v, expected fallback to hard-coded price", err)
	}

	// Should fall back to hard-coded price for t4g.medium
	if price != 0.0336 {
		t.Errorf("GetPrice() = %v, want fallback price 0.0336", price)
	}
}

func TestPriceFetcher_GetPrice_NoPriceList_FallsBackToHardcoded(t *testing.T) {
	mockClient := &mockPricingClient{
		getProductsFunc: func(_ context.Context, _ *pricing.GetProductsInput, _ ...func(*pricing.Options)) (*pricing.GetProductsOutput, error) {
			return &pricing.GetProductsOutput{
				PriceList: []string{}, // Empty price list
			}, nil
		},
	}

	pf := cost.NewPriceFetcherWithClient(mockClient, "us-east-1")

	price, err := pf.GetPrice(context.Background(), "t4g.medium")
	if err != nil {
		t.Fatalf("GetPrice() error = %v, expected fallback to hard-coded price", err)
	}

	// Should fall back to hard-coded price
	if price != 0.0336 {
		t.Errorf("GetPrice() = %v, want fallback price 0.0336", price)
	}
}

func TestPriceFetcher_GetPrice_Caching(t *testing.T) {
	callCount := 0
	mockClient := &mockPricingClient{
		getProductsFunc: func(_ context.Context, _ *pricing.GetProductsInput, _ ...func(*pricing.Options)) (*pricing.GetProductsOutput, error) {
			callCount++
			priceJSON := `{
				"terms": {
					"OnDemand": {
						"SKU123": {
							"priceDimensions": {
								"DIM123": {
									"pricePerUnit": {
										"USD": "0.0500"
									}
								}
							}
						}
					}
				}
			}`
			return &pricing.GetProductsOutput{
				PriceList: []string{priceJSON},
			}, nil
		},
	}

	pf := cost.NewPriceFetcherWithClient(mockClient, "us-east-1")

	// First call should hit the API
	_, err := pf.GetPrice(context.Background(), "c7g.large")
	if err != nil {
		t.Fatalf("GetPrice() error = %v", err)
	}

	// Second call should use cache
	_, err = pf.GetPrice(context.Background(), "c7g.large")
	if err != nil {
		t.Fatalf("GetPrice() error = %v", err)
	}

	if callCount != 1 {
		t.Errorf("API was called %d times, expected 1 (caching should prevent second call)", callCount)
	}
}

func TestPriceFetcher_GetPrice_UnknownInstanceType_FallsBackToDefault(t *testing.T) {
	mockClient := &mockPricingClient{
		getProductsFunc: func(_ context.Context, _ *pricing.GetProductsInput, _ ...func(*pricing.Options)) (*pricing.GetProductsOutput, error) {
			return nil, errors.New("API error")
		},
	}

	pf := cost.NewPriceFetcherWithClient(mockClient, "us-east-1")

	// Unknown instance type should fall back to default (t4g.medium price)
	price, err := pf.GetPrice(context.Background(), "unknown.xlarge")
	if err != nil {
		t.Fatalf("GetPrice() error = %v", err)
	}

	if price != 0.0336 {
		t.Errorf("GetPrice() = %v, want default fallback price 0.0336", price)
	}
}

func TestPriceFetcher_GetPrice_MalformedJSON(t *testing.T) {
	mockClient := &mockPricingClient{
		getProductsFunc: func(_ context.Context, _ *pricing.GetProductsInput, _ ...func(*pricing.Options)) (*pricing.GetProductsOutput, error) {
			return &pricing.GetProductsOutput{
				PriceList: []string{`{invalid json}`},
			}, nil
		},
	}

	pf := cost.NewPriceFetcherWithClient(mockClient, "us-east-1")

	// Should fall back to hard-coded price on malformed JSON
	price, err := pf.GetPrice(context.Background(), "t4g.medium")
	if err != nil {
		t.Fatalf("GetPrice() error = %v, expected fallback", err)
	}

	if price != 0.0336 {
		t.Errorf("GetPrice() = %v, want fallback price 0.0336", price)
	}
}

func TestPriceFetcher_GetPricing_MultiplTypes(t *testing.T) {
	mockClient := &mockPricingClient{
		getProductsFunc: func(_ context.Context, params *pricing.GetProductsInput, _ ...func(*pricing.Options)) (*pricing.GetProductsOutput, error) {
			// Return different prices based on instance type filter
			var instanceType string
			for _, filter := range params.Filters {
				if *filter.Field == "instanceType" {
					instanceType = *filter.Value
					break
				}
			}

			var price string
			switch instanceType {
			case "t4g.small":
				price = "0.0168"
			case "c7g.large":
				price = "0.0725"
			default:
				price = "0.0336"
			}

			priceJSON := `{
				"terms": {
					"OnDemand": {
						"SKU123": {
							"priceDimensions": {
								"DIM123": {
									"pricePerUnit": {
										"USD": "` + price + `"
									}
								}
							}
						}
					}
				}
			}`

			return &pricing.GetProductsOutput{
				PriceList: []string{priceJSON},
			}, nil
		},
	}

	pf := cost.NewPriceFetcherWithClient(mockClient, "us-east-1")

	instanceTypes := []string{"t4g.small", "c7g.large", "t4g.medium"}
	prices := pf.GetPricing(context.Background(), instanceTypes)

	if len(prices) != 3 {
		t.Errorf("GetPricing() returned %d prices, want 3", len(prices))
	}

	if prices["t4g.small"] != 0.0168 {
		t.Errorf("Price for t4g.small = %v, want 0.0168", prices["t4g.small"])
	}

	if prices["c7g.large"] != 0.0725 {
		t.Errorf("Price for c7g.large = %v, want 0.0725", prices["c7g.large"])
	}
}

func TestPriceFetcher_RefreshCache(t *testing.T) {
	callCount := 0
	mockClient := &mockPricingClient{
		getProductsFunc: func(_ context.Context, _ *pricing.GetProductsInput, _ ...func(*pricing.Options)) (*pricing.GetProductsOutput, error) {
			callCount++
			priceJSON := `{
				"terms": {
					"OnDemand": {
						"SKU123": {
							"priceDimensions": {
								"DIM123": {
									"pricePerUnit": {
										"USD": "0.0336"
									}
								}
							}
						}
					}
				}
			}`
			return &pricing.GetProductsOutput{
				PriceList: []string{priceJSON},
			}, nil
		},
	}

	pf := cost.NewPriceFetcherWithClient(mockClient, "us-east-1")

	// RefreshCache should call the API for all known instance types
	err := pf.RefreshCache(context.Background())
	if err != nil {
		t.Fatalf("RefreshCache() error = %v", err)
	}

	// Should have called the API multiple times (once per known instance type)
	if callCount < 10 {
		t.Errorf("RefreshCache() made %d API calls, expected at least 10 for known instance types", callCount)
	}
}

func TestPriceFetcher_RegionToLocation(t *testing.T) {
	tests := []struct {
		region   string
		location string
	}{
		{"us-east-1", "US East (N. Virginia)"},
		{"us-west-2", "US West (Oregon)"},
		{"eu-west-1", "EU (Ireland)"},
		{"ap-northeast-1", "Asia Pacific (Tokyo)"},
		{"unknown-region", "US East (N. Virginia)"}, // Should default to us-east-1
	}

	for _, tt := range tests {
		t.Run(tt.region, func(t *testing.T) {
			// We can't directly test the private method, but we can verify
			// that the region is used in API calls via the filter
			mockClient := &mockPricingClient{
				getProductsFunc: func(_ context.Context, params *pricing.GetProductsInput, _ ...func(*pricing.Options)) (*pricing.GetProductsOutput, error) {
					// Check the location filter
					for _, filter := range params.Filters {
						if *filter.Field == "location" {
							if *filter.Value != tt.location {
								t.Errorf("location filter = %v, want %v", *filter.Value, tt.location)
							}
						}
					}
					return &pricing.GetProductsOutput{PriceList: []string{}}, nil
				},
			}

			pf := cost.NewPriceFetcherWithClient(mockClient, tt.region)
			_, _ = pf.GetPrice(context.Background(), "t4g.medium")
		})
	}
}

func TestPriceFetcher_FallbackPrices(t *testing.T) {
	// Test that all known instance types have fallback prices
	mockClient := &mockPricingClient{
		getProductsFunc: func(_ context.Context, _ *pricing.GetProductsInput, _ ...func(*pricing.Options)) (*pricing.GetProductsOutput, error) {
			return nil, errors.New("API unavailable")
		},
	}

	pf := cost.NewPriceFetcherWithClient(mockClient, "us-east-1")

	knownTypes := []struct {
		instanceType string
		expectedMin  float64
		expectedMax  float64
	}{
		{"t4g.micro", 0.005, 0.02},
		{"t4g.small", 0.01, 0.03},
		{"t4g.medium", 0.02, 0.05},
		{"t4g.large", 0.05, 0.10},
		{"c7g.medium", 0.02, 0.05},
		{"c7g.large", 0.05, 0.10},
		{"m7g.medium", 0.02, 0.06},
		{"m7g.large", 0.05, 0.12},
	}

	for _, tt := range knownTypes {
		t.Run(tt.instanceType, func(t *testing.T) {
			price, err := pf.GetPrice(context.Background(), tt.instanceType)
			if err != nil {
				t.Fatalf("GetPrice() error = %v", err)
			}

			if price < tt.expectedMin || price > tt.expectedMax {
				t.Errorf("GetPrice(%s) = %v, expected between %v and %v",
					tt.instanceType, price, tt.expectedMin, tt.expectedMax)
			}
		})
	}
}

// Note: Testing with a real time.Duration for cache TTL would require time manipulation
// which is complex. The caching behavior is tested indirectly through the callCount tests.
var _ = time.Second // Ensure time package is used
var _ types.Filter  // Ensure types package is used
var _ = aws.String  // Ensure aws package is used
