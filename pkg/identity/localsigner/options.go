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
	"errors"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openkruise/agents/pkg/identity"
	"github.com/openkruise/agents/pkg/identity/jwtissuer"
)

// Options are the operator-supplied startup settings for local signing.
//
// Leaving both fields empty keeps the community default provider in place;
// setting only one is a configuration error rather than a silent fallback.
type Options struct {
	// IssuerURL is the public HTTPS URL the sandbox gateway discovers. It
	// becomes the iss claim, so it must match the URL the gateway verifies.
	IssuerURL string
	// SigningKeyFile is the PEM private key mounted into the process.
	SigningKeyFile string
}

// Enabled reports whether local signing was configured at all.
func (o Options) Enabled() bool {
	return o.IssuerURL != "" || o.SigningKeyFile != ""
}

// NewIssuer builds the issuer from the configured key material.
func (o Options) NewIssuer() (*jwtissuer.Issuer, error) {
	if o.IssuerURL == "" || o.SigningKeyFile == "" {
		return nil, errors.New("localsigner: issuer URL and signing key file must be set together")
	}
	key, err := jwtissuer.LoadSigningKeyFromFile(o.SigningKeyFile)
	if err != nil {
		return nil, fmt.Errorf("localsigner: %w", err)
	}
	issuer, err := jwtissuer.New(o.IssuerURL, key)
	if err != nil {
		return nil, fmt.Errorf("localsigner: build issuer: %w", err)
	}
	return issuer, nil
}

// Install builds the issuer and registers the provider. Call it during startup,
// before any controller issues a token.
func (o Options) Install(reader client.Reader) error {
	issuer, err := o.NewIssuer()
	if err != nil {
		return err
	}
	provider, err := New(issuer, reader)
	if err != nil {
		return err
	}
	identity.RegisterProvider(provider)
	return nil
}
