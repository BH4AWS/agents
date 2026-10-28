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

package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/openkruise/agents/pkg/identity/jwtissuer"
)

// issuerShutdownTimeout bounds how long Stop waits for in-flight discovery
// requests before closing the listener anyway.
const issuerShutdownTimeout = 5 * time.Second

// issuerServer publishes the OIDC discovery document and the JWKS for the
// signing key the other control-plane processes sign with. It is a
// controller-runtime Runnable rather than a leader-elected component: every
// replica that signs must be able to point a gateway at a reachable JWKS, so
// serving on all replicas is the point.
//
// TLS is not terminated here. The advertised issuer URL is the public HTTPS
// URL, and the ingress or service in front of this listener is what makes it
// HTTPS; serving plaintext behind that is the usual in-cluster arrangement.
type issuerServer struct {
	server *http.Server
}

// newIssuerServer builds the handler once so a key that cannot be published
// fails startup instead of failing the first discovery request.
func newIssuerServer(bindAddress string, issuer *jwtissuer.Issuer) (*issuerServer, error) {
	if bindAddress == "" {
		return nil, errors.New("identity issuer bind address must not be empty")
	}
	if issuer == nil {
		return nil, errors.New("identity issuer is required")
	}
	handler, err := issuer.Handler()
	if err != nil {
		return nil, fmt.Errorf("build OIDC handler: %w", err)
	}
	return &issuerServer{
		server: &http.Server{
			Addr:              bindAddress,
			Handler:           handler,
			ReadHeaderTimeout: 10 * time.Second,
		},
	}, nil
}

// Start serves until the manager context is cancelled, then shuts down
// gracefully. A listener error is returned so the manager exits instead of
// running a control plane whose JWKS endpoint is dead.
func (s *issuerServer) Start(ctx context.Context) error {
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- s.server.ListenAndServe()
	}()

	select {
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), issuerShutdownTimeout)
		defer cancel()
		if err := s.server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutdown OIDC server: %w", err)
		}
		return nil
	}
}
