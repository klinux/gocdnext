package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/gocdnext/gocdnext/agent/internal/engine"
)

// This file is the Kubernetes counterpart of docker_envleak_test.go.
// The leak it guards (#309): job secrets used to reach the task
// container as plain `EnvVar.Value` entries, so anyone with `get pods`
// in the runners' namespace could read every pipeline secret with
// `kubectl get pod -o yaml` — a far cheaper RBAC grant than
// `get secrets`, and a read path neither the UI nor LogMasks covers.
//
// The invariant asserted here is deliberately blunt: serialise the
// whole Pod object and assert that no sensitive value appears ANYWHERE
// in the bytes. That survives refactors which move env around
// (annotations, args, a new sidecar) in a way per-field assertions
// would not.

const (
	leakToken = "ghp_abc123_should_never_appear_in_a_pod_spec"
	leakPEM   = "-----BEGIN COSIGN PRIVATE KEY-----\n" +
		"line1_secret_bytes_NEVER_IN_POD_SPEC\n" +
		"line2_more_secret\n" +
		"-----END COSIGN PRIVATE KEY-----\n"
	leakKubeconfig = "apiVersion: v1\nusers:\n- user:\n    token: eyJhbGciOiJSUzI1NiJ9.k8stoken\n"
	// A composed value: the YAML said
	// `DATABASE_URL: postgres://u:${{ PG_PASSWORD }}@db/app`, so the
	// var is not itself a declared secret but embeds one verbatim.
	leakPassword    = "pgp4ssw0rd"
	leakDatabaseURL = "postgres://u:" + leakPassword + "@db/app"
)

// leakEnv is the env a secret-bearing job actually ships with: the
// declared secret, a plugin setting resolved from that same secret
// (PLUGIN_* — a key the SERVER cannot name, which is why detection
// matches on values), a managed-cluster kubeconfig, a composed URL,
// and the plain vars that must stay readable for debugging.
func leakEnv() map[string]string {
	return map[string]string{
		"GHCR_TOKEN":         leakToken,
		"PLUGIN_PASSWORD":    leakToken,
		"COSIGN_PRIVATE_KEY": leakPEM,
		"PLUGIN_KUBECONFIG":  leakKubeconfig,
		"DATABASE_URL":       leakDatabaseURL,
		"CI_BRANCH":          "main",
		"PLUGIN_REGISTRY":    "ghcr.io",
	}
}

// leakMasks is what the scheduler puts in JobAssignment.log_masks for
// that job — note DATABASE_URL is absent: only PG_PASSWORD was
// declared, the composed URL must be caught by substring matching.
func leakMasks() []string {
	return []string{leakToken, leakPEM, leakKubeconfig, leakPassword}
}

var leakSecretValues = []string{leakToken, leakPEM, leakKubeconfig, leakPassword, leakDatabaseURL}

// assertNoSecretInPod serialises the Pod and fails on any sensitive
// substring. Reported without echoing the value (the error message
// would become the leak).
func assertNoSecretInPod(t *testing.T, pod *corev1.Pod) {
	t.Helper()
	blob, err := json.Marshal(pod)
	if err != nil {
		t.Fatalf("marshal pod: %v", err)
	}
	for i, v := range leakSecretValues {
		if strings.Contains(string(blob), v) {
			t.Errorf("sensitive value #%d appears in the serialised Pod — the #309 leak is back", i)
		}
	}
}

// assertPlainStillInline keeps the fix from degenerating into "move
// everything": CI_* vars and plugin inputs that are not secrets stay
// visible in the Pod spec, which is what operators read when they
// debug a job.
func assertPlainStillInline(t *testing.T, task *corev1.Container) {
	t.Helper()
	want := map[string]string{"CI_BRANCH": "main", "PLUGIN_REGISTRY": "ghcr.io"}
	for _, e := range task.Env {
		if w, ok := want[e.Name]; ok {
			if e.Value != w {
				t.Errorf("%s: value = %q, want %q inline", e.Name, e.Value, w)
			}
			if e.ValueFrom != nil {
				t.Errorf("%s: moved to a secretKeyRef, but it is not sensitive", e.Name)
			}
			delete(want, e.Name)
		}
	}
	for name := range want {
		t.Errorf("plain var %s disappeared from the task env", name)
	}
}

// assertSecretBacked checks every sensitive var resolves from
// secretName, and returns the Secret data keys the Pod references.
func assertSecretBacked(t *testing.T, task *corev1.Container, secretName string) map[string]bool {
	t.Helper()
	refs := map[string]bool{}
	for _, name := range []string{
		"GHCR_TOKEN", "PLUGIN_PASSWORD", "COSIGN_PRIVATE_KEY",
		"PLUGIN_KUBECONFIG", "DATABASE_URL",
	} {
		var found *corev1.EnvVar
		for i := range task.Env {
			if task.Env[i].Name == name {
				found = &task.Env[i]
				break
			}
		}
		if found == nil {
			t.Errorf("%s: missing from the task env entirely", name)
			continue
		}
		if found.Value != "" {
			t.Errorf("%s: carries an inline Value", name)
			continue
		}
		if found.ValueFrom == nil || found.ValueFrom.SecretKeyRef == nil {
			t.Errorf("%s: no secretKeyRef (%+v)", name, found.ValueFrom)
			continue
		}
		ref := found.ValueFrom.SecretKeyRef
		if ref.Name != secretName {
			t.Errorf("%s: secretKeyRef.name = %q, want %q", name, ref.Name, secretName)
		}
		if want := engine.SecretEnvKey(name); ref.Key != want {
			t.Errorf("%s: secretKeyRef.key = %q, want %q", name, ref.Key, want)
		}
		if ref.Optional != nil && *ref.Optional {
			t.Errorf("%s: optional ref — a missing key must fail the container loudly", name)
		}
		refs[ref.Key] = true
	}
	return refs
}

// stampPodUID makes the fake clientset behave like a real apiserver
// for the one field the ownerRef patch needs: Create assigns a UID.
// Without it every secret-owner patch fails with "pod must have a
// UID" (see TestCreateIsolatedJobPod_DeletesSecretOnOwnerPatchFailure,
// which relies on exactly that gap).
func stampPodUID(cli *fake.Clientset) {
	cli.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		pod, ok := action.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
		if ok && pod.UID == "" {
			pod.UID = types.UID("uid-" + pod.Name)
		}
		return false, nil, nil
	})
}

func taskContainer(t *testing.T, pod *corev1.Pod) *corev1.Container {
	t.Helper()
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == "task" {
			return &pod.Spec.Containers[i]
		}
	}
	t.Fatalf("no task container in pod %s", pod.Name)
	return nil
}

func TestBuildPodSpec_SharedMode_NeverInlinesSecretValues(t *testing.T) {
	k, _ := newFakeEngine(t, engine.KubernetesConfig{
		DefaultImage:       "alpine:3.19",
		WorkspaceMountPath: "/workspace",
	})
	pod := k.BuildPodSpec(engine.ScriptSpec{
		Script:       "echo hi",
		Env:          leakEnv(),
		SecretValues: leakMasks(),
	})

	assertNoSecretInPod(t, pod)
	task := taskContainer(t, pod)
	assertPlainStillInline(t, task)
	// Shared mode has no assignment Secret: RunScript creates one
	// named after the pod. The suffix is the contract between
	// BuildPodSpec and RunScript.
	assertSecretBacked(t, task, pod.Name+"-env")
}

func TestBuildPodSpec_SharedMode_NoSecretsLeavesSpecUnchanged(t *testing.T) {
	k, _ := newFakeEngine(t, engine.KubernetesConfig{DefaultImage: "alpine:3.19"})
	pod := k.BuildPodSpec(engine.ScriptSpec{
		Script: "echo hi",
		Env:    map[string]string{"CI_BRANCH": "main"},
	})
	for _, e := range pod.Spec.Containers[0].Env {
		if e.ValueFrom != nil {
			t.Errorf("%s: secretKeyRef on a job with no masks", e.Name)
		}
	}
}

func TestBuildIsolatedJobPodSpec_NeverInlinesSecretValues(t *testing.T) {
	k, _ := newFakeEngine(t, engine.KubernetesConfig{
		WorkspaceMode:      engine.WorkspaceModeIsolated,
		AgentImage:         "ghcr.io/klinux/gocdnext-agent:test",
		HousekeeperImage:   "alpine:3.19",
		DefaultImage:       "alpine:3.19",
		WorkspaceMountPath: "/workspace",
		WorkspaceSize:      "10Gi",
	})
	pod, err := k.BuildIsolatedJobPodSpec(engine.IsolatedJobSpec{
		RunID:                "run-A",
		JobID:                "job-B",
		Image:                "node:20",
		Script:               "pnpm test",
		WorkDir:              "/workspace",
		AssignmentSecretName: "gocdnext-job-test01-assignment",
		EnvSecretName:        "gocdnext-job-test01-env",
		Env:                  leakEnv(),
		SecretValues:         leakMasks(),
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	assertNoSecretInPod(t, pod)
	task := taskContainer(t, pod)
	assertPlainStillInline(t, task)
	// NOT the assignment Secret: the runner deletes that one as soon as
	// prep terminates, which is before the kubelet resolves the task
	// container's env. The env Secret outlives prep.
	assertSecretBacked(t, task, "gocdnext-job-test01-env")
}

// TestBuildIsolatedJobPodSpec_RefusesSecretEnvWithoutItsSecret: a
// secret-backed var with no Secret to resolve from would leave the pod
// in CreateContainerConfigError. Fail at build time with a message
// naming the cause instead — and never by falling back to an inline
// value, which is the leak itself.
func TestBuildIsolatedJobPodSpec_RefusesSecretEnvWithoutItsSecret(t *testing.T) {
	k, _ := newFakeEngine(t, engine.KubernetesConfig{
		WorkspaceMode:      engine.WorkspaceModeIsolated,
		AgentImage:         "ghcr.io/klinux/gocdnext-agent:test",
		HousekeeperImage:   "alpine:3.19",
		DefaultImage:       "alpine:3.19",
		WorkspaceMountPath: "/workspace",
		WorkspaceSize:      "10Gi",
	})
	_, err := k.BuildIsolatedJobPodSpec(engine.IsolatedJobSpec{
		RunID:                "run-A",
		JobID:                "job-B",
		Image:                "node:20",
		Script:               "pnpm test",
		AssignmentSecretName: "gocdnext-job-test01-assignment",
		// EnvSecretName deliberately empty.
		Env:          leakEnv(),
		SecretValues: leakMasks(),
	})
	if err == nil {
		t.Fatal("built a pod with secret-backed env and no env Secret to resolve it from")
	}
	if !strings.Contains(err.Error(), "EnvSecretName") {
		t.Errorf("error = %q, want it to name the missing field", err)
	}
}

// TestBuildIsolatedJobPodSpec_NoSecretsNeedsNoEnvSecret keeps the
// common case free of the new requirement: nothing sensitive, nothing
// to reference, no error.
func TestBuildIsolatedJobPodSpec_NoSecretsNeedsNoEnvSecret(t *testing.T) {
	k, _ := newFakeEngine(t, engine.KubernetesConfig{
		WorkspaceMode:      engine.WorkspaceModeIsolated,
		AgentImage:         "ghcr.io/klinux/gocdnext-agent:test",
		HousekeeperImage:   "alpine:3.19",
		DefaultImage:       "alpine:3.19",
		WorkspaceMountPath: "/workspace",
		WorkspaceSize:      "10Gi",
	})
	pod, err := k.BuildIsolatedJobPodSpec(engine.IsolatedJobSpec{
		RunID:                "run-A",
		JobID:                "job-B",
		Image:                "node:20",
		Script:               "pnpm test",
		AssignmentSecretName: "gocdnext-job-test01-assignment",
		Env:                  map[string]string{"CI_BRANCH": "main"},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	for _, e := range taskContainer(t, pod).Env {
		if e.ValueFrom != nil {
			t.Errorf("%s: secretKeyRef on a job with no masks", e.Name)
		}
	}
}

// TestCreateIsolatedJobPod_EnvSecretCarriesEveryReferencedKey is the
// anti-drift test for the isolated path: the Pod builder and the
// Secret builder derive their key sets independently from the same
// inputs, so a mismatch would strand the job in
// CreateContainerConfigError. It also pins the SPLIT between the two
// objects — the assignment Secret must keep carrying nothing but the
// payload, because the runner deletes it the moment prep terminates.
func TestCreateIsolatedJobPod_EnvSecretCarriesEveryReferencedKey(t *testing.T) {
	k, cli := newFakeEngine(t, engine.KubernetesConfig{
		WorkspaceMode:      engine.WorkspaceModeIsolated,
		AgentImage:         "ghcr.io/klinux/gocdnext-agent:test",
		HousekeeperImage:   "alpine:3.19",
		DefaultImage:       "alpine:3.19",
		WorkspaceMountPath: "/workspace",
		WorkspaceSize:      "10Gi",
	})

	stampPodUID(cli)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pod, secretName, err := k.CreateIsolatedJobPod(ctx, engine.IsolatedJobSpec{
		RunID:        "run-A",
		JobID:        "job-B",
		Image:        "node:20",
		Script:       "pnpm test",
		WorkDir:      "/workspace",
		Env:          leakEnv(),
		SecretValues: leakMasks(),
	}, []byte("serialised-assignment"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if secretName == "" {
		t.Fatal("empty assignment secret name")
	}

	assertNoSecretInPod(t, pod)
	envSecretName := engine.EnvSecretName(pod.Name)
	refs := assertSecretBacked(t, taskContainer(t, pod), envSecretName)

	envSecret, err := cli.CoreV1().Secrets("gocdnext-tests").Get(ctx, envSecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get env secret: %v", err)
	}
	for key := range refs {
		if _, ok := envSecret.Data[key]; !ok {
			t.Errorf("pod references key %q, absent from the env Secret", key)
		}
	}
	if got := string(envSecret.Data[engine.SecretEnvKey("GHCR_TOKEN")]); got != leakToken {
		t.Errorf("env secret data for GHCR_TOKEN = %q, want the token", got)
	}
	// Owned by the pod: nothing else ever deletes it, so a missing
	// ownerRef means a Secret that leaks for the namespace's lifetime.
	if len(envSecret.OwnerReferences) != 1 || envSecret.OwnerReferences[0].UID != pod.UID {
		t.Errorf("env secret ownerRefs = %+v, want one pointing at the pod", envSecret.OwnerReferences)
	}

	assignment, err := cli.CoreV1().Secrets("gocdnext-tests").Get(ctx, secretName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get assignment secret: %v", err)
	}
	if len(assignment.Data) != 1 {
		t.Errorf("assignment secret data keys = %v, want only the payload", mapKeys(assignment.Data))
	}
	if got := string(assignment.Data[engine.AssignmentSecretKey]); got != "serialised-assignment" {
		t.Errorf("assignment payload = %q, want it preserved", got)
	}
}

// TestCreateIsolatedJobPod_NoSecretsCreatesNoEnvSecret: the common
// case must not pay an extra create + patch per job.
func TestCreateIsolatedJobPod_NoSecretsCreatesNoEnvSecret(t *testing.T) {
	k, cli := newFakeEngine(t, engine.KubernetesConfig{
		WorkspaceMode:      engine.WorkspaceModeIsolated,
		AgentImage:         "ghcr.io/klinux/gocdnext-agent:test",
		HousekeeperImage:   "alpine:3.19",
		DefaultImage:       "alpine:3.19",
		WorkspaceMountPath: "/workspace",
		WorkspaceSize:      "10Gi",
	})

	stampPodUID(cli)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pod, _, err := k.CreateIsolatedJobPod(ctx, engine.IsolatedJobSpec{
		RunID:   "run-A",
		JobID:   "job-B",
		Image:   "node:20",
		Script:  "pnpm test",
		WorkDir: "/workspace",
		Env:     map[string]string{"CI_BRANCH": "main"},
	}, []byte("serialised-assignment"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, err := cli.CoreV1().Secrets("gocdnext-tests").Get(ctx, engine.EnvSecretName(pod.Name), metav1.GetOptions{}); !kerrors.IsNotFound(err) {
		t.Errorf("env secret exists (err=%v) for a job with no sensitive env", err)
	}
}

// TestCreateIsolatedJobPod_PodCreateFailureDeletesBothSecrets: both
// secrets commit before the pod, so a failed pod create must leave
// neither behind — there is no owner to GC them.
func TestCreateIsolatedJobPod_PodCreateFailureDeletesBothSecrets(t *testing.T) {
	k, cli := newFakeEngine(t, engine.KubernetesConfig{
		WorkspaceMode:      engine.WorkspaceModeIsolated,
		AgentImage:         "ghcr.io/klinux/gocdnext-agent:test",
		HousekeeperImage:   "alpine:3.19",
		DefaultImage:       "alpine:3.19",
		WorkspaceMountPath: "/workspace",
		WorkspaceSize:      "10Gi",
	})
	cli.PrependReactor("create", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("admission webhook denied the request")
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, err := k.CreateIsolatedJobPod(ctx, engine.IsolatedJobSpec{
		RunID:        "run-A",
		JobID:        "job-B",
		Image:        "node:20",
		Script:       "pnpm test",
		WorkDir:      "/workspace",
		Env:          leakEnv(),
		SecretValues: leakMasks(),
	}, []byte("serialised-assignment")); err == nil {
		t.Fatal("CreateIsolatedJobPod returned nil error on a failed pod create")
	}

	list, err := cli.CoreV1().Secrets("gocdnext-tests").List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list secrets: %v", err)
	}
	if len(list.Items) != 0 {
		t.Errorf("secrets left behind: %d", len(list.Items))
	}
}

func mapKeys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestRunScript_SharedMode_CreatesEnvSecretBeforePod pins the create
// ORDER (the pod references the Secret, so the Secret must exist
// first) plus the ownerRef that makes the kubelet GC it with the pod.
func TestRunScript_SharedMode_CreatesEnvSecretBeforePod(t *testing.T) {
	k, cli := newFakeEngine(t, engine.KubernetesConfig{
		DefaultImage:       "alpine:3.19",
		WorkspaceMountPath: "/workspace",
	})

	var order []string
	cli.PrependReactor("create", "*", func(action k8stesting.Action) (bool, runtime.Object, error) {
		order = append(order, "create:"+action.GetResource().Resource)
		return false, nil, nil
	})
	cli.PrependReactor("patch", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
		order = append(order, "patch:secrets")
		return false, nil, nil
	})

	var podName string
	cli.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		pod := action.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
		podName = pod.Name
		go advancePod(t, cli, "gocdnext-tests", pod.Name, corev1.PodSucceeded, 0)
		return false, nil, nil
	})
	stampPodUID(cli)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	exit, err := k.RunScript(ctx, engine.ScriptSpec{
		Script:       "echo hi",
		Env:          leakEnv(),
		SecretValues: leakMasks(),
		OnLine:       func(string, string) {},
	})
	if err != nil {
		t.Fatalf("RunScript: %v", err)
	}
	if exit != 0 {
		t.Errorf("exit = %d", exit)
	}

	if len(order) < 3 {
		t.Fatalf("api calls = %v, want secret create → pod create → secret patch", order)
	}
	if order[0] != "create:secrets" || order[1] != "create:pods" {
		t.Errorf("order = %v, want the Secret created BEFORE the pod", order)
	}
	if order[2] != "patch:secrets" {
		t.Errorf("order = %v, want the ownerRef patch after the pod create", order)
	}

	secret, err := cli.CoreV1().Secrets("gocdnext-tests").Get(ctx, podName+"-env", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get env secret: %v", err)
	}
	if got := string(secret.Data[engine.SecretEnvKey("GHCR_TOKEN")]); got != leakToken {
		t.Errorf("env secret data for GHCR_TOKEN = %q", got)
	}
	if _, ok := secret.Data[engine.SecretEnvKey("CI_BRANCH")]; ok {
		t.Error("plain CI_BRANCH stored in the Secret — it should stay inline")
	}
}

// TestRunScript_SharedMode_NoSecretsSkipsTheSecretEntirely is the
// fast-path regression guard: a job with nothing sensitive must not
// pay an extra create+patch round trip per task pod.
func TestRunScript_SharedMode_NoSecretsSkipsTheSecretEntirely(t *testing.T) {
	k, cli := newFakeEngine(t, engine.KubernetesConfig{
		DefaultImage:       "alpine:3.19",
		WorkspaceMountPath: "/workspace",
	})

	var secretCalls int
	cli.PrependReactor("*", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		secretCalls++
		return false, nil, nil
	})
	cli.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		pod := action.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
		go advancePod(t, cli, "gocdnext-tests", pod.Name, corev1.PodSucceeded, 0)
		return false, nil, nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := k.RunScript(ctx, engine.ScriptSpec{
		Script: "echo hi",
		Env:    map[string]string{"CI_BRANCH": "main"},
		OnLine: func(string, string) {},
	}); err != nil {
		t.Fatalf("RunScript: %v", err)
	}
	if secretCalls != 0 {
		t.Errorf("secret API calls = %d, want 0 for a job with no sensitive env", secretCalls)
	}
}

// TestRunScript_SharedMode_PodCreateFailureDeletesTheSecret: the
// Secret commits first, so a failed pod create must not leave the
// values sitting in the namespace with no owner to GC them.
func TestRunScript_SharedMode_PodCreateFailureDeletesTheSecret(t *testing.T) {
	k, cli := newFakeEngine(t, engine.KubernetesConfig{
		DefaultImage:       "alpine:3.19",
		WorkspaceMountPath: "/workspace",
	})

	deleted := make(chan string, 1)
	cli.PrependReactor("delete", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
		deleted <- action.(k8stesting.DeleteAction).GetName()
		return false, nil, nil
	})
	cli.PrependReactor("create", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("admission webhook denied the request")
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := k.RunScript(ctx, engine.ScriptSpec{
		Script:       "echo hi",
		Env:          leakEnv(),
		SecretValues: leakMasks(),
		OnLine:       func(string, string) {},
	}); err == nil {
		t.Fatal("RunScript returned nil error on a failed pod create")
	}

	select {
	case name := <-deleted:
		if !strings.HasSuffix(name, "-env") {
			t.Errorf("deleted %q, want the env secret", name)
		}
	default:
		t.Error("env secret not deleted after the pod create failed")
	}

	list, err := cli.CoreV1().Secrets("gocdnext-tests").List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list secrets: %v", err)
	}
	if len(list.Items) != 0 {
		t.Errorf("secrets left behind: %d", len(list.Items))
	}
}
