package engine

import (
	"bufio"
	"context"
	"fmt"
	"path"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/utils/ptr"
)

// IsolatedJobSpec describes a full job's pod in isolated workspace
// mode — one pod with an init container that materialises the
// workspace, a main task container that runs the user's command,
// and a housekeeper sidecar the agent execs into for post-task
// work (artifact upload, cache store).
//
// Unlike ScriptSpec (one task per call), IsolatedJobSpec is
// one POD per JOB. Multi-task jobs are not yet supported in
// isolated mode (the runner validates before constructing this).
type IsolatedJobSpec struct {
	// RunID + JobID identify the job; used for labels + secret name.
	RunID string
	JobID string

	// Image is the task container image (plugin image or job image).
	Image string

	// Script, when non-empty, becomes `sh -c -- <script>` on the
	// task container. When empty, the image's ENTRYPOINT runs as-is
	// (plugin path).
	Script string

	// WorkDir is the workspace mount path inside the pod, typically
	// the engine's configured WorkspaceMountPath.
	WorkDir string

	// Env carries env vars for the task container (PLUGIN_* for
	// plugin jobs, user-declared env for script jobs).
	Env map[string]string

	// Docker requests a DinD sidecar.
	Docker bool

	// Resources is the task container's compute envelope.
	Resources Resources

	// Profile is the runner profile name, stamped as a Pod label.
	Profile string

	// AgentTags are the agent's tags, stamped as labels.
	AgentTags []string

	// HostAliases plumbs service hostnames into the task container.
	HostAliases []HostAlias

	// AssignmentSecretName is the Secret already created by the
	// caller, carrying the serialised JobAssignment. Mounted on the
	// "prep" init container at /etc/gocdnext.
	AssignmentSecretName string

	// OutputsRelPath is the workspace-relative path of the
	// structured-outputs file the task container should write to
	// (set via GOCDNEXT_OUTPUT_FILE in the task env). Empty when
	// the job did not declare outputs:; engine then skips the env
	// injection. The path must be the same one prep mkdir+touch
	// at workspace materialisation time, otherwise the plugin
	// writes one place and post-task exec reads another.
	OutputsRelPath string

	// NodeSelector + Tolerations carry scheduling hints resolved
	// from the runner profile. Merged with agent-level config
	// inside BuildIsolatedJobPodSpec: NodeSelector uses
	// profile-wins on key collisions (profile is more specific
	// than agent default), Tolerations are concatenated (no
	// dedup; kubelet ignores exact duplicates). Empty/nil = use
	// agent-level only.
	NodeSelector map[string]string
	Tolerations  []corev1.Toleration

	// NeedsCacheFetchInit toggles the second init container —
	// `cache-fetch` — that the agent uses to read workspace files
	// for `{{ hash "..." }}` cache-key resolution in isolated mode.
	// When true, BuildIsolatedJobPodSpec emits the alpine sleep-
	// container after prep; it waits on a marker the agent touches
	// after resolving + fetching templated cache entries. Caller
	// (executeIsolated) sets this only when at least one cache
	// entry's key carries `{{` — avoids the extra container's
	// startup cost (~200ms image pull on cold node) for the 90%
	// of jobs without templated caches.
	NeedsCacheFetchInit bool
}

// BuildIsolatedJobPodSpec materialises the multi-container Pod for
// isolated mode. Pod structure:
//
//	initContainers:
//	  prep        — gocdnext-agent prep --assignment=... --workspace=...
//	containers:
//	  task        — user/plugin image, runs the task command
//	  housekeeper — sleep infinity, agent execs into it for post-task
//	  dind        — only when spec.Docker (existing sidecar)
//
// Volumes:
//
//	workspace   — ephemeral PVC (RWO, configured storage class+size)
//	assignment  — Secret (read-only, mode 0o400)
func (k *Kubernetes) BuildIsolatedJobPodSpec(spec IsolatedJobSpec) (*corev1.Pod, error) {
	if k.cfg.WorkspaceMode != WorkspaceModeIsolated {
		return nil, fmt.Errorf("isolated pod: engine configured for mode %q, not isolated", k.cfg.WorkspaceMode)
	}
	if k.cfg.AgentImage == "" {
		return nil, fmt.Errorf("isolated pod: AgentImage required for the prep init container")
	}
	if spec.AssignmentSecretName == "" {
		return nil, fmt.Errorf("isolated pod: AssignmentSecretName required")
	}

	image := spec.Image
	if image == "" {
		image = k.cfg.DefaultImage
	}
	// mountPath is ALWAYS the PVC root — the agent's --workspace
	// arg, the volume mount on every container, and the housekeeper's
	// WorkingDir all anchor here. workDir is the task container's
	// CWD which MAY dive into a subdir (the first checkout's
	// target_dir, derived agent-side by executeIsolated). Mixing
	// the two — as v0.5.0 did when spec.WorkDir was just an alias
	// of WorkspaceMountPath, and v0.5.1 broke when spec.WorkDir
	// started carrying the target_dir suffix — moves the mount
	// point and double-joins target_dir inside prep, leaving the
	// task with an empty workspace.
	mountPath := k.cfg.WorkspaceMountPath
	workDir := spec.WorkDir
	if workDir == "" {
		workDir = mountPath
	}

	// Outputs (issue #10 isolated parity): inject
	// GOCDNEXT_OUTPUT_FILE pointing at the workspace-relative path
	// prep mkdir+touched.
	//
	// CRITICAL: join against `workDir` (= the task container's
	// CWD), NOT `mountPath`. When a checkout has target_dir set,
	// scriptWorkDir (= workDir here) becomes `<mountPath>/<target_dir>`
	// and prep creates the outputs file under
	// `<scriptWorkDir>/.gocdnext/outputs/X.env`. Joining with
	// mountPath instead would point the plugin's
	// `> $GOCDNEXT_OUTPUT_FILE` at a sibling directory that
	// doesn't exist (prep never created it there), failing the
	// task with `No such file or directory` BEFORE the parser
	// ever runs.
	//
	// Path uses package `path` (not filepath) because the value is
	// consumed inside a Linux container — `/` is the only correct
	// separator regardless of the agent's host OS. Empty
	// OutputsRelPath → no env injection, same shape as shared
	// mode's `if spec.OutputsHostPath != "" {...}` guard.
	specEnv := spec.Env
	if spec.OutputsRelPath != "" {
		specEnv = withOutputsEnv(specEnv, path.Join(workDir, spec.OutputsRelPath))
	}

	env := make([]corev1.EnvVar, 0, len(specEnv)+2)
	for key, v := range specEnv {
		env = append(env, corev1.EnvVar{Name: key, Value: v})
	}
	if spec.Docker {
		env = append(env,
			corev1.EnvVar{Name: "DOCKER_HOST", Value: dindHost},
			corev1.EnvVar{Name: "DOCKER_TLS_CERTDIR", Value: ""},
		)
	}

	pullSecrets := make([]corev1.LocalObjectReference, 0, len(k.cfg.ImagePullSecrets))
	for _, n := range k.cfg.ImagePullSecrets {
		pullSecrets = append(pullSecrets, corev1.LocalObjectReference{Name: n})
	}

	// Ephemeral PVC: lifetime tied to the pod. Storage class +
	// size from KubernetesConfig. Each pod gets its own PVC; no
	// cross-pod sharing (the whole point of isolated mode).
	storageQty, err := resource.ParseQuantity(k.cfg.WorkspaceSize)
	if err != nil {
		return nil, fmt.Errorf("isolated pod: parse workspace size %q: %w", k.cfg.WorkspaceSize, err)
	}
	pvcSpec := corev1.PersistentVolumeClaimSpec{
		AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
		Resources: corev1.VolumeResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceStorage: storageQty},
		},
	}
	if k.cfg.WorkspaceStorageClass != "" {
		scn := k.cfg.WorkspaceStorageClass
		pvcSpec.StorageClassName = &scn
	}
	workspaceVolume := corev1.Volume{
		Name: "workspace",
		VolumeSource: corev1.VolumeSource{
			Ephemeral: &corev1.EphemeralVolumeSource{
				VolumeClaimTemplate: &corev1.PersistentVolumeClaimTemplate{
					Spec: pvcSpec,
				},
			},
		},
	}

	assignmentVolume := corev1.Volume{
		Name: "assignment",
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName:  spec.AssignmentSecretName,
				DefaultMode: ptr.To[int32](0o400),
			},
		},
	}

	workspaceMount := corev1.VolumeMount{
		Name:      "workspace",
		MountPath: mountPath,
	}
	assignmentMount := corev1.VolumeMount{
		Name:      "assignment",
		MountPath: "/etc/gocdnext",
		ReadOnly:  true,
	}

	// Prep init container — uses gocdnext-agent image so the
	// `gocdnext-agent prep` subcommand is on PATH. Same binary as
	// the agent itself; the subcommand keeps it proto-aware
	// without pulling a separate image.
	prepContainer := corev1.Container{
		Name:            "prep",
		Image:           k.cfg.AgentImage,
		ImagePullPolicy: imagePullPolicyFor(k.cfg.AgentImage),
		// --workspace MUST be the mount root: Prep itself derives
		// scriptWorkDir from the assignment's first checkout
		// target_dir and joins under this base. Passing the
		// already-joined workDir here would cause prep to clone
		// to /workspace/<target>/<target> and leave /workspace/<target>
		// (= task WorkingDir) empty.
		Command: []string{
			"gocdnext-agent", "prep",
			"--assignment=/etc/gocdnext/" + AssignmentSecretKey,
			"--workspace=" + mountPath,
		},
		WorkingDir:   mountPath,
		VolumeMounts: []corev1.VolumeMount{workspaceMount, assignmentMount},
	}
	if k.cfg.ForceImagePullAlways {
		prepContainer.ImagePullPolicy = corev1.PullAlways
	}

	// Task container — same shape as shared-mode task. Plugin
	// images keep their ENTRYPOINT; scripts get `sh -c -- <script>`.
	taskPullPolicy := imagePullPolicyFor(image)
	if k.cfg.ForceImagePullAlways {
		taskPullPolicy = corev1.PullAlways
	}
	taskContainer := corev1.Container{
		Name:            "task",
		Image:           image,
		ImagePullPolicy: taskPullPolicy,
		WorkingDir:      workDir,
		Env:             env,
		Resources:       buildResourceRequirements(spec.Resources),
		VolumeMounts:    []corev1.VolumeMount{workspaceMount},
	}
	if spec.Script != "" {
		taskContainer.Command = []string{"sh", "-c", "--", spec.Script}
	}

	// Housekeeper — kept alive after the task terminates so the
	// agent can exec into it to tar + stream out artifacts/cache
	// content via signed-URL PUT. Trivial alpine image; just needs
	// `sh` + `tar` (both in busybox).
	housekeeperContainer := corev1.Container{
		Name:            "housekeeper",
		Image:           k.cfg.HousekeeperImage,
		ImagePullPolicy: imagePullPolicyFor(k.cfg.HousekeeperImage),
		Command:         []string{"sh", "-c", "trap 'exit 0' TERM; while :; do sleep 3600 & wait $!; done"},
		WorkingDir:      workDir,
		VolumeMounts:    []corev1.VolumeMount{workspaceMount},
		// Minimal resources — housekeeper is idle waiting for exec.
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("10m"),
				corev1.ResourceMemory: resource.MustParse("16Mi"),
			},
		},
	}
	if k.cfg.ForceImagePullAlways {
		housekeeperContainer.ImagePullPolicy = corev1.PullAlways
	}

	containers := []corev1.Container{taskContainer, housekeeperContainer}

	if spec.Docker {
		privileged := true
		containers = append(containers, corev1.Container{
			Name:  "dind",
			Image: k.cfg.DinDImage,
			Env: []corev1.EnvVar{
				{Name: "DOCKER_TLS_CERTDIR", Value: ""},
			},
			Args: []string{
				"--host=tcp://0.0.0.0:" + strconv.Itoa(dindTCPPort),
				"--host=unix:///var/run/docker.sock",
			},
			SecurityContext: &corev1.SecurityContext{Privileged: &privileged},
			Ports: []corev1.ContainerPort{{
				Name:          "docker",
				ContainerPort: int32(dindTCPPort),
				Protocol:      corev1.ProtocolTCP,
			}},
		})
	}

	hostAliases := make([]corev1.HostAlias, 0, len(spec.HostAliases))
	for _, ha := range spec.HostAliases {
		hostAliases = append(hostAliases, corev1.HostAlias{
			IP:        ha.IP,
			Hostnames: append([]string(nil), ha.Hostnames...),
		})
	}

	labels := podLabelsIsolated(spec)

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      k.nowName(),
			Namespace: k.cfg.Namespace,
			Labels:    labels,
		},
		Spec: corev1.PodSpec{
			RestartPolicy:    corev1.RestartPolicyNever,
			NodeSelector:     mergeNodeSelector(k.cfg.NodeSelector, spec.NodeSelector),
			Tolerations:      concatTolerations(k.cfg.Tolerations, spec.Tolerations),
			ImagePullSecrets: pullSecrets,
			Volumes:          []corev1.Volume{workspaceVolume, assignmentVolume},
			InitContainers:   buildIsolatedInitContainers(prepContainer, spec, k.cfg.HousekeeperImage, workspaceMount),
			Containers:       containers,
			HostAliases:      hostAliases,
			// task container runs user-supplied code; mounting the
			// agent SA token is unnecessary and would expose API
			// access if the SA's RBAC is later widened by mistake.
			// Belt-and-suspenders defence against a permissive RBAC
			// regression slipping into job runtime.
			AutomountServiceAccountToken: ptr.To(false),
		},
	}, nil
}

// CacheFetchInitContainerName is the canonical name of the second
// init container that mediates `{{ hash "..." }}` cache-key
// resolution. Constant so the agent's isolated-mode orchestration
// can target it via PodExecutor.Exec (find/cat for hashing, tar
// over stdin to populate workspace, touch the marker file to
// signal exit) without string drift between the engine and runner
// packages.
const CacheFetchInitContainerName = "cache-fetch"

// CacheFetchReadyMarker is the workspace-relative path the
// cache-fetch init container's wait loop polls. When the agent
// finishes resolving + populating templated caches, it touches
// this file via exec; the cache-fetch container's `until [ -f ]`
// loop exits, K8s starts the main containers, and the task can
// run with caches already in place. Workspace-relative so the
// existing PVC mount carries it without any extra plumbing.
const CacheFetchReadyMarker = ".gocdnext/cache-done"

// buildIsolatedInitContainers assembles the init container list
// for an isolated-mode pod. `prep` is always first; `cache-fetch`
// is appended only when spec.NeedsCacheFetchInit is set (i.e., the
// assignment carries at least one templated cache key the agent
// must resolve against the post-checkout workspace).
//
// Order matters: K8s runs init containers sequentially, so
// cache-fetch starts only AFTER prep has terminated. That's the
// invariant the runner depends on — by the time the agent execs
// into cache-fetch, prep has already done the `git clone`, so
// `find` returns the files the operator's hash() globs match.
func buildIsolatedInitContainers(
	prep corev1.Container,
	spec IsolatedJobSpec,
	housekeeperImage string,
	workspaceMount corev1.VolumeMount,
) []corev1.Container {
	inits := []corev1.Container{prep}
	if !spec.NeedsCacheFetchInit {
		return inits
	}
	// cache-fetch — alpine sleep waiting for the agent to touch the
	// marker. We use the same image as housekeeper so the cluster's
	// image cache amortises across the two (alpine is tiny enough
	// the cold pull is sub-second, but warming once helps the next
	// job on the same node).
	//
	// The `until` loop polls every 0.2s. Faster polling shortens
	// the gap between marker-touch and main-container start; slower
	// would waste CPU. 0.2s is the lower bound at which `sleep` is
	// reliably granular across busybox/bash without measurable
	// busy-wait.
	cacheFetch := corev1.Container{
		Name:            CacheFetchInitContainerName,
		Image:           housekeeperImage,
		ImagePullPolicy: imagePullPolicyFor(housekeeperImage),
		Command: []string{
			"sh", "-c",
			// `mkdir -p` defends against the marker dir not existing
			// when prep didn't materialise it (e.g., prep skipped
			// checkout for a no-material job).
			"mkdir -p $(dirname /workspace/" + CacheFetchReadyMarker + ") && " +
				"until [ -f /workspace/" + CacheFetchReadyMarker + " ]; do sleep 0.2; done",
		},
		WorkingDir:   "/workspace",
		VolumeMounts: []corev1.VolumeMount{workspaceMount},
		// Tiny — the container just sleeps. Same shape as
		// housekeeper's idle profile.
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("10m"),
				corev1.ResourceMemory: resource.MustParse("16Mi"),
			},
		},
	}
	return append(inits, cacheFetch)
}

// CreateIsolatedJobPod is the high-level entry: builds the Secret
// for the assignment, creates the Pod referencing it, patches the
// Secret with an OwnerReference back to the Pod for GC, and
// returns the created Pod plus the secret name (so the caller can
// explicitly delete the secret after prep terminates — see
// HIGH/SEC finding in v0.5.0 review: don't keep the JobAssignment
// payload alive longer than necessary even when the Pod is kept
// for debugging).
//
// Failure modes:
//   - Secret create fails → return error, no pod created.
//   - Pod create fails → delete the orphan Secret, return error.
//   - Secret-owner patch fails → DELETE the secret eagerly (not
//     waiting for Pod GC, since the ownerRef didn't take), then
//     return the pod + error so the caller can decide whether to
//     also tear down the pod.
func (k *Kubernetes) CreateIsolatedJobPod(ctx context.Context, spec IsolatedJobSpec, assignmentBytes []byte) (*corev1.Pod, string, error) {
	if k.cfg.WorkspaceMode != WorkspaceModeIsolated {
		return nil, "", fmt.Errorf("create isolated pod: engine mode is %q, not isolated", k.cfg.WorkspaceMode)
	}
	// Decide names: pod name first (used by the secret too so the
	// orphan reaper can correlate via a shared prefix).
	podName := k.nowName()
	secretName := podName + "-assignment"
	spec.AssignmentSecretName = secretName

	// Create the secret first — pod mount references it. If pod
	// create fails afterwards we delete the secret to avoid leaks.
	secret, err := BuildAssignmentSecret(secretName, k.cfg.Namespace, assignmentBytes, spec.RunID, spec.JobID)
	if err != nil {
		return nil, "", err
	}
	if _, err := k.client.CoreV1().Secrets(k.cfg.Namespace).Create(ctx, secret, metav1.CreateOptions{}); err != nil {
		return nil, "", fmt.Errorf("create assignment secret: %w", err)
	}

	pod, err := k.BuildIsolatedJobPodSpec(spec)
	if err != nil {
		_ = k.client.CoreV1().Secrets(k.cfg.Namespace).Delete(ctx, secretName, metav1.DeleteOptions{})
		return nil, "", err
	}
	pod.Name = podName

	created, err := k.client.CoreV1().Pods(k.cfg.Namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		_ = k.client.CoreV1().Secrets(k.cfg.Namespace).Delete(ctx, secretName, metav1.DeleteOptions{})
		return nil, "", fmt.Errorf("create isolated pod: %w", err)
	}

	if err := k.PatchAssignmentSecretOwner(ctx, secretName, created); err != nil {
		// OwnerRef didn't take — delete the secret eagerly instead
		// of relying on Pod GC (which wouldn't cascade without the
		// ref). Caller still gets the pod so it can decide whether
		// to also clean it up.
		_ = k.client.CoreV1().Secrets(k.cfg.Namespace).Delete(ctx, secretName, metav1.DeleteOptions{})
		return created, "", fmt.Errorf("patch assignment secret owner: %w", err)
	}
	return created, secretName, nil
}

// DeleteIsolatedJobPod removes the pod (and via owner refs, the
// assignment Secret + the ephemeral PVC). Best-effort: a NotFound
// on either is treated as success — kubelet may have already GC'd
// after a long cleanup window.
func (k *Kubernetes) DeleteIsolatedJobPod(ctx context.Context, podName string) error {
	err := k.client.CoreV1().Pods(k.cfg.Namespace).Delete(ctx, podName, metav1.DeleteOptions{})
	if err != nil && !kerrors.IsNotFound(err) {
		return fmt.Errorf("delete isolated pod %s: %w", podName, err)
	}
	return nil
}

// WaitForInitStarted blocks until the named init container of the
// pod reaches Running or Terminated. Capped by StartupTimeout so a
// stuck Pending phase (PVC bind blocked, image pull backoff,
// unschedulable) surfaces as an error instead of hanging the job
// forever. The phase fallback (Succeeded/Failed) covers pods that
// already finished before this poll observes them — minimal test
// fixtures + races with very-fast jobs.
func (k *Kubernetes) WaitForInitStarted(ctx context.Context, podName, initContainerName string) error {
	startup, cancel := context.WithTimeout(ctx, k.cfg.StartupTimeout)
	defer cancel()
	return wait.PollUntilContextCancel(startup, k.cfg.PollInterval, true, func(ctx context.Context) (bool, error) {
		pod, err := k.client.CoreV1().Pods(k.cfg.Namespace).Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		for _, cs := range pod.Status.InitContainerStatuses {
			if cs.Name == initContainerName {
				if cs.State.Running != nil || cs.State.Terminated != nil {
					return true, nil
				}
			}
		}
		switch pod.Status.Phase {
		case corev1.PodSucceeded, corev1.PodFailed:
			return true, nil
		}
		return false, nil
	})
}

// WaitForInitTerminated blocks until the named init container of
// the pod reaches Terminated state and returns its exit code.
// Used by the runner to detect prep-phase success/failure before
// the main task container starts.
//
// Falls back to Pod phase for minimal test fixtures (mirrors
// waitForTaskTerminated semantics). Caller is expected to bound
// this with the job's own deadline — there is no per-init
// runtime cap here because the prep work is bounded by the
// JobAssignment's TimeoutSeconds.
func (k *Kubernetes) WaitForInitTerminated(ctx context.Context, podName, initContainerName string) (int, error) {
	var pod *corev1.Pod
	err := wait.PollUntilContextCancel(ctx, k.cfg.PollInterval, true, func(ctx context.Context) (bool, error) {
		p, err := k.client.CoreV1().Pods(k.cfg.Namespace).Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		pod = p
		for _, cs := range pod.Status.InitContainerStatuses {
			if cs.Name == initContainerName && cs.State.Terminated != nil {
				return true, nil
			}
		}
		switch pod.Status.Phase {
		case corev1.PodSucceeded, corev1.PodFailed:
			return true, nil
		}
		return false, nil
	})
	if err != nil {
		return -1, err
	}
	for _, cs := range pod.Status.InitContainerStatuses {
		if cs.Name == initContainerName && cs.State.Terminated != nil {
			return int(cs.State.Terminated.ExitCode), nil
		}
	}
	// Phase-only fallback: succeeded → 0, failed → -1.
	if pod.Status.Phase == corev1.PodSucceeded {
		return 0, nil
	}
	return -1, nil
}

// StreamInitLogs follows the named init container's log stream
// and emits one line per scanner.Text() into OnLine. Same shape
// as streamLogs but parameterised by container name (init prep vs
// regular task).
func (k *Kubernetes) StreamInitLogs(ctx context.Context, podName, initContainerName string, emit func(string, string)) {
	k.streamContainerLogs(ctx, podName, initContainerName, emit)
}

// WaitForTaskStarted is the post-init counterpart of
// WaitForInitStarted: caps the time the task container can spend
// in Waiting (ImagePullBackOff, missing secret/configmap mount,
// admission webhook lag). Without this, an ImagePullBackOff on
// the task image while the housekeeper sidecar runs Successfully
// leaves the Pod in phase=Running with task.State=Waiting; both
// WaitForTaskTerminated and the log streamer get stuck because
// neither has a hook for "started but never started running".
func (k *Kubernetes) WaitForTaskStarted(ctx context.Context, podName string) error {
	startup, cancel := context.WithTimeout(ctx, k.cfg.StartupTimeout)
	defer cancel()
	return wait.PollUntilContextCancel(startup, k.cfg.PollInterval, true, func(ctx context.Context) (bool, error) {
		pod, err := k.client.CoreV1().Pods(k.cfg.Namespace).Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		for _, cs := range pod.Status.ContainerStatuses {
			if cs.Name == "task" {
				if cs.State.Running != nil || cs.State.Terminated != nil {
					return true, nil
				}
			}
		}
		// Terminal pod state (e.g. an init container failed AFTER
		// our WaitForInitTerminated call returned a 0 — racy
		// fake-client fixtures) — stop polling either way.
		switch pod.Status.Phase {
		case corev1.PodSucceeded, corev1.PodFailed:
			return true, nil
		}
		return false, nil
	})
}

// StreamTaskLogs streams the "task" container's log stream.
// Exposed for the isolated-mode runner so it doesn't have to
// poke at internals.
func (k *Kubernetes) StreamTaskLogs(ctx context.Context, podName string, emit func(string, string)) {
	k.streamContainerLogs(ctx, podName, "task", emit)
}

// WaitForTaskTerminated is the exported counterpart of
// waitForTaskTerminated — for runner consumption in isolated mode.
func (k *Kubernetes) WaitForTaskTerminated(ctx context.Context, podName string) (int, error) {
	return k.waitForTaskTerminated(ctx, podName)
}

// streamContainerLogs is the generic implementation shared by
// StreamInitLogs and StreamTaskLogs.
//
// Retries opening the stream until the container is ready to emit
// logs OR the caller's context cancels. A freshly-created pod sits
// in PodInitializing / ContainerCreating for a beat before kubelet
// produces a log stream; without this loop a stream attempt issued
// right after Create returns "container not yet ready" and the
// caller silently gets ZERO log lines for the whole run — which is
// what the operator perceives as "no output, no clue why".
//
// The loop bounds itself by StartupTimeout (per attempt) so a
// permanently-unable pod can't pin the goroutine forever.
func (k *Kubernetes) streamContainerLogs(ctx context.Context, podName, containerName string, emit func(string, string)) {
	if emit == nil {
		return
	}
	deadline := time.Now().Add(k.cfg.StartupTimeout)
	for {
		if ctx.Err() != nil {
			return
		}
		req := k.client.CoreV1().Pods(k.cfg.Namespace).GetLogs(podName, &corev1.PodLogOptions{
			Container: containerName,
			Follow:    true,
		})
		stream, err := req.Stream(ctx)
		if err == nil {
			defer func() { _ = stream.Close() }()
			scanner := bufio.NewScanner(stream)
			scanner.Buffer(make([]byte, 64*1024), 1<<20)
			for scanner.Scan() {
				emit("stdout", scanner.Text())
			}
			return
		}
		if time.Now().After(deadline) {
			return
		}
		select {
		case <-time.After(k.cfg.PollInterval):
		case <-ctx.Done():
			return
		}
	}
}

// DeleteAssignmentSecret removes the assignment Secret that the
// init container consumed. Called by the runner once prep
// terminates so the JobAssignment payload (which may carry
// sensitive env/inputs) doesn't outlive the prep window even if
// the pod itself is kept around for debugging
// (CleanupOnFailure=false). Best-effort: NotFound is treated as
// success (the owner-ref GC may have already picked it up).
func (k *Kubernetes) DeleteAssignmentSecret(ctx context.Context, secretName string) error {
	err := k.client.CoreV1().Secrets(k.cfg.Namespace).Delete(ctx, secretName, metav1.DeleteOptions{})
	if err != nil && !kerrors.IsNotFound(err) {
		return fmt.Errorf("delete assignment secret %s: %w", secretName, err)
	}
	return nil
}

// podLabelsIsolated mirrors podLabels but bumps the `component`
// label to "isolated-job" so operators can grep by pattern without
// confusing them with shared-mode task pods.
func podLabelsIsolated(spec IsolatedJobSpec) map[string]string {
	labels := map[string]string{
		"app.kubernetes.io/name":       "gocdnext-job",
		"app.kubernetes.io/component":  "isolated-job",
		"app.kubernetes.io/managed-by": "gocdnext-agent",
	}
	if v, ok := sanitizeLabelValue(spec.RunID); ok {
		labels["gocdnext.io/run-id"] = v
	}
	if v, ok := sanitizeLabelValue(spec.JobID); ok {
		labels["gocdnext.io/job-id"] = v
	}
	if v, ok := sanitizeLabelValue(spec.Profile); ok {
		labels["gocdnext.io/profile"] = v
	}
	for _, t := range spec.AgentTags {
		v, ok := sanitizeLabelValue(t)
		if !ok {
			continue
		}
		labels["gocdnext.io/tag-"+v] = "true"
	}
	return labels
}
