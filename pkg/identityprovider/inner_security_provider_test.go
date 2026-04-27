/*
Copyright 2026.

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
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

// ---------------------------------------------------------------------------
// resolveFilePath
// ---------------------------------------------------------------------------

func TestResolveFilePath(t *testing.T) {
	tests := []struct {
		name        string
		explicit    string
		certDir     string
		defaultName string
		expected    string
	}{
		{
			name:        "explicit takes precedence",
			explicit:    "/custom/ca.pem",
			certDir:     "/certs",
			defaultName: "ca.crt",
			expected:    "/custom/ca.pem",
		},
		{
			name:        "certDir + default name when no explicit",
			explicit:    "",
			certDir:     "/certs",
			defaultName: "ca.crt",
			expected:    filepath.Join("/certs", "ca.crt"),
		},
		{
			name:        "empty when both empty",
			explicit:    "",
			certDir:     "",
			defaultName: "ca.crt",
			expected:    "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveFilePath(tt.explicit, tt.certDir, tt.defaultName)
			assert.Equal(t, tt.expected, got)
		})
	}
}

// ---------------------------------------------------------------------------
// identityProviderOptionsFromEnv
// ---------------------------------------------------------------------------

func TestIdentityProviderOptionsFromEnv(t *testing.T) {
	tests := []struct {
		name   string
		envs   map[string]string
		verify func(t *testing.T, opts IdentityProviderOptions)
	}{
		{
			name: "basic endpoint and auth token",
			envs: map[string]string{
				EnvIdentityProviderEndpoint:  "https://idp.example.com/token",
				EnvIdentityProviderAuthToken: "bearer-123",
			},
			verify: func(t *testing.T, opts IdentityProviderOptions) {
				assert.Equal(t, "https://idp.example.com/token", opts.Endpoint)
				assert.Equal(t, "bearer-123", opts.AuthToken)
			},
		},
		{
			name: "cert dir resolves defaults",
			envs: map[string]string{
				EnvIdentityProviderEndpoint: "https://idp.example.com",
				EnvIdentityProviderCertDir:  "/run/secrets/certs",
			},
			verify: func(t *testing.T, opts IdentityProviderOptions) {
				assert.Equal(t, "/run/secrets/certs/ca.crt", opts.CAFile)
				assert.Equal(t, "/run/secrets/certs/client.crt", opts.CertFile)
				assert.Equal(t, "/run/secrets/certs/client.key", opts.KeyFile)
			},
		},
		{
			name: "individual cert files override cert dir",
			envs: map[string]string{
				EnvIdentityProviderEndpoint: "https://idp.example.com",
				EnvIdentityProviderCertDir:  "/run/secrets/certs",
				EnvIdentityProviderCAFile:   "/custom/my-ca.pem",
			},
			verify: func(t *testing.T, opts IdentityProviderOptions) {
				assert.Equal(t, "/custom/my-ca.pem", opts.CAFile)
				// CertFile and KeyFile should still come from cert dir.
				assert.Equal(t, "/run/secrets/certs/client.crt", opts.CertFile)
				assert.Equal(t, "/run/secrets/certs/client.key", opts.KeyFile)
			},
		},
		{
			name: "timeout parsed correctly",
			envs: map[string]string{
				EnvIdentityProviderEndpoint: "https://idp.example.com",
				EnvIdentityProviderTimeout:  "5s",
			},
			verify: func(t *testing.T, opts IdentityProviderOptions) {
				assert.Equal(t, 5*time.Second, opts.Timeout)
			},
		},
		{
			name: "invalid timeout keeps zero",
			envs: map[string]string{
				EnvIdentityProviderEndpoint: "https://idp.example.com",
				EnvIdentityProviderTimeout:  "not-a-duration",
			},
			verify: func(t *testing.T, opts IdentityProviderOptions) {
				assert.Zero(t, opts.Timeout)
			},
		},
		{
			name: "insecure skip verify true",
			envs: map[string]string{
				EnvIdentityProviderEndpoint:           "https://idp.example.com",
				EnvIdentityProviderInsecureSkipVerify: "true",
			},
			verify: func(t *testing.T, opts IdentityProviderOptions) {
				assert.True(t, opts.InsecureSkipVerify)
			},
		},
		{
			name: "insecure skip verify false",
			envs: map[string]string{
				EnvIdentityProviderEndpoint:           "https://idp.example.com",
				EnvIdentityProviderInsecureSkipVerify: "false",
			},
			verify: func(t *testing.T, opts IdentityProviderOptions) {
				assert.False(t, opts.InsecureSkipVerify)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Set env vars.
			for k, v := range tt.envs {
				t.Setenv(k, v)
			}
			opts := identityProviderOptionsFromEnv()
			tt.verify(t, opts)
		})
	}
}

// ---------------------------------------------------------------------------
// buildTLSConfig
// ---------------------------------------------------------------------------

// generateSelfSignedCert creates a temp self-signed cert/key pair for testing.
func generateSelfSignedCert(t *testing.T, dir string) (caFile, certFile, keyFile string) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		IsCA:         true,
	}
	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	caFile = filepath.Join(dir, "ca.crt")
	certFile = filepath.Join(dir, "client.crt")
	keyFile = filepath.Join(dir, "client.key")
	require.NoError(t, os.WriteFile(caFile, certPEM, 0600))
	require.NoError(t, os.WriteFile(certFile, certPEM, 0600))
	require.NoError(t, os.WriteFile(keyFile, keyPEM, 0600))
	return
}

func TestBuildTLSConfig_NoOptions(t *testing.T) {
	cfg, err := buildTLSConfig(IdentityProviderOptions{})
	require.NoError(t, err)
	assert.Nil(t, cfg.RootCAs, "no custom CA")
	assert.Empty(t, cfg.Certificates, "no client certs")
	assert.False(t, cfg.InsecureSkipVerify)
}

func TestBuildTLSConfig_InsecureSkipVerify(t *testing.T) {
	cfg, err := buildTLSConfig(IdentityProviderOptions{InsecureSkipVerify: true})
	require.NoError(t, err)
	assert.True(t, cfg.InsecureSkipVerify)
}

func TestBuildTLSConfig_WithCA(t *testing.T) {
	dir := t.TempDir()
	caFile, _, _ := generateSelfSignedCert(t, dir)

	cfg, err := buildTLSConfig(IdentityProviderOptions{CAFile: caFile})
	require.NoError(t, err)
	require.NotNil(t, cfg.RootCAs)
}

func TestBuildTLSConfig_WithMTLS(t *testing.T) {
	dir := t.TempDir()
	caFile, certFile, keyFile := generateSelfSignedCert(t, dir)

	cfg, err := buildTLSConfig(IdentityProviderOptions{
		CAFile:   caFile,
		CertFile: certFile,
		KeyFile:  keyFile,
	})
	require.NoError(t, err)
	require.NotNil(t, cfg.RootCAs)
	require.Len(t, cfg.Certificates, 1, "should have one client certificate")
}

func TestBuildTLSConfig_InvalidCAFile(t *testing.T) {
	_, err := buildTLSConfig(IdentityProviderOptions{CAFile: "/nonexistent/ca.crt"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to read CA file")
}

func TestBuildTLSConfig_InvalidCertKeyPair(t *testing.T) {
	dir := t.TempDir()
	certFile := filepath.Join(dir, "bad.crt")
	keyFile := filepath.Join(dir, "bad.key")
	require.NoError(t, os.WriteFile(certFile, []byte("not a cert"), 0600))
	require.NoError(t, os.WriteFile(keyFile, []byte("not a key"), 0600))

	_, err := buildTLSConfig(IdentityProviderOptions{CertFile: certFile, KeyFile: keyFile})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to load client certificate")
}

func TestBuildTLSConfig_InvalidCAContent(t *testing.T) {
	dir := t.TempDir()
	caFile := filepath.Join(dir, "bad-ca.crt")
	require.NoError(t, os.WriteFile(caFile, []byte("not pem"), 0600))

	_, err := buildTLSConfig(IdentityProviderOptions{CAFile: caFile})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to parse CA certificate")
}

// ---------------------------------------------------------------------------
// NewIdentityProviderTokenProvider
// ---------------------------------------------------------------------------

func TestNewIdentityProviderTokenProvider_DefaultTimeout(t *testing.T) {
	provider, err := NewIdentityProviderTokenProvider(IdentityProviderOptions{
		Endpoint:           "https://example.com",
		InsecureSkipVerify: true,
	})
	require.NoError(t, err)
	p := provider.(*identityProviderTokenProvider)
	assert.Equal(t, 10*time.Second, p.httpClient.Timeout)
}

func TestNewIdentityProviderTokenProvider_CustomTimeout(t *testing.T) {
	provider, err := NewIdentityProviderTokenProvider(IdentityProviderOptions{
		Endpoint:           "https://example.com",
		Timeout:            3 * time.Second,
		InsecureSkipVerify: true,
	})
	require.NoError(t, err)
	p := provider.(*identityProviderTokenProvider)
	assert.Equal(t, 3*time.Second, p.httpClient.Timeout)
}

func TestNewIdentityProviderTokenProvider_InvalidCA(t *testing.T) {
	_, err := NewIdentityProviderTokenProvider(IdentityProviderOptions{
		Endpoint: "https://example.com",
		CAFile:   "/nonexistent/ca.crt",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to build TLS config")
}

// ---------------------------------------------------------------------------
// identityProviderTokenProvider.IssueToken (httptest)
// ---------------------------------------------------------------------------

func TestIdentityProviderTokenProvider_IssueToken_Success(t *testing.T) {
	expected := TokenResponse{RequestID: "req-1", AccessToken: "issued-token"}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
		assert.Equal(t, ActionName, r.Header.Get("X-Api-Action-Name"))
		assert.Equal(t, "Bearer my-auth", r.Header.Get("Authorization"))

		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(expected)
	}))
	defer server.Close()

	provider := &identityProviderTokenProvider{
		endpoint:   server.URL,
		authToken:  "my-auth",
		httpClient: server.Client(),
	}
	resp, err := provider.IssueToken(context.Background(), TokenRequest{TokenType: TokenTypeAgent})
	require.NoError(t, err)
	assert.Equal(t, "req-1", resp.RequestID)
	assert.Equal(t, "issued-token", resp.AccessToken)
}

func TestIdentityProviderTokenProvider_IssueToken_Non200(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("server error"))
	}))
	defer server.Close()

	provider := &identityProviderTokenProvider{
		endpoint:   server.URL,
		httpClient: server.Client(),
	}
	_, err := provider.IssueToken(context.Background(), TokenRequest{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "identity provider returned status 500")
}

func TestIdentityProviderTokenProvider_IssueToken_EmptyAccessToken(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(TokenResponse{RequestID: "req-1"})
	}))
	defer server.Close()

	provider := &identityProviderTokenProvider{
		endpoint:   server.URL,
		httpClient: server.Client(),
	}
	_, err := provider.IssueToken(context.Background(), TokenRequest{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "identity provider returned empty access token")
}

func TestIdentityProviderTokenProvider_IssueToken_InvalidJSON(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("not json"))
	}))
	defer server.Close()

	provider := &identityProviderTokenProvider{
		endpoint:   server.URL,
		httpClient: server.Client(),
	}
	_, err := provider.IssueToken(context.Background(), TokenRequest{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to unmarshal identity provider response")
}

func TestIdentityProviderTokenProvider_IssueToken_ConnectionError(t *testing.T) {
	provider := &identityProviderTokenProvider{
		endpoint:   "https://127.0.0.1:1", // unreachable
		httpClient: &http.Client{Timeout: 100 * time.Millisecond, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}},
	}
	_, err := provider.IssueToken(context.Background(), TokenRequest{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to call identity provider")
}

func TestIdentityProviderTokenProvider_IssueToken_NoAuthHeader(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Empty(t, r.Header.Get("Authorization"), "no auth header when authToken is empty")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(TokenResponse{RequestID: "req-1", AccessToken: "tok"})
	}))
	defer server.Close()

	provider := &identityProviderTokenProvider{
		endpoint:   server.URL,
		authToken:  "", // no auth
		httpClient: server.Client(),
	}
	resp, err := provider.IssueToken(context.Background(), TokenRequest{})
	require.NoError(t, err)
	assert.NotEmpty(t, resp.AccessToken)
}

func TestIdentityProviderTokenProvider_IssueToken_CancelledContext(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(TokenResponse{RequestID: "r", AccessToken: "t"})
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	provider := &identityProviderTokenProvider{
		endpoint:   server.URL,
		httpClient: server.Client(),
	}
	_, err := provider.IssueToken(ctx, TokenRequest{})
	require.Error(t, err)
}

// ---------------------------------------------------------------------------
// secureIdentityProvider
// ---------------------------------------------------------------------------

func TestNewSecureIdentityProvider_CopiesPropagators(t *testing.T) {
	propagators := []SecurityTokenPropagator{
		func(_ context.Context, _ *agentsv1alpha1.Sandbox, _ *TokenResponse) error { return nil },
	}
	provider := newSecureIdentityProvider(&mockTokenProvider{}, propagators)
	sp := provider.(*secureIdentityProvider)

	// Modify original slice — internal copy should be unaffected.
	propagators = append(propagators, func(_ context.Context, _ *agentsv1alpha1.Sandbox, _ *TokenResponse) error { return nil })
	assert.Len(t, sp.propagators, 1, "internal copy should not be affected by external mutation")
}

func TestSecureIdentityProvider_IssueToken_Delegates(t *testing.T) {
	expected := &TokenResponse{RequestID: "r1", AccessToken: "t1"}
	mock := &mockTokenProvider{resp: expected}
	provider := newSecureIdentityProvider(mock, nil)

	resp, err := provider.IssueToken(context.Background(), TokenRequest{})
	require.NoError(t, err)
	assert.Equal(t, expected, resp)
}

func TestSecureIdentityProvider_PropagateSecurityToken_NoPropagators(t *testing.T) {
	provider := newSecureIdentityProvider(&mockTokenProvider{}, nil)
	err := provider.PropagateSecurityToken(context.Background(), &agentsv1alpha1.Sandbox{}, &TokenResponse{})
	assert.NoError(t, err, "no propagators should return nil")
}

func TestSecureIdentityProvider_PropagateSecurityToken_AllSuccess(t *testing.T) {
	var callOrder []int
	propagators := []SecurityTokenPropagator{
		func(_ context.Context, _ *agentsv1alpha1.Sandbox, _ *TokenResponse) error {
			callOrder = append(callOrder, 0)
			return nil
		},
		func(_ context.Context, _ *agentsv1alpha1.Sandbox, _ *TokenResponse) error {
			callOrder = append(callOrder, 1)
			return nil
		},
	}
	provider := newSecureIdentityProvider(&mockTokenProvider{}, propagators)
	err := provider.PropagateSecurityToken(context.Background(), &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
	}, &TokenResponse{AccessToken: "tok"})
	assert.NoError(t, err)
	assert.Equal(t, []int{0, 1}, callOrder, "all propagators should run in order")
}

func TestSecureIdentityProvider_PropagateSecurityToken_FirstErrorReturned(t *testing.T) {
	firstErr := fmt.Errorf("first error")
	secondErr := fmt.Errorf("second error")
	propagators := []SecurityTokenPropagator{
		func(_ context.Context, _ *agentsv1alpha1.Sandbox, _ *TokenResponse) error { return firstErr },
		func(_ context.Context, _ *agentsv1alpha1.Sandbox, _ *TokenResponse) error { return secondErr },
	}
	provider := newSecureIdentityProvider(&mockTokenProvider{}, propagators)
	err := provider.PropagateSecurityToken(context.Background(), &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
	}, &TokenResponse{AccessToken: "tok"})
	assert.Equal(t, firstErr, err, "should return first error")
}

func TestSecureIdentityProvider_PropagateSecurityToken_ContinuesAfterError(t *testing.T) {
	var called []int
	propagators := []SecurityTokenPropagator{
		func(_ context.Context, _ *agentsv1alpha1.Sandbox, _ *TokenResponse) error {
			called = append(called, 0)
			return fmt.Errorf("fail")
		},
		func(_ context.Context, _ *agentsv1alpha1.Sandbox, _ *TokenResponse) error {
			called = append(called, 1)
			return nil
		},
	}
	provider := newSecureIdentityProvider(&mockTokenProvider{}, propagators)
	_ = provider.PropagateSecurityToken(context.Background(), &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
	}, &TokenResponse{AccessToken: "tok"})
	assert.Equal(t, []int{0, 1}, called, "should continue after first propagator error")
}

// ---------------------------------------------------------------------------
// initSecureProvider
// ---------------------------------------------------------------------------

func TestInitSecureProvider_NoEndpoint(t *testing.T) {
	savedProvider := DefaultProvider
	defer func() { DefaultProvider = savedProvider }()

	// Ensure no endpoint is set.
	t.Setenv(EnvIdentityProviderEndpoint, "")

	DefaultProvider = NewUUIDIdentityProvider()
	initSecureProvider()

	// DefaultProvider should remain UUID.
	_, ok := DefaultProvider.(*uuidTokenProvider)
	assert.True(t, ok, "should stay UUID provider when endpoint is empty")
}

func TestInitSecureProvider_WithEndpoint(t *testing.T) {
	savedProvider := DefaultProvider
	savedPropagators := securityTokenPropagators
	defer func() {
		DefaultProvider = savedProvider
		securityTokenPropagators = savedPropagators
	}()
	securityTokenPropagators = nil

	// Start a test HTTPS server.
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(TokenResponse{RequestID: "r", AccessToken: "t"})
	}))
	defer server.Close()

	t.Setenv(EnvIdentityProviderEndpoint, server.URL)
	t.Setenv(EnvIdentityProviderInsecureSkipVerify, "true")

	DefaultProvider = NewUUIDIdentityProvider()
	initSecureProvider()

	_, ok := DefaultProvider.(*secureIdentityProvider)
	assert.True(t, ok, "should be secureIdentityProvider when endpoint is set")
}
