package buildxshim

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// CredsFetcher abstracts instance-profile credential retrieval so the shim can
// be tested with a fake and so a total IMDS failure is a plain error the caller
// turns into passthrough.
type CredsFetcher interface {
	FetchCredentials(ctx context.Context) (Credentials, error)
}

const (
	imdsBaseURL      = "http://169.254.169.254"
	imdsTokenPath    = "/latest/api/token"
	imdsCredsPath    = "/latest/meta-data/iam/security-credentials/"
	imdsTokenTTLSecs = "60"
	imdsTotalTimeout = 2 * time.Second
)

// IMDSClient fetches instance-profile session credentials from IMDSv2 on the
// host (hop-limit 1 is fine because the shim runs on the host, not in a
// container). It is bounded by a short total timeout so a broken or absent IMDS
// degrades to passthrough quickly.
type IMDSClient struct {
	baseURL    string
	httpClient *http.Client
	timeout    time.Duration
}

// NewIMDSClient returns an IMDSClient configured against the standard link-local
// metadata endpoint with a short total timeout.
func NewIMDSClient() *IMDSClient {
	return &IMDSClient{
		baseURL:    imdsBaseURL,
		httpClient: &http.Client{Timeout: imdsTotalTimeout},
		timeout:    imdsTotalTimeout,
	}
}

type imdsCredsDoc struct {
	AccessKeyID     string `json:"AccessKeyId"`
	SecretAccessKey string `json:"SecretAccessKey"`
	Token           string `json:"Token"`
}

// FetchCredentials performs the IMDSv2 handshake: PUT for a session token, GET
// the role name, then GET the role's credentials. Any failure returns an error;
// the caller must treat that as "no creds" and pass through.
func (c *IMDSClient) FetchCredentials(ctx context.Context) (Credentials, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	token, err := c.fetchToken(ctx)
	if err != nil {
		return Credentials{}, err
	}

	role, err := c.get(ctx, c.baseURL+imdsCredsPath, token)
	if err != nil {
		return Credentials{}, fmt.Errorf("fetch role name: %w", err)
	}
	role = strings.TrimSpace(role)
	if role == "" {
		return Credentials{}, fmt.Errorf("empty instance-profile role name")
	}

	body, err := c.get(ctx, c.baseURL+imdsCredsPath+role, token)
	if err != nil {
		return Credentials{}, fmt.Errorf("fetch role credentials: %w", err)
	}
	var doc imdsCredsDoc
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		return Credentials{}, fmt.Errorf("decode credentials: %w", err)
	}
	creds := Credentials{
		AccessKeyID:     doc.AccessKeyID,
		SecretAccessKey: doc.SecretAccessKey,
		SessionToken:    doc.Token,
	}
	if !creds.complete() {
		return Credentials{}, fmt.Errorf("incomplete credentials from IMDS")
	}
	return creds, nil
}

func (c *IMDSClient) fetchToken(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.baseURL+imdsTokenPath, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-aws-ec2-metadata-token-ttl-seconds", imdsTokenTTLSecs)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("imds token request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("imds token status %d", resp.StatusCode)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

func (c *IMDSClient) get(ctx context.Context, url, token string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-aws-ec2-metadata-token", token)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("imds status %d for %s", resp.StatusCode, url)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
