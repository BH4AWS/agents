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
	"os"
	"path/filepath"
	"strconv"
	"time"

	"k8s.io/klog/v2"

	"github.com/openkruise/agents/pkg/features"
	utilfeature "github.com/openkruise/agents/pkg/utils/feature"
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

// DefaultProvider is the global TokenProvider instance used for issuing sandbox access tokens.
//
// Initialization:
//   - init(): Always sets UUID-based provider as the safe community default.
//   - InitProvider(): Must be called after pflag.Parse(). When SecurityIdentityProviderGate
//     is enabled and IDENTITY_PROVIDER_ENDPOINT is configured, upgrades to HTTPS-based
//     identity provider with UUID fallback.
//
// All callers should use this variable directly: identityprovider.DefaultProvider.IssueToken(ctx, req)
var DefaultProvider TokenProvider

func init() {
	// Community default: UUID-based token provider.
	// init() runs before pflag.Parse(), so FeatureGate is not available here.
	// The real provider initialization happens in InitProvider() after flag parsing.
	DefaultProvider = NewUUIDTokenProvider()
}

// InitProvider initializes the DefaultProvider based on the SecurityIdentityProviderGate feature gate.
// This MUST be called after pflag.Parse() so that feature gate values are available.
//
// Behavior:
//   - FeatureGate disabled (default): keeps UUID provider — no changes.
//   - FeatureGate enabled + IDENTITY_PROVIDER_ENDPOINT set: creates HTTPS provider with UUID fallback.
//   - FeatureGate enabled + IDENTITY_PROVIDER_ENDPOINT not set: logs warning, keeps UUID provider.
func InitProvider() {
	if !utilfeature.DefaultFeatureGate.Enabled(features.SecurityIdentityProviderGate) {
		klog.Infof("identity provider: %s feature gate is disabled, using UUID mode (community default)",
			features.SecurityIdentityProviderGate)
		return
	}

	endpoint := os.Getenv(EnvIdentityProviderEndpoint)
	if endpoint == "" {
		klog.Warningf("identity provider: %s feature gate is enabled but %s is not set, using UUID mode",
			features.SecurityIdentityProviderGate, EnvIdentityProviderEndpoint)
		return
	}

	// FeatureGate enabled + endpoint configured: create the real identity provider.
	klog.Infof("identity provider: %s feature gate is enabled, initializing HTTPS provider (endpoint: %s)",
		features.SecurityIdentityProviderGate, endpoint)
	opts := identityProviderOptionsFromEnv()
	provider, err := NewIdentityProviderTokenProvider(opts)
	if err != nil {
		klog.Errorf("failed to create identity provider: %v, keeping UUID mode", err)
		return
	}
	// Wrap with fallback: if the real provider fails at runtime, auto-degrade to UUID.
	DefaultProvider = NewFallbackTokenProvider(provider)
	klog.Infof("identity provider initialized: HTTPS mode with UUID fallback (endpoint: %s)", endpoint)
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

// RegisterTokenProvider replaces the global DefaultProvider with a custom implementation.
// This is provided for testing or programmatic registration.
func RegisterTokenProvider(provider TokenProvider) {
	DefaultProvider = provider
	klog.Infof("identity provider replaced: %T", provider)
}
