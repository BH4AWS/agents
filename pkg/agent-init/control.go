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

package agentinit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

type reply struct {
	status Status
	err    error
}
type command struct {
	ctx    context.Context
	action string
	init   *InitRequest
	reply  chan reply
}

func allowedAction(action string) bool {
	switch action {
	case "status", "start", "stop", "restart", "init":
		return true
	}
	return false
}

// Control only controls the original argv saved by Run. Init payloads are sent
// in a bounded HTTP body; response bodies contain no credentials.
func Control(ctx context.Context, socket, action string, init *InitRequest) (Status, error) {
	if !allowedAction(action) {
		return Status{}, errors.New("unsupported control action")
	}
	if (action == "init") != (init != nil) {
		return Status{}, errors.New("init payload/action mismatch")
	}
	if socket == "" {
		socket = DefaultSocket
	}
	var body []byte
	if init != nil {
		validated, err := copyInit(*init)
		if err != nil {
			return Status{}, err
		}
		body, err = json.Marshal(validated)
		if err != nil {
			return Status{}, errors.New("encode init request")
		}
		if len(body) > maxBody {
			return Status{}, errors.New("init request too large")
		}
	}
	ctx, cancel := context.WithTimeout(ctx, controlTimeout)
	defer cancel()
	transport := unixTransport(socket)
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: controlTimeout, CheckRedirect: noRedirect}
	method := http.MethodPost
	if action == "status" {
		method = http.MethodGet
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://unix/"+action, bytes.NewReader(body))
	if err != nil {
		return Status{}, errors.New("create control request")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return Status{}, errors.New("control request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Status{}, errors.New("control action rejected")
	}
	var status Status
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&status); err != nil {
		return Status{}, errors.New("invalid control response")
	}
	return status, nil
}

func noRedirect(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }

func unixTransport(socket string) *http.Transport {
	return &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, "unix", socket)
	}}
}

// Accept verifies kernel-supplied credentials as well as the socket mode. A
// same-UID business process can control this socket by design.
type controlListener struct{ *net.UnixListener }

func (l controlListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.AcceptUnix()
		if err != nil {
			return nil, err
		}
		uid, err := peerUID(conn)
		if err == nil && uid == uint32(os.Geteuid()) {
			return conn, nil
		}
		if err := conn.Close(); err != nil {
			return nil, err
		}
	}
}

func listenControl(socket string) (controlListener, func() error, error) {
	// Never unlink an existing path: it may belong to another live instance.
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		return controlListener{}, nil, errors.New("listen on control socket")
	}
	listener.SetUnlinkOnClose(false)
	info, statErr := os.Lstat(socket)
	chmodErr := os.Chmod(socket, 0600)
	cleanup := func() error {
		closeErr := listener.Close()
		if errors.Is(closeErr, net.ErrClosed) {
			closeErr = nil
		}
		current, err := os.Lstat(socket)
		if err == nil && info != nil && os.SameFile(info, current) {
			err = os.Remove(socket)
		}
		if errors.Is(err, os.ErrNotExist) {
			err = nil
		}
		return errors.Join(closeErr, err)
	}
	if statErr != nil || chmodErr != nil {
		return controlListener{}, nil, errors.Join(errors.New("secure control socket"), cleanup())
	}
	return controlListener{listener}, cleanup, nil
}

func controlHandler(commands chan<- command, done <-chan struct{}) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		action := strings.TrimPrefix(r.URL.Path, "/")
		if !allowedAction(action) || r.URL.RawQuery != "" {
			http.Error(w, "unsupported action", 404)
			return
		}
		method := http.MethodPost
		if action == "status" {
			method = http.MethodGet
		}
		if r.Method != method {
			http.Error(w, "unsupported method", 405)
			return
		}
		cmd := command{ctx: r.Context(), action: action, reply: make(chan reply, 1)}
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		if action == "init" {
			var init InitRequest
			decoder := json.NewDecoder(r.Body)
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&init); err != nil {
				http.Error(w, "invalid payload", 400)
				return
			}
			var extra any
			if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
				http.Error(w, "invalid payload", 400)
				return
			}
			validated, err := copyInit(init)
			if err != nil {
				http.Error(w, "invalid init", 400)
				return
			}
			cmd.init = &validated
		} else {
			data, err := io.ReadAll(r.Body)
			if err != nil || len(data) != 0 {
				http.Error(w, "unexpected payload", 400)
				return
			}
		}
		select {
		case commands <- cmd:
		case <-done:
			http.Error(w, "shutting down", 503)
			return
		case <-r.Context().Done():
			return
		}
		select {
		case result := <-cmd.reply:
			if result.err != nil {
				http.Error(w, "action unavailable or conflicting", 409)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			// Connection failure is handled by net/http; no payload is logged.
			if err := json.NewEncoder(w).Encode(result.status); err != nil {
				return
			}
		case <-done:
			http.Error(w, "shutting down", 503)
		case <-r.Context().Done():
		}
	})
}

func backendInit(ctx context.Context, cfg Config, init InitRequest) error {
	// Lifecycle identity and revision are local control metadata, not part of
	// envd/helper's existing wire format.
	body, err := json.Marshal(struct {
		EnvVars     map[string]string `json:"envVars"`
		AccessToken string            `json:"accessToken,omitempty"`
	}{init.EnvVars, init.AccessToken})
	if err != nil {
		return errors.New("encode backend init")
	}
	transport := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 2 * time.Second}).DialContext}
	endpoint := strings.TrimRight(cfg.BackendURL, "/") + "/init"
	if cfg.BackendSocket != "" {
		transport = unixTransport(cfg.BackendSocket)
		endpoint = "http://unix/init"
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 2 * time.Second, CheckRedirect: noRedirect}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	for attempt := 1; ; attempt++ {
		if ctx.Err() != nil {
			return errors.New("backend init timed out or canceled")
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return errors.New("create backend init request")
		}
		req.Header.Set("Content-Type", "application/json")
		if init.AccessToken != "" {
			req.Header.Set("X-Access-Token", init.AccessToken)
		}
		resp, requestErr := client.Do(req)
		if requestErr == nil {
			ok := resp.StatusCode >= 200 && resp.StatusCode < 300
			closeErr := resp.Body.Close()
			if ok && closeErr == nil {
				return nil
			}
		}
		timer := time.NewTimer(backoff(cfg.RestartBackoff, attempt))
		select {
		case <-ctx.Done():
			timer.Stop()
			return errors.New("backend init timed out or canceled")
		case <-timer.C:
		}
	}
}
