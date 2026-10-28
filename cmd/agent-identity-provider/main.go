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

// Command agent-identity-provider runs the controllers and services that own
// agent identity: the AgentIdentity controller, and the OIDC discovery and
// JWKS endpoints publishing the verification keys for locally signed tokens.
//
// It is a separate process from agent-sandbox-controller on purpose: identity is
// a distinct failure domain with its own RBAC and its own release cadence, and a
// cluster that has not opted into the identity path should not be running this
// code at all. Keeping it out of the shared controller manager is what makes
// that possible.
package main

import (
	"flag"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	securityv1alpha1 "github.com/openkruise/agents/api/security/v1alpha1"
	"github.com/openkruise/agents/client"
	"github.com/openkruise/agents/pkg/agentidentity"
	"github.com/openkruise/agents/pkg/identity/localsigner"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(securityv1alpha1.AddToScheme(scheme))
}

func main() {
	var metricsAddr, probeAddr string
	var enableLeaderElection bool
	var identityIssuerURL, identitySigningKeyFile, identityIssuerBindAddress string
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election, ensuring only one active instance reconciles.")
	flag.StringVar(&identityIssuerURL, "identity-issuer-url", "",
		"Public HTTPS URL advertised as the OIDC issuer. Together with --identity-signing-key-file it enables "+
			"serving OIDC discovery and the JWKS; empty disables both.")
	flag.StringVar(&identitySigningKeyFile, "identity-signing-key-file", "",
		"PEM private key whose public half is published in the JWKS. Must be the key the control plane signs with, "+
			"and must be set together with --identity-issuer-url.")
	flag.StringVar(&identityIssuerBindAddress, "identity-issuer-bind-address", "",
		"Address the OIDC discovery and JWKS listener binds to. Required when OIDC endpoints are enabled; "+
			"TLS is expected to be terminated in front of this listener.")

	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	cfg := ctrl.GetConfigOrDie()

	// The generic client backs discovery, which the controller uses to decide
	// whether the AgentIdentity CRD is installed before it registers anything.
	if err := client.NewRegistry(cfg); err != nil {
		setupLog.Error(err, "unable to initialise the client registry")
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "agent-identity-provider.agents.kruise.io",
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err := agentidentity.Add(mgr); err != nil {
		setupLog.Error(err, "unable to add the AgentIdentity controller")
		os.Exit(1)
	}

	// Publish the verification keys exactly when a signing key is configured.
	// The endpoints are optional because a deployment that does not issue local
	// tokens has nothing to publish.
	identityOptions := localsigner.Options{IssuerURL: identityIssuerURL, SigningKeyFile: identitySigningKeyFile}
	if identityOptions.Enabled() {
		issuer, err := identityOptions.NewIssuer()
		if err != nil {
			setupLog.Error(err, "unable to load the identity signing key")
			os.Exit(1)
		}
		oidcServer, err := newIssuerServer(identityIssuerBindAddress, issuer)
		if err != nil {
			setupLog.Error(err, "unable to set up the OIDC discovery endpoints")
			os.Exit(1)
		}
		if err := mgr.Add(oidcServer); err != nil {
			setupLog.Error(err, "unable to add the OIDC discovery server")
			os.Exit(1)
		}
		setupLog.Info("OIDC discovery endpoints enabled", "issuerURL", identityIssuerURL, "bindAddress", identityIssuerBindAddress)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up the health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up the ready check")
		os.Exit(1)
	}

	setupLog.Info("starting agent-identity-provider")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
