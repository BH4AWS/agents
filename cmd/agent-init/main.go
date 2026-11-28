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
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"time"

	agentinit "github.com/openkruise/agents/pkg/agent-init"
)

type arguments []string

func (a *arguments) String() string         { return "" }
func (a *arguments) Set(value string) error { *a = append(*a, value); return nil }

func parseRun(args []string, getenv func(string) string) (agentinit.Config, []string, error) {
	cfg := agentinit.Config{BackendURL: "http://127.0.0.1:49983", StopTimeout: 5 * time.Second,
		MaxRestarts: 5, RestartBackoff: 100 * time.Millisecond}
	if getenv("AGENT_RUNTIME_MODE") == "helper" {
		cfg.BackendSocket = getenv("AGENT_HELPER_SOCKET_PATH")
		if cfg.BackendSocket == "" {
			cfg.BackendSocket = "/mnt/envd/helper/agent-helper.sock"
		}
	}
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.BoolVar(&cfg.WaitForInit, "wait-for-init", false, "wait for backend init confirmation")
	fs.StringVar(&cfg.SocketPath, "socket", agentinit.DefaultSocket, "control socket")
	backend := ""
	var backendArgs arguments
	fs.StringVar(&backend, "backend", "", "foreground backend executable")
	fs.Var(&backendArgs, "backend-arg", "backend argument (repeatable)")
	fs.StringVar(&cfg.BackendURL, "backend-url", cfg.BackendURL, "backend HTTP URL")
	fs.StringVar(&cfg.BackendSocket, "backend-socket", cfg.BackendSocket, "backend Unix socket")
	fs.DurationVar(&cfg.StopTimeout, "stop-timeout", cfg.StopTimeout, "TERM grace period")
	fs.BoolVar(&cfg.AutoRestart, "auto-restart", false, "opt into business restarts")
	fs.IntVar(&cfg.MaxRestarts, "restart-limit", cfg.MaxRestarts, "bounded business restart count")
	if err := fs.Parse(args); err != nil {
		return cfg, nil, errors.New("invalid run flags")
	}
	argv := fs.Args()
	consumed := len(args) - len(argv)
	if consumed == 0 || args[consumed-1] != "--" || len(argv) == 0 {
		return cfg, nil, errors.New("run requires -- ORIGINAL_ARGV")
	}
	if backend == "" {
		dir := getenv("ENVD_DIR")
		if dir == "" {
			return cfg, nil, errors.New("default backend requires ENVD_DIR")
		}
		cfg.Backend = []string{"/bin/bash", filepath.Join(dir, "envd-run.sh"), "--foreground"}
		cfg.Backend = append(cfg.Backend, backendArgs...)
	} else {
		cfg.Backend = append([]string{backend}, backendArgs...)
	}
	return cfg, argv, nil
}

func execute(ctx context.Context, args []string, out io.Writer, getenv func(string) string) error {
	if len(args) == 0 {
		return errors.New("action required")
	}
	action := args[0]
	if action == "run" {
		cfg, argv, err := parseRun(args[1:], getenv)
		if err != nil {
			return err
		}
		return agentinit.Run(ctx, cfg, argv)
	}
	switch action {
	case "status", "start", "stop", "restart", "init":
	default:
		return errors.New("unsupported action")
	}
	fs := flag.NewFlagSet(action, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	socket := fs.String("socket", agentinit.DefaultSocket, "control socket")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 {
		return errors.New("invalid control flags")
	}
	var init *agentinit.InitRequest
	if action == "init" {
		payload := getenv(agentinit.PayloadEnv)
		if len(payload) > 1<<20 {
			return errors.New("init payload too large")
		}
		init = &agentinit.InitRequest{}
		if err := json.Unmarshal([]byte(payload), init); err != nil {
			return errors.New("invalid init payload environment")
		}
	}
	status, err := agentinit.Control(ctx, *socket, action, init)
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(status)
}

func main() {
	if err := execute(context.Background(), os.Args[1:], os.Stdout, os.Getenv); err != nil {
		// Keep diagnostics generic: flag values and payloads may be secret.
		if _, writeErr := io.WriteString(os.Stderr, "agent-init: operation failed\n"); writeErr != nil {
			os.Exit(1)
		}
		os.Exit(1)
	}
}
