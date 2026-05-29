package main

import (
	"net/http"
	"time"

	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
)

// newAWSHTTPClient builds the shared HTTP client for all AWS SDK calls with a
// bounded ResponseHeaderTimeout, so a stalled connection is abandoned and retried
// rather than blocking until the request context expires.
func newAWSHTTPClient(responseHeaderTimeout time.Duration) *awshttp.BuildableClient {
	return awshttp.NewBuildableClient().WithTransportOptions(func(tr *http.Transport) {
		tr.ResponseHeaderTimeout = responseHeaderTimeout
	})
}
