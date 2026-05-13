/*
Copyright 2025 The Kruise Authors.

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

package identity

import (
	"context"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

// MockIdentityProvider implements IdentityProvider for testing.
// It allows configuring individual method behaviors via function fields.
type MockIdentityProvider struct {
	IssueTokenFunc     func(ctx context.Context, req TokenRequest) (*TokenResponse, error)
	PropagateTokenFunc func(ctx context.Context, sbx *agentsv1alpha1.Sandbox, tokenResp *TokenResponse) error
	GetCABundleFunc    func(ctx context.Context, req GetProxyCABundleRequest) (*GetProxyCABundleResponse, error)
}

// IssueToken implements IdentityProvider.IssueToken.
func (m *MockIdentityProvider) IssueToken(ctx context.Context, req TokenRequest) (*TokenResponse, error) {
	if m.IssueTokenFunc != nil {
		return m.IssueTokenFunc(ctx, req)
	}
	return &TokenResponse{AccessToken: "mock-test-token"}, nil
}

// PropagateSecurityToken implements IdentityProvider.PropagateSecurityToken.
func (m *MockIdentityProvider) PropagateSecurityToken(ctx context.Context, sbx *agentsv1alpha1.Sandbox, tokenResp *TokenResponse) error {
	if m.PropagateTokenFunc != nil {
		return m.PropagateTokenFunc(ctx, sbx, tokenResp)
	}
	return nil
}

// GetProxyCABundle implements IdentityProvider.GetProxyCABundle.
func (m *MockIdentityProvider) GetProxyCABundle(ctx context.Context, req GetProxyCABundleRequest) (*GetProxyCABundleResponse, error) {
	if m.GetCABundleFunc != nil {
		return m.GetCABundleFunc(ctx, req)
	}
	return &GetProxyCABundleResponse{CABundle: "mock-test-ca-bundle"}, nil
}
