package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func newIsolatedTestEngine(t *testing.T) *Kubernetes {
	t.Helper()
	k := NewKubernetesWithClient(fake.NewSimpleClientset(), KubernetesConfig{
		Namespace:             "ci",
		WorkspaceMode:         WorkspaceModeIsolated,
		WorkspaceStorageClass: "pd-ssd",
		WorkspaceSize:         "10Gi",
		AgentImage:            "ghcr.io/klinux/gocdnext-agent:test",
		HousekeeperImage:      "alpine:3.19",
		DefaultImage:          "alpine:3.19",
		WorkspaceMountPath:    "/workspace",
	})
	k.nowName = func() string { return "gocdnext-job-test01" }
	return k
}

func TestBuildIsolatedJobPodSpec_StructureScript(t *testing.T) {
	k := newIsolatedTestEngine(t)
	pod, err := k.BuildIsolatedJobPodSpec(IsolatedJobSpec{
		RunID:                "run-A",
		JobID:                "job-B",
		Image:                "node:20",
		Script:               "pnpm test",
		WorkDir:              "/workspace",
		AssignmentSecretName: "gocdnext-job-test01-assignment",
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	if got := len(pod.Spec.InitContainers); got != 1 {
		t.Fatalf("init containers: want 1, got %d", got)
	}
	prep := pod.Spec.InitContainers[0]
	if prep.Name != "prep" {
		t.Errorf("init[0].Name: want prep, got %q", prep.Name)
	}
	if got := strings.Join(prep.Command, " "); !strings.HasPrefix(got, "gocdnext-agent prep") {
		t.Errorf("prep command: want starts with `gocdnext-agent prep`, got %q", got)
	}
	if prep.Image != "ghcr.io/klinux/gocdnext-agent:test" {
		t.Errorf("prep image: got %q", prep.Image)
	}

	// 2 main containers: task + housekeeper
	gotMain := containerNames(pod.Spec.Containers)
	wantMain := []string{"task", "housekeeper"}
	if !equalSet(gotMain, wantMain) {
		t.Errorf("main containers: want %v, got %v", wantMain, gotMain)
	}

	task := findContainer(pod.Spec.Containers, "task")
	if task == nil {
		t.Fatal("no task container")
	}
	if task.Image != "node:20" {
		t.Errorf("task image: got %q", task.Image)
	}
	if got := strings.Join(task.Command, " "); !strings.Contains(got, "pnpm test") {
		t.Errorf("task command should embed script: got %q", got)
	}

	hk := findContainer(pod.Spec.Containers, "housekeeper")
	if hk == nil {
		t.Fatal("no housekeeper container")
	}
	if got := strings.Join(hk.Command, " "); !strings.Contains(got, "sleep") {
		t.Errorf("housekeeper should sleep: got %q", got)
	}

	// Volumes: workspace (ephemeral) + assignment (secret)
	gotVols := volumeNames(pod.Spec.Volumes)
	wantVols := []string{"workspace", "assignment"}
	if !equalSet(gotVols, wantVols) {
		t.Errorf("volumes: want %v, got %v", wantVols, gotVols)
	}
	ws := findVolume(pod.Spec.Volumes, "workspace")
	if ws == nil || ws.Ephemeral == nil {
		t.Fatal("workspace must be ephemeral")
	}
	scn := ws.Ephemeral.VolumeClaimTemplate.Spec.StorageClassName
	if scn == nil || *scn != "pd-ssd" {
		t.Errorf("storage class: want pd-ssd, got %v", scn)
	}
	modes := ws.Ephemeral.VolumeClaimTemplate.Spec.AccessModes
	if len(modes) != 1 || modes[0] != corev1.ReadWriteOnce {
		t.Errorf("access modes: want [RWO], got %v", modes)
	}

	asn := findVolume(pod.Spec.Volumes, "assignment")
	if asn == nil || asn.Secret == nil {
		t.Fatal("assignment must be a Secret volume")
	}
	if asn.Secret.SecretName != "gocdnext-job-test01-assignment" {
		t.Errorf("secret name: got %q", asn.Secret.SecretName)
	}
	if asn.Secret.DefaultMode == nil || *asn.Secret.DefaultMode != 0o400 {
		t.Errorf("secret default mode: want 0400, got %v", asn.Secret.DefaultMode)
	}

	// Each container that needs workspace MUST mount it.
	for _, c := range append(pod.Spec.InitContainers, pod.Spec.Containers...) {
		if c.Name == "dind" {
			continue
		}
		found := false
		for _, vm := range c.VolumeMounts {
			if vm.Name == "workspace" && vm.MountPath == "/workspace" {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("container %q missing workspace mount", c.Name)
		}
	}

	// Labels carry run/job IDs for orphan reaping.
	if got := pod.Labels["gocdnext.io/run-id"]; got != "run-A" {
		t.Errorf("run-id label: got %q", got)
	}
	if got := pod.Labels["gocdnext.io/job-id"]; got != "job-B" {
		t.Errorf("job-id label: got %q", got)
	}
}

func TestBuildIsolatedJobPodSpec_MountPathStaysAtRoot_WhenWorkDirIsSubdir(t *testing.T) {
	// Regression for v0.5.1 → v0.5.2: when executeIsolated sets
	// spec.WorkDir to /workspace/src/<hash> (the first checkout's
	// target_dir), the workspace volume must STILL mount at the
	// PVC root (/workspace). v0.5.1 used workDir for both the
	// mount path and the task WorkingDir, so the PVC ended up
	// mounted at /workspace/src/<hash>, prep cloned to
	// /workspace/src/<hash>/src/<hash>, and the task at
	// /workspace/src/<hash> saw an empty directory.
	k := newIsolatedTestEngine(t)
	pod, err := k.BuildIsolatedJobPodSpec(IsolatedJobSpec{
		RunID:                "r",
		JobID:                "j",
		Image:                "node:20",
		Script:               "pnpm test",
		WorkDir:              "/workspace/src/abc12345",
		AssignmentSecretName: "s",
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	// Every workspace mount on every container must be the PVC
	// root, NOT the task subdir.
	for _, c := range pod.Spec.InitContainers {
		for _, vm := range c.VolumeMounts {
			if vm.Name == "workspace" && vm.MountPath != "/workspace" {
				t.Errorf("init container %q mounts workspace at %q; want /workspace",
					c.Name, vm.MountPath)
			}
		}
	}
	for _, c := range pod.Spec.Containers {
		for _, vm := range c.VolumeMounts {
			if vm.Name == "workspace" && vm.MountPath != "/workspace" {
				t.Errorf("container %q mounts workspace at %q; want /workspace",
					c.Name, vm.MountPath)
			}
		}
	}

	// prep's --workspace must also be the root, NOT the task
	// subdir — Prep joins target_dir against it internally.
	prep := pod.Spec.InitContainers[0]
	wantArg := "--workspace=/workspace"
	found := false
	for _, a := range prep.Command {
		if a == wantArg {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("prep command missing %q; got %v", wantArg, prep.Command)
	}

	// Task container WorkingDir must be the deep path so plugins
	// land where prep cloned the primary material.
	task := findContainer(pod.Spec.Containers, "task")
	if task == nil {
		t.Fatal("no task container")
	}
	if got, want := task.WorkingDir, "/workspace/src/abc12345"; got != want {
		t.Errorf("task WorkingDir: want %q, got %q", want, got)
	}
}

func TestBuildIsolatedJobPodSpec_Plugin(t *testing.T) {
	k := newIsolatedTestEngine(t)
	pod, err := k.BuildIsolatedJobPodSpec(IsolatedJobSpec{
		RunID:                "r1",
		JobID:                "j1",
		Image:                "ghcr.io/klinux/gocdnext-plugin-node:v1",
		Script:               "", // plugin: no wrapping
		AssignmentSecretName: "s",
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	task := findContainer(pod.Spec.Containers, "task")
	if task == nil {
		t.Fatal("no task")
	}
	if len(task.Command) != 0 {
		t.Errorf("plugin task should NOT have Command (use ENTRYPOINT), got %v", task.Command)
	}
}

func TestBuildIsolatedJobPodSpec_OutputsEnvInjected(t *testing.T) {
	// When OutputsRelPath is set, the task container's env must
	// carry GOCDNEXT_OUTPUT_FILE pointing at mountPath+rel — same
	// shape as shared-mode RunScript (kubernetes.go:305-307).
	// Without this the plugin would never know where to write
	// structured outputs.
	k := newIsolatedTestEngine(t)
	pod, err := k.BuildIsolatedJobPodSpec(IsolatedJobSpec{
		RunID:                "run-A",
		JobID:                "job-B",
		Image:                "plugin:1",
		WorkDir:              "/workspace",
		AssignmentSecretName: "gocdnext-job-test01-assignment",
		OutputsRelPath:       ".gocdnext/outputs/abc123def456.env",
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	task := findContainer(pod.Spec.Containers, "task")
	if task == nil {
		t.Fatal("no task container")
	}
	var got string
	var found bool
	for _, e := range task.Env {
		if e.Name == OutputsEnvName {
			got = e.Value
			found = true
		}
	}
	if !found {
		t.Fatalf("task env missing %s; env=%+v", OutputsEnvName, task.Env)
	}
	want := "/workspace/.gocdnext/outputs/abc123def456.env"
	if got != want {
		t.Errorf("%s: want %q, got %q", OutputsEnvName, want, got)
	}
}

func TestBuildIsolatedJobPodSpec_MergesNodeSelectorWithProfileWinning(t *testing.T) {
	// Same merge contract as shared mode: profile wins on key
	// collision, agent-only keys survive. Defends the isolated path
	// against drift where shared-mode tests would have caught the
	// shared engine but a regression in BuildIsolatedJobPodSpec
	// wouldn't be visible.
	k := newIsolatedTestEngine(t)
	k.cfg.NodeSelector = map[string]string{"tier": "ci", "pool": "ci"}
	pod, err := k.BuildIsolatedJobPodSpec(IsolatedJobSpec{
		RunID: "r", JobID: "j",
		Image: "alpine:3.19", Script: "true",
		WorkDir:              "/workspace",
		AssignmentSecretName: "gocdnext-job-test01-assignment",
		NodeSelector:         map[string]string{"pool": "gradle"},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if pod.Spec.NodeSelector["tier"] != "ci" || pod.Spec.NodeSelector["pool"] != "gradle" {
		t.Errorf("merge wrong: %v", pod.Spec.NodeSelector)
	}
}

func TestBuildIsolatedJobPodSpec_ConcatsTolerations(t *testing.T) {
	k := newIsolatedTestEngine(t)
	k.cfg.Tolerations = []corev1.Toleration{
		{Key: "node.kubernetes.io/unschedulable", Operator: corev1.TolerationOpExists},
	}
	pod, err := k.BuildIsolatedJobPodSpec(IsolatedJobSpec{
		RunID: "r", JobID: "j",
		Image: "alpine:3.19", Script: "true",
		WorkDir:              "/workspace",
		AssignmentSecretName: "gocdnext-job-test01-assignment",
		Tolerations: []corev1.Toleration{
			{Key: "spot", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute},
		},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(pod.Spec.Tolerations) != 2 ||
		pod.Spec.Tolerations[0].Key != "node.kubernetes.io/unschedulable" ||
		pod.Spec.Tolerations[1].Key != "spot" {
		t.Errorf("concat wrong: %v", pod.Spec.Tolerations)
	}
}

func TestBuildIsolatedJobPodSpec_OutputsEnv_AnchoredAtWorkDir_NotMountRoot(t *testing.T) {
	// Regression: when the first checkout has target_dir, the
	// agent passes spec.WorkDir = mountPath + "/" + target_dir.
	// GOCDNEXT_OUTPUT_FILE MUST land at workDir/<rel>, not
	// mountPath/<rel>, otherwise the plugin's
	// `> $GOCDNEXT_OUTPUT_FILE` writes to a sibling dir that
	// prep never created and the task fails with
	// "No such file or directory" before producing anything.
	k := newIsolatedTestEngine(t)
	pod, err := k.BuildIsolatedJobPodSpec(IsolatedJobSpec{
		RunID:                "run-A",
		JobID:                "job-B",
		Image:                "plugin:1",
		WorkDir:              "/workspace/app", // ← target_dir nesting
		AssignmentSecretName: "gocdnext-job-test01-assignment",
		OutputsRelPath:       ".gocdnext/outputs/abc123def456.env",
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	task := findContainer(pod.Spec.Containers, "task")
	var got string
	for _, e := range task.Env {
		if e.Name == OutputsEnvName {
			got = e.Value
		}
	}
	want := "/workspace/app/.gocdnext/outputs/abc123def456.env"
	if got != want {
		t.Errorf("%s: want %q (anchored at workDir incl. target_dir), got %q",
			OutputsEnvName, want, got)
	}
}

func TestBuildIsolatedJobPodSpec_OutputsEnv_OmittedWhenRelPathEmpty(t *testing.T) {
	// Empty OutputsRelPath → no env injection at all. Required so
	// jobs that don't declare outputs: don't gain a confusing
	// stray env var.
	k := newIsolatedTestEngine(t)
	pod, err := k.BuildIsolatedJobPodSpec(IsolatedJobSpec{
		RunID:                "run-A",
		JobID:                "job-B",
		Image:                "plugin:1",
		WorkDir:              "/workspace",
		AssignmentSecretName: "gocdnext-job-test01-assignment",
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	task := findContainer(pod.Spec.Containers, "task")
	for _, e := range task.Env {
		if e.Name == OutputsEnvName {
			t.Errorf("task env unexpectedly carries %s=%q with no OutputsRelPath", e.Name, e.Value)
		}
	}
}

func TestBuildIsolatedJobPodSpec_DockerSidecar(t *testing.T) {
	k := newIsolatedTestEngine(t)
	pod, err := k.BuildIsolatedJobPodSpec(IsolatedJobSpec{
		RunID:                "r1",
		JobID:                "j1",
		Image:                "node:20",
		Script:               "docker build .",
		Docker:               true,
		AssignmentSecretName: "s",
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	got := containerNames(pod.Spec.Containers)
	wantAll := []string{"task", "housekeeper", "dind"}
	if !equalSet(got, wantAll) {
		t.Errorf("docker=true containers: want %v, got %v", wantAll, got)
	}
	task := findContainer(pod.Spec.Containers, "task")
	if !hasEnv(task.Env, "DOCKER_HOST") {
		t.Errorf("task should have DOCKER_HOST env when docker=true")
	}
}

// TestBuildIsolatedJobPodSpec_DockerSocketShared is the v0.14.10
// regression: when docker:true, DinD + task share a tmpfs emptyDir
// at /run/dind, DinD adds a `--host=unix://…` listener writing the
// socket there, the task's DOCKER_HOST points at the same path, and
// DinD's postStart `chmod 666` makes the socket addressable by the
// task's non-root user.
//
// Without this plumbing the task either uses TCP (Nagle + IP stack
// overhead → flaky testcontainers timing) or can't reach the socket
// at all (permission denied as user 1000 against root:docker 660).
func TestBuildIsolatedJobPodSpec_DockerSocketShared(t *testing.T) {
	k := newIsolatedTestEngine(t)
	pod, err := k.BuildIsolatedJobPodSpec(IsolatedJobSpec{
		RunID:                "r1",
		JobID:                "j1",
		Image:                "node:20",
		Script:               "docker build .",
		Docker:               true,
		AssignmentSecretName: "s",
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	// 1. Volume is present and tmpfs-backed.
	var sockVol *corev1.Volume
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == "dind-socket" {
			sockVol = &pod.Spec.Volumes[i]
			break
		}
	}
	if sockVol == nil {
		t.Fatal("dind-socket volume missing on docker:true pod")
	}
	if sockVol.EmptyDir == nil || sockVol.EmptyDir.Medium != corev1.StorageMediumMemory {
		t.Errorf("dind-socket volume should be tmpfs (Memory medium); got %+v", sockVol.EmptyDir)
	}

	// 2. DinD container has --host=unix://... arg, the socket-shared
	//    mount, and a postStart chmod hook.
	dind := findContainer(pod.Spec.Containers, "dind")
	var sawUnixHostArg bool
	for _, a := range dind.Args {
		if a == "--host=unix:///run/dind/docker.sock" {
			sawUnixHostArg = true
		}
	}
	if !sawUnixHostArg {
		t.Errorf("DinD args missing the shared-unix-socket listener: %v", dind.Args)
	}
	var sawSharedMount bool
	for _, m := range dind.VolumeMounts {
		if m.Name == "dind-socket" && m.MountPath == "/run/dind" {
			sawSharedMount = true
		}
	}
	if !sawSharedMount {
		t.Errorf("DinD should mount dind-socket at /run/dind; mounts=%v", dind.VolumeMounts)
	}
	if dind.Lifecycle == nil || dind.Lifecycle.PostStart == nil || dind.Lifecycle.PostStart.Exec == nil {
		t.Error("DinD missing postStart chmod hook — task as non-root will hit EPERM")
	} else {
		gotCmd := dind.Lifecycle.PostStart.Exec.Command
		joined := ""
		for _, c := range gotCmd {
			joined += c + " "
		}
		if !contains(joined, "chmod 666") || !contains(joined, "/run/dind/docker.sock") {
			t.Errorf("postStart hook should chmod 666 the shared socket; got %v", gotCmd)
		}
	}

	// 3. Task container's DOCKER_HOST is the unix socket path, NOT TCP.
	task := findContainer(pod.Spec.Containers, "task")
	var gotHost string
	for _, e := range task.Env {
		if e.Name == "DOCKER_HOST" {
			gotHost = e.Value
		}
	}
	if gotHost != "unix:///run/dind/docker.sock" {
		t.Errorf("isolated DOCKER_HOST = %q, want unix:///run/dind/docker.sock", gotHost)
	}

	// 4. Task container mounts the same shared volume so its
	//    DOCKER_HOST actually resolves to a real file.
	var taskHasShare bool
	for _, m := range task.VolumeMounts {
		if m.Name == "dind-socket" && m.MountPath == "/run/dind" {
			taskHasShare = true
		}
	}
	if !taskHasShare {
		t.Errorf("task container missing dind-socket mount; mounts=%v", task.VolumeMounts)
	}

	// 5. TCP listener preserved — escape hatch for operators who
	//    explicitly override DOCKER_HOST=tcp://localhost:2375.
	var sawTCPArg bool
	for _, a := range dind.Args {
		if a == "--host=tcp://0.0.0.0:2375" {
			sawTCPArg = true
		}
	}
	if !sawTCPArg {
		t.Errorf("TCP listener should be preserved for explicit-override escape hatch; args=%v", dind.Args)
	}
}

// TestBuildIsolatedJobPodSpec_NoDockerNoSocketVolume — when the job
// doesn't ask for docker, no volume + no socket plumbing. Stays
// minimal so an emptyDir isn't allocated for nothing.
func TestBuildIsolatedJobPodSpec_NoDockerNoSocketVolume(t *testing.T) {
	k := newIsolatedTestEngine(t)
	pod, err := k.BuildIsolatedJobPodSpec(IsolatedJobSpec{
		RunID:                "r1",
		JobID:                "j1",
		Image:                "node:20",
		Script:               "echo no docker",
		Docker:               false,
		AssignmentSecretName: "s",
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	for _, v := range pod.Spec.Volumes {
		if v.Name == "dind-socket" {
			t.Errorf("docker:false should not produce a dind-socket volume")
		}
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestBuildIsolatedJobPodSpec_RejectsWrongMode(t *testing.T) {
	k := NewKubernetesWithClient(fake.NewSimpleClientset(), KubernetesConfig{
		Namespace:     "ci",
		WorkspaceMode: WorkspaceModeShared,
		AgentImage:    "agent:v1",
	})
	if _, err := k.BuildIsolatedJobPodSpec(IsolatedJobSpec{
		RunID:                "r",
		JobID:                "j",
		Image:                "alpine",
		Script:               "echo",
		AssignmentSecretName: "s",
	}); err == nil {
		t.Fatalf("expected error when engine is configured for shared mode")
	}
}

func TestBuildIsolatedJobPodSpec_DisablesAutomountSAToken(t *testing.T) {
	// Task containers run user code; the agent's SA token should
	// not be reachable from inside them even if the SA's RBAC is
	// later widened by mistake.
	k := newIsolatedTestEngine(t)
	pod, err := k.BuildIsolatedJobPodSpec(IsolatedJobSpec{
		RunID:                "r",
		JobID:                "j",
		Image:                "alpine",
		Script:               "echo hi",
		AssignmentSecretName: "s",
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if pod.Spec.AutomountServiceAccountToken == nil {
		t.Fatal("AutomountServiceAccountToken should be set explicitly")
	}
	if got := *pod.Spec.AutomountServiceAccountToken; got != false {
		t.Errorf("AutomountServiceAccountToken: want false, got %v", got)
	}
}

func TestCreateIsolatedJobPod_DeletesSecretOnOwnerPatchFailure(t *testing.T) {
	// The fake clientset does NOT populate UIDs on Create — the
	// returned Pod has empty UID, which makes
	// PatchAssignmentSecretOwner fail with "pod must have a UID".
	// That gives us a natural failure path to exercise the eager
	// secret-deletion behaviour without injecting a reactor.
	client := fake.NewSimpleClientset()
	k := NewKubernetesWithClient(client, KubernetesConfig{
		Namespace:        "ci",
		WorkspaceMode:    WorkspaceModeIsolated,
		AgentImage:       "agent:v1",
		HousekeeperImage: "alpine:3.19",
		DefaultImage:     "alpine:3.19",
		WorkspaceSize:    "10Gi",
	})

	pod, secretName, err := k.CreateIsolatedJobPod(context.Background(), IsolatedJobSpec{
		RunID:   "r",
		JobID:   "j",
		Image:   "alpine",
		Script:  "echo",
		WorkDir: "/workspace",
	}, []byte("assignment-bytes"))
	if err == nil {
		t.Fatal("expected owner-patch error (fake clientset returns pod with empty UID)")
	}
	if pod == nil {
		t.Fatal("pod should be returned alongside the error")
	}
	if secretName != "" {
		t.Errorf("secretName should be empty when patch failed (got %q)", secretName)
	}
	if _, getErr := client.CoreV1().Secrets("ci").Get(
		context.Background(),
		pod.Name+"-assignment",
		metav1.GetOptions{},
	); getErr == nil {
		t.Fatal("expected secret to be deleted after owner-patch failure")
	}
}

func TestWaitForTaskStarted_TimesOutOnImagePullBackOff(t *testing.T) {
	// Reproduce the corner case the round-3 review flagged: task
	// container stuck in Waiting (ImagePullBackOff), housekeeper
	// Running so Pod phase is Running, no terminal signal —
	// WaitForTaskTerminated would poll forever without the
	// StartupTimeout-bounded "Started" gate.
	client := fake.NewSimpleClientset(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "stuck-pod",
			Namespace: "ci",
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name: "task",
					State: corev1.ContainerState{
						Waiting: &corev1.ContainerStateWaiting{
							Reason:  "ImagePullBackOff",
							Message: "back-off pulling image",
						},
					},
				},
				{
					Name: "housekeeper",
					State: corev1.ContainerState{
						Running: &corev1.ContainerStateRunning{},
					},
				},
			},
		},
	})
	k := NewKubernetesWithClient(client, KubernetesConfig{
		Namespace:      "ci",
		PollInterval:   5 * time.Millisecond,
		StartupTimeout: 30 * time.Millisecond,
	})

	err := k.WaitForTaskStarted(context.Background(), "stuck-pod")
	if err == nil {
		t.Fatal("expected timeout error when task stays in Waiting")
	}
}

func TestWaitForTaskStarted_OKWhenTaskRunning(t *testing.T) {
	client := fake.NewSimpleClientset(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ok-pod",
			Namespace: "ci",
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name: "task",
					State: corev1.ContainerState{
						Running: &corev1.ContainerStateRunning{},
					},
				},
			},
		},
	})
	k := NewKubernetesWithClient(client, KubernetesConfig{
		Namespace:      "ci",
		PollInterval:   5 * time.Millisecond,
		StartupTimeout: time.Second,
	})

	if err := k.WaitForTaskStarted(context.Background(), "ok-pod"); err != nil {
		t.Fatalf("expected nil for task in Running, got %v", err)
	}
}

func TestDeleteAssignmentSecret_NotFoundIsOK(t *testing.T) {
	client := fake.NewSimpleClientset()
	k := NewKubernetesWithClient(client, KubernetesConfig{Namespace: "ci"})
	// Deletes against a non-existent secret should NOT surface as
	// errors — the orphan reaper (owner-ref GC) may have raced us
	// and that's a valid outcome.
	if err := k.DeleteAssignmentSecret(context.Background(), "missing"); err != nil {
		t.Fatalf("expected nil for NotFound, got %v", err)
	}
}

func TestDeleteAssignmentSecret_RemovesExisting(t *testing.T) {
	client := fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "gocdnext-job-x-assignment",
			Namespace: "ci",
		},
		Data: map[string][]byte{AssignmentSecretKey: []byte("payload")},
	})
	k := NewKubernetesWithClient(client, KubernetesConfig{Namespace: "ci"})
	if err := k.DeleteAssignmentSecret(context.Background(), "gocdnext-job-x-assignment"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := client.CoreV1().Secrets("ci").Get(
		context.Background(), "gocdnext-job-x-assignment", metav1.GetOptions{},
	); err == nil {
		t.Fatal("expected secret to be gone")
	}
}

func TestBuildIsolatedJobPodSpec_RejectsMissingAgentImage(t *testing.T) {
	k := NewKubernetesWithClient(fake.NewSimpleClientset(), KubernetesConfig{
		Namespace:     "ci",
		WorkspaceMode: WorkspaceModeIsolated,
		AgentImage:    "", // missing
	})
	if _, err := k.BuildIsolatedJobPodSpec(IsolatedJobSpec{
		RunID:                "r",
		JobID:                "j",
		Image:                "alpine",
		Script:               "echo",
		AssignmentSecretName: "s",
	}); err == nil {
		t.Fatalf("expected error when AgentImage is empty")
	}
}

// ---- helpers --------------------------------------------------------------

func containerNames(cs []corev1.Container) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.Name)
	}
	return out
}

func volumeNames(vs []corev1.Volume) []string {
	out := make([]string, 0, len(vs))
	for _, v := range vs {
		out = append(out, v.Name)
	}
	return out
}

func findContainer(cs []corev1.Container, name string) *corev1.Container {
	for i, c := range cs {
		if c.Name == name {
			return &cs[i]
		}
	}
	return nil
}

func findVolume(vs []corev1.Volume, name string) *corev1.VolumeSource {
	for i, v := range vs {
		if v.Name == name {
			return &vs[i].VolumeSource
		}
	}
	return nil
}

func hasEnv(env []corev1.EnvVar, name string) bool {
	for _, e := range env {
		if e.Name == name {
			return true
		}
	}
	return false
}

func equalSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[string]bool{}
	for _, s := range a {
		seen[s] = true
	}
	for _, s := range b {
		if !seen[s] {
			return false
		}
	}
	return true
}

// Housekeeper resources must carry EXPLICIT limits: it's the
// container `tar -czf` runs in for cache/artifact upload, and a
// populated Go/gradle cache is hundreds of MB to GBs. Without
// explicit limits a cluster LimitRange can default the container to
// something pathological (16Mi-ish memory, 100m CPU) and the tar
// exec dies with exit 137 mid-upload (operator-reported).
func TestIsolatedPod_HousekeeperHasExplicitLimits(t *testing.T) {
	k := newIsolatedTestEngine(t)
	pod, err := k.BuildIsolatedJobPodSpec(IsolatedJobSpec{
		RunID:                "run-A",
		JobID:                "job-B",
		Image:                "node:20",
		Script:               "true",
		WorkDir:              "/workspace",
		AssignmentSecretName: "gocdnext-job-test01-assignment",
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	hk := findContainer(pod.Spec.Containers, "housekeeper")
	if hk == nil {
		t.Fatal("no housekeeper container")
	}
	cpu := hk.Resources.Limits.Cpu()
	mem := hk.Resources.Limits.Memory()
	if cpu == nil || cpu.IsZero() {
		t.Fatal("housekeeper has no CPU limit — LimitRange defaults would apply")
	}
	if mem == nil || mem.IsZero() {
		t.Fatal("housekeeper has no memory limit — LimitRange defaults would apply")
	}
	if mem.Value() < 256<<20 {
		t.Fatalf("housekeeper memory limit %s too small for tar+gzip of real caches", mem)
	}
}
