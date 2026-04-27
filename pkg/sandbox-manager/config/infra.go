package config

import (
	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"

	"github.com/openkruise/agents/pkg/identityprovider"
)

type InitRuntimeOptions struct {
	EnvVars     map[string]string   `json:"envVars,omitempty"`
	AccessToken *AccessTokenOptions `json:"accessToken,omitempty"`
	ReInit      bool                `json:"-"`
}

// AccessTokenType defines the generation method for sandbox access tokens.
type AccessTokenType string

const (
	// AccessTokenTypeUUID generates a random UUID-based token.
	// This is the community default, requiring no external service.
	AccessTokenTypeUUID AccessTokenType = "uuid"

	// AccessTokenTypeIdentityProvider generates a token via an external identity provider service.
	// This is used in internal deployments for identity-aware security tokens.
	AccessTokenTypeIdentityProvider AccessTokenType = "identity_provider"
)

// AccessTokenOptions holds the access token value and its generation method.
type AccessTokenOptions struct {
	// AccessToken is the issued token value.
	AccessToken string `json:"accessToken,omitempty"`
	// AccessTokenType indicates how the token was generated.
	AccessTokenType AccessTokenType `json:"accessTokenType,omitempty"`
}

// GetAccessToken returns the access token string, or empty string if the receiver is nil.
func (o *AccessTokenOptions) GetAccessToken() string {
	if o == nil {
		return ""
	}
	return o.AccessToken
}

// NewDefaultAccessToken generates a default access token using UUID.
// This is the community default; internal deployments can override this
// by issuing tokens via the identity provider (SecurityToken flow).
func NewDefaultAccessToken() string {
	return uuid.NewString()
}

const DefaultCSIMountConcurrency = 3

type CSIMountOptions struct {
	MountOptionList    []MountConfig `json:"mountOptionList"`
	MountOptionListRaw string        `json:"mountOptionListRaw"`    // the raw json string for mount options
	Concurrency        int           `json:"concurrency,omitempty"` // max concurrent CSI mount operations, 0 or negative means unlimited, default is DefaultCSIMountConcurrency
}

type SecurityTokenOptions struct {
	identityprovider.TokenResponse
}

type MountConfig struct {
	Driver     string `json:"driver"`
	RequestRaw string `json:"requestRaw"`
}

type InplaceUpdateOptions struct {
	Image string
	// Resources specifies in-place resource update options.
	// +optional
	Resources *InplaceUpdateResourcesOptions `json:"resources,omitempty"`
}

type InplaceUpdateResourcesOptions struct {
	// Requests specifies the target resource requests.
	Requests corev1.ResourceList
	// Limits specifies the target resource limits.
	Limits corev1.ResourceList
}
