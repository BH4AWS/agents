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

package core

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils"
	trafficproxy "github.com/openkruise/agents/pkg/utils/sidecarutils/traffic-proxy"
)

const managedProcessBootstrap = `until [ -x "$ENVD_DIR/agent-init" ]; do sleep 0.1; done; exec "$ENVD_DIR/agent-init" "$@"`

func managedProcessCommand() []string {
	return []string{"/bin/bash", "-c", managedProcessBootstrap, "agent-init"}
}

// wrapManagedProcess runs after runtime injection and checkpoint replay, for all
// pod generators. The pod template annotation, not a cluster default or a stale
// Sandbox capability stamp, is the opt-in source. No image ENTRYPOINT is used.
func wrapManagedProcess(pod *corev1.Pod) (bool, error) {
	if pod.Annotations[utils.AnnotationManagedProcess] != "true" {
		return false, nil
	}
	if pod.Spec.HostPID || (pod.Spec.ShareProcessNamespace != nil && *pod.Spec.ShareProcessNamespace) {
		return false, fmt.Errorf("managed process requires a private container PID namespace (no hostPID or shareProcessNamespace)")
	}
	if len(pod.Spec.Containers) == 0 {
		return false, fmt.Errorf("managed process requires a main container")
	}
	main := &pod.Spec.Containers[0]
	if len(main.Command) == 0 || strings.TrimSpace(main.Command[0]) == "" {
		return false, fmt.Errorf("managed process requires an explicit non-empty main container command")
	}
	envdDir := path.Dir(utils.ManagedProcessBinary)
	hasEnv := false
	for _, env := range main.Env {
		if env.Name == "ENVD_DIR" {
			if env.Value != envdDir || env.ValueFrom != nil {
				return false, fmt.Errorf("managed process requires ENVD_DIR=%s", envdDir)
			}
			hasEnv = true
		}
	}
	hasMount := false
	for _, mount := range main.VolumeMounts {
		if mount.MountPath == envdDir && mount.SubPath == "" && mount.SubPathExpr == "" {
			for _, volume := range pod.Spec.Volumes {
				if volume.Name == mount.Name {
					hasMount = true
				}
			}
		}
	}
	if !hasEnv || !hasMount {
		return false, fmt.Errorf("managed process requires injected ENVD_DIR and a volume mounted at %s", envdDir)
	}
	for _, named := range []struct {
		name  string
		probe *corev1.Probe
	}{
		{"readiness", main.ReadinessProbe},
		{"startup", main.StartupProbe},
		{"liveness", main.LivenessProbe},
	} {
		probe, err := managedProcessOriginalProbe(pod, main.Name, named.probe)
		if err != nil {
			return false, err
		}
		// Application liveness/startup probes would terminate PID 1 after a stop.
		if probe != nil && (probe.TCPSocket == nil || probe.TCPSocket.Port.Type != intstr.Int ||
			probe.TCPSocket.Port.IntVal != utils.RuntimePort || probe.TCPSocket.Host != "" ||
			probe.Exec != nil || probe.HTTPGet != nil || probe.GRPC != nil) {
			return false, fmt.Errorf("managed process conflicts with main container %s probe; only envd TCP %d is supported", named.name, utils.RuntimePort)
		}
	}
	if slices.Equal(main.Command, managedProcessCommand()) {
		// A checkpoint delta may already carry our bootstrap and restored hook.
		// Do not wrap it again or move the restored user hook into the backend.
		separator := slices.Index(main.Args, "--")
		if len(main.Args) < 2 || main.Args[0] != "run" ||
			separator < 2 || separator+1 >= len(main.Args) || main.Args[separator+1] == "" {
			return false, fmt.Errorf("invalid managed process bootstrap arguments")
		}
		return true, nil
	}

	backend := "/bin/bash"
	backendArgs := []string{envdDir + "/envd-run.sh"}
	var restoredHook *corev1.LifecycleHandler
	moveHook := false
	if main.Lifecycle != nil && main.Lifecycle.PostStart != nil && main.Lifecycle.PostStart.Exec != nil {
		hook := main.Lifecycle.PostStart
		argv := hook.Exec.Command
		// Match a direct script invocation only, never interpret user shell text.
		scriptIndex := slices.Index(argv, backendArgs[0])
		if scriptIndex >= 0 && (scriptIndex == 0 || argv[0] == "bash" || argv[0] == "/bin/bash") {
			if hook.HTTPGet != nil || hook.TCPSocket != nil || hook.Sleep != nil ||
				(scriptIndex != 0 && (scriptIndex != 1 || (argv[0] != "bash" && argv[0] != "/bin/bash"))) {
				return false, fmt.Errorf("managed process requires a direct bash envd-run.sh postStart argv")
			}
			if scriptIndex == 1 && argv[0] != "bash" {
				backend = argv[0]
			}
			rest := argv[scriptIndex+1:]
			if separator := slices.Index(rest, "--"); separator >= 0 {
				if separator+1 < len(rest) {
					restoredHook = &corev1.LifecycleHandler{Exec: &corev1.ExecAction{Command: slices.Clone(rest[separator+1:])}}
				}
				rest = rest[:separator]
			}
			backendArgs = append(backendArgs, rest...)
			moveHook = true
		}
	}
	if !slices.Contains(backendArgs[1:], "--foreground") {
		backendArgs = append(backendArgs, "--foreground")
	}
	args := []string{"run", "--backend=" + backend}
	for _, arg := range backendArgs {
		args = append(args, "--backend-arg="+arg)
	}
	if slices.Contains(backendArgs[1:], "--helper") {
		socket := "/var/run/agent-helper/helper.sock"
		for index, arg := range backendArgs[1:] {
			if strings.HasPrefix(arg, "--helper-socket=") {
				socket = strings.TrimPrefix(arg, "--helper-socket=")
			} else if arg == "--helper-socket" {
				if index+2 >= len(backendArgs) || strings.HasPrefix(backendArgs[index+2], "--") {
					return false, fmt.Errorf("managed process helper socket requires a value")
				}
				socket = backendArgs[index+2]
			}
		}
		if !path.IsAbs(socket) {
			return false, fmt.Errorf("managed process helper socket must be an absolute path")
		}
		args = append(args, "--backend-socket="+socket)
	}
	args = append(args, "--")
	args = append(args, main.Command...)
	args = append(args, main.Args...)
	main.Command = managedProcessCommand()
	main.Args = args
	if moveHook {
		main.Lifecycle.PostStart = restoredHook
	}
	return true, nil
}

func managedProcessOriginalProbe(pod *corev1.Pod, container string, probe *corev1.Probe) (*corev1.Probe, error) {
	if probe == nil || probe.HTTPGet == nil || probe.Exec != nil || probe.TCPSocket != nil || probe.GRPC != nil ||
		pod.Annotations[trafficproxy.HealthProbeRewriteAnnotation] != "true" {
		return probe, nil
	}
	http := probe.HTTPGet
	if http.Port != intstr.FromInt32(15020) || http.Host != "" || len(http.HTTPHeaders) != 0 ||
		(http.Scheme != "" && http.Scheme != corev1.URISchemeHTTP) ||
		!slices.Contains([]string{"/app-health/" + container + "/readyz", "/app-health/" + container + "/livez", "/app-health/" + container + "/startupz"}, http.Path) {
		return probe, nil
	}
	proxyName := pod.Annotations[trafficproxy.SidecarProxyAnnotation]
	if proxyName == "" {
		proxyName = trafficproxy.ProxyContainerName
	}
	for _, containers := range [][]corev1.Container{pod.Spec.InitContainers, pod.Spec.Containers} {
		for _, sidecar := range containers {
			if sidecar.Name != proxyName {
				continue
			}
			for _, env := range sidecar.Env {
				if env.Name != trafficproxy.KubeAppProberEnv {
					continue
				}
				var originals trafficproxy.KubeAppProbers
				if err := json.Unmarshal([]byte(env.Value), &originals); err != nil {
					return nil, fmt.Errorf("managed process cannot decode original traffic-proxy probes: %w", err)
				}
				if original := originals[http.Path]; original != nil {
					return &corev1.Probe{ProbeHandler: corev1.ProbeHandler{TCPSocket: original.TCPSocket, HTTPGet: original.HTTPGet, GRPC: original.GRPC}}, nil
				}
			}
		}
	}
	return probe, nil
}

// stampManagedProcessAnnotation persists only the enabled capability before pod
// creation. Copy-on-write leaves the caller unchanged on a failed patch.
func (c *PodControl) stampManagedProcessAnnotation(ctx context.Context, box *agentsv1alpha1.Sandbox) error {
	if box.Annotations[utils.AnnotationManagedProcess] == "true" {
		return nil
	}
	updated := box.DeepCopy()
	if updated.Annotations == nil {
		updated.Annotations = map[string]string{}
	}
	updated.Annotations[utils.AnnotationManagedProcess] = "true"
	if err := c.Patch(ctx, updated, client.MergeFrom(box.DeepCopy())); err != nil {
		logf.FromContext(ctx).Error(err, "failed to stamp managed process annotation on sandbox")
		c.recorder.Event(box, corev1.EventTypeWarning, "ManagedProcessStampFailed",
			fmt.Sprintf("Failed to stamp managed process annotation: %v", err))
		return fmt.Errorf("failed to stamp managed process annotation: %w", err)
	}
	box.Annotations = updated.Annotations
	box.ResourceVersion = updated.ResourceVersion
	return nil
}
