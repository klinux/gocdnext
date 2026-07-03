package engine_test

import (
	"context"
	"math"
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
	wireup, err := k.EnsureServices(context.Background(), nil, "run-1", "job-1", 0, nil, nil)
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

func TestEnsureServices_RejectsEmptyRunID(t *testing.T) {
	// runID is now the load-bearing identity for run-scoped pod
	// naming (gocdnext-svc-<runShort>-<svc>); jobID stays for label
	// observability only.
	k, _ := newFakeEngine(t, engine.KubernetesConfig{DefaultImage: "alpine:3.19"})
	_, err := k.EnsureServices(context.Background(),
		[]engine.ServiceSpec{{Name: "postgres", Image: "postgres:16"}},
		"", "job-1", 0, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "non-empty runID") {
		t.Fatalf("expected non-empty runID error, got %v", err)
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
				"run-1", "job-1", 0, nil, nil)
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
	}, "run-1", "job-1", 0, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "declared twice") {
		t.Fatalf("expected duplicate-name error, got %v", err)
	}
}

func TestEnsureServices_RejectsEmptyImage(t *testing.T) {
	k, _ := newFakeEngine(t, engine.KubernetesConfig{DefaultImage: "alpine:3.19"})
	_, err := k.EnsureServices(context.Background(),
		[]engine.ServiceSpec{{Name: "postgres", Image: ""}},
		"run-1", "job-1", 0, nil, nil)
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
	// gocdnext-svc-<runshort>-<svcname>. The runID is 12 chars w/o
	// dashes so shortDockerID is the identity here — keeps the
	// expected pod name straightforward to assert on. (Run-scoped
	// naming is the round-1 fix: every job of the same run resolves
	// to the same pod; jobID stays in labels for observability.)
	assignPodIPAsync(t, cli, "gocdnext-tests", "gocdnext-svc-runxyz12abcd-g0-postgres", "10.0.0.10", 5*time.Millisecond)
	assignPodIPAsync(t, cli, "gocdnext-tests", "gocdnext-svc-runxyz12abcd-g0-redis", "10.0.0.11", 5*time.Millisecond)

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
		"runxyz12abcd", "jobxyz12abcd", 0, log, nil)
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

	// wireup.Cleanup is a NO-OP in the run-scoped model — per-job
	// teardown would kill services other jobs of the same run still
	// need. Run-terminal teardown happens via CleanupRunServices
	// (label-selector delete), exercised separately below.
	wireup.Cleanup()
	pods, err = cli.CoreV1().Pods("gocdnext-tests").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list pods after Cleanup: %v", err)
	}
	if len(pods.Items) != 2 {
		t.Errorf("Cleanup should be a no-op; pods=%d, want 2", len(pods.Items))
	}

	// Now exercise the run-terminal teardown: CleanupRunServices does
	// the label-selector delete by runID. This is the path the server
	// fires via the CleanupRunServices ServerMessage on run terminal.
	deleted, err := k.CleanupRunServices(context.Background(), "runxyz12abcd", math.MaxInt64, nil)
	if err != nil {
		t.Fatalf("CleanupRunServices: %v", err)
	}
	if deleted != 2 {
		t.Errorf("CleanupRunServices deleted %d, want 2", deleted)
	}
	pods, err = cli.CoreV1().Pods("gocdnext-tests").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list pods after CleanupRunServices: %v", err)
	}
	if len(pods.Items) != 0 {
		var names []string
		for _, p := range pods.Items {
			names = append(names, p.Name)
		}
		t.Errorf("pods remaining after CleanupRunServices: %v", names)
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
func TestEnsureServices_AppliesAgentNodeSelectorAndTolerations(t *testing.T) {
	// Services share the agent-level scheduling baseline with task
	// pods. Without this, a cluster with NoSchedule taints isolating
	// CI would schedule the task pod but the service pod (postgres,
	// redis, etc.) would land Pending — breaking pipelines that
	// declare services with no obvious cause.
	k, cli := newFakeEngine(t, engine.KubernetesConfig{
		DefaultImage:   "alpine:3.19",
		PollInterval:   2 * time.Millisecond,
		StartupTimeout: time.Second,
		NodeSelector:   map[string]string{"pool": "ci"},
		Tolerations: []corev1.Toleration{
			{Key: "node.kubernetes.io/unschedulable", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
			{Key: "ci-only", Operator: corev1.TolerationOpEqual, Value: "true", Effect: corev1.TaintEffectNoSchedule},
		},
	})

	assignPodIPAsync(t, cli, "gocdnext-tests", "gocdnext-svc-runabcd123ab-g0-postgres", "10.0.0.10", 5*time.Millisecond)

	log := func(stream, text string) {}
	if _, err := k.EnsureServices(context.Background(),
		[]engine.ServiceSpec{{Name: "postgres", Image: "postgres:16"}},
		"runabcd123ab", "jobabcd123456", 0, log, nil); err != nil {
		t.Fatalf("EnsureServices: %v", err)
	}

	pod, err := cli.CoreV1().Pods("gocdnext-tests").Get(
		context.Background(), "gocdnext-svc-runabcd123ab-g0-postgres", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get service pod: %v", err)
	}
	if pod.Spec.NodeSelector["pool"] != "ci" {
		t.Errorf("service pod NodeSelector missing pool=ci: %+v", pod.Spec.NodeSelector)
	}
	if len(pod.Spec.Tolerations) != 2 {
		t.Fatalf("service pod Tolerations len = %d, want 2; %+v",
			len(pod.Spec.Tolerations), pod.Spec.Tolerations)
	}
	if pod.Spec.Tolerations[0].Key != "node.kubernetes.io/unschedulable" ||
		pod.Spec.Tolerations[1].Key != "ci-only" {
		t.Errorf("service pod Tolerations wrong: %+v", pod.Spec.Tolerations)
	}
}

func TestEnsureServices_CommandLandsInArgsNotCommand(t *testing.T) {
	k, cli := newFakeEngine(t, engine.KubernetesConfig{
		DefaultImage:   "alpine:3.19",
		PollInterval:   2 * time.Millisecond,
		StartupTimeout: time.Second,
	})

	assignPodIPAsync(t, cli, "gocdnext-tests", "gocdnext-svc-runwithcmd1-g0-postgres", "10.0.0.30", 5*time.Millisecond)

	_, err := k.EnsureServices(context.Background(),
		[]engine.ServiceSpec{
			{
				Name:    "postgres",
				Image:   "postgres:16-alpine",
				Command: []string{"-c", "fsync=off"},
			},
		},
		"runwithcmd1", "jobwithcmd1", 0, nil, nil)
	if err != nil {
		t.Fatalf("EnsureServices: %v", err)
	}

	pod, err := cli.CoreV1().Pods("gocdnext-tests").Get(context.Background(), "gocdnext-svc-runwithcmd1-g0-postgres", metav1.GetOptions{})
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

// TestEnsureServices_CleanupIsNoop locks in the run-scoped semantics:
// the returned Cleanup is intentionally a no-op so a single job's
// teardown doesn't kill services other jobs of the same run still
// need. Run-terminal teardown is the server's job via the new
// CleanupRunServices RPC. Pre-refactor, the wireup's Cleanup
// force-deleted the per-job pod — the test below was the cover.
// The assertion is inverted: pods MUST survive Cleanup.
func TestEnsureServices_CleanupIsNoop(t *testing.T) {
	k, cli := newFakeEngine(t, engine.KubernetesConfig{
		DefaultImage:   "alpine:3.19",
		PollInterval:   2 * time.Millisecond,
		StartupTimeout: time.Second,
	})

	assignPodIPAsync(t, cli, "gocdnext-tests", "gocdnext-svc-runnoopclean-g0-postgres", "10.0.0.20", 5*time.Millisecond)

	wireup, err := k.EnsureServices(context.Background(), []engine.ServiceSpec{
		{Name: "postgres", Image: "postgres:16"},
	}, "runnoopclean", "jobnoopclean", 0, nil, nil)
	if err != nil {
		t.Fatalf("EnsureServices: %v", err)
	}

	wireup.Cleanup()
	pods, err := cli.CoreV1().Pods("gocdnext-tests").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list pods: %v", err)
	}
	if len(pods.Items) != 1 {
		t.Errorf("Cleanup is supposed to be a no-op; pods=%d, want 1 (sibling jobs may still need it)", len(pods.Items))
	}
}

// TestEnsureServices_TimeoutLeavesPodForRunCleanup — when waitForPodIP
// times out, EnsureServices now LEAVES the (broken) pod up rather
// than tearing it down per-job. Rationale: another job of the same
// run may be about to retry; OR the run will fail and the
// CleanupRunServices on run terminal will sweep the corpse via the
// gocdnext.io/run-id label selector. Tests the latter path
// end-to-end.
func TestEnsureServices_TimeoutLeavesPodForRunCleanup(t *testing.T) {
	k, cli := newFakeEngine(t, engine.KubernetesConfig{
		DefaultImage:   "alpine:3.19",
		PollInterval:   2 * time.Millisecond,
		StartupTimeout: 50 * time.Millisecond,
	})

	// NEVER assign a podIP — forces waitForPodIP to time out.
	_, err := k.EnsureServices(context.Background(),
		[]engine.ServiceSpec{{Name: "postgres", Image: "postgres:16"}},
		"runstuck00001", "jobstuck00001", 0, nil, nil)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "wait service podIP") {
		t.Errorf("error should call out the wait-podIP failure: %v", err)
	}

	// Pod stays. CleanupRunServices then sweeps it via label.
	pods, _ := cli.CoreV1().Pods("gocdnext-tests").List(context.Background(), metav1.ListOptions{})
	if len(pods.Items) != 1 {
		t.Fatalf("pod count after timeout = %d, want 1 (left for run-terminal sweep)", len(pods.Items))
	}

	deleted, err := k.CleanupRunServices(context.Background(), "runstuck00001", math.MaxInt64, nil)
	if err != nil {
		t.Fatalf("CleanupRunServices: %v", err)
	}
	if deleted != 1 {
		t.Errorf("CleanupRunServices deleted %d, want 1", deleted)
	}
}

// TestEnsureServices_RefusesUnlabelledExistingPod — defends against
// silent adoption of an unrelated pod that just happens to share
// our deterministic name (12-hex collision / leftover from an old
// gocdnext version / operator-deployed pod). EnsureServices Gets
// the existing pod and refuses to reuse it unless the full label
// tuple matches.
func TestEnsureServices_RefusesUnlabelledExistingPod(t *testing.T) {
	k, cli := newFakeEngine(t, engine.KubernetesConfig{
		DefaultImage:   "alpine:3.19",
		PollInterval:   2 * time.Millisecond,
		StartupTimeout: time.Second,
	})

	// Plant a pod with our exact NAME but missing the
	// managed-by/component labels — looks ours by name, isn't ours
	// by ownership.
	const runID = "runalien0001"
	podName := "gocdnext-svc-" + runID + "-g0-postgres"
	_, err := cli.CoreV1().Pods("gocdnext-tests").Create(context.Background(), &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: "gocdnext-tests",
			Labels: map[string]string{
				// note: missing managed-by + component + service
				"gocdnext.io/run-id": runID,
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "x", Image: "alpine:3.19"}},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("seed alien pod: %v", err)
	}

	_, err = k.EnsureServices(context.Background(),
		[]engine.ServiceSpec{{Name: "postgres", Image: "postgres:16"}},
		runID, "jobalien", 0, nil, nil)
	if err == nil {
		t.Fatal("expected error refusing to reuse unlabelled pod")
	}
	if !strings.Contains(err.Error(), "refusing to reuse pod") {
		t.Errorf("error should call out refuse-to-reuse: %v", err)
	}
	if !strings.Contains(err.Error(), "managed-by") &&
		!strings.Contains(err.Error(), "component") {
		t.Errorf("error should name which label was missing: %v", err)
	}
}

// TestCleanupRunServices_RequiresFullLabelTuple guards the
// MED/SEC concern: a bare gocdnext.io/run-id filter could match
// non-service pods that happen to carry the label. The label
// selector must include managed-by + component too so the
// deletion is bounded to our own resources.
func TestCleanupRunServices_RequiresFullLabelTuple(t *testing.T) {
	k, cli := newFakeEngine(t, engine.KubernetesConfig{
		DefaultImage: "alpine:3.19",
	})

	const runID = "runtight00001"

	// A real gocdnext service pod (full label set).
	_, _ = cli.CoreV1().Pods("gocdnext-tests").Create(context.Background(), &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "gocdnext-svc-" + runID + "-postgres",
			Namespace: "gocdnext-tests",
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "gocdnext-agent",
				"app.kubernetes.io/component":  "service",
				"gocdnext.io/service":          "postgres",
				"gocdnext.io/run-id":           runID,
			},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "x", Image: "alpine"}}},
	}, metav1.CreateOptions{})

	// An imposter pod with the run-id label but NOT the
	// managed-by/component tuple. Operator-deployed, must survive.
	_, _ = cli.CoreV1().Pods("gocdnext-tests").Create(context.Background(), &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "user-pod-with-runid",
			Namespace: "gocdnext-tests",
			Labels:    map[string]string{"gocdnext.io/run-id": runID},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "x", Image: "alpine"}}},
	}, metav1.CreateOptions{})

	deleted, err := k.CleanupRunServices(context.Background(), runID, math.MaxInt64, nil)
	if err != nil {
		t.Fatalf("CleanupRunServices: %v", err)
	}
	if deleted != 1 {
		t.Errorf("deleted=%d, want 1 (only the labelled service pod)", deleted)
	}

	// Imposter must still exist.
	_, err = cli.CoreV1().Pods("gocdnext-tests").Get(context.Background(), "user-pod-with-runid", metav1.GetOptions{})
	if err != nil {
		t.Errorf("imposter pod swept by cleanup — selector too loose: %v", err)
	}
}

// TestCleanupRunServices_GenerationFilter — a supersede/terminal cleanup carries the
// generation it decided to tear down (#97); the label-selector delete skips pods whose
// generation label is HIGHER, so a run revived + re-dispatched under the same run_id
// (dispatched at generation+1 with a fresh name+label) keeps its new pods. Pods at or
// below the ceiling still go, and an unlabeled (pre-#97) pod reads as generation 0.
func TestCleanupRunServices_GenerationFilter(t *testing.T) {
	k, cli := newFakeEngine(t, engine.KubernetesConfig{DefaultImage: "alpine:3.19"})
	const runID = "rungen0000001"

	mkPod := func(name, generation string) {
		labels := map[string]string{
			"app.kubernetes.io/managed-by": "gocdnext-agent",
			"app.kubernetes.io/component":  "service",
			"gocdnext.io/service":          "postgres",
			"gocdnext.io/run-id":           runID,
		}
		if generation != "" {
			labels["gocdnext.io/service-generation"] = generation
		}
		_, _ = cli.CoreV1().Pods("gocdnext-tests").Create(context.Background(), &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: "gocdnext-tests",
				Labels:    labels,
			},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "x", Image: "alpine"}}},
		}, metav1.CreateOptions{})
	}
	mkPod("unlabeled-pod", "") // pre-#97 → reads as gen 0 → deleted (<= 0)
	mkPod("gen0-pod", "0")     // superseded generation → deleted
	mkPod("gen1-pod", "1")     // revived generation → must survive

	deleted, err := k.CleanupRunServices(context.Background(), runID, 0, nil) // max_generation = 0
	if err != nil {
		t.Fatalf("CleanupRunServices: %v", err)
	}
	if deleted != 2 {
		t.Fatalf("deleted=%d, want 2 (the unlabeled + gen0 pods)", deleted)
	}
	for _, gone := range []string{"unlabeled-pod", "gen0-pod"} {
		if _, err := cli.CoreV1().Pods("gocdnext-tests").Get(context.Background(), gone, metav1.GetOptions{}); err == nil {
			t.Errorf("pod %s should have been deleted (generation <= 0)", gone)
		}
	}
	if _, err := cli.CoreV1().Pods("gocdnext-tests").Get(context.Background(), "gen1-pod", metav1.GetOptions{}); err != nil {
		t.Errorf("gen1-pod deleted — a revived run's higher-generation pods must survive: %v", err)
	}
}

// TestEnsureServices_AlreadyExistsReusesPod — the round-1 fix's
// hot path: when two jobs of the same run race EnsureServices,
// only the first creates the pod; the second gets AlreadyExists
// from Create + Get returns the same PodIP. Both jobs end up
// with HostAliases pointing at the same /etc/hosts entry, so the
// `postgres:5432` resolution is shared.
func TestEnsureServices_AlreadyExistsReusesPod(t *testing.T) {
	k, cli := newFakeEngine(t, engine.KubernetesConfig{
		DefaultImage:   "alpine:3.19",
		PollInterval:   2 * time.Millisecond,
		StartupTimeout: time.Second,
	})

	const runID = "runshared001"
	// Generation goes in the name (#97): same run+generation → same name → reuse.
	podName := "gocdnext-svc-" + runID + "-g0-postgres"
	assignPodIPAsync(t, cli, "gocdnext-tests", podName, "10.0.0.50", 5*time.Millisecond)

	// Job 1 creates the pod.
	w1, err := k.EnsureServices(context.Background(),
		[]engine.ServiceSpec{{Name: "postgres", Image: "postgres:16"}},
		runID, "jobone", 0, nil, nil)
	if err != nil {
		t.Fatalf("first EnsureServices: %v", err)
	}
	if len(w1.HostAliases) != 1 || w1.HostAliases[0].IP != "10.0.0.50" {
		t.Fatalf("first hostAliases = %+v", w1.HostAliases)
	}

	// Job 2 of the same run+generation — Create returns AlreadyExists, engine
	// recovers via Get + waitForPodIP.
	w2, err := k.EnsureServices(context.Background(),
		[]engine.ServiceSpec{{Name: "postgres", Image: "postgres:16"}},
		runID, "jobtwo", 0, nil, nil)
	if err != nil {
		t.Fatalf("second EnsureServices (should reuse): %v", err)
	}
	if len(w2.HostAliases) != 1 || w2.HostAliases[0].IP != "10.0.0.50" {
		t.Fatalf("second hostAliases mismatch: %+v", w2.HostAliases)
	}

	// Exactly ONE pod in the cluster — the second EnsureServices
	// did NOT create a duplicate.
	pods, _ := cli.CoreV1().Pods("gocdnext-tests").List(context.Background(), metav1.ListOptions{})
	if len(pods.Items) != 1 {
		t.Errorf("expected 1 shared pod, got %d", len(pods.Items))
	}
}

// TestEnsureServices_NewGenerationDoesNotReusePriorPod is the crux of the #97 HIGH
// fix: a run revived under the same run_id (higher generation) must build a FRESH
// service pod, NOT reuse the superseded generation's pod via the AlreadyExists path —
// otherwise a still-pending supersede cleanup (delete <= old gen) would tear down the
// pod the revived run just adopted. Generation-0 and generation-1 pods coexist under
// distinct names, so the cleanup targets only the old generation.
func TestEnsureServices_NewGenerationDoesNotReusePriorPod(t *testing.T) {
	k, cli := newFakeEngine(t, engine.KubernetesConfig{
		DefaultImage:   "alpine:3.19",
		PollInterval:   2 * time.Millisecond,
		StartupTimeout: time.Second,
	})

	const runID = "runrevive001"
	gen0Pod := "gocdnext-svc-" + runID + "-g0-postgres"
	gen1Pod := "gocdnext-svc-" + runID + "-g1-postgres"
	// gen-1's pod is only created by the SECOND EnsureServices call, which runs after
	// the first returns — so its IP assignment must fire later than gen-0's, or the
	// async Get races pod creation.
	assignPodIPAsync(t, cli, "gocdnext-tests", gen0Pod, "10.0.0.60", 5*time.Millisecond)
	assignPodIPAsync(t, cli, "gocdnext-tests", gen1Pod, "10.0.0.61", 40*time.Millisecond)

	// Generation 0 (superseded run).
	if _, err := k.EnsureServices(context.Background(),
		[]engine.ServiceSpec{{Name: "postgres", Image: "postgres:16"}},
		runID, "jobone", 0, nil, nil); err != nil {
		t.Fatalf("gen-0 EnsureServices: %v", err)
	}

	// Generation 1 (revived run, same run_id) — must NOT reuse the gen-0 pod.
	w, err := k.EnsureServices(context.Background(),
		[]engine.ServiceSpec{{Name: "postgres", Image: "postgres:16"}},
		runID, "jobtwo", 1, nil, nil)
	if err != nil {
		t.Fatalf("gen-1 EnsureServices: %v", err)
	}
	if len(w.HostAliases) != 1 || w.HostAliases[0].IP != "10.0.0.61" {
		t.Fatalf("gen-1 hostAliases = %+v, want the fresh gen-1 pod IP 10.0.0.61", w.HostAliases)
	}

	// Two distinct pods now — one per generation — so a stale cleanup carrying the
	// old generation can delete gen-0 without touching gen-1.
	pods, _ := cli.CoreV1().Pods("gocdnext-tests").List(context.Background(), metav1.ListOptions{})
	if len(pods.Items) != 2 {
		t.Fatalf("expected 2 pods (one per generation), got %d", len(pods.Items))
	}
	got, err := cli.CoreV1().Pods("gocdnext-tests").Get(context.Background(), gen1Pod, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get gen-1 pod: %v", err)
	}
	if g := got.Labels["gocdnext.io/service-generation"]; g != "1" {
		t.Errorf("gen-1 pod generation label = %q, want \"1\"", g)
	}
}
