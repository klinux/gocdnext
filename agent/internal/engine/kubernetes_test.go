package engine_test

import (
	"context"
	"errors"
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
func advancePod(t *testing.T, cli *fake.Clientset, ns, name string, phase corev1.PodPhase, exitCode int32) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	pod, err := cli.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	pod.Status.Phase = phase
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: "task",
		State: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{ExitCode: exitCode},
		},
	}}
	if _, err := cli.CoreV1().Pods(ns).UpdateStatus(ctx, pod, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("updateStatus: %v", err)
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
	if len(c.Command) != 3 || c.Command[0] != "sh" || c.Command[1] != "-c" {
		t.Errorf("command = %+v", c.Command)
	}
	if c.Command[2] != "echo hi" {
		t.Errorf("script = %q", c.Command[2])
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

func TestKubernetes_Name(t *testing.T) {
	k, _ := newFakeEngine(t, engine.KubernetesConfig{})
	if n := k.Name(); n != "kubernetes" {
		t.Errorf("Name = %q", n)
	}
}

func TestNewKubernetes_RejectsMissingNamespace(t *testing.T) {
	_, err := engine.NewKubernetes(engine.KubernetesConfig{})
	if err == nil {
		t.Error("expected error for missing namespace")
	}
}
