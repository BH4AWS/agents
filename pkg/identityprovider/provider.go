/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package identityprovider

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/google/uuid"
	"k8s.io/klog/v2"
)

// TokenProvider is the interface for issuing sandbox access tokens.
// Implementations can provide simple UUID-based tokens or identity-aware tokens
// from an external identity provider service.
type TokenProvider interface {
	// IssueToken generates an access token for the given token request.
	// The context can carry deadlines and cancellation signals.
	IssueToken(ctx context.Context, req TokenRequest) (*TokenResponse, error)
}

// uuidTokenProvider is the default community implementation that generates
// random UUID tokens without contacting any external service.
type uuidTokenProvider struct{}

// NewUUIDTokenProvider creates a TokenProvider that generates random UUID-based tokens.
// This is the default fallback used when no external identity provider is configured.
func NewUUIDTokenProvider() TokenProvider {
	return &uuidTokenProvider{}
}

func (u *uuidTokenProvider) IssueToken(_ context.Context, _ TokenRequest) (*TokenResponse, error) {
	return &TokenResponse{
		RequestID:   uuid.NewString(),
		AccessToken: uuid.NewString(),
	}, nil
}

// identityProviderTokenProvider issues tokens by calling an external identity provider service via HTTPS.
type identityProviderTokenProvider struct {
	endpoint   string
	authToken  string
	httpClient *http.Client
}

// IdentityProviderOptions contains configuration for the external identity provider.
type IdentityProviderOptions struct {
	// Endpoint is the HTTPS URL of the identity provider token issuance API.
	Endpoint string

	// AuthToken is the Bearer token used for authenticating with the identity provider.
	AuthToken string

	// Timeout is the HTTP request timeout for calling the identity provider. Defaults to 10s.
	Timeout time.Duration

	// CAFile is the path to the PEM-encoded CA certificate used to verify the identity provider's server certificate.
	// If empty, the system root CAs are used.
	CAFile string

	// CertFile is the path to the PEM-encoded client certificate for mTLS authentication.
	// Must be used together with KeyFile.
	CertFile string

	// KeyFile is the path to the PEM-encoded client private key for mTLS authentication.
	// Must be used together with CertFile.
	KeyFile string

	// InsecureSkipVerify controls whether the TLS certificate verification is skipped.
	// This should only be used in development/testing environments.
	InsecureSkipVerify bool
}

// NewIdentityProviderTokenProvider creates a TokenProvider that issues tokens by calling
// an external identity provider service via HTTPS.
func NewIdentityProviderTokenProvider(opts IdentityProviderOptions) (TokenProvider, error) {
	timeout := opts.Timeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}

	tlsConfig, err := buildTLSConfig(opts)
	if err != nil {
		return nil, fmt.Errorf("failed to build TLS config for identity provider: %w", err)
	}

	transport := &http.Transport{
		TLSClientConfig: tlsConfig,

		// Connection pooling: reuse connections to the single identity provider endpoint.
		MaxIdleConns:        10,
		MaxIdleConnsPerHost: 10,
		MaxConnsPerHost:     20,

		// Idle connection reaping: proactively close idle connections to avoid stale sockets
		// and upstream load balancer idle timeouts (typically 60s–120s).
		IdleConnTimeout: 30 * time.Second,

		// TLS handshake timeout: bound the TLS negotiation to avoid hanging on slow endpoints.
		TLSHandshakeTimeout: 5 * time.Second,

		// Response header timeout: fail fast if the server accepts the connection but
		// does not start responding within this duration.
		ResponseHeaderTimeout: 8 * time.Second,

		// Expect-Continue timeout: for POST requests, limit the wait for "100 Continue".
		ExpectContinueTimeout: 1 * time.Second,

		// Disable keep-alive if explicitly requested (not recommended for production).
		DisableKeepAlives: false,

		// Force attempt HTTP/2 when TLS is used, improving multiplexing and latency.
		ForceAttemptHTTP2: true,
	}

	return &identityProviderTokenProvider{
		endpoint:  opts.Endpoint,
		authToken: opts.AuthToken,
		httpClient: &http.Client{
			Timeout:   timeout,
			Transport: transport,
		},
	}, nil
}

// buildTLSConfig constructs a tls.Config based on the provided options.
// It supports custom CA certificate, client certificate (mTLS), and InsecureSkipVerify.
func buildTLSConfig(opts IdentityProviderOptions) (*tls.Config, error) {
	tlsConfig := &tls.Config{
		InsecureSkipVerify: opts.InsecureSkipVerify, //nolint:gosec // configurable for dev/test environments
	}

	// Load custom CA certificate
	if opts.CAFile != "" {
		caCert, err := os.ReadFile(opts.CAFile)
		if err != nil {
			return nil, fmt.Errorf("failed to read CA file %s: %w", opts.CAFile, err)
		}
		caPool := x509.NewCertPool()
		if !caPool.AppendCertsFromPEM(caCert) {
			return nil, fmt.Errorf("failed to parse CA certificate from %s", opts.CAFile)
		}
		tlsConfig.RootCAs = caPool
	}

	// Load client certificate for mTLS
	if opts.CertFile != "" && opts.KeyFile != "" {
		cert, err := tls.LoadX509KeyPair(opts.CertFile, opts.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("failed to load client certificate (%s, %s): %w", opts.CertFile, opts.KeyFile, err)
		}
		tlsConfig.Certificates = []tls.Certificate{cert}
	}

	return tlsConfig, nil
}

// fallbackTokenProvider wraps a primary TokenProvider and falls back to the
// UUID-based provider when the primary one returns an error. This ensures that
// sandbox claim is never blocked by an external identity provider outage.
type fallbackTokenProvider struct {
	primary  TokenProvider
	fallback TokenProvider
}

// NewFallbackTokenProvider creates a TokenProvider that delegates to the primary provider
// and automatically falls back to UUID-based token generation on any error.
func NewFallbackTokenProvider(primary TokenProvider) TokenProvider {
	return &fallbackTokenProvider{
		primary:  primary,
		fallback: NewUUIDTokenProvider(),
	}
}

func (f *fallbackTokenProvider) IssueToken(ctx context.Context, req TokenRequest) (*TokenResponse, error) {
	logger := klog.FromContext(ctx)
	resp, err := f.primary.IssueToken(ctx, req)
	if err != nil {
		logger.Error(err, "primary token provider failed, falling back to UUID token provider")
		return f.fallback.IssueToken(ctx, req)
	}
	return resp, nil
}

func (p *identityProviderTokenProvider) IssueToken(ctx context.Context, req TokenRequest) (*TokenResponse, error) {
	logger := klog.FromContext(ctx)
	start := time.Now()

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal token request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create HTTP request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Api-Action-Name", ActionName)
	if p.authToken != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.authToken)
	}

	logger.V(5).Info("issuing token from identity provider", "endpoint", p.endpoint, "tokenType", req.TokenType)

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("failed to call identity provider: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read identity provider response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("identity provider returned status %d: %s", resp.StatusCode, string(respBody))
	}

	var tokenResp TokenResponse
	if err := json.Unmarshal(respBody, &tokenResp); err != nil {
		return nil, fmt.Errorf("failed to unmarshal identity provider response: %w", err)
	}

	if tokenResp.AccessToken == "" {
		return nil, fmt.Errorf("identity provider returned empty access token")
	}

	logger.Info("token issued from identity provider", "requestId", tokenResp.RequestID, "tokenType", req.TokenType, "cost", time.Since(start))
	return &tokenResp, nil
}
