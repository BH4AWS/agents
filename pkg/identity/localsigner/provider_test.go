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

package localsigner

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	securityv1alpha1 "github.com/openkruise/agents/api/security/v1alpha1"
	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/identity"
	"github.com/openkruise/agents/pkg/identity/jwtissuer"
)

const (
	testIssuerURL  = "https://identity.example"
	testNamespace  = "team-a"
	testAgentName  = "sample-agent"
	testSandboxUID = "89d24507-936c-4a04-a958-b5d6a8277ed5"
)

type sandboxClaim struct {
	SandboxID  string `json:"sandboxId"`
	SandboxUID string `json:"sandboxUid"`
}

type trafficClaims struct {
	jwt.Claims
	Sandbox sandboxClaim `json:"sandbox"`
}

type agentClaims struct {
	jwt.Claims
	TokenType         string `json:"token_type"`
	Namespace         string `json:"namespace"`
	AgentIdentityName string `json:"agent_identity_name"`
	SandboxUID        string `json:"sandbox_uid"`
}

func newTestProvider(t *testing.T, objects ...*securityv1alpha1.AgentIdentity) *Provider {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, securityv1alpha1.AddToScheme(scheme))
	builder := fake.NewClientBuilder().WithScheme(scheme)
	for _, obj := range objects {
		builder = builder.WithObjects(obj)
	}

	provider, err := New(newTestIssuer(t), builder.Build())
	require.NoError(t, err)
	return provider
}

func newTestIssuer(t *testing.T) *jwtissuer.Issuer {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	issuer, err := jwtissuer.New(testIssuerURL, jwtissuer.SigningKey{KeyID: "test", PrivateKey: key})
	require.NoError(t, err)
	return issuer
}

func testSandbox(annotations map[string]string) *agentsv1alpha1.Sandbox {
	return &agentsv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{
		Name:        "sandbox-a",
		Namespace:   testNamespace,
		UID:         types.UID(testSandboxUID),
		Annotations: annotations,
	}}
}

func testAgentIdentity(generation int64, ready metav1.ConditionStatus) *securityv1alpha1.AgentIdentity {
	return &securityv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{
			Name:       testAgentName,
			Namespace:  testNamespace,
			Generation: generation,
		},
		Status: securityv1alpha1.AgentIdentityStatus{
			ObservedGeneration: generation,
			Conditions: []metav1.Condition{{
				Type:               securityv1alpha1.ConditionReady,
				Status:             ready,
				ObservedGeneration: generation,
				Reason:             "Validated",
			}},
		},
	}
}

func decodeTrafficClaims(t *testing.T, rawJWT string) trafficClaims {
	t.Helper()
	parsed, err := jwt.ParseSigned(rawJWT, []jose.SignatureAlgorithm{jose.ES256})
	require.NoError(t, err)
	var claims trafficClaims
	require.NoError(t, parsed.UnsafeClaimsWithoutVerification(&claims))
	return claims
}

func decodeAgentClaims(t *testing.T, rawJWT string) agentClaims {
	t.Helper()
	parsed, err := jwt.ParseSigned(rawJWT, []jose.SignatureAlgorithm{jose.ES256})
	require.NoError(t, err)
	var claims agentClaims
	require.NoError(t, parsed.UnsafeClaimsWithoutVerification(&claims))
	return claims
}

func TestNew(t *testing.T) {
	issuer := newTestIssuer(t)

	_, err := New(nil, fake.NewClientBuilder().Build())
	assert.ErrorContains(t, err, "issuer is required")

	_, err = New(issuer, nil)
	assert.ErrorContains(t, err, "reader is required")
}

func TestIssueAccessToken(t *testing.T) {
	tests := []struct {
		name     string
		validity time.Duration
		expected time.Duration
	}{
		{name: "zero validity uses the issuer default", validity: 0, expected: jwtissuer.DefaultTokenLifetime},
		{name: "requested validity is honoured", validity: 30 * time.Minute, expected: 30 * time.Minute},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := newTestProvider(t)
			before := time.Now()

			resp, err := provider.IssueToken(context.Background(), testSandbox(nil),
				identity.TokenOptions{Kind: identity.TokenKindAccessToken, RequestedValidity: tt.validity})
			require.NoError(t, err)
			require.NotEmpty(t, resp.RequestID)
			require.NotEmpty(t, resp.AccessToken)

			expiresAt, err := time.Parse(time.RFC3339, resp.AccessTokenExpiration)
			require.NoError(t, err)
			assert.WithinDuration(t, before.Add(tt.expected), expiresAt, 5*time.Second)

			claims := decodeTrafficClaims(t, resp.AccessToken)
			assert.Equal(t, testIssuerURL, claims.Issuer)
			assert.Equal(t, "e2b:controlplane:team-a--sandbox-a", claims.Subject)
			assert.Equal(t, "team-a--sandbox-a", claims.Sandbox.SandboxID)
			assert.Equal(t, testSandboxUID, claims.Sandbox.SandboxUID)
			assert.Equal(t, expiresAt.Unix(), claims.Expiry.Time().Unix())
		})
	}
}

func TestIssueIDToken(t *testing.T) {
	provider := newTestProvider(t, testAgentIdentity(3, metav1.ConditionTrue))
	sbx := testSandbox(map[string]string{identity.AnnotationAgentName: testAgentName})

	resp, err := provider.IssueToken(context.Background(), sbx, identity.TokenOptions{Kind: identity.TokenKindIDToken})
	require.NoError(t, err)

	claims := decodeAgentClaims(t, resp.AccessToken)
	assert.Equal(t, testIssuerURL, claims.Issuer)
	assert.Equal(t, "agent:team-a:sample-agent", claims.Subject)
	assert.Equal(t, jwtissuer.AgentTokenType, claims.TokenType)
	assert.Equal(t, jwtissuer.AgentTokenAudience, claims.Audience[0])
	assert.Equal(t, testNamespace, claims.Namespace)
	assert.Equal(t, testAgentName, claims.AgentIdentityName)
	assert.Equal(t, testSandboxUID, claims.SandboxUID)

	expiresAt, err := time.Parse(time.RFC3339, resp.AccessTokenExpiration)
	require.NoError(t, err)
	assert.Equal(t, expiresAt.Unix(), claims.Expiry.Time().Unix())
}

func TestIssueTokenFailsClosed(t *testing.T) {
	annotated := func() map[string]string {
		return map[string]string{identity.AnnotationAgentName: testAgentName}
	}
	deleting := testAgentIdentity(1, metav1.ConditionTrue)
	deleting.DeletionTimestamp = ptrTo(metav1.Now())
	deleting.Finalizers = []string{"test"}
	stale := testAgentIdentity(2, metav1.ConditionTrue)
	stale.Status.Conditions[0].ObservedGeneration = 1

	tests := []struct {
		name        string
		objects     []*securityv1alpha1.AgentIdentity
		sbx         *agentsv1alpha1.Sandbox
		kind        identity.TokenKind
		expectError string
	}{
		{
			name:        "nil sandbox",
			sbx:         nil,
			expectError: "sandbox is required",
		},
		{
			name:        "sandbox without UID",
			sbx:         &agentsv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-a", Namespace: testNamespace}},
			expectError: "has no UID",
		},
		{
			name:        "unsupported token kind",
			sbx:         testSandbox(annotated()),
			kind:        identity.TokenKind("Unknown"),
			expectError: "unsupported token kind",
		},
		{
			name:        "ID token without agent annotation",
			sbx:         testSandbox(nil),
			kind:        identity.TokenKindIDToken,
			expectError: "does not select an AgentIdentity",
		},
		{
			name:        "AgentIdentity not found",
			sbx:         testSandbox(annotated()),
			kind:        identity.TokenKindIDToken,
			expectError: "get AgentIdentity",
		},
		{
			name:        "AgentIdentity being deleted",
			objects:     []*securityv1alpha1.AgentIdentity{deleting},
			sbx:         testSandbox(annotated()),
			kind:        identity.TokenKindIDToken,
			expectError: "is being deleted",
		},
		{
			name:        "AgentIdentity without a Ready condition",
			objects:     []*securityv1alpha1.AgentIdentity{{ObjectMeta: metav1.ObjectMeta{Name: testAgentName, Namespace: testNamespace}}},
			sbx:         testSandbox(annotated()),
			kind:        identity.TokenKindIDToken,
			expectError: "has not reported readiness",
		},
		{
			name:        "AgentIdentity readiness describes an older generation",
			objects:     []*securityv1alpha1.AgentIdentity{stale},
			sbx:         testSandbox(annotated()),
			kind:        identity.TokenKindIDToken,
			expectError: "readiness describes generation",
		},
		{
			name:        "AgentIdentity not Ready",
			objects:     []*securityv1alpha1.AgentIdentity{testAgentIdentity(1, metav1.ConditionFalse)},
			sbx:         testSandbox(annotated()),
			kind:        identity.TokenKindIDToken,
			expectError: "is not Ready",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := newTestProvider(t, tt.objects...)
			resp, err := provider.IssueToken(context.Background(), tt.sbx, identity.TokenOptions{Kind: tt.kind})
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.expectError)
			assert.Nil(t, resp)
		})
	}
}

func TestPropagateSecurityToken(t *testing.T) {
	provider := newTestProvider(t)
	assert.NoError(t, provider.PropagateSecurityToken(context.Background(), testSandbox(nil),
		&identity.TokenResponse{AccessToken: "token"}))
}

func TestOptions(t *testing.T) {
	assert.False(t, Options{}.Enabled())
	assert.True(t, Options{IssuerURL: testIssuerURL}.Enabled())
	assert.True(t, Options{SigningKeyFile: "/key.pem"}.Enabled())

	_, err := Options{IssuerURL: testIssuerURL}.NewIssuer()
	assert.ErrorContains(t, err, "must be set together")

	_, err = Options{IssuerURL: "http://identity.example", SigningKeyFile: "/does-not-exist.pem"}.NewIssuer()
	assert.ErrorContains(t, err, "read signing key")
}

func ptrTo[T any](v T) *T {
	return &v
}
