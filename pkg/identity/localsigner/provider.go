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

// Package localsigner implements identity.IdentityProvider by signing sandbox
// tokens in-process with a local jwtissuer.Issuer.
//
// It is the community counterpart of the enterprise identity-provider client:
// there is no network hop, only a signing key supplied at startup. The tokens
// minted here are the ones the sandbox gateway verifies against the JWKS the
// same issuer publishes, so issuance and verification share one key.
package localsigner

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	securityv1alpha1 "github.com/openkruise/agents/api/security/v1alpha1"
	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/identity"
	"github.com/openkruise/agents/pkg/identity/jwtissuer"
	"github.com/openkruise/agents/pkg/sandboxid"
	agentsruntime "github.com/openkruise/agents/pkg/utils/runtime"
)

// accessTokenSubjectPrefix mirrors the subject shape the gateway access-token
// path already uses, so a locally minted token carries the same identity as one
// minted by an external identity provider.
const accessTokenSubjectPrefix = "e2b:controlplane:"

// +kubebuilder:rbac:groups=security.agents.kruise.io,resources=agentidentities,verbs=get;list;watch

// Provider signs sandbox tokens with a local issuer and verifies the declared
// AgentIdentity before minting an ID token.
type Provider struct {
	issuer *jwtissuer.Issuer
	reader client.Reader
}

// New returns a Provider. Both arguments are required: without an issuer there
// is nothing to sign with, and without a reader the AgentIdentity readiness
// contract cannot be enforced.
func New(issuer *jwtissuer.Issuer, reader client.Reader) (*Provider, error) {
	if issuer == nil {
		return nil, errors.New("localsigner: issuer is required")
	}
	if reader == nil {
		return nil, errors.New("localsigner: AgentIdentity reader is required")
	}
	return &Provider{issuer: issuer, reader: reader}, nil
}

// IssueToken mints the token selected by opts.Kind.
//
// The access token binds the Sandbox ID and UID the gateway checks against the
// selected route. The ID token binds the logical agent the sandbox selected
// through its annotation, and is issued only while that AgentIdentity is Ready.
func (p *Provider) IssueToken(ctx context.Context, sbx *agentsv1alpha1.Sandbox, opts identity.TokenOptions) (*identity.TokenResponse, error) {
	if sbx == nil {
		return nil, errors.New("localsigner: sandbox is required to issue a token")
	}
	if sbx.UID == "" {
		return nil, fmt.Errorf("localsigner: sandbox %s/%s has no UID", sbx.Namespace, sbx.Name)
	}

	switch opts.Kind {
	case identity.TokenKindAccessToken:
		return p.issueAccessToken(sbx, opts.RequestedValidity)
	case identity.TokenKindIDToken, "":
		return p.issueIDToken(ctx, sbx)
	default:
		return nil, fmt.Errorf("localsigner: unsupported token kind %q", opts.Kind)
	}
}

func (p *Provider) issueAccessToken(sbx *agentsv1alpha1.Sandbox, validity time.Duration) (*identity.TokenResponse, error) {
	sandboxID := sandboxid.Resolve(sbx)
	rawJWT, expiry, err := p.issuer.IssueTrafficAccessToken(
		accessTokenSubjectPrefix+sandboxID,
		jwtissuer.SandboxBinding{SandboxID: sandboxID, SandboxUID: string(sbx.UID)},
		validity,
	)
	if err != nil {
		return nil, fmt.Errorf("localsigner: sign access token for sandbox %s/%s: %w", sbx.Namespace, sbx.Name, err)
	}
	return newTokenResponse(rawJWT, expiry), nil
}

func (p *Provider) issueIDToken(ctx context.Context, sbx *agentsv1alpha1.Sandbox) (*identity.TokenResponse, error) {
	agentName := sbx.GetAnnotations()[identity.AnnotationAgentName]
	if agentName == "" {
		return nil, fmt.Errorf("localsigner: sandbox %s/%s does not select an AgentIdentity via annotation %q",
			sbx.Namespace, sbx.Name, identity.AnnotationAgentName)
	}
	if err := p.requireAgentIdentityReady(ctx, sbx.Namespace, agentName); err != nil {
		return nil, err
	}

	rawJWT, expiry, err := p.issuer.IssueAgentToken(jwtissuer.AgentBinding{
		Namespace:         sbx.Namespace,
		AgentIdentityName: agentName,
		SandboxUID:        string(sbx.UID),
	}, 0)
	if err != nil {
		return nil, fmt.Errorf("localsigner: sign ID token for sandbox %s/%s: %w", sbx.Namespace, sbx.Name, err)
	}
	return newTokenResponse(rawJWT, expiry), nil
}

// requireAgentIdentityReady enforces the AgentIdentity contract: a sandbox that
// selects an identity may receive a token only while that identity's Ready
// verdict describes its current spec. A missing identity, deletion, a missing
// condition, a stale generation and a negative verdict all fail closed.
func (p *Provider) requireAgentIdentityReady(ctx context.Context, namespace, name string) error {
	agentIdentity := &securityv1alpha1.AgentIdentity{}
	if err := p.reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, agentIdentity); err != nil {
		return fmt.Errorf("localsigner: get AgentIdentity %s/%s: %w", namespace, name, err)
	}
	if !agentIdentity.DeletionTimestamp.IsZero() {
		return fmt.Errorf("localsigner: AgentIdentity %s/%s is being deleted", namespace, name)
	}

	ready := apiMeta.FindStatusCondition(agentIdentity.Status.Conditions, securityv1alpha1.ConditionReady)
	if ready == nil {
		return fmt.Errorf("localsigner: AgentIdentity %s/%s has not reported readiness", namespace, name)
	}
	if ready.ObservedGeneration != agentIdentity.Generation || agentIdentity.Status.ObservedGeneration != agentIdentity.Generation {
		return fmt.Errorf("localsigner: AgentIdentity %s/%s readiness describes generation %d but the spec is at generation %d",
			namespace, name, ready.ObservedGeneration, agentIdentity.Generation)
	}
	if ready.Status != metav1.ConditionTrue {
		return fmt.Errorf("localsigner: AgentIdentity %s/%s is not Ready: %s", namespace, name, ready.Message)
	}
	return nil
}

func newTokenResponse(rawJWT string, expiry time.Time) *identity.TokenResponse {
	return &identity.TokenResponse{
		RequestID:             uuid.NewString(),
		AccessToken:           rawJWT,
		AccessTokenExpiration: expiry.UTC().Format(time.RFC3339),
	}
}

// PropagateSecurityToken is a no-op. The community build registers no
// propagators, so a token is delivered by whatever consumes the TokenResponse.
// Keeping this a no-op mirrors defaultTokenProvider, so replacing the provider
// does not silently introduce a runtime hop.
func (p *Provider) PropagateSecurityToken(_ context.Context, _ *agentsv1alpha1.Sandbox, _ *identity.TokenResponse,
	_ ...agentsruntime.Option) error {
	return nil
}
