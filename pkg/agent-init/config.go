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

// Package agentinit implements an opt-in init for one saved business argv and
// one foreground backend. Its same-UID socket is not a hostile-user boundary.
// Without cgroups, process-group cleanup is best effort; setsid can escape it.
package agentinit

import (
	"errors"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	PayloadEnv     = "AGENT_INIT_PAYLOAD"
	DefaultSocket  = "/tmp/agent-init.sock"
	maxBody        = 1 << 20
	controlTimeout = 30 * time.Second
)

type InitRequest struct {
	LifecycleID string            `json:"lifecycleID"`
	Revision    int64             `json:"revision"`
	EnvVars     map[string]string `json:"envVars,omitempty"`
	AccessToken string            `json:"accessToken,omitempty"`
}

type Config struct {
	SocketPath    string
	Backend       []string
	BackendURL    string
	BackendSocket string
	WaitForInit   bool
	StopTimeout   time.Duration
	AutoRestart   bool
	// MaxRestarts bounds business retries; backend retries use capped backoff.
	MaxRestarts    int
	RestartBackoff time.Duration
}

// Status contains observations only, never saved argv, environment or tokens.
type Status struct {
	PID         int    `json:"pid"`
	BackendPID  int    `json:"backendPID"`
	Initialized bool   `json:"initialized"`
	State       string `json:"state"`
	Generation  uint64 `json:"generation"`
	InstanceID  string `json:"instanceID"`
	Revision    int64  `json:"revision"`
	Error       string `json:"error,omitempty"`
}

func normalizeConfig(cfg Config, argv []string) (Config, error) {
	if cfg.SocketPath == "" {
		cfg.SocketPath = DefaultSocket
	}
	if !filepath.IsAbs(cfg.SocketPath) {
		return cfg, errors.New("control socket must be absolute")
	}
	if cfg.StopTimeout < 0 || cfg.RestartBackoff < 0 || cfg.MaxRestarts < 0 {
		return cfg, errors.New("negative timeout or restart limit")
	}
	if cfg.StopTimeout == 0 {
		cfg.StopTimeout = 5 * time.Second
	}
	if cfg.RestartBackoff == 0 {
		cfg.RestartBackoff = 100 * time.Millisecond
	}
	if err := validateArgv(argv); err != nil {
		return cfg, err
	}
	if err := validateArgv(cfg.Backend); err != nil {
		return cfg, errors.New("invalid backend argv")
	}
	if cfg.BackendSocket != "" && !filepath.IsAbs(cfg.BackendSocket) {
		return cfg, errors.New("backend socket must be absolute")
	}
	if cfg.BackendURL == "" {
		cfg.BackendURL = "http://127.0.0.1:49983"
	}
	u, err := url.Parse(cfg.BackendURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return cfg, errors.New("invalid backend URL")
	}
	cfg.Backend = append([]string(nil), cfg.Backend...)
	return cfg, nil
}

func validateArgv(argv []string) error {
	if len(argv) == 0 || argv[0] == "" {
		return errors.New("explicit nonempty argv is required")
	}
	for _, arg := range argv {
		if strings.ContainsRune(arg, 0) {
			return errors.New("argv contains NUL")
		}
	}
	return nil
}

func copyInit(req InitRequest) (InitRequest, error) {
	if req.LifecycleID == "" || req.Revision < 0 {
		return req, errors.New("invalid init identity or revision")
	}
	vars := make(map[string]string, len(req.EnvVars))
	for k, v := range req.EnvVars {
		if k == "" || strings.ContainsAny(k, "=\x00") || strings.ContainsRune(v, 0) {
			return req, errors.New("invalid environment entry")
		}
		// PayloadEnv belongs to the control client, not child processes.
		if k == PayloadEnv {
			return req, errors.New("reserved environment entry")
		}
		vars[k] = v
	}
	req.EnvVars = vars
	return req, nil
}

func environment(base []string, req *InitRequest) []string {
	vars := make(map[string]string, len(base))
	for _, entry := range base {
		k, v, ok := strings.Cut(entry, "=")
		if ok && k != PayloadEnv {
			vars[k] = v
		}
	}
	if req != nil {
		for k, v := range req.EnvVars {
			vars[k] = v
		}
	}
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	for _, k := range keys {
		result = append(result, k+"="+vars[k])
	}
	return result
}

func backoff(base time.Duration, attempt int) time.Duration {
	const ceiling = 5 * time.Second
	if base >= ceiling {
		return ceiling
	}
	for i := 1; i < attempt && base < ceiling; i++ {
		if base > ceiling/2 {
			return ceiling
		}
		base *= 2
	}
	return base
}
