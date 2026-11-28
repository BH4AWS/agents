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

package runtime

// init.go groups the runtime initialization capability: resolving the init
// request from sandbox annotations and driving the /init handshake against the
// runtime sidecar. The InitAPI capability group rides on the shared call
// transport, so retry/refresh semantics and HTTPS with forced resolution
// (WithTLS) apply to init exactly as they do to the other capability groups.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils"
	"github.com/openkruise/agents/pkg/utils/logs"
	"github.com/openkruise/agents/pkg/utils/runtime/config"
	"github.com/openkruise/agents/proto/envd/process"
)

// initPath is the route of the agent-runtime init handshake endpoint. The
// handler authenticates via the accessToken carried in the JSON body (not the
// X-Access-Token header) and answers 204 No Content on success.
const initPath = "/init"

// initRequestTimeout bounds a single /init attempt, preserving the historical
// InitRuntime behavior (1s per attempt across the retry schedule).
const initRequestTimeout = time.Second

// InitAPI is the runtime initialization capability group. It maps to the
// /init handshake endpoint exposed by agent-runtime.
type InitAPI interface {
	// Init drives the POST /init handshake, delivering env vars and the access
	// token in the JSON body. A 204/2xx response means the runtime accepted the
	// init data. For legacy sandboxes, opts.ReInit treats 401 as success. Managed
	// sandboxes require a successful handshake followed by PID-1 acknowledgement.
	Init(ctx context.Context, opts config.InitRuntimeOptions) error
}

// initAPI is the default InitAPI implementation. It delegates transport to the
// owning runtimeClient and carries only the init-specific status mapping.
type initAPI struct {
	r *runtimeClient
}

// Init implements InitAPI by posting the init options to the runtime init
// endpoint through the shared call transport (retry, refresh and optional
// TLS/pinned addressing included).
//
// The handshake runs once per sandbox lifecycle, so the resolved transport is
// logged at the default verbosity: it is the cheapest way to tell whether init
// reached the runtime over HTTPS/mTLS or plaintext HTTP, without turning on the
// V(DebugLogLevel) per-attempt logs of the shared transport.
func (i *initAPI) Init(ctx context.Context, opts config.InitRuntimeOptions) error {
	log := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(i.r.sbx))
	transport := i.r.transportLogValues(i.r.sbx)
	log.Info("init runtime starting", append(transport, "reInit", opts.ReInit)...)

	// call refreshes a local sandbox variable, not r.sbx. Track that binding in
	// a per-invocation client copy; never mutate a shared Runtime or its options.
	r := *i.r
	bound := r.sbx
	refresh := r.refreshFn
	r.refreshFn = func(ctx context.Context) (*agentsv1alpha1.Sandbox, error) {
		if refresh != nil {
			updated, err := refresh(ctx)
			if err != nil {
				return nil, err
			}
			if updated != nil {
				bound = updated
			}
		}
		if bound == nil || bound.Annotations[utils.AnnotationManagedProcess] != "true" {
			// Keep legacy transport, request and response semantics untouched.
			r.client, r.tlsEnabled = i.r.client, i.r.tlsEnabled
			return bound, nil
		}
		// call historically accepts non-error 3xx responses. Managed init must
		// observe a real 2xx, not silently release the workload after a redirect.
		// Resolve the existing transport first (including pinned TLS), then bind
		// it to this invocation only so both HTTP and HTTPS enforce that rule.
		base, httpClient, err := i.r.resolveTransport(bound, i.r.client)
		if err != nil {
			return nil, err
		}
		initClient := *httpClient
		transport := initClient.Transport
		if transport == nil {
			transport = http.DefaultTransport
		}
		initClient.Transport = initStatusTransport{transport}
		r.client, r.tlsEnabled = &initClient, false
		addressed := bound.DeepCopy()
		addressed.Annotations[agentsv1alpha1.AnnotationRuntimeURL] = base
		return addressed, nil
	}
	start := time.Now()
	err := r.call(ctx, http.MethodPost, initPath, opts, nil)
	managed := bound != nil && bound.Annotations[utils.AnnotationManagedProcess] == "true"
	// The runtime rejects an init whose body token is absent or no longer
	// matches its stored token with 401. On a re-init (resume/recreate) that
	// means the runtime is already initialized, which is the desired end state.
	// call never retries a 4xx, so this classification happens exactly once.
	var apiErr *APIError
	if !managed && opts.ReInit && errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusUnauthorized {
		log.Info("init runtime returned 401, treated as success because ReInit is true",
			append(transport, "cost", time.Since(start))...)
		return nil
	}
	if err != nil {
		// Logged here (in addition to the caller's own error handling) because
		// this is the only layer that knows which transport was actually used,
		// which is the first thing to check on a runtime connectivity failure.
		log.Error(err, "init runtime failed", append(transport, "cost", time.Since(start))...)
		return err
	}
	if managed {
		r = *i.r
		// /init has just installed this token. Do not authenticate the process
		// RPC using a stale annotation, and do not mutate a cached Sandbox.
		r.sbx = bound.DeepCopy()
		if r.sbx.Annotations == nil {
			r.sbx.Annotations = map[string]string{}
		}
		r.sbx.Annotations[agentsv1alpha1.AnnotationRuntimeAccessToken] = opts.AccessToken
		delete(r.sbx.Annotations, agentsv1alpha1.AnnotationEnvdAccessToken)
		if err := notifyManagedProcess(ctx, &r, opts); err != nil {
			return err
		}
	}
	log.Info("init runtime completed", append(transport, "cost", time.Since(start))...)
	return nil
}

// initStatusTransport rejects non-2xx, non-error responses without changing the
// shared runtime transport or following redirects for a managed handshake.
// HTTP error responses still flow through call's existing APIError mapping.
type initStatusTransport struct {
	http.RoundTripper
}

func (t initStatusTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.RoundTripper.RoundTrip(req)
	if err != nil {
		return resp, err
	}
	if resp.StatusCode < http.StatusOK || (resp.StatusCode >= http.StatusMultipleChoices && resp.StatusCode < http.StatusBadRequest) {
		if err := resp.Body.Close(); err != nil {
			return nil, fmt.Errorf("close non-successful init response: %w", err)
		}
		return nil, &APIError{Path: initPath, StatusCode: resp.StatusCode}
	}
	return resp, nil
}

// notifyManagedProcess makes one bounded RPC after /init. It does not retry:
// outer init retries drive recovery, so a payload's revision never changes
// between attempts to deliver that payload. No environment or output is logged.
func notifyManagedProcess(ctx context.Context, r *runtimeClient, opts config.InitRuntimeOptions) error {
	if r.sbx.UID == "" {
		return fmt.Errorf("managed process initialization requires a Sandbox UID")
	}
	payload, err := json.Marshal(struct {
		LifecycleID string            `json:"lifecycleID"`
		Revision    int64             `json:"revision"`
		EnvVars     map[string]string `json:"envVars"`
		AccessToken string            `json:"accessToken"`
	}{string(r.sbx.UID), time.Now().UnixNano(), opts.EnvVars, opts.AccessToken})
	if err != nil {
		return fmt.Errorf("encode managed process initialization: %w", err)
	}
	result, err := r.Process().Run(ctx, RunCommandRequest{
		ProcessConfig: &process.ProcessConfig{
			Cmd:  utils.ManagedProcessBinary,
			Args: []string{"init"},
			Envs: map[string]string{"AGENT_INIT_PAYLOAD": string(payload)},
		},
		Timeout:        10 * time.Second,
		ProcessTimeout: 10 * time.Second,
		OutputOptions:  ProcessOutputOptions{MaxBytes: 4 << 10, SuppressResultLog: true},
		// The demo runtime is root. Non-root runtimes must be configured to
		// permit resolving root and accessing the supervisor's control socket.
		AuthUser: "root",
	})
	if err != nil {
		return fmt.Errorf("notify managed process initialization: %w", err)
	}
	if !result.Exited || result.ExitCode != 0 {
		return fmt.Errorf("managed process initialization did not exit successfully (exited=%t, exitCode=%d)", result.Exited, result.ExitCode)
	}
	var response struct {
		Initialized bool `json:"initialized"`
	}
	if err := json.Unmarshal([]byte(strings.Join(result.Stdout, "")), &response); err != nil {
		return fmt.Errorf("invalid managed process initialization acknowledgement: %w", err)
	}
	if !response.Initialized {
		return fmt.Errorf("managed process initialization was not acknowledged")
	}
	return nil
}

// GetInitRuntimeRequest parses init runtime configuration from object annotations.
func GetInitRuntimeRequest(s metav1.Object) (*config.InitRuntimeOptions, error) {
	// Build initRuntimeOpts from annotation at the beginning
	var initRuntimeOpts *config.InitRuntimeOptions
	if initRuntimeRequest := s.GetAnnotations()[agentsv1alpha1.AnnotationInitRuntimeRequest]; initRuntimeRequest != "" {
		var opts config.InitRuntimeOptions
		if err := json.Unmarshal([]byte(initRuntimeRequest), &opts); err != nil {
			return nil, fmt.Errorf("failed to unmarshal init runtime request: %w", err)
		}
		initRuntimeOpts = &opts
	}
	return initRuntimeOpts, nil
}

// InitRuntime sends an init request to the sandbox runtime sidecar. It is a
// thin adapter over the Runtime Init capability kept for signature
// compatibility with existing callers.
//
// The sbx parameter is the raw Sandbox API object. When opts.SkipRefresh is
// false and refreshFn is provided, the sandbox is re-resolved before each retry
// attempt so a freshly stamped runtime URL (or Pod IP in TLS mode) is picked
// up. Additional Options (e.g. WithTLS to reach the runtime over HTTPS with
// forced resolution) are appended after the init defaults and may override
// them.
func InitRuntime(ctx context.Context, sbx *agentsv1alpha1.Sandbox, opts config.InitRuntimeOptions, refreshFn RefreshFunc, rtOpts ...Option) (time.Duration, error) {
	ctx = logs.Extend(ctx, "action", "initRuntime")
	start := time.Now()
	options := []Option{WithRequestTimeout(initRequestTimeout)}
	if !opts.SkipRefresh && refreshFn != nil {
		options = append(options, WithRefresh(refreshFn))
	}
	options = append(options, rtOpts...)
	err := NewRuntime(sbx, options...).Init().Init(ctx, opts)
	return time.Since(start), err
}
