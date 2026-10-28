---
title: Local Token Signing Identity Provider
authors:
  - "@jicheng"
reviewers:
  - "@TBD"
creation-date: 2026-10-28
last-updated: 2026-10-28
status: provisional
---

# Local Token Signing Identity Provider

## Summary

The community `IdentityProvider` implementation generates random UUID tokens
that no component can verify. This proposal adds `pkg/identity/localsigner`, a
provider that mints the same JWTs `pkg/identity/jwtissuer` already issues for
traffic access tokens and agent tokens, signing them in-process with a key
supplied at startup.

Two control-plane processes sign: `sandbox-manager` (claim, traffic token) and
`agent-sandbox-controller` (resume). `agent-identity-provider` publishes the
matching OIDC discovery document and JWKS so the sandbox gateway can verify
those tokens with no shared secret beyond the signing key.

Agent (ID) tokens are minted only while the `AgentIdentity` the sandbox selects
through `security.agents.kruise.io/agent-name` reports `Ready=True` for its
current generation. Every other case fails closed.

## Motivation

The gateway verifier (`pkg/sandbox-gateway/jwtauth` -> `pkg/identity/oidc`)
already accepts JWTs discovered from an OIDC issuer, and `jwtissuer` already
signs them. What is missing is an issuance path that does not depend on an
external identity provider, so a community deployment cannot actually put a
usable credential behind `security.agents.kruise.io/enable-jwt-auth`.

Without this change, enabling JWT auth in a community cluster either fails
verification or requires running the enterprise identity-provider client,
which is not part of this repository.

## Goals

- Sign both token kinds locally, with no network hop and no external service.
- Make the advertised issuer, the signing key and the published JWKS a single
  operator-configured unit.
- Fail closed when an ID token is requested for a sandbox whose
  `AgentIdentity` is missing, deleting, stale, or not Ready.
- Keep the community default provider exactly as it is when the feature is not
  configured.
- Serve discovery and JWKS from every `agent-identity-provider` replica, not
  only the leader.

## Non-Goals

- Key generation, storage, or rotation automation. Operators supply PEM key
  material; rotation is restart-driven, as the gateway snapshots JWKS today.
- Serving the OIDC endpoints over TLS. TLS is terminated in front of the
  listener.
- Token templates, claims policy, or scopes beyond what `jwtissuer` emits.
- Replacing the enterprise identity-provider client.

## Design

### Provider

`pkg/identity/localsigner.Provider` implements `identity.IdentityProvider`:

- `TokenKindAccessToken` -> `Issuer.IssueTrafficAccessToken` with
  `sandboxid.Resolve(sandbox)` and subject `e2b:controlplane:<sandboxID>`, the
  same subject shape the gateway path already uses.
- `TokenKindIDToken` (or an empty kind, matching the existing default) ->
  `Issuer.IssueAgentToken` after the `AgentIdentity` readiness gate.
- `PropagateSecurityToken` is a no-op: the credential is not written into the
  runtime by the provider.

Readiness gate (fail closed, no fallback):

1. The sandbox must carry the `agent-name` annotation.
2. The `AgentIdentity` must exist in the sandbox namespace and not be deleting.
3. The `Ready` condition must exist, be `True`, and have
   `observedGeneration == metadata.generation`.

Absent, stale, or negative verdicts all return an error. Callers already
propagate issuance errors instead of degrading to a UUID token.

### Wiring

Both signing processes accept the same two flags and install the provider
during startup:

```
--identity-issuer-url=<public HTTPS issuer URL>
--identity-signing-key-file=<PEM private key path>
```

Empty keeps the community default provider. Setting only one is a startup
error. The controller installs against the manager client; `sandbox-manager`
builds a small dedicated client with the `security.agents.kruise.io` scheme,
because its shared cache intentionally covers only core and `agents.kruise.io`
types and must not gain a hard dependency on an optional CRD.

### Publishing keys

`agent-identity-provider` gains three flags:

```
--identity-issuer-url, --identity-signing-key-file
--identity-issuer-bind-address
```

With a key configured it serves `/.well-known/openid-configuration` and
`/jwks.json` from `jwtissuer.Handler()` on every replica. Operators point the
gateway at `<issuer-url>/.well-known/openid-configuration`.

### RBAC

- `controller-role`: `agentidentities` `get/list/watch`.
- `sandbox-manager` ClusterRole: the same rule.
- No RBAC change for `agent-identity-provider`; it already reads
  `agentidentities`.

## Risks

- Every signing process must load the same key material and the same issuer
  URL. A mismatch surfaces as verification failures, not issuance failures, so
  deployment must mount one Secret into all three processes.
- The gateway snapshots JWKS at startup; a key rotation requires publishing the
  new key before rolling the signers.
- The ID-token gate performs a live read per issuance. On the controller this
  creates an `AgentIdentity` informer on first use; a cluster that enables the
  flags without installing the CRD will block on that read rather than fall
  back.

## Alternatives

- **Keep issuing random tokens**: unusable with JWT auth and with a
  credential-aware gateway, which is the only reason to issue at all.
- **Add a signing HTTP service**: another hop and another failure domain for a
  key both callers can load directly.
- **Read readiness from the gateway side**: the gateway only sees a signed
  token; readiness must be decided where the token is minted.
