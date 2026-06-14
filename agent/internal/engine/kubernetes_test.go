package engine_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/gocdnext/gocdnext/agent/internal/engine"
)

// Tests against k8s.io/client-go/kubernetes/fake. The fake clientset
// doesn't simulate lifecycles — a Pod stays in its Pending phase
// until we explicitly patch it. We drive the phase transitions via
// reactors so the wait loops fire.

func newFakeEngine(t *testing.T, cfg engine.KubernetesConfig) (*engine.Kubernetes, *fake.Clientset) {
	t.Helper()
	cfg.Namespace = "gocdnext-tests"
	if cfg.PollInterval == 0 {
		cfg.PollInterval = 5 * time.Millisecond
	}
	if cfg.StartupTimeout == 0 {
		cfg.StartupTimeout = 2 * time.Second
	}
	cli := fake.NewSimpleClientset()
	return engine.NewKubernetesWithClient(cli, cfg), cli
}

// advancePod adjusts the Pod's phase + ContainerStatuses via Update,
// simulating what kubelet would do in a real cluster.
//
// Uses t.Errorf (not Fatalf) because callers dispatch this via
// `go advancePod(...)`. Fatalf from a non-test goroutine only
// exits the goroutine — go vet rightfully flags that, and the
// test would continue silently toward a timeout instead of
// failing fast. Errorf is goroutine-safe and the downstream
// RunScript assertion catches the cascade when advancing the
// pod fails.
func advancePod(t *testing.T, cli *fake.Clientset, ns, name string, phase corev1.PodPhase, exitCode int32) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	pod, err := cli.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Errorf("get: %v", err)
		return
	}
	pod.Status.Phase = phase
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: "task",
		State: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{ExitCode: exitCode},
		},
	}}
	if _, err := cli.CoreV1().Pods(ns).UpdateStatus(ctx, pod, metav1.UpdateOptions{}); err != nil {
		t.Errorf("updateStatus: %v", err)
	}
}

func TestKubernetes_BuildPodSpec_Defaults(t *testing.T) {
	k, _ := newFakeEngine(t, engine.KubernetesConfig{
		DefaultImage: "alpine:3.19",
	})
	pod := k.BuildPodSpec(engine.ScriptSpec{
		Script: "echo hi",
		Env:    map[string]string{"FOO": "bar"},
	})
	if pod.Namespace != "gocdnext-tests" {
		t.Errorf("namespace = %q", pod.Namespace)
	}
	if got := pod.Labels["app.kubernetes.io/managed-by"]; got != "gocdnext-agent" {
		t.Errorf("label: %q", got)
	}
	if len(pod.Spec.Containers) != 1 {
		t.Fatalf("containers = %d", len(pod.Spec.Containers))
	}
	c := pod.Spec.Containers[0]
	if c.Image != "alpine:3.19" {
		t.Errorf("image = %q", c.Image)
	}
	// `sh -c -- <script>` — `--` after -c stops option parsing so a
	// script that legitimately starts with `-` reaches sh as the
	// command, not as a flag. Asserting on the exact shape locks
	// the behaviour against accidental drift.
	if len(c.Command) != 4 || c.Command[0] != "sh" || c.Command[1] != "-c" || c.Command[2] != "--" {
		t.Errorf("command = %+v", c.Command)
	}
	if c.Command[3] != "echo hi" {
		t.Errorf("script = %q", c.Command[3])
	}
	// Env must include FOO=bar.
	var foo *corev1.EnvVar
	for i := range c.Env {
		if c.Env[i].Name == "FOO" {
			foo = &c.Env[i]
		}
	}
	if foo == nil || foo.Value != "bar" {
		t.Errorf("env FOO missing or wrong")
	}
	// Volume + mount should exist.
	if len(pod.Spec.Volumes) != 1 || pod.Spec.Volumes[0].Name != "workspace" {
		t.Errorf("volumes = %+v", pod.Spec.Volumes)
	}
	if len(c.VolumeMounts) != 1 || c.VolumeMounts[0].MountPath != "/workspace" {
		t.Errorf("volume mounts = %+v", c.VolumeMounts)
	}
}

func TestKubernetes_BuildPodSpec_MergesNodeSelectorWithProfileWinning(t *testing.T) {
	// Agent NodeSelector is the StatefulSet-level baseline; profile
	// NodeSelector is the per-spec override. On key collision, the
	// profile WINS — a job pointing at `pool: gradle` lands on
	// gradle nodes even if the agent's default says `pool: ci`.
	k, _ := newFakeEngine(t, engine.KubernetesConfig{
		DefaultImage: "alpine:3.19",
		NodeSelector: map[string]string{"tier": "ci", "pool": "ci"},
	})
	pod := k.BuildPodSpec(engine.ScriptSpec{
		Script:       "true",
		NodeSelector: map[string]string{"pool": "gradle"},
	})
	if pod.Spec.NodeSelector["tier"] != "ci" {
		t.Errorf("agent tier lost: %v", pod.Spec.NodeSelector)
	}
	if pod.Spec.NodeSelector["pool"] != "gradle" {
		t.Errorf("profile pool did not win: %v", pod.Spec.NodeSelector)
	}
}

func TestKubernetes_BuildPodSpec_ConcatsTolerations(t *testing.T) {
	// Agent-level Tolerations come first; profile entries are
	// APPENDED. Kubelet handles dedup, so we don't.
	k, _ := newFakeEngine(t, engine.KubernetesConfig{
		DefaultImage: "alpine:3.19",
		Tolerations: []corev1.Toleration{
			{Key: "node.kubernetes.io/unschedulable", Operator: corev1.TolerationOpExists},
		},
	})
	pod := k.BuildPodSpec(engine.ScriptSpec{
		Script: "true",
		Tolerations: []corev1.Toleration{
			{Key: "ci-only", Operator: corev1.TolerationOpEqual, Value: "true"},
		},
	})
	if len(pod.Spec.Tolerations) != 2 {
		t.Fatalf("expected 2 tolerations, got %d", len(pod.Spec.Tolerations))
	}
	if pod.Spec.Tolerations[0].Key != "node.kubernetes.io/unschedulable" {
		t.Errorf("agent toleration not first: %v", pod.Spec.Tolerations)
	}
	if pod.Spec.Tolerations[1].Key != "ci-only" {
		t.Errorf("profile toleration not appended: %v", pod.Spec.Tolerations)
	}
}

func TestKubernetes_BuildPodSpec_OutputsEnv_AnchoredAtWorkDir(t *testing.T) {
	// Regression: shared K8s mounts the WHOLE PVC at
	// ContainerWorkspaceMount, so a target_dir-nested workspace
	// (scriptWorkDir = mount + "/" + target_dir) puts the outputs
	// file at <mount>/<target_dir>/.gocdnext/outputs/X.env on
	// disk. The previous code joined the env path with
	// ContainerWorkspaceMount, sending the plugin's
	// `> $GOCDNEXT_OUTPUT_FILE` to <mount>/.gocdnext/... — a
	// sibling dir nobody created. Fix: join with workDir.
	k, _ := newFakeEngine(t, engine.KubernetesConfig{DefaultImage: "alpine:3.19"})
	pod := k.BuildPodSpec(engine.ScriptSpec{
		Script:          "echo hi",
		WorkDir:         "/workspace/r1/j1/app", // ← target_dir nest
		OutputsHostPath: "/host/.../app/.gocdnext/outputs/abc.env",
		OutputsRelPath:  ".gocdnext/outputs/abc.env",
	})
	c := pod.Spec.Containers[0]
	var got string
	for _, e := range c.Env {
		if e.Name == engine.OutputsEnvName {
			got = e.Value
		}
	}
	want := "/workspace/r1/j1/app/.gocdnext/outputs/abc.env"
	if got != want {
		t.Errorf("%s: want %q (anchored at workDir incl. target_dir), got %q",
			engine.OutputsEnvName, want, got)
	}
}

func TestKubernetes_BuildPodSpec_OutputsEnv_FallsBackToMountWhenWorkDirEmpty(t *testing.T) {
	// Plugin-only jobs have no checkouts → spec.WorkDir empty →
	// BuildPodSpec falls back to WorkspaceMountPath. The env
	// injection must follow the same fallback so the env stays
	// well-defined.
	k, _ := newFakeEngine(t, engine.KubernetesConfig{DefaultImage: "alpine:3.19"})
	pod := k.BuildPodSpec(engine.ScriptSpec{
		Script:          "echo hi",
		OutputsHostPath: "/host/.gocdnext/outputs/abc.env",
		OutputsRelPath:  ".gocdnext/outputs/abc.env",
	})
	c := pod.Spec.Containers[0]
	var got string
	for _, e := range c.Env {
		if e.Name == engine.OutputsEnvName {
			got = e.Value
		}
	}
	want := "/workspace/.gocdnext/outputs/abc.env"
	if got != want {
		t.Errorf("%s: want %q (mount fallback when WorkDir empty), got %q",
			engine.OutputsEnvName, want, got)
	}
}

func TestKubernetes_BuildPodSpec_AppliesResources(t *testing.T) {
	k, _ := newFakeEngine(t, engine.KubernetesConfig{DefaultImage: "alpine:3.19"})
	pod := k.BuildPodSpec(engine.ScriptSpec{
		Script: "make",
		Resources: engine.Resources{
			CPURequest:    "500m",
			CPULimit:      "2",
			MemoryRequest: "512Mi",
			MemoryLimit:   "2Gi",
		},
	})
	c := pod.Spec.Containers[0]
	if got := c.Resources.Requests.Cpu().String(); got != "500m" {
		t.Errorf("cpu request = %q, want 500m", got)
	}
	if got := c.Resources.Requests.Memory().String(); got != "512Mi" {
		t.Errorf("memory request = %q, want 512Mi", got)
	}
	if got := c.Resources.Limits.Cpu().String(); got != "2" {
		t.Errorf("cpu limit = %q, want 2", got)
	}
	if got := c.Resources.Limits.Memory().String(); got != "2Gi" {
		t.Errorf("memory limit = %q, want 2Gi", got)
	}
}

func TestKubernetes_BuildPodSpec_NoResourcesLeavesContainerEmpty(t *testing.T) {
	k, _ := newFakeEngine(t, engine.KubernetesConfig{DefaultImage: "alpine:3.19"})
	pod := k.BuildPodSpec(engine.ScriptSpec{Script: "make"})
	c := pod.Spec.Containers[0]
	if len(c.Resources.Requests) != 0 || len(c.Resources.Limits) != 0 {
		t.Errorf("expected empty resources, got %+v", c.Resources)
	}
}

func TestKubernetes_BuildPodSpec_PartialResourcesArePartial(t *testing.T) {
	k, _ := newFakeEngine(t, engine.KubernetesConfig{DefaultImage: "alpine:3.19"})
	pod := k.BuildPodSpec(engine.ScriptSpec{
		Script:    "make",
		Resources: engine.Resources{MemoryLimit: "1Gi"},
	})
	c := pod.Spec.Containers[0]
	if len(c.Resources.Requests) != 0 {
		t.Errorf("requests should be empty, got %+v", c.Resources.Requests)
	}
	if got := c.Resources.Limits.Memory().String(); got != "1Gi" {
		t.Errorf("memory limit = %q, want 1Gi", got)
	}
	// CPU limit must NOT be set when not provided — kubelet defaults it.
	if _, ok := c.Resources.Limits[corev1.ResourceCPU]; ok {
		t.Errorf("cpu limit should not be set when empty")
	}
}

func TestKubernetes_BuildPodSpec_LabelsCarryProfileAndTags(t *testing.T) {
	k, _ := newFakeEngine(t, engine.KubernetesConfig{DefaultImage: "alpine:3.19"})
	pod := k.BuildPodSpec(engine.ScriptSpec{
		Script:    "make",
		Profile:   "gpu",
		AgentTags: []string{"linux", "amd64", "with space"},
	})
	if got := pod.Labels["gocdnext.io/profile"]; got != "gpu" {
		t.Errorf("profile label = %q, want gpu", got)
	}
	if _, ok := pod.Labels["gocdnext.io/tag-linux"]; !ok {
		t.Errorf("expected gocdnext.io/tag-linux label, got %+v", pod.Labels)
	}
	if _, ok := pod.Labels["gocdnext.io/tag-amd64"]; !ok {
		t.Errorf("expected gocdnext.io/tag-amd64 label, got %+v", pod.Labels)
	}
	// "with space" violates DNS-1123 → dropped silently.
	for k := range pod.Labels {
		if strings.Contains(k, "space") {
			t.Errorf("malformed tag should have been dropped, found %q", k)
		}
	}
	// Static labels still present.
	if pod.Labels["app.kubernetes.io/managed-by"] != "gocdnext-agent" {
		t.Errorf("static labels missing: %+v", pod.Labels)
	}
}

func TestKubernetes_BuildPodSpec_PVCWhenConfigured(t *testing.T) {
	k, _ := newFakeEngine(t, engine.KubernetesConfig{
		WorkspacePVCName:   "gocdnext-ws",
		WorkspaceMountPath: "/w",
	})
	pod := k.BuildPodSpec(engine.ScriptSpec{Image: "golang:1.23", Script: "go build"})
	if pod.Spec.Volumes[0].PersistentVolumeClaim == nil {
		t.Fatalf("expected PVC volume source, got %+v", pod.Spec.Volumes[0].VolumeSource)
	}
	if pod.Spec.Volumes[0].PersistentVolumeClaim.ClaimName != "gocdnext-ws" {
		t.Errorf("claim = %q", pod.Spec.Volumes[0].PersistentVolumeClaim.ClaimName)
	}
	if pod.Spec.Containers[0].VolumeMounts[0].MountPath != "/w" {
		t.Errorf("mount = %q", pod.Spec.Containers[0].VolumeMounts[0].MountPath)
	}
	if pod.Spec.Containers[0].Image != "golang:1.23" {
		t.Errorf("image override ignored: %q", pod.Spec.Containers[0].Image)
	}
}

func TestKubernetes_RunScript_SuccessReturnsExitZero(t *testing.T) {
	k, cli := newFakeEngine(t, engine.KubernetesConfig{})

	// Drive the Pod's lifecycle: on first Get after create, flip to
	// Succeeded with exit 0. Reactor fires on Get.
	var mu sync.Mutex
	driven := false
	cli.PrependReactor("get", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		mu.Lock()
		defer mu.Unlock()
		if driven {
			return false, nil, nil // fall through to tracker
		}
		driven = true
		name := a.(k8stesting.GetAction).GetName()
		go advancePod(t, cli, "gocdnext-tests", name, corev1.PodSucceeded, 0)
		return false, nil, nil
	})

	exit, err := k.RunScript(context.Background(), engine.ScriptSpec{
		Script: "true",
	})
	if err != nil {
		t.Fatalf("RunScript: %v", err)
	}
	if exit != 0 {
		t.Errorf("exit = %d", exit)
	}
}

func TestKubernetes_RunScript_FailedReturnsExitCode(t *testing.T) {
	k, cli := newFakeEngine(t, engine.KubernetesConfig{})

	var mu sync.Mutex
	driven := false
	cli.PrependReactor("get", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		mu.Lock()
		defer mu.Unlock()
		if driven {
			return false, nil, nil
		}
		driven = true
		name := a.(k8stesting.GetAction).GetName()
		go advancePod(t, cli, "gocdnext-tests", name, corev1.PodFailed, 7)
		return false, nil, nil
	})

	exit, err := k.RunScript(context.Background(), engine.ScriptSpec{Script: "exit 7"})
	if err != nil {
		t.Fatalf("RunScript: %v", err)
	}
	if exit != 7 {
		t.Errorf("exit = %d", exit)
	}
}

func TestKubernetes_RunScript_StartupTimeout(t *testing.T) {
	cfg := engine.KubernetesConfig{StartupTimeout: 50 * time.Millisecond}
	k, _ := newFakeEngine(t, cfg)
	// Don't drive anything — Pod stays Pending, startup timer fires.
	start := time.Now()
	_, err := k.RunScript(context.Background(), engine.ScriptSpec{Script: "true"})
	if err == nil {
		t.Fatal("expected startup timeout error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		// wait.PollUntilContextCancel wraps; accept either.
		t.Logf("err chain: %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("took too long: %v", time.Since(start))
	}
}

func TestKubernetes_RunScript_CleanupOnSuccess(t *testing.T) {
	k, cli := newFakeEngine(t, engine.KubernetesConfig{
		CleanupOnSuccess: true,
	})
	var mu sync.Mutex
	driven := false
	var createdName string
	cli.PrependReactor("get", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		mu.Lock()
		defer mu.Unlock()
		if driven {
			return false, nil, nil
		}
		driven = true
		name := a.(k8stesting.GetAction).GetName()
		createdName = name
		go advancePod(t, cli, "gocdnext-tests", name, corev1.PodSucceeded, 0)
		return false, nil, nil
	})

	if _, err := k.RunScript(context.Background(), engine.ScriptSpec{Script: "true"}); err != nil {
		t.Fatalf("RunScript: %v", err)
	}
	// Pod should be gone — fake tracks this.
	_, err := cli.CoreV1().Pods("gocdnext-tests").Get(context.Background(), createdName, metav1.GetOptions{})
	if err == nil {
		t.Errorf("pod %q should have been deleted after success", createdName)
	}
}

func TestKubernetes_RunScript_KeepsPodWhenCleanupDisabled(t *testing.T) {
	k, cli := newFakeEngine(t, engine.KubernetesConfig{}) // defaults = keep

	var mu sync.Mutex
	driven := false
	var createdName string
	cli.PrependReactor("get", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		mu.Lock()
		defer mu.Unlock()
		if driven {
			return false, nil, nil
		}
		driven = true
		name := a.(k8stesting.GetAction).GetName()
		createdName = name
		go advancePod(t, cli, "gocdnext-tests", name, corev1.PodFailed, 1)
		return false, nil, nil
	})

	_, _ = k.RunScript(context.Background(), engine.ScriptSpec{Script: "false"})
	_, err := cli.CoreV1().Pods("gocdnext-tests").Get(context.Background(), createdName, metav1.GetOptions{})
	if err != nil {
		t.Errorf("pod should still exist for debugging: %v", err)
	}
}

// TestKubernetes_RunScript_DeletesPodOnCancelDespiteCleanupOnFailureFalse
// is the shared-mode regression for the v0.14.2 cancel-doesn't-kill-pod
// bug. With CleanupOnFailure=false (the default operators run), a job
// canceled mid-flight via Runner.Cancel (which propagates as
// context.Canceled on the run ctx) used to leave its pod Running until
// someone deleted it by hand. After the fix the cleanup path detects
// the cancellation and force-deletes, using a fresh background ctx so
// the canceled run ctx doesn't abort the DELETE itself.
func TestKubernetes_RunScript_DeletesPodOnCancelDespiteCleanupOnFailureFalse(t *testing.T) {
	k, cli := newFakeEngine(t, engine.KubernetesConfig{
		CleanupOnFailure: false, // would normally KEEP the pod
	})
	ctx, cancel := context.WithCancel(context.Background())

	var mu sync.Mutex
	driven := false
	var createdName string
	// First Get on the pod = the polling loop has started. Cancel
	// the ctx right then — simulates an operator hitting Cancel
	// while the task is running. PollUntilContextCancel sees the
	// canceled ctx on its next iteration and returns, RunScript
	// proceeds to maybeCleanup, and our fix overrides the keep-
	// pod decision and deletes the pod.
	cli.PrependReactor("get", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		mu.Lock()
		defer mu.Unlock()
		if driven {
			return false, nil, nil
		}
		driven = true
		createdName = a.(k8stesting.GetAction).GetName()
		cancel()
		return false, nil, nil
	})

	// RunScript will see ctx canceled and exit with an error — fine,
	// we only care about the cleanup decision afterwards.
	_, _ = k.RunScript(ctx, engine.ScriptSpec{Script: "sleep 9999"})

	// Pod MUST be gone — cancellation overrides CleanupOnFailure=false.
	if _, err := cli.CoreV1().Pods("gocdnext-tests").Get(
		context.Background(), createdName, metav1.GetOptions{}); err == nil {
		t.Errorf("pod %q should have been deleted on cancel despite CleanupOnFailure=false", createdName)
	}
}

func TestKubernetes_Name(t *testing.T) {
	k, _ := newFakeEngine(t, engine.KubernetesConfig{})
	if n := k.Name(); n != "kubernetes" {
		t.Errorf("Name = %q", n)
	}
}

func TestKubernetes_BuildPodSpec_DinDSidecarWhenDockerRequested(t *testing.T) {
	k, _ := newFakeEngine(t, engine.KubernetesConfig{})
	pod := k.BuildPodSpec(engine.ScriptSpec{
		Image:  "node:22",
		Script: "docker run --rm hello-world",
		Docker: true,
	})
	if len(pod.Spec.Containers) != 2 {
		t.Fatalf("containers = %d, want 2 (task + dind)", len(pod.Spec.Containers))
	}

	var task, dind *corev1.Container
	for i := range pod.Spec.Containers {
		switch pod.Spec.Containers[i].Name {
		case "task":
			task = &pod.Spec.Containers[i]
		case "dind":
			dind = &pod.Spec.Containers[i]
		}
	}
	if task == nil {
		t.Fatal("task container missing")
	}
	if dind == nil {
		t.Fatal("dind container missing")
	}

	// Task must know where to find the daemon — docker clients
	// auto-detect via DOCKER_HOST; anything else is opt-in.
	var gotHost string
	for _, e := range task.Env {
		if e.Name == "DOCKER_HOST" {
			gotHost = e.Value
		}
	}
	if gotHost != "tcp://localhost:2375" {
		t.Errorf("DOCKER_HOST = %q, want tcp://localhost:2375", gotHost)
	}

	// DinD must be privileged — non-privileged DinD cannot manage
	// the kernel namespaces it needs. If this assertion ever
	// regresses the daemon fails at boot with a cryptic error.
	if dind.SecurityContext == nil || dind.SecurityContext.Privileged == nil || !*dind.SecurityContext.Privileged {
		t.Errorf("dind not privileged: %+v", dind.SecurityContext)
	}

	// Port exposed so the test documents the contract the task
	// env var points at — matches the single source of truth.
	if len(dind.Ports) != 1 || dind.Ports[0].ContainerPort != 2375 {
		t.Errorf("dind ports = %+v", dind.Ports)
	}
}

func TestKubernetes_BuildPodSpec_NoDinDByDefault(t *testing.T) {
	// Default job must stay single-container — a DinD leak into
	// every pipeline Pod would be a surprise rollout cost (extra
	// image pull per job) even before anyone opts into docker:true.
	k, _ := newFakeEngine(t, engine.KubernetesConfig{})
	pod := k.BuildPodSpec(engine.ScriptSpec{Image: "alpine", Script: "true"})
	if len(pod.Spec.Containers) != 1 {
		t.Errorf("containers = %d, want 1 when Docker=false", len(pod.Spec.Containers))
	}
	for _, e := range pod.Spec.Containers[0].Env {
		if e.Name == "DOCKER_HOST" {
			t.Errorf("DOCKER_HOST set without Docker=true: %q", e.Value)
		}
	}
}

func TestKubernetes_RunScript_DinDReturnsExitOnTaskCompletionEvenIfSidecarLive(t *testing.T) {
	// The core DinD invariant: task container terminates but the
	// dind sidecar keeps running → we must still report exit from
	// the task container without waiting for Pod.Phase to flip to
	// Succeeded/Failed (it never will while dind lives).
	k, cli := newFakeEngine(t, engine.KubernetesConfig{})

	var mu sync.Mutex
	driven := false
	cli.PrependReactor("get", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		mu.Lock()
		defer mu.Unlock()
		if driven {
			return false, nil, nil
		}
		driven = true
		name := a.(k8stesting.GetAction).GetName()
		// Patch task=Terminated(0) BUT keep pod Running (dind
		// still alive): the engine must return exit=0 regardless.
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			pod, err := cli.CoreV1().Pods("gocdnext-tests").Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				t.Errorf("get: %v", err)
				return
			}
			pod.Status.Phase = corev1.PodRunning
			pod.Status.ContainerStatuses = []corev1.ContainerStatus{
				{
					Name: "task",
					State: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{ExitCode: 0},
					},
				},
				{
					Name:  "dind",
					State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
				},
			}
			if _, err := cli.CoreV1().Pods("gocdnext-tests").UpdateStatus(ctx, pod, metav1.UpdateOptions{}); err != nil {
				t.Errorf("updateStatus: %v", err)
			}
		}()
		return false, nil, nil
	})

	exit, err := k.RunScript(context.Background(), engine.ScriptSpec{
		Image:  "node:22",
		Script: "docker run hello-world",
		Docker: true,
	})
	if err != nil {
		t.Fatalf("RunScript: %v", err)
	}
	if exit != 0 {
		t.Errorf("exit = %d, want 0", exit)
	}
	// Pod must have been deleted regardless of CleanupOnSuccess —
	// otherwise dind would leak indefinitely. Walk the list (an
	// empty-name Get always errors, so it can't prove deletion).
	list, _ := cli.CoreV1().Pods("gocdnext-tests").List(context.Background(), metav1.ListOptions{})
	if len(list.Items) != 0 {
		t.Errorf("pod not force-cleaned after DinD run: %d remaining", len(list.Items))
	}
}

func TestNewKubernetes_RejectsMissingNamespace(t *testing.T) {
	_, err := engine.NewKubernetes(engine.KubernetesConfig{})
	if err == nil {
		t.Error("expected error for missing namespace")
	}
}

// TestBuildPodSpec_ImagePullPolicy_MovingTagAlwaysPullsFresh is the
// v0.4.10 regression cover: a job pulling :v1 (or any other moving
// tag) must mark the container ImagePullPolicy: Always so a node with
// the old image cached re-pulls after a release moves the tag.
// Pre-fix, k8s defaulted to IfNotPresent for any tag except :latest,
// and the GKE node kept serving the stale image indefinitely.
func TestBuildPodSpec_ImagePullPolicy(t *testing.T) {
	tests := []struct {
		name      string
		image     string
		want      corev1.PullPolicy
		forceAll  bool
		wantForce corev1.PullPolicy
	}{
		// Moving channel tags → Always.
		{"plugin v1 channel", "ghcr.io/klinux/gocdnext-plugin-buildx:v1", corev1.PullAlways, true, corev1.PullAlways},
		{"plugin v0.4 channel", "ghcr.io/klinux/gocdnext-plugin-buildx:v0.4", corev1.PullAlways, true, corev1.PullAlways},
		{"explicit latest", "alpine:latest", corev1.PullAlways, true, corev1.PullAlways},
		{"implicit latest (no tag)", "alpine", corev1.PullAlways, true, corev1.PullAlways},
		{"named channel main", "myorg/app:main", corev1.PullAlways, true, corev1.PullAlways},
		{"named channel nightly", "myorg/app:nightly", corev1.PullAlways, true, corev1.PullAlways},
		// Pinned tags → IfNotPresent (unless force-always on).
		{"semver pinned", "myorg/app:v1.2.3", corev1.PullIfNotPresent, true, corev1.PullAlways},
		{"alpine point release", "alpine:3.19", corev1.PullIfNotPresent, true, corev1.PullAlways},
		{"sha-prefixed tag", "ghcr.io/klinux/gocdnext-server:sha-deadbeef", corev1.PullIfNotPresent, true, corev1.PullAlways},
		{"digest reference", "ghcr.io/klinux/app@sha256:00000000000000000000000000000000000000000000000000000000deadbeef", corev1.PullIfNotPresent, true, corev1.PullAlways},
		// Registry with port handled correctly (a naive last-`:` rule would
		// read "5000" as the tag for the first case).
		{"localhost registry port, no tag", "localhost:5000/foo", corev1.PullAlways, true, corev1.PullAlways},
		{"localhost registry port + moving tag", "localhost:5000/foo:v1", corev1.PullAlways, true, corev1.PullAlways},
		{"localhost registry port + pinned tag", "localhost:5000/foo:1.2.3", corev1.PullIfNotPresent, true, corev1.PullAlways},
		// Empty image → BuildPodSpec resolves to DefaultImage
		// (alpine:3.19 by default) — pinned tag → IfNotPresent.
		// The empty-string Always branch in imagePullPolicyFor only
		// kicks in when an operator override of DefaultImage is itself
		// empty, which the constructor short-circuits at boot.
		{"empty image (default fallback alpine:3.19)", "", corev1.PullIfNotPresent, true, corev1.PullAlways},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			eng, _ := newFakeEngine(t, engine.KubernetesConfig{})
			pod := eng.BuildPodSpec(engine.ScriptSpec{Image: tc.image, Script: "true"})
			if got := pod.Spec.Containers[0].ImagePullPolicy; got != tc.want {
				t.Errorf("image=%q heuristic policy = %q, want %q", tc.image, got, tc.want)
			}

			// With ForceImagePullAlways the operator overrides every
			// per-tag decision uniformly.
			engForce, _ := newFakeEngine(t, engine.KubernetesConfig{ForceImagePullAlways: true})
			podForce := engForce.BuildPodSpec(engine.ScriptSpec{Image: tc.image, Script: "true"})
			if got := podForce.Spec.Containers[0].ImagePullPolicy; got != tc.wantForce {
				t.Errorf("image=%q force-always policy = %q, want %q", tc.image, got, tc.wantForce)
			}
		})
	}
}
