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

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils"
	"github.com/openkruise/agents/pkg/utils/runtime/config"
	"github.com/openkruise/agents/proto/envd/process"
	"github.com/openkruise/agents/proto/envd/process/processconnect"
)

func TestInitManagedProcess(t *testing.T) {
	for _, tt := range []struct {
		name       string
		marker     string
		status     int
		reInit     bool
		stdout     string
		exitCode   int32
		noEnd      bool
		notExited  bool
		rpcError   bool
		endError   bool
		refresh    bool
		refresh401 bool
		wantRPC    bool
		wantErr    string
	}{
		{name: "legacy init unchanged", status: 204},
		{name: "literal true only", marker: "True", status: 204},
		{name: "legacy ReInit 401 remains successful", status: 401, reInit: true},
		{name: "managed ReInit 401 rejected", marker: "true", status: 401, reInit: true, wantErr: "status 401"},
		{name: "managed init 403 rejected", marker: "true", status: 403, wantErr: "status 403"},
		{name: "managed init 500 rejected", marker: "true", status: 500, wantErr: "status 500"},
		{name: "managed init 304 is not success", marker: "true", status: 304, wantErr: "status 304"},
		{name: "managed redirect is not success", marker: "true", status: 302, wantErr: "status 302"},
		{name: "managed success", marker: "true", status: 204, stdout: `{"initialized":true}`, wantRPC: true},
		{name: "managed ReInit true 2xx notifies", marker: "true", status: 200, reInit: true, stdout: `{"initialized":true}`, wantRPC: true},
		{name: "refreshed marker identity and address", status: 204, stdout: `{"initialized":true}`, refresh: true, wantRPC: true},
		{name: "refreshed marker rejects 401", status: 401, reInit: true, refresh401: true, wantErr: "status 401"},
		{name: "notification RPC error", marker: "true", status: 204, rpcError: true, wantRPC: true, wantErr: "notify managed process"},
		{name: "nonzero exit", marker: "true", status: 204, stdout: `{"initialized":true}`, exitCode: 2, wantRPC: true, wantErr: "exitCode=2"},
		{name: "stdout without terminal event", marker: "true", status: 204, stdout: `{"initialized":true}`, noEnd: true, wantRPC: true, wantErr: "exited=false"},
		{name: "terminal event without exit", marker: "true", status: 204, stdout: `{"initialized":true}`, notExited: true, wantRPC: true, wantErr: "exited=false"},
		{name: "process end error", marker: "true", status: 204, stdout: `{"initialized":true}`, endError: true, wantRPC: true, wantErr: "notify managed process"},
		{name: "no acknowledgement", marker: "true", status: 204, wantRPC: true, wantErr: "invalid managed process"},
		{name: "false acknowledgement", marker: "true", status: 204, stdout: `{"initialized":false}`, wantRPC: true, wantErr: "not acknowledged"},
		{name: "missing initialized field", marker: "true", status: 204, stdout: `{}`, wantRPC: true, wantErr: "not acknowledged"},
		{name: "incomplete acknowledgement", marker: "true", status: 204, stdout: `{"initialized":tru`, wantRPC: true, wantErr: "invalid managed process"},
		{name: "capture bounded at 4KB", marker: "true", status: 204, stdout: strings.Repeat(" ", 4096) + `{"initialized":true}`, wantRPC: true, wantErr: "invalid managed process"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			opts := config.InitRuntimeOptions{
				EnvVars:     map[string]string{"SPACE": "a b", "QUOTE": `"secret"`},
				AccessToken: "new-runtime-token", ReInit: tt.reInit,
			}
			var initCalls, rpcCalls atomic.Int32
			payloads := make(chan string, 1)
			handler := &mockProcessHandler{startFn: func(_ context.Context, req *connect.Request[process.StartRequest], stream *connect.ServerStream[process.StartResponse]) error {
				rpcCalls.Add(1)
				assert.Equal(t, int32(1), initCalls.Load(), "the real /init must precede notification")
				assert.Equal(t, utils.ManagedProcessBinary, req.Msg.Process.Cmd)
				assert.Equal(t, []string{"init"}, req.Msg.Process.Args)
				assert.Len(t, req.Msg.Process.Envs, 1, "the payload must be environment-only")
				assert.Equal(t, opts.AccessToken, req.Header().Get(accessTokenHeader), "RPC must use the token accepted by /init")
				assert.Equal(t, basicAuthHeader("root"), req.Header().Get("Authorization"))
				assert.Equal(t, "10000", req.Header().Get("Connect-Timeout-Ms"))
				payloadJSON := req.Msg.Process.Envs["AGENT_INIT_PAYLOAD"]
				payloads <- payloadJSON
				var payload struct {
					LifecycleID string            `json:"lifecycleID"`
					Revision    int64             `json:"revision"`
					EnvVars     map[string]string `json:"envVars"`
					AccessToken string            `json:"accessToken"`
				}
				assert.NoError(t, json.Unmarshal([]byte(payloadJSON), &payload))
				assert.Equal(t, "bound-sandbox-uid", payload.LifecycleID)
				assert.Positive(t, payload.Revision)
				assert.Equal(t, opts.EnvVars, payload.EnvVars)
				assert.Equal(t, opts.AccessToken, payload.AccessToken)
				if tt.rpcError {
					return connect.NewError(connect.CodeInternal, fmt.Errorf("notification unavailable"))
				}
				// Split stdout across stream frames to exercise real accumulation.
				for _, chunk := range []string{tt.stdout[:len(tt.stdout)/2], tt.stdout[len(tt.stdout)/2:]} {
					if err := stream.Send(&process.StartResponse{Event: &process.ProcessEvent{Event: &process.ProcessEvent_Data{
						Data: &process.ProcessEvent_DataEvent{Output: &process.ProcessEvent_DataEvent_Stdout{Stdout: []byte(chunk)}},
					}}}); err != nil {
						return err
					}
				}
				if tt.noEnd {
					return nil
				}
				var endError *string
				if tt.endError {
					message := "init process failed"
					endError = &message
				}
				return stream.Send(&process.StartResponse{Event: &process.ProcessEvent{Event: &process.ProcessEvent_End{
					End: &process.ProcessEvent_EndEvent{ExitCode: tt.exitCode, Exited: !tt.notExited, Error: endError},
				}}})
			}}
			_, processHandler := processconnect.NewProcessHandler(handler)
			mux := http.NewServeMux()
			mux.Handle("/", processHandler)
			mux.HandleFunc(initPath, func(w http.ResponseWriter, req *http.Request) {
				initCalls.Add(1)
				assert.Equal(t, http.MethodPost, req.Method)
				assert.Empty(t, req.Header.Get("Authorization"), "/init must not gain the RPC root header")
				var received config.InitRuntimeOptions
				assert.NoError(t, json.NewDecoder(req.Body).Decode(&received))
				assert.Equal(t, opts.EnvVars, received.EnvVars)
				assert.Equal(t, opts.AccessToken, received.AccessToken)
				if tt.status == 302 {
					w.Header().Set("Location", "/not-init")
				}
				w.WriteHeader(tt.status)
			})
			server := httptest.NewServer(mux)
			t.Cleanup(server.Close)
			bound := &agentsv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{
				Name: "test", Namespace: "default", UID: types.UID("bound-sandbox-uid"),
				Annotations: map[string]string{
					agentsv1alpha1.AnnotationRuntimeURL:         server.URL,
					agentsv1alpha1.AnnotationRuntimeAccessToken: "stale-runtime-token",
					utils.AnnotationManagedProcess:              tt.marker,
				},
			}}
			initial := bound
			runtimeOpts := []Option{WithRetry(wait.Backoff{Steps: 1})}
			if tt.refresh || tt.refresh401 {
				bound.Annotations[utils.AnnotationManagedProcess] = "true"
				initial = &agentsv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: bound.Name, Namespace: bound.Namespace}}
				runtimeOpts = append(runtimeOpts, WithRefresh(func(context.Context) (*agentsv1alpha1.Sandbox, error) { return bound, nil }))
			}
			before := bound.DeepCopy()
			r := NewRuntime(initial, runtimeOpts...).(*runtimeClient)
			err := r.Init().Init(t.Context(), opts)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, int32(1), initCalls.Load())
			wantRPC := int32(0)
			if tt.wantRPC {
				wantRPC = 1
			}
			assert.Equal(t, wantRPC, rpcCalls.Load(), "notification must run once and only after actual init success")
			assert.Equal(t, before, bound, "cached Sandbox must not be mutated")
			assert.Same(t, initial, r.sbx, "Runtime binding must not be mutated")
			if tt.wantRPC {
				select {
				case payload := <-payloads:
					assert.NotEmpty(t, payload)
				default:
					t.Error("notification payload was not received")
				}
			}
		})
	}
}

func TestNotifyManagedProcessWaitsForStreamCompletion(t *testing.T) {
	// A successful-looking End event is insufficient until the stream itself
	// finishes without an RPC error; Run consumes the complete real stream.
	handler := &mockProcessHandler{startFn: func(_ context.Context, _ *connect.Request[process.StartRequest], stream *connect.ServerStream[process.StartResponse]) error {
		if err := stream.Send(&process.StartResponse{Event: &process.ProcessEvent{Event: &process.ProcessEvent_Data{
			Data: &process.ProcessEvent_DataEvent{Output: &process.ProcessEvent_DataEvent_Stdout{Stdout: []byte(`{"initialized":true}`)}},
		}}}); err != nil {
			return err
		}
		if err := stream.Send(&process.StartResponse{Event: &process.ProcessEvent{Event: &process.ProcessEvent_End{
			End: &process.ProcessEvent_EndEvent{Exited: true},
		}}}); err != nil {
			return err
		}
		return connect.NewError(connect.CodeInternal, fmt.Errorf("stream did not finish cleanly"))
	}}
	_, sbx := newMockRuntimeServer(t, handler)
	sbx.UID = types.UID("test-uid")
	r := NewRuntime(sbx).(*runtimeClient)
	err := notifyManagedProcess(t.Context(), r, config.InitRuntimeOptions{})
	require.ErrorContains(t, err, "stream did not finish cleanly")
}

func TestNotifyManagedProcessCancellation(t *testing.T) {
	handler := &mockProcessHandler{startFn: func(ctx context.Context, _ *connect.Request[process.StartRequest], _ *connect.ServerStream[process.StartResponse]) error {
		<-ctx.Done()
		return ctx.Err()
	}}
	_, sbx := newMockRuntimeServer(t, handler)
	sbx.UID = types.UID("test-uid")
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	err := notifyManagedProcess(ctx, NewRuntime(sbx).(*runtimeClient), config.InitRuntimeOptions{})
	require.Error(t, err)
}
