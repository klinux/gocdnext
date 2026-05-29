package engine_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/gocdnext/gocdnext/agent/internal/engine"
)

// assignPodIPAsync simulates the kubelet pumping a podIP into status
// after a small delay. Returns once the IP is set so the caller can
// chain the next stage of the test without sleeping.
func assignPodIPAsync(t *testing.T, cli *fake.Clientset, ns, name, ip string, after time.Duration) {
	t.Helper()
	go func() {
		time.Sleep(after)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		pod, err := cli.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Errorf("assignPodIPAsync get %s: %v", name, err)
			return
		}
		pod.Status.PodIP = ip
		if _, err := cli.CoreV1().Pods(ns).UpdateStatus(ctx, pod, metav1.UpdateOptions{}); err != nil {
			t.Errorf("assignPodIPAsync update %s: %v", name, err)
		}
	}()
}

func TestEnsureServices_NoopWhenEmpty(t *testing.T) {
	k, _ := newFakeEngine(t, engine.KubernetesConfig{DefaultImage: "alpine:3.19"})
	wireup, err := k.EnsureServices(context.Background(), nil, "job-1", nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(wireup.HostAliases) != 0 {
		t.Errorf("hostAliases on noop = %v", wireup.HostAliases)
	}
	if wireup.Cleanup == nil {
		t.Fatal("noop wireup missing Cleanup")
	}
	// Cleanup on noop wireup must not panic.
	wireup.Cleanup()
}

func TestEnsureServices_RejectsEmptyJobID(t *testing.T) {
	k, _ := newFakeEngine(t, engine.KubernetesConfig{DefaultImage: "alpine:3.19"})
	_, err := k.EnsureServices(context.Background(),
		[]engine.ServiceSpec{{Name: "postgres", Image: "postgres:16"}},
		"", nil)
	if err == nil || !strings.Contains(err.Error(), "non-empty jobID") {
		t.Fatalf("expected non-empty jobID error, got %v", err)
	}
}

func TestEnsureServices_RejectsBadServiceName(t *testing.T) {
	k, _ := newFakeEngine(t, engine.KubernetesConfig{DefaultImage: "alpine:3.19"})
	cases := []string{
		"BadCase",          // uppercase
		"-leading-dash",    // can't start with dash
		"trailing-dash-",   // can't end with dash
		"with.dot",         // dot disallowed (pod name would still validate but DNS-resolution semantics get squishy)
		"name with spaces", // obvious injection target
		strings.Repeat("a", 33),
	}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := k.EnsureServices(context.Background(),
				[]engine.ServiceSpec{{Name: name, Image: "postgres:16"}},
				"job-1", nil)
			if err == nil {
				t.Fatalf("expected error for name %q", name)
			}
			if !strings.Contains(err.Error(), "invalid") {
				t.Errorf("error should call out invalid name: %v", err)
			}
		})
	}
}

func TestEnsureServices_RejectsDuplicateServiceName(t *testing.T) {
	k, _ := newFakeEngine(t, engine.KubernetesConfig{DefaultImage: "alpine:3.19"})
	_, err := k.EnsureServices(context.Background(), []engine.ServiceSpec{
		{Name: "postgres", Image: "postgres:16"},
		{Name: "postgres", Image: "postgres:15"},
	}, "job-1", nil)
	if err == nil || !strings.Contains(err.Error(), "declared twice") {
		t.Fatalf("expected duplicate-name error, got %v", err)
	}
}

func TestEnsureServices_RejectsEmptyImage(t *testing.T) {
	k, _ := newFakeEngine(t, engine.KubernetesConfig{DefaultImage: "alpine:3.19"})
	_, err := k.EnsureServices(context.Background(),
		[]engine.ServiceSpec{{Name: "postgres", Image: ""}},
		"job-1", nil)
	if err == nil || !strings.Contains(err.Error(), "empty image") {
		t.Fatalf("expected empty-image error, got %v", err)
	}
}

func TestEnsureServices_CreatesPodPerServiceAndReturnsHostAliases(t *testing.T) {
	k, cli := newFakeEngine(t, engine.KubernetesConfig{
		DefaultImage:   "alpine:3.19",
		PollInterval:   2 * time.Millisecond,
		StartupTimeout: time.Second,
	})

	// Pre-arm: simulate kubelet assigning podIPs after a short delay.
	// Pod names match the engine's deterministic scheme:
	// gocdnext-svc-<jobshort>-<svcname>. jobID is 12 chars w/o dashes
	// so shortDockerID is the identity here — keeps the expected
	// pod name straightforward to assert on.
	assignPodIPAsync(t, cli, "gocdnext-tests", "gocdnext-svc-jobxyz12abcd-postgres", "10.0.0.10", 5*time.Millisecond)
	assignPodIPAsync(t, cli, "gocdnext-tests", "gocdnext-svc-jobxyz12abcd-redis", "10.0.0.11", 5*time.Millisecond)

	var logs []string
	var mu sync.Mutex
	log := func(stream, text string) {
		mu.Lock()
		defer mu.Unlock()
		logs = append(logs, stream+":"+text)
	}

	wireup, err := k.EnsureServices(context.Background(),
		[]engine.ServiceSpec{
			{Name: "postgres", Image: "postgres:16", Env: map[string]string{"POSTGRES_PASSWORD": "x"}},
			{Name: "redis", Image: "redis:7"},
		},
		"jobxyz12abcd", log)
	if err != nil {
		t.Fatalf("EnsureServices: %v", err)
	}

	if len(wireup.HostAliases) != 2 {
		t.Fatalf("hostAliases = %+v, want 2", wireup.HostAliases)
	}
	// Order must match declaration order — runner relies on this
	// implicitly when threading into ScriptSpec, and downstream YAML
	// authors expect the first service they listed to map to the
	// first alias they see.
	if wireup.HostAliases[0].Hostnames[0] != "postgres" {
		t.Errorf("first alias = %q, want postgres", wireup.HostAliases[0].Hostnames[0])
	}
	if wireup.HostAliases[0].IP != "10.0.0.10" {
		t.Errorf("postgres IP = %q", wireup.HostAliases[0].IP)
	}
	if wireup.HostAliases[1].IP != "10.0.0.11" {
		t.Errorf("redis IP = %q", wireup.HostAliases[1].IP)
	}

	// Service log line must be present in stdout — operator gets a
	// visible breadcrumb that the service phase actually ran.
	mu.Lock()
	defer mu.Unlock()
	found := false
	for _, l := range logs {
		if strings.Contains(l, "starting service postgres") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected starting-service log line, got %v", logs)
	}

	// Pods must actually exist in the fake clientset with the right
	// labels so an operator can grep them.
	pods, err := cli.CoreV1().Pods("gocdnext-tests").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list pods: %v", err)
	}
	if len(pods.Items) != 2 {
		t.Fatalf("created %d pods, want 2", len(pods.Items))
	}
	for _, pod := range pods.Items {
		if pod.Labels["gocdnext.io/service"] == "" {
			t.Errorf("pod %s missing gocdnext.io/service label", pod.Name)
		}
		if pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
			t.Errorf("pod %s restart policy = %v, want Never", pod.Name, pod.Spec.RestartPolicy)
		}
		c := pod.Spec.Containers[0]
		// Critical: svc.Command must land in Container.Args, NOT
		// Container.Command. Postgres + similar images carry an
		// ENTRYPOINT (docker-entrypoint.sh) that interprets `-c
		// fsync=off`-style args; setting Container.Command would
		// shadow the entrypoint and runc would fail with
		// "exec: -c: executable file not found".
		if len(c.Command) != 0 {
			t.Errorf("pod %s sets Container.Command=%v — must be empty so image ENTRYPOINT runs", pod.Name, c.Command)
		}
	}

	// Cleanup must delete every pod it brought up. Important: even
	// after caller's ctx might be canceled, the cleanup needs to
	// reach the API — services_kubernetes.go uses context.Background
	// inside the cleanup closure specifically for this.
	wireup.Cleanup()
	pods, err = cli.CoreV1().Pods("gocdnext-tests").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list pods after cleanup: %v", err)
	}
	if len(pods.Items) != 0 {
		var names []string
		for _, p := range pods.Items {
			names = append(names, p.Name)
		}
		t.Errorf("pods remaining after cleanup: %v", names)
	}
}

// TestEnsureServices_CommandLandsInArgsNotCommand is the regression
// cover for v0.4.23 → v0.4.24: pipelines declaring
//
//	services:
//	  - name: postgres
//	    image: postgres:16-alpine
//	    command: ["-c", "fsync=off"]
//
// previously made the engine populate Container.Command = ["-c",
// "fsync=off"], which shadowed the image's ENTRYPOINT
// (docker-entrypoint.sh) and failed at containerd-create time with
// `exec: "-c": executable file not found`. Container.Args is the
// correct slot — it appends to the image's ENTRYPOINT, matching the
// docker engine's `docker run postgres -c fsync=off` semantics.
func TestEnsureServices_CommandLandsInArgsNotCommand(t *testing.T) {
	k, cli := newFakeEngine(t, engine.KubernetesConfig{
		DefaultImage:   "alpine:3.19",
		PollInterval:   2 * time.Millisecond,
		StartupTimeout: time.Second,
	})

	assignPodIPAsync(t, cli, "gocdnext-tests", "gocdnext-svc-jobwithcmd1-postgres", "10.0.0.30", 5*time.Millisecond)

	_, err := k.EnsureServices(context.Background(),
		[]engine.ServiceSpec{
			{
				Name:    "postgres",
				Image:   "postgres:16-alpine",
				Command: []string{"-c", "fsync=off"},
			},
		},
		"jobwithcmd1", nil)
	if err != nil {
		t.Fatalf("EnsureServices: %v", err)
	}

	pod, err := cli.CoreV1().Pods("gocdnext-tests").Get(context.Background(), "gocdnext-svc-jobwithcmd1-postgres", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pod: %v", err)
	}
	c := pod.Spec.Containers[0]
	if len(c.Command) != 0 {
		t.Errorf("Container.Command = %v, want empty (image ENTRYPOINT must run)", c.Command)
	}
	if len(c.Args) != 2 || c.Args[0] != "-c" || c.Args[1] != "fsync=off" {
		t.Errorf("Container.Args = %v, want [-c fsync=off]", c.Args)
	}
}

func TestEnsureServices_CleanupRunsAfterCallerCtxCanceled(t *testing.T) {
	k, cli := newFakeEngine(t, engine.KubernetesConfig{
		DefaultImage:   "alpine:3.19",
		PollInterval:   2 * time.Millisecond,
		StartupTimeout: time.Second,
	})

	assignPodIPAsync(t, cli, "gocdnext-tests", "gocdnext-svc-jobcancel001-postgres", "10.0.0.20", 5*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	wireup, err := k.EnsureServices(ctx, []engine.ServiceSpec{
		{Name: "postgres", Image: "postgres:16"},
	}, "jobcancel001", nil)
	if err != nil {
		t.Fatalf("EnsureServices: %v", err)
	}

	// Simulate the runner exiting and canceling the job ctx before
	// invoking cleanup. Real flow: defer servicesPhase.cleanup() at
	// runner.go runs AFTER ctx cancel in normal cancellation paths.
	cancel()

	wireup.Cleanup()
	pods, err := cli.CoreV1().Pods("gocdnext-tests").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list pods: %v", err)
	}
	if len(pods.Items) != 0 {
		t.Errorf("cleanup did not reach API after ctx cancel: %d pods left", len(pods.Items))
	}
}

func TestEnsureServices_TimeoutCleansUpStartedPods(t *testing.T) {
	k, cli := newFakeEngine(t, engine.KubernetesConfig{
		DefaultImage:   "alpine:3.19",
		PollInterval:   2 * time.Millisecond,
		StartupTimeout: 50 * time.Millisecond,
	})

	// NEVER assign a podIP — forces waitForPodIP to time out so we
	// can verify the cleanup-on-error path tears the created pod down.
	_, err := k.EnsureServices(context.Background(),
		[]engine.ServiceSpec{{Name: "postgres", Image: "postgres:16"}},
		"jobstuck00001", nil)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "wait service podIP") {
		t.Errorf("error should call out the wait-podIP failure: %v", err)
	}

	pods, listErr := cli.CoreV1().Pods("gocdnext-tests").List(context.Background(), metav1.ListOptions{})
	if listErr != nil {
		t.Fatalf("list pods: %v", listErr)
	}
	if len(pods.Items) != 0 {
		var names []string
		for _, p := range pods.Items {
			names = append(names, p.Name)
		}
		t.Errorf("timeout did not clean up created pods: %v", names)
	}
}
