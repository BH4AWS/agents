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
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils"
	trafficproxy "github.com/openkruise/agents/pkg/utils/sidecarutils/traffic-proxy"
)

func managedProcessTestPod() *corev1.Pod {
	root := int64(0)
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{utils.AnnotationManagedProcess: "true"}},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name: "main", Image: "demo", Command: []string{"/app/main", "command arg"},
				Args:       []string{"a b", `"quoted"`, "'single'", "$(literal)", "", "x;y"},
				WorkingDir: "/work", SecurityContext: &corev1.SecurityContext{RunAsUser: &root},
				Env:            []corev1.EnvVar{{Name: "ENVD_DIR", Value: "/mnt/envd"}, {Name: "USER_ENV", Value: "preserved"}},
				VolumeMounts:   []corev1.VolumeMount{{Name: "envd", MountPath: "/mnt/envd"}},
				ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(utils.RuntimePort)}}},
				Lifecycle: &corev1.Lifecycle{
					PostStart: &corev1.LifecycleHandler{Exec: &corev1.ExecAction{Command: []string{"bash", "/mnt/envd/envd-run.sh"}}},
					PreStop:   &corev1.LifecycleHandler{Exec: &corev1.ExecAction{Command: []string{"/app/cleanup", "a b"}}},
				},
			}, {Name: "sidecar", Command: []string{"sidecar"}}},
			Volumes: []corev1.Volume{{Name: "envd", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}},
		},
	}
}

func TestWrapManagedProcessDisabled(t *testing.T) {
	for _, marker := range []string{"", "false", "True", "1", " true "} {
		t.Run(marker, func(t *testing.T) {
			pod := managedProcessTestPod()
			pod.Annotations[utils.AnnotationManagedProcess] = marker
			// Invalid topology/command must not matter when disabled.
			pod.Spec.HostPID = true
			pod.Spec.Containers[0].Command = nil
			before := pod.DeepCopy()
			enabled, err := wrapManagedProcess(pod)
			require.NoError(t, err)
			assert.False(t, enabled)
			assert.Equal(t, before, pod)
		})
	}
}

func TestWrapManagedProcessValidation(t *testing.T) {
	for _, tt := range []struct {
		name string
		edit func(*corev1.Pod)
		want string
	}{
		{"no main", func(p *corev1.Pod) { p.Spec.Containers = nil }, "main container"},
		{"missing command", func(p *corev1.Pod) { p.Spec.Containers[0].Command = nil }, "explicit non-empty"},
		{"empty command", func(p *corev1.Pod) { p.Spec.Containers[0].Command = []string{""} }, "explicit non-empty"},
		{"missing env", func(p *corev1.Pod) { p.Spec.Containers[0].Env = nil }, "injected ENVD_DIR"},
		{"custom envd path", func(p *corev1.Pod) { p.Spec.Containers[0].Env[0].Value = "/other" }, "ENVD_DIR=/mnt/envd"},
		{"dynamic envd path", func(p *corev1.Pod) { p.Spec.Containers[0].Env[0].ValueFrom = &corev1.EnvVarSource{} }, "ENVD_DIR=/mnt/envd"},
		{"missing mount", func(p *corev1.Pod) { p.Spec.Containers[0].VolumeMounts = nil }, "volume mounted"},
		{"missing volume", func(p *corev1.Pod) { p.Spec.Volumes = nil }, "volume mounted"},
		{"subpath mount", func(p *corev1.Pod) { p.Spec.Containers[0].VolumeMounts[0].SubPath = "other" }, "volume mounted"},
		{"host PID", func(p *corev1.Pod) { p.Spec.HostPID = true }, "private container PID"},
		{"shared PID", func(p *corev1.Pod) { yes := true; p.Spec.ShareProcessNamespace = &yes }, "private container PID"},
		{"readiness HTTP", func(p *corev1.Pod) {
			p.Spec.Containers[0].ReadinessProbe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/ready"}}}
		}, "readiness probe"},
		{"startup exec", func(p *corev1.Pod) {
			p.Spec.Containers[0].StartupProbe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"check"}}}}
		}, "startup probe"},
		{"liveness TCP business", func(p *corev1.Pod) {
			p.Spec.Containers[0].LivenessProbe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(8080)}}}
		}, "liveness probe"},
		{"shell hook rejected", func(p *corev1.Pod) {
			p.Spec.Containers[0].Lifecycle.PostStart.Exec.Command = []string{"bash", "-c", "/mnt/envd/envd-run.sh"}
		}, "direct bash"},
		{"helper socket missing", func(p *corev1.Pod) {
			p.Spec.Containers[0].Lifecycle.PostStart.Exec.Command = []string{"bash", "/mnt/envd/envd-run.sh", "--helper", "--helper-socket"}
		}, "requires a value"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			pod := managedProcessTestPod()
			tt.edit(pod)
			before := pod.DeepCopy()
			enabled, err := wrapManagedProcess(pod)
			require.ErrorContains(t, err, tt.want)
			assert.False(t, enabled)
			assert.Equal(t, before, pod, "validation failures must not partially mutate the pod")
		})
	}
}

func TestWrapManagedProcessArgvAndHooks(t *testing.T) {
	userHook := []string{"/app/hook", "a b", `"quote"`, "'single'", "x;y"}
	for _, tt := range []struct {
		name        string
		hook        *corev1.LifecycleHandler
		backendArgs []string
		wantHook    *corev1.LifecycleHandler
		socket      string
	}{
		{"plain backend", &corev1.LifecycleHandler{Exec: &corev1.ExecAction{Command: []string{"bash", "/mnt/envd/envd-run.sh"}}}, []string{"/mnt/envd/envd-run.sh", "--foreground"}, nil, ""},
		{"user hook restored", &corev1.LifecycleHandler{Exec: &corev1.ExecAction{Command: append([]string{"/bin/bash", "/mnt/envd/envd-run.sh", "--debug", "a b", "--"}, userHook...)}}, []string{"/mnt/envd/envd-run.sh", "--debug", "a b", "--foreground"}, &corev1.LifecycleHandler{Exec: &corev1.ExecAction{Command: userHook}}, ""},
		{"user exec preserved", &corev1.LifecycleHandler{Exec: &corev1.ExecAction{Command: userHook}}, []string{"/mnt/envd/envd-run.sh", "--foreground"}, &corev1.LifecycleHandler{Exec: &corev1.ExecAction{Command: userHook}}, ""},
		{"user HTTP preserved", &corev1.LifecycleHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/init-hook", Port: intstr.FromInt32(8080)}}, []string{"/mnt/envd/envd-run.sh", "--foreground"}, &corev1.LifecycleHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/init-hook", Port: intstr.FromInt32(8080)}}, ""},
		{"helper default socket", &corev1.LifecycleHandler{Exec: &corev1.ExecAction{Command: []string{"bash", "/mnt/envd/envd-run.sh", "--helper"}}}, []string{"/mnt/envd/envd-run.sh", "--helper", "--foreground"}, nil, "/var/run/agent-helper/helper.sock"},
		{"helper equals socket", &corev1.LifecycleHandler{Exec: &corev1.ExecAction{Command: []string{"bash", "/mnt/envd/envd-run.sh", "--helper", "--helper-socket=/run/custom.sock", "--foreground"}}}, []string{"/mnt/envd/envd-run.sh", "--helper", "--helper-socket=/run/custom.sock", "--foreground"}, nil, "/run/custom.sock"},
		{"helper split socket", &corev1.LifecycleHandler{Exec: &corev1.ExecAction{Command: []string{"bash", "/mnt/envd/envd-run.sh", "--helper", "--helper-socket", "/run/a b.sock", "--"}}}, []string{"/mnt/envd/envd-run.sh", "--helper", "--helper-socket", "/run/a b.sock", "--foreground"}, nil, "/run/a b.sock"},
		{"no hook", nil, []string{"/mnt/envd/envd-run.sh", "--foreground"}, nil, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			pod := managedProcessTestPod()
			main := &pod.Spec.Containers[0]
			main.Lifecycle.PostStart = tt.hook
			if tt.socket != "" {
				main.ReadinessProbe = nil // Helper backends need no envd TCP probe.
			}
			before := pod.DeepCopy()
			original := append(slices.Clone(main.Command), main.Args...)
			enabled, err := wrapManagedProcess(pod)
			require.NoError(t, err)
			assert.True(t, enabled)
			assert.Equal(t, managedProcessCommand(), main.Command)
			want := []string{"run", "--backend=/bin/bash"}
			for _, arg := range tt.backendArgs {
				want = append(want, "--backend-arg="+arg)
			}
			if tt.socket != "" {
				want = append(want, "--backend-socket="+tt.socket)
			}
			want = append(want, "--")
			want = append(want, original...)
			assert.Equal(t, want, main.Args)
			assert.Equal(t, tt.wantHook, main.Lifecycle.PostStart)
			assert.Equal(t, before.Spec.Containers[0].Lifecycle.PreStop, main.Lifecycle.PreStop)
			assert.Equal(t, before.Spec.Containers[0].WorkingDir, main.WorkingDir)
			assert.Equal(t, before.Spec.Containers[0].SecurityContext, main.SecurityContext)
			assert.Equal(t, before.Spec.Containers[0].Env, main.Env)
			assert.Equal(t, before.Spec.Containers[1], pod.Spec.Containers[1])
			wrapped := pod.DeepCopy()
			enabled, err = wrapManagedProcess(pod)
			require.NoError(t, err)
			assert.True(t, enabled)
			assert.Equal(t, wrapped, pod)
		})
	}
}

func TestWrapManagedProcessTrafficProxy(t *testing.T) {
	for _, tt := range []struct {
		name      string
		initProxy bool
		custom    bool
		port      int32
		badJSON   bool
		missing   bool
	}{
		{name: "runtime probes", port: utils.RuntimePort},
		{name: "native sidecar", initProxy: true, port: utils.RuntimePort},
		{name: "custom proxy", custom: true, port: utils.RuntimePort},
		{name: "business probes rejected", port: 8080},
		{name: "invalid originals rejected", port: utils.RuntimePort, badJSON: true},
		{name: "missing originals rejected", port: utils.RuntimePort, missing: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			pod := managedProcessTestPod()
			main := &pod.Spec.Containers[0]
			main.ReadinessProbe.TCPSocket.Port = intstr.FromInt32(tt.port)
			main.StartupProbe = main.ReadinessProbe.DeepCopy()
			main.LivenessProbe = main.ReadinessProbe.DeepCopy()
			pod.Annotations[trafficproxy.HealthProbeRewriteAnnotation] = "true"
			proxy := corev1.Container{Name: trafficproxy.ProxyContainerName}
			if tt.custom {
				proxy.Name = "custom-proxy"
				pod.Annotations[trafficproxy.SidecarProxyAnnotation] = proxy.Name
			}
			if tt.initProxy {
				always := corev1.ContainerRestartPolicyAlways
				proxy.RestartPolicy = &always
				pod.Spec.InitContainers = append(pod.Spec.InitContainers, proxy)
			} else {
				pod.Spec.Containers = append(pod.Spec.Containers, proxy)
			}
			require.NoError(t, trafficproxy.ApplyHealthProbeRewrite(pod))
			for _, containers := range [][]corev1.Container{pod.Spec.InitContainers, pod.Spec.Containers} {
				for i := range containers {
					if containers[i].Name == proxy.Name {
						if tt.badJSON {
							containers[i].Env[0].Value = "{"
						}
						if tt.missing {
							containers[i].Env = nil
						}
					}
				}
			}
			before := pod.DeepCopy()
			enabled, err := wrapManagedProcess(pod)
			if tt.port != utils.RuntimePort || tt.badJSON || tt.missing {
				require.Error(t, err)
				assert.False(t, enabled)
				assert.Equal(t, before, pod)
				return
			}
			require.NoError(t, err)
			assert.True(t, enabled)
			assert.Equal(t, before.Spec.Containers[0].ReadinessProbe, pod.Spec.Containers[0].ReadinessProbe)
			assert.Equal(t, before.Spec.Containers[0].StartupProbe, pod.Spec.Containers[0].StartupProbe)
			assert.Equal(t, before.Spec.Containers[0].LivenessProbe, pod.Spec.Containers[0].LivenessProbe)
			assert.NotContains(t, pod.Spec.Containers[0].Args, "--wait-for-init")
		})
	}
}

func TestCreatePodManagedProcessStamp(t *testing.T) {
	for _, tt := range []struct {
		name     string
		enabled  bool
		stamp    string
		patchErr bool
		wrapped  bool
	}{
		{name: "template opt-in", enabled: true},
		{name: "existing false marker upgraded", enabled: true, stamp: "false"},
		{name: "already stamped checkpoint", enabled: true, stamp: "true", wrapped: true},
		{name: "sandbox marker alone does not opt in", stamp: "true"},
		{name: "default disabled"},
		{name: "patch failure aborts creation", enabled: true, patchErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			require.NoError(t, clientgoscheme.AddToScheme(scheme))
			require.NoError(t, agentsv1alpha1.AddToScheme(scheme))
			box := &agentsv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{
				Name: "managed", Namespace: "default", UID: types.UID("sandbox-uid"),
				Annotations: map[string]string{"other": "keep"},
			}}
			if tt.stamp != "" {
				box.Annotations[utils.AnnotationManagedProcess] = tt.stamp
			}
			patches, creates := 0, 0
			cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(box.DeepCopy()).WithInterceptorFuncs(interceptor.Funcs{
				Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					patches++
					data, err := patch.Data(obj)
					require.NoError(t, err)
					var got map[string]any
					require.NoError(t, json.Unmarshal(data, &got))
					assert.Equal(t, map[string]any{"metadata": map[string]any{"annotations": map[string]any{utils.AnnotationManagedProcess: "true"}}}, got)
					if tt.patchErr {
						return fmt.Errorf("patch denied")
					}
					return c.Patch(ctx, obj, patch, opts...)
				},
				Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
					creates++
					if tt.enabled {
						stored := &agentsv1alpha1.Sandbox{}
						require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(box), stored))
						assert.Equal(t, "true", stored.Annotations[utils.AnnotationManagedProcess])
					}
					return c.Create(ctx, obj, opts...)
				},
			}).Build()
			gen := func(_ context.Context, args PodGenerateArgs) (*corev1.Pod, error) {
				pod := managedProcessTestPod()
				pod.Name, pod.Namespace = args.Box.Name, args.Box.Namespace
				if !tt.enabled {
					delete(pod.Annotations, utils.AnnotationManagedProcess)
				}
				return pod, nil
			}
			before := box.DeepCopy()
			control := NewPodControl(cli, record.NewFakeRecorder(10), gen)
			var delta *runtime.RawExtension
			if tt.wrapped {
				checkpointPod := managedProcessTestPod()
				_, err := wrapManagedProcess(checkpointPod)
				require.NoError(t, err)
				raw, err := json.Marshal(map[string]any{"spec": map[string]any{"containers": checkpointPod.Spec.Containers}})
				require.NoError(t, err)
				delta = &runtime.RawExtension{Raw: raw}
			}
			pod, err := control.CreatePod(t.Context(), CreatePodArgs{Box: box, NewStatus: &agentsv1alpha1.SandboxStatus{}, PodTemplateDelta: delta})
			if tt.patchErr {
				require.ErrorContains(t, err, "patch denied")
				assert.Nil(t, pod)
				assert.Zero(t, creates)
				assert.Equal(t, before, box)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, 1, creates)
			if tt.enabled {
				assert.Equal(t, "true", box.Annotations[utils.AnnotationManagedProcess])
				assert.Equal(t, "true", pod.Annotations[utils.AnnotationManagedProcess])
				assert.Equal(t, managedProcessCommand(), pod.Spec.Containers[0].Command)
				if tt.wrapped {
					runs := 0
					for _, arg := range pod.Spec.Containers[0].Args {
						if arg == "run" {
							runs++
						}
					}
					assert.Equal(t, 1, runs)
				}
			}
			wantPatches := 0
			if tt.enabled && tt.stamp != "true" {
				wantPatches = 1
			}
			assert.Equal(t, wantPatches, patches)
			assert.Equal(t, "keep", box.Annotations["other"])
		})
	}
}
