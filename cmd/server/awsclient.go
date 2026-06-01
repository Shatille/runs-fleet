package main

import (
	"net"
	"net/http"
	"time"

	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"

	"github.com/Shavakan/runs-fleet/pkg/config"
)

// newAWSHTTPClient builds the shared HTTP client for all AWS SDK calls. It
// bounds two distinct failure modes on a wedged keep-alive connection:
//
//   - ResponseHeaderTimeout abandons a request whose peer accepted it but never
//     sends response headers (the await-response phase).
//   - The dialer applies TCP_USER_TIMEOUT and aggressive TCP keepalive, which
//     cover the write/ACK phase: when a peer stops ACKing, bytes wedge in the
//     socket Send-Q with no RST and the kernel would retry for ~15 minutes
//     (tcp_retries2). TCP_USER_TIMEOUT tears the socket down in ~20s and removes
//     it from the connection pool, so the SDK retries on a fresh connection.
//
// TCP_USER_TIMEOUT does not interfere with SQS long polling: the long-poll wait
// carries no unacknowledged outbound data, so there is nothing for the kernel to
// time out. Both the fast and SQS clients therefore use the same dialer config.
func newAWSHTTPClient(responseHeaderTimeout time.Duration) *awshttp.BuildableClient {
	return awshttp.NewBuildableClient().
		WithDialerOptions(func(d *net.Dialer) {
			d.Control = setTCPUserTimeout(config.AWSTCPUserTimeout)
			// KeepAlive is the legacy single-knob field; KeepAliveConfig is the
			// Go 1.23+ replacement. Disable the legacy field so it does not
			// conflict with the explicit config below.
			d.KeepAlive = -1
			d.KeepAliveConfig = net.KeepAliveConfig{
				Enable:   true,
				Idle:     config.AWSKeepAliveIdle,
				Interval: config.AWSKeepAliveInterval,
				Count:    config.AWSKeepAliveCount,
			}
		}).
		WithTransportOptions(func(tr *http.Transport) {
			tr.ResponseHeaderTimeout = responseHeaderTimeout
			tr.MaxIdleConns = awsMaxIdleConns
			tr.MaxIdleConnsPerHost = awsMaxIdleConnsPerHost
		})
}
