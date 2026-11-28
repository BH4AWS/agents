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
	"io"
	"reflect"
	"testing"

	agentinit "github.com/openkruise/agents/pkg/agent-init"
)

func TestParseRun(t *testing.T) {
	for _, tc := range []struct {
		name          string
		args          []string
		env           map[string]string
		backend, argv []string
		socket        string
		bad           bool
	}{
		{"default", []string{"--", "/bin/echo", "a b", "", "$X"}, map[string]string{"ENVD_DIR": "/mnt/envd"}, []string{"/bin/bash", "/mnt/envd/envd-run.sh", "--foreground"}, []string{"/bin/echo", "a b", "", "$X"}, "", false},
		{"explicit", []string{"--backend", "/bin/sleep", "--backend-arg", "60", "--wait-for-init", "--", "/bin/true"}, nil, []string{"/bin/sleep", "60"}, []string{"/bin/true"}, "", false},
		{"repeat", []string{"--backend", "/bin/echo", "--backend-arg", "a b", "--backend-arg", "--", "--", "/bin/true"}, nil, []string{"/bin/echo", "a b", "--"}, []string{"/bin/true"}, "", false},
		{"helper default", []string{"--backend", "/bin/sleep", "--", "/bin/true"}, map[string]string{"AGENT_RUNTIME_MODE": "helper"}, []string{"/bin/sleep"}, []string{"/bin/true"}, "/mnt/envd/helper/agent-helper.sock", false},
		{"helper socket", []string{"--backend", "/bin/sleep", "--", "/bin/true"}, map[string]string{"AGENT_RUNTIME_MODE": "helper", "AGENT_HELPER_SOCKET_PATH": "/tmp/helper.sock"}, []string{"/bin/sleep"}, []string{"/bin/true"}, "/tmp/helper.sock", false},
		{"missing envd", []string{"--", "/bin/true"}, nil, nil, nil, "", true},
		{"no separator", []string{"--backend", "/bin/sleep", "/bin/true"}, nil, nil, nil, "", true},
		{"no argv", []string{"--backend", "/bin/sleep", "--"}, nil, nil, nil, "", true},
		{"invalid flags", []string{"--bad=secret", "--", "/bin/true"}, nil, nil, nil, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, argv, err := parseRun(tc.args, func(k string) string { return tc.env[k] })
			if (err != nil) != tc.bad {
				t.Fatalf("parse error = %v", err)
			}
			if !tc.bad && (!reflect.DeepEqual(cfg.Backend, tc.backend) || !reflect.DeepEqual(argv, tc.argv) || cfg.BackendSocket != tc.socket) {
				t.Fatal("argv or backend defaults changed")
			}
		})
	}
}

func TestControlPayloadNotFromArgv(t *testing.T) {
	for _, args := range [][]string{{"exec"}, {"init", `{"accessToken":"secret"}`}, {"init"}, {"status", "extra"}} {
		err := execute(context.Background(), args, io.Discard, func(k string) string {
			if k == agentinit.PayloadEnv {
				return "invalid secret payload"
			}
			return ""
		})
		if err == nil {
			t.Fatal("invalid action or payload accepted")
		}
	}
}
