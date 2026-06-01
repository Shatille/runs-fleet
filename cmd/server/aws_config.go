package main

import (
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
)

const (
	// awsMaxIdleConns and awsMaxIdleConnsPerHost size the AWS HTTP connection
	// pool for a high-fan-out orchestrator. The SDK default MaxIdleConnsPerHost
	// is 10, which forces TCP+TLS churn when concurrent requests to a single AWS
	// endpoint exceed it under burst load. The SDK clones the transport per
	// service client, so these limits apply per client (ec2, sqs, dynamodb, ...),
	// not across all clients combined. They are applied by newAWSHTTPClient.
	awsMaxIdleConns        = 200
	awsMaxIdleConnsPerHost = 100
)

// awsRetryer returns an adaptive-mode retryer. Adaptive mode adds a client-side
// rate limiter that throttles the send rate when the SDK observes throttling
// errors, so retries back off instead of amplifying a throttle storm. The SDK
// keeps MaxAttempts at its standard default (3) and applies no delay until a
// throttle is observed. Adaptive mode is marked experimental in the SDK, so
// revisit this on SDK upgrades.
func awsRetryer() aws.Retryer {
	return retry.NewAdaptiveMode()
}
