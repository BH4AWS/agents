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
	"path/filepath"
	"strconv"
	"time"

	"k8s.io/klog/v2"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/features"
)

// Environment variable keys for identity provider configuration.
// These provide the connection parameters when SecurityIdentityProviderGate is enabled.
const (
	EnvIdentityProviderEndpoint           = "IDENTITY_PROVIDER_ENDPOINT"
	EnvIdentityProviderAuthToken          = "IDENTITY_PROVIDER_AUTH_TOKEN"
	EnvIdentityProviderCAFile             = "IDENTITY_PROVIDER_CA_FILE"
	EnvIdentityProviderCertFile           = "IDENTITY_PROVIDER_CERT_FILE"
	EnvIdentityProviderKeyFile            = "IDENTITY_PROVIDER_KEY_FILE"
	EnvIdentityProviderTimeout            = "IDENTITY_PROVIDER_TIMEOUT"
	EnvIdentityProviderInsecureSkipVerify = "IDENTITY_PROVIDER_INSECURE_SKIP_VERIFY"

	// EnvIdentityProviderCertDir is the mount path of a Kubernetes Secret volume
	// containing mTLS certificates. When set, the provider automatically resolves:
	//   <dir>/ca.crt      → CA certificate
	//   <dir>/client.crt   → client certificate
	//   <dir>/client.key   → client private key
	// Individual file env vars (CA_FILE, CERT_FILE, KEY_FILE) take precedence if set.
	EnvIdentityProviderCertDir = "IDENTITY_PROVIDER_CERT_DIR"
)

// Default file names for Secret volume mount.
const (
	DefaultCACertFileName     = "ca.crt"
	DefaultClientCertFileName = "client.crt"
	DefaultClientKeyFileName  = "client.key"
)

// initSecureProvider creates and registers the secure identity provider.
// Called by init() in inner_config.go for internal deployments.
//
// Behavior:
//   - FeatureGate disabled: keeps UUID provider (community default).
//   - IDENTITY_PROVIDER_ENDPOINT not set: logs warning, keeps UUID provider.
//   - IDENTITY_PROVIDER_ENDPOINT set: creates secureIdentityProvider with
//     HTTPS token issuance (UUID fallback) and all registered propagators.
func initSecureProvider() {
	endpoint := os.Getenv(EnvIdentityProviderEndpoint)
	if endpoint == "" {
		klog.Warningf("identity provider: %s feature gate is enabled but %s is not set, using UUID mode",
			features.SecurityIdentityProviderGate, EnvIdentityProviderEndpoint)
		return
	}

	klog.Infof("identity provider: %s feature gate is enabled, initializing secure provider (endpoint: %s)",
		features.SecurityIdentityProviderGate, endpoint)
	opts := identityProviderOptionsFromEnv()
	provider, err := NewIdentityProviderTokenProvider(opts)
	if err != nil {
		klog.Errorf("failed to create identity provider: %v, keeping UUID mode", err)
		return
	}
	// Wrap with fallback: if the real provider fails at runtime, auto-degrade to UUID.
	tokenProvider := NewFallbackTokenProvider(provider)
	// Create secure identity provider with HTTPS token issuance and registered propagators.
	DefaultProvider = newSecureIdentityProvider(tokenProvider, securityTokenPropagators)
	klog.Infof("identity provider initialized: secure mode with %d propagator(s) (endpoint: %s)",
		len(securityTokenPropagators), endpoint)
}

// identityProviderOptionsFromEnv reads identity provider configuration from environment variables.
// Certificate resolution priority:
//  1. Individual file env vars (IDENTITY_PROVIDER_CA_FILE, etc.) — highest priority.
//  2. IDENTITY_PROVIDER_CERT_DIR — Secret volume mount directory with conventional file names.
//  3. Empty — no mTLS, only server-side TLS.
func identityProviderOptionsFromEnv() IdentityProviderOptions {
	opts := IdentityProviderOptions{
		Endpoint:  os.Getenv(EnvIdentityProviderEndpoint),
		AuthToken: os.Getenv(EnvIdentityProviderAuthToken),
	}

	// Resolve certificate file paths: individual env vars take precedence over cert dir.
	certDir := os.Getenv(EnvIdentityProviderCertDir)
	opts.CAFile = resolveFilePath(os.Getenv(EnvIdentityProviderCAFile), certDir, DefaultCACertFileName)
	opts.CertFile = resolveFilePath(os.Getenv(EnvIdentityProviderCertFile), certDir, DefaultClientCertFileName)
	opts.KeyFile = resolveFilePath(os.Getenv(EnvIdentityProviderKeyFile), certDir, DefaultClientKeyFileName)

	if certDir != "" {
		klog.Infof("identity provider cert dir: %s, resolved: ca=%s, cert=%s, key=%s",
			certDir, opts.CAFile, opts.CertFile, opts.KeyFile)
	}

	if timeoutStr := os.Getenv(EnvIdentityProviderTimeout); timeoutStr != "" {
		if d, err := time.ParseDuration(timeoutStr); err == nil {
			opts.Timeout = d
		} else {
			klog.Warningf("invalid %s value %q, using default: %v", EnvIdentityProviderTimeout, timeoutStr, err)
		}
	}

	if skipVerifyStr := os.Getenv(EnvIdentityProviderInsecureSkipVerify); skipVerifyStr != "" {
		if v, err := strconv.ParseBool(skipVerifyStr); err == nil {
			opts.InsecureSkipVerify = v
		}
	}

	return opts
}

// resolveFilePath returns the explicit path if set; otherwise joins certDir + defaultName.
// Returns empty string if both explicit and certDir are empty.
func resolveFilePath(explicit, certDir, defaultName string) string {
	if explicit != "" {
		return explicit
	}
	if certDir != "" {
		return filepath.Join(certDir, defaultName)
	}
	return ""
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

// secureIdentityProvider is the internal deployment implementation that wraps a TokenProvider
// (typically HTTPS + fallback) with registered security token propagators.
// It implements IdentityProvider with real propagation.
type secureIdentityProvider struct {
	tokenProvider TokenProvider
	propagators   []SecurityTokenPropagator
}

// newSecureIdentityProvider creates an IdentityProvider that delegates token issuance to the
// given TokenProvider and executes the provided propagators after token propagation.
// Propagators are copied to avoid mutation after construction.
func newSecureIdentityProvider(tokenProvider TokenProvider, propagators []SecurityTokenPropagator) IdentityProvider {
	copied := make([]SecurityTokenPropagator, len(propagators))
	copy(copied, propagators)
	return &secureIdentityProvider{
		tokenProvider: tokenProvider,
		propagators:   copied,
	}
}

func (s *secureIdentityProvider) IssueToken(ctx context.Context, req TokenRequest) (*TokenResponse, error) {
	return s.tokenProvider.IssueToken(ctx, req)
}

// PropagateSecurityToken executes all registered propagators sequentially.
// Returns the first encountered error, but continues executing remaining propagators.
func (s *secureIdentityProvider) PropagateSecurityToken(ctx context.Context, sbx *agentsv1alpha1.Sandbox, tokenResp *TokenResponse) error {
	if len(s.propagators) == 0 {
		return nil
	}
	log := klog.FromContext(ctx)
	start := time.Now()
	var firstErr error
	for i, propagator := range s.propagators {
		propStart := time.Now()
		if propErr := propagator(ctx, sbx, tokenResp); propErr != nil {
			log.Error(propErr, "security token propagator failed", "propagatorIndex", i, "cost", time.Since(propStart))
			if firstErr == nil {
				firstErr = propErr
			}
		} else {
			log.V(5).Info("security token propagator completed", "propagatorIndex", i, "cost", time.Since(propStart))
		}
	}
	log.Info("security token propagation completed", "propagatorCount", len(s.propagators), "cost", time.Since(start))
	return firstErr
}
