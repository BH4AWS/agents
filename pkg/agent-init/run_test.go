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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
		argv []string
		bad  bool
	}{
		{"defaults", Config{Backend: []string{"/bin/sleep", "60"}}, []string{"/bin/true"}, false},
		{"empty argv", Config{Backend: []string{"/bin/true"}}, nil, true},
		{"empty executable", Config{Backend: []string{"/bin/true"}}, []string{""}, true},
		{"NUL argv", Config{Backend: []string{"/bin/true"}}, []string{"/bin/true", "a\x00b"}, true},
		{"empty backend", Config{}, []string{"/bin/true"}, true},
		{"negative limit", Config{Backend: []string{"/bin/true"}, MaxRestarts: -1}, []string{"/bin/true"}, true},
		{"negative grace", Config{Backend: []string{"/bin/true"}, StopTimeout: -1}, []string{"/bin/true"}, true},
		{"negative backoff", Config{Backend: []string{"/bin/true"}, RestartBackoff: -1}, []string{"/bin/true"}, true},
		{"relative socket", Config{Backend: []string{"/bin/true"}, SocketPath: "socket"}, []string{"/bin/true"}, true},
		{"URL credentials", Config{Backend: []string{"/bin/true"}, BackendURL: "http://secret@localhost"}, []string{"/bin/true"}, true},
		{"URL query", Config{Backend: []string{"/bin/true"}, BackendURL: "http://localhost/?token=secret"}, []string{"/bin/true"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := normalizeConfig(tc.cfg, tc.argv)
			if (err != nil) != tc.bad {
				t.Fatalf("validation error = %v", err)
			}
			if !tc.bad && (cfg.SocketPath != DefaultSocket || cfg.StopTimeout <= 0) {
				t.Fatal("missing defaults")
			}
		})
	}
	for _, tc := range []struct {
		name string
		req  InitRequest
		bad  bool
	}{
		{"valid", InitRequest{LifecycleID: "life", Revision: 0}, false},
		{"missing lifecycle", InitRequest{}, true},
		{"negative revision", InitRequest{LifecycleID: "life", Revision: -1}, true},
		{"bad key", InitRequest{LifecycleID: "life", EnvVars: map[string]string{"A=B": "x"}}, true},
		{"empty key", InitRequest{LifecycleID: "life", EnvVars: map[string]string{"": "x"}}, true},
		{"NUL value", InitRequest{LifecycleID: "life", EnvVars: map[string]string{"A": "\x00"}}, true},
		{"reserved payload", InitRequest{LifecycleID: "life", EnvVars: map[string]string{PayloadEnv: "secret"}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := copyInit(tc.req)
			if (err != nil) != tc.bad {
				t.Fatalf("validation error = %v", err)
			}
		})
	}
}

func TestEnvironmentSnapshot(t *testing.T) {
	base := []string{"A=old", "PATH=/bin", "A=last", PayloadEnv + "=secret", "UNCHANGED=yes"}
	init := &InitRequest{EnvVars: map[string]string{"A": "new", "B": "x=y"}}
	got := environment(base, init)
	want := []string{"A=new", "B=x=y", "PATH=/bin", "UNCHANGED=yes"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected environment keys")
	}
	if base[0] != "A=old" {
		t.Fatal("base mutated")
	}
	copy, err := copyInit(InitRequest{LifecycleID: "l", EnvVars: init.EnvVars})
	if err != nil {
		t.Fatal(err)
	}
	init.EnvVars["A"] = "changed"
	if copy.EnvVars["A"] != "new" {
		t.Fatal("init not copied")
	}
}

type childRecord struct {
	PID     int
	Args    []string
	CWD     string
	Value   string
	Payload string
}

// The business child is a real independently grouped process. No process,
// waiter, clock or signal implementation is mocked.
func TestChildProcess(t *testing.T) {
	if os.Getenv("AGENT_INIT_TEST_CHILD") != "1" {
		return
	}
	var args []string
	for i, arg := range os.Args {
		if arg == "--" {
			args = os.Args[i+1:]
			break
		}
	}
	if len(args) < 2 {
		os.Exit(2)
	}
	mode, dir := args[0], args[1]
	if mode == "ignore" {
		signal.Ignore(syscall.SIGTERM, syscall.SIGINT)
	}
	cwd, err := os.Getwd()
	if err != nil {
		os.Exit(3)
	}
	record := childRecord{os.Getpid(), args[2:], cwd, os.Getenv("INIT_TEST_VALUE"), os.Getenv(PayloadEnv)}
	data, err := json.Marshal(record)
	if err != nil {
		os.Exit(4)
	}
	if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("child-%d.json", os.Getpid())), data, 0600); err != nil {
		os.Exit(5)
	}
	if mode == "exit" {
		os.Exit(0)
	}
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	<-timer.C
	os.Exit(0)
}

type fixture struct {
	socket, dir string
	cfg         Config
	cancel      context.CancelFunc
	result      chan error
}

func newFixture(t *testing.T, cfg Config, mode string) *fixture {
	t.Helper()
	t.Setenv("AGENT_INIT_TEST_CHILD", "1")
	t.Setenv("INIT_TEST_VALUE", "original")
	t.Setenv(PayloadEnv, "must-not-reach-child")
	dir, err := os.MkdirTemp("/tmp", "agent-init-test-")
	if err != nil {
		t.Fatal(err)
	}
	cfg.SocketPath = filepath.Join(dir, "control.sock")
	if len(cfg.Backend) == 0 {
		cfg.Backend = []string{"/bin/sleep", "60"}
	}
	if cfg.StopTimeout == 0 {
		cfg.StopTimeout = 60 * time.Millisecond
	}
	if cfg.RestartBackoff == 0 {
		cfg.RestartBackoff = 10 * time.Millisecond
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	argv := []string{exe, "-test.run=^TestChildProcess$", "--", mode, dir, "space argument", "", "$NOT_EXPANDED", "semi;colon"}
	if mode == "missing" {
		argv = []string{filepath.Join(dir, "business")}
	}
	ctx, cancel := context.WithCancel(context.Background())
	f := &fixture{cfg.SocketPath, dir, cfg, cancel, make(chan error, 1)}
	go func() { f.result <- Run(ctx, cfg, argv) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-f.result:
			if err != nil {
				t.Errorf("Run: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("Run did not shut down")
		}
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
	})
	f.wait(t, func(st Status) bool { return st.BackendPID != 0 })
	return f
}

func (f *fixture) control(t *testing.T, action string, init *InitRequest) Status {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	st, err := Control(ctx, f.socket, action, init)
	if err != nil {
		t.Fatalf("%s failed: %v", action, err)
	}
	return st
}

func (f *fixture) wait(t *testing.T, predicate func(Status) bool) Status {
	t.Helper()
	// Race-instrumented children sleep ~1s at exit, so multi-generation waits
	// need more than a few seconds under -race.
	deadline := time.Now().Add(10 * time.Second)
	var last Status
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		st, err := Control(ctx, f.socket, "status", nil)
		cancel()
		if err == nil && predicate(st) {
			return st
		}
		if err == nil {
			last = st
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("status condition timed out, last state=%s generation=%d pid=%d", last.State, last.Generation, last.PID)
	return Status{}
}

func (f *fixture) record(t *testing.T, pid int) childRecord {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(filepath.Join(f.dir, fmt.Sprintf("child-%d.json", pid)))
		if err == nil {
			var record childRecord
			if json.Unmarshal(data, &record) == nil {
				return record
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("child record timed out")
	return childRecord{}
}

// httptest is used only for the backend HTTP wire/confirmation contract. The
// foreground backend process is real /bin/sleep (not a simulated supervisor).
// Actual envd/helper integration and Linux PID1 are verified by the container
// caller; linking those Linux runtime packages into Darwin unit tests would
// turn this focused protocol test into an unrelated runtime deployment.
func TestLifecycle(t *testing.T) {
	for _, name := range []string{"gate", "stop-before-init", "manual-exit", "duplicate-start", "restart", "init-order", "environment", "backend-recovery", "auto-restart-limit", "shutdown", "immediate", "ungated-backend-recovery", "ungated-environment", "business-exec-failure"} {
		t.Run(name, func(t *testing.T) {
			var mu sync.Mutex
			var received []InitRequest
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/init" {
					http.Error(w, "bad route", 404)
					return
				}
				var req InitRequest
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					http.Error(w, "bad body", 400)
					return
				}
				mu.Lock()
				received = append(received, req)
				mu.Unlock()
				w.WriteHeader(http.StatusNoContent)
			}))
			defer server.Close()
			cfg := Config{BackendURL: server.URL, WaitForInit: true, MaxRestarts: 3}
			mode := "hold"
			if name == "manual-exit" || name == "auto-restart-limit" {
				mode = "exit"
			}
			if name == "shutdown" {
				mode = "ignore"
				cfg.WaitForInit = false
			}
			if name == "auto-restart-limit" {
				cfg.AutoRestart = true
				cfg.MaxRestarts = 2
			}
			if name == "immediate" || name == "ungated-backend-recovery" || name == "ungated-environment" || name == "business-exec-failure" {
				cfg.WaitForInit = false
			}
			if name == "business-exec-failure" {
				mode = "missing"
			}
			f := newFixture(t, cfg, mode)
			init := &InitRequest{LifecycleID: "life", Revision: 1, EnvVars: map[string]string{"INIT_TEST_VALUE": "initialized"}, AccessToken: "recovery-token"}
			if name == "stop-before-init" {
				f.control(t, "stop", nil)
				st := f.control(t, "init", init)
				if !st.Initialized || st.PID != 0 || st.State != "stopped" {
					t.Fatal("init revived stopped business")
				}
				f.control(t, "start", nil)
				f.wait(t, func(st Status) bool { return st.PID != 0 })
				return
			}
			if name == "shutdown" {
				st := f.wait(t, func(st Status) bool { return st.PID != 0 })
				f.record(t, st.PID)
				f.cancel()
				return
			}
			if name == "business-exec-failure" {
				st := f.wait(t, func(st Status) bool { return st.PID == 0 && st.Error != "" })
				if st.State != "stopped" || st.BackendPID == 0 {
					t.Fatal("business exec failure stopped the supervisor/backend")
				}
				if err := os.WriteFile(filepath.Join(f.dir, "business"), []byte("#!/bin/sh\nexec /bin/sleep 60\n"), 0700); err != nil {
					t.Fatal(err)
				}
				next := f.control(t, "start", nil)
				if next.PID == 0 || next.BackendPID != st.BackendPID || next.Error != "" {
					t.Fatal("manual start did not recover a failed exec")
				}
				return
			}
			if name == "immediate" || name == "ungated-backend-recovery" || name == "ungated-environment" {
				st := f.wait(t, func(st Status) bool { return st.PID != 0 })
				if st.Initialized || f.record(t, st.PID).Value != "original" {
					t.Fatal("immediate startup pretended to initialize or changed environment")
				}
				if name == "ungated-environment" {
					next := f.control(t, "init", init)
					if !next.Initialized || next.PID != st.PID || f.record(t, st.PID).Value != "original" {
						t.Fatal("init restarted or mutated the running business")
					}
					next = f.control(t, "restart", nil)
					if f.record(t, next.PID).Value != "initialized" {
						t.Fatal("restart did not use the initialized environment")
					}
				} else if name == "ungated-backend-recovery" {
					for i := 0; i < cfg.MaxRestarts+2; i++ {
						backend := st.BackendPID
						if err := syscall.Kill(backend, syscall.SIGKILL); err != nil {
							t.Fatal(err)
						}
						st = f.wait(t, func(next Status) bool { return next.BackendPID != 0 && next.BackendPID != backend })
					}
					f.control(t, "stop", nil)
					next := f.control(t, "start", nil)
					if next.PID == 0 || next.Initialized {
						t.Fatal("backend recovery closed the ungated business startup")
					}
				}
				return
			}
			if name == "gate" {
				st := f.control(t, "start", nil)
				if st.PID != 0 || st.Initialized {
					t.Fatal("business escaped gate")
				}
			}
			st := f.control(t, "init", init)
			if !st.Initialized {
				t.Fatal("2xx did not open gate")
			}
			switch name {
			case "manual-exit":
				end := f.wait(t, func(st Status) bool { return st.PID == 0 && st.State == "stopped" })
				if end.BackendPID == 0 || end.Generation != 1 {
					t.Fatal("business exit stopped parent/backend or restarted")
				}
				f.control(t, "init", init)
				if f.control(t, "status", nil).Generation != 1 {
					t.Fatal("duplicate init revived business")
				}
			case "auto-restart-limit":
				end := f.wait(t, func(st Status) bool { return st.PID == 0 && st.State == "stopped" && st.Generation == 3 })
				if end.BackendPID == 0 {
					t.Fatal("backend stopped")
				}
			case "duplicate-start":
				for i := 0; i < 5; i++ {
					next := f.control(t, "start", nil)
					if next.PID != st.PID || next.Generation != st.Generation {
						t.Fatal("start was not idempotent")
					}
				}
			case "restart":
				f.record(t, st.PID)
				next := f.control(t, "restart", nil)
				if next.PID == 0 || next.PID == st.PID || next.Generation != st.Generation+1 {
					t.Fatal("restart was not stop-clean-start")
				}
				if err := syscall.Kill(st.PID, 0); !errors.Is(err, syscall.ESRCH) {
					t.Fatal("old business remains")
				}
			case "init-order":
				for _, req := range []InitRequest{
					{LifecycleID: "life", Revision: 0}, {LifecycleID: "other", Revision: 2},
					{LifecycleID: "life", Revision: 1, AccessToken: "different"},
				} {
					if _, err := Control(context.Background(), f.socket, "init", &req); err == nil {
						t.Fatal("conflicting init accepted")
					}
				}
				next := f.control(t, "init", init)
				if next.PID != st.PID || next.Generation != st.Generation {
					t.Fatal("duplicate init restarted business")
				}
				updated := *init
				updated.Revision++
				next = f.control(t, "init", &updated)
				if next.PID != st.PID || next.Revision != 2 {
					t.Fatal("new init did not retain running business")
				}
			case "environment":
				record := f.record(t, st.PID)
				cwd, err := os.Getwd()
				if err != nil {
					t.Fatal(err)
				}
				if record.CWD != cwd || record.Value != "initialized" || record.Payload != "" || !reflect.DeepEqual(record.Args, []string{"space argument", "", "$NOT_EXPANDED", "semi;colon"}) {
					t.Fatal("argv/cwd/env snapshot changed")
				}
				updated := *init
				updated.Revision++
				updated.EnvVars = map[string]string{}
				f.control(t, "init", &updated)
				next := f.control(t, "restart", nil)
				if f.record(t, next.PID).Value != "original" {
					t.Fatal("environment accumulated previous init")
				}
			case "backend-recovery":
				if err := syscall.Kill(st.BackendPID, syscall.SIGKILL); err != nil {
					t.Fatal(err)
				}
				next := f.wait(t, func(next Status) bool {
					return next.BackendPID != 0 && next.BackendPID != st.BackendPID && next.Initialized
				})
				if next.PID != st.PID || next.InstanceID == st.InstanceID {
					t.Fatal("recovery killed business or reused instance identity")
				}
				mu.Lock()
				restored := received[len(received)-1]
				count := len(received)
				mu.Unlock()
				if count < 2 || restored.AccessToken != init.AccessToken || restored.EnvVars["INIT_TEST_VALUE"] != "initialized" || restored.LifecycleID != "" {
					t.Fatal("backend init snapshot was not restored in backend wire format")
				}
			}
			encoded, err := json.Marshal(f.control(t, "status", nil))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(encoded), "recovery-token") || strings.Contains(string(encoded), "INIT_TEST_VALUE") {
				t.Fatal("status leaked credentials")
			}
		})
	}
}

func TestStopDuringBackendInit(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(entered) })
		select {
		case <-release:
			w.WriteHeader(204)
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	defer close(release)
	f := newFixture(t, Config{BackendURL: server.URL, WaitForInit: true}, "hold")
	initDone := make(chan error, 1)
	go func() {
		_, err := Control(context.Background(), f.socket, "init", &InitRequest{LifecycleID: "life", Revision: 1})
		initDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("backend request not started")
	}
	start := time.Now()
	st := f.control(t, "stop", nil)
	if time.Since(start) > 500*time.Millisecond || st.PID != 0 {
		t.Fatal("stop blocked on init HTTP")
	}
	f.cancel()
	select {
	case <-initDone:
	case <-time.After(time.Second):
		t.Fatal("init request not canceled on shutdown")
	}
}

func TestControlValidation(t *testing.T) {
	f := newFixture(t, Config{WaitForInit: true}, "hold")
	info, err := os.Stat(f.socket)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatal("control socket is not 0600")
	}
	transport := unixTransport(f.socket)
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: time.Second}
	for _, tc := range []struct {
		name, method, path, body string
		code                     int
	}{
		{"unknown", "POST", "/exec", "", 404},
		{"method", "GET", "/stop", "", 405},
		{"command body", "POST", "/start", `{"command":"/bin/true"}`, 400},
		{"unknown init field", "POST", "/init", `{"lifecycleID":"l","revision":1,"command":"x"}`, 400},
		{"trailing JSON", "POST", "/init", `{"lifecycleID":"l","revision":1} {}`, 400},
		{"oversized", "POST", "/init", strings.Repeat(" ", maxBody+1), 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(tc.method, "http://unix"+tc.path, strings.NewReader(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := io.Copy(io.Discard, resp.Body); err != nil {
				t.Error(err)
			}
			if err := resp.Body.Close(); err != nil {
				t.Error(err)
			}
			if resp.StatusCode != tc.code {
				t.Fatalf("status %d, want %d", resp.StatusCode, tc.code)
			}
		})
	}
	for _, tc := range []struct {
		action string
		req    *InitRequest
	}{{"exec", nil}, {"init", nil}, {"stop", &InitRequest{LifecycleID: "l"}}} {
		if _, err := Control(context.Background(), f.socket, tc.action, tc.req); err == nil {
			t.Fatal("invalid client action accepted")
		}
	}
}

func TestBackendConfirmation(t *testing.T) {
	for _, code := range []int{200, 204, 302, 409, 500} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", "/success")
				w.WriteHeader(code)
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			err := backendInit(ctx, Config{BackendURL: server.URL, RestartBackoff: 5 * time.Millisecond}, InitRequest{LifecycleID: "l"})
			if (err == nil) != (code >= 200 && code < 300) {
				t.Fatalf("confirmation error = %v", err)
			}
		})
	}
}
