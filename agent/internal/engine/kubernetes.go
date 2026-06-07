package engine

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// movingTagPattern marks image tags that point to a moving target
// (`latest`, named channels, and shortened semver like `v1` or
// `v0.4` which we ourselves publish on every release per
// .github/workflows/release.yml + plugins.yml). When a tag matches,
// imagePullPolicyFor returns PullAlways so the agent re-resolves the
// manifest on every job — otherwise k8s defaults to IfNotPresent
// for non-`latest` tags and a node with a stale `:v1` cached image
// keeps using the old build indefinitely after we cut a release.
//
// Full semver (`v1.2.3`, `0.4.9`), SHA-prefixed (`sha-abc123`) and
// arbitrary pinned tags fall to IfNotPresent: the user pinned
// tightly to opt OUT of per-job re-pulls.
var movingTagPattern = regexp.MustCompile(
	`^(latest|v\d+|v\d+\.\d+|main|master|dev|develop|edge|nightly|stable)$`,
)

// imagePullPolicyFor picks Always vs IfNotPresent based on whether
// the image reference points at a moving target. Treats:
//
//	`<image>@sha256:...`          → IfNotPresent (digest-pinned, immutable)
//	`` (empty)                    → Always       (falls back to default image)
//	`<image>` (no tag)            → Always       (implicit `:latest`)
//	`<image>:latest`              → Always
//	`<image>:v1`, `:v0.4`         → Always       (major / major.minor channels)
//	`<image>:main|nightly|dev|…`  → Always       (named channels)
//	everything else               → IfNotPresent (pinned semver, SHA tags, …)
//
// Defined at package level + tested separately so kubernetes.go stays
// the consumer of a tiny, deterministic policy decision.
func imagePullPolicyFor(image string) corev1.PullPolicy {
	if image == "" {
		return corev1.PullAlways
	}
	// Digest references are by construction immutable; the docker
	// content-addressable store guarantees identical bytes for the
	// same sha256, so re-pulling buys nothing.
	if strings.Contains(image, "@") {
		return corev1.PullIfNotPresent
	}
	tag := extractImageTag(image)
	if tag == "" {
		// No tag = implicit :latest = moving by docker convention.
		return corev1.PullAlways
	}
	if movingTagPattern.MatchString(tag) {
		return corev1.PullAlways
	}
	return corev1.PullIfNotPresent
}

// extractImageTag returns the tag portion of an image reference or
// "" when there is none. Handles `registry:port/image:tag` by only
// scanning for the last `:` after the last `/` — a naive
// LastIndex(":") would mistakenly read the registry port as the tag
// for inputs like `localhost:5000/foo`.
func extractImageTag(image string) string {
	afterLastSlash := image
	if i := strings.LastIndex(image, "/"); i != -1 {
		afterLastSlash = image[i+1:]
	}
	if i := strings.LastIndex(afterLastSlash, ":"); i != -1 {
		return afterLastSlash[i+1:]
	}
	return ""
}

// WorkspaceMode selects how the job's filesystem is provisioned.
//
//   - WorkspaceModeShared: the agent itself runs in-cluster with a
//     PVC at WorkspaceMountPath, and each job Pod it creates mounts
//     the SAME PVC. The agent prepares the workspace on its local
//     filesystem (which IS the mounted volume), then creates a Pod
//     whose main container's workingDir is that same path. Requires
//     RWX storage class so the agent and job pods can co-mount.
//     Pre-v0.5.0 behaviour; preserved for backward compatibility.
//
//   - WorkspaceModeIsolated: each job Pod owns an ephemeral PVC
//     provisioned via volume.ephemeral. The Pod has an init
//     container ("prep") that runs `gocdnext-agent prep` inside the
//     pod — clones materials, downloads upstream artefacts, fetches
//     caches — then the main "task" container runs the user's
//     command, plus a "housekeeper" sidecar the agent execs into for
//     post-task work (artifact upload, cache store). PVC dies with
//     the pod. Works with any storage class (RWO is fine since each
//     pod has its own PVC). Default new mode.
type WorkspaceMode string

const (
	WorkspaceModeShared   WorkspaceMode = "shared"
	WorkspaceModeIsolated WorkspaceMode = "isolated"
)

// KubernetesConfig configures the K8s engine. See WorkspaceMode for
// how the workspace volume is wired in each mode.
type KubernetesConfig struct {
	Namespace          string
	KubeconfigPath     string // empty = in-cluster
	WorkspacePVCName   string // shared mode: PVC the agent + jobs co-mount. Ignored in isolated mode.
	WorkspaceMountPath string // default "/workspace"
	DefaultImage       string // fallback when ScriptSpec.Image is empty
	ImagePullSecrets   []string
	NodeSelector       map[string]string

	// WorkspaceMode picks shared (legacy) vs isolated (per-job
	// ephemeral PVC). Empty defaults to WorkspaceModeShared so
	// existing deployments keep working.
	WorkspaceMode WorkspaceMode

	// WorkspaceStorageClass names the storage class used for the
	// per-job ephemeral PVC in isolated mode. Empty means "cluster
	// default". Operator picks pd-ssd / local-ssd / etc. depending
	// on what their cluster offers and how much they care about
	// pod-startup time vs throughput. Ignored in shared mode.
	WorkspaceStorageClass string

	// WorkspaceSize is the requested size of the per-job ephemeral
	// PVC in isolated mode (a k8s resource.Quantity literal like
	// "20Gi"). Empty defaults to "20Gi". Ignored in shared mode.
	WorkspaceSize string

	// HousekeeperImage is the small image used for the "housekeeper"
	// sidecar in isolated mode — the container the agent execs into
	// after the task terminates to tar workspace files + stream them
	// to signed upload URLs. Needs `tar` + `sh` available. Default
	// "alpine:3.19".
	HousekeeperImage string

	// AgentImage is the image used for the "prep" init container in
	// isolated mode — must be the same gocdnext-agent binary the
	// agent itself runs so `gocdnext-agent prep` is on PATH. Empty
	// defaults to a sentinel that fails BuildPodSpec loudly: operator
	// must configure this via the Helm chart.
	AgentImage string

	// DinDImage is the image used for the Docker-in-Docker sidecar
	// when a job sets `docker: true`. Default "docker:24-dind".
	// Operators who need a pinned-digest or mirror-sourced image
	// override here; anyone on the default gets the upstream
	// moby/dind release stream.
	DinDImage string

	// PollInterval controls how often we poll the Pod status
	// transition. Default 1s; tests set lower.
	PollInterval time.Duration
	// StartupTimeout caps the Pending → Running transition. A Pod
	// that can't pull its image in this window is reported as
	// failed. Default 5 min.
	StartupTimeout time.Duration
	// CleanupOnSuccess deletes the Pod after a successful run.
	// False keeps it for operator debugging.
	CleanupOnSuccess bool
	// CleanupOnFailure ditto for non-zero exits.
	CleanupOnFailure bool

	// ForceImagePullAlways overrides the per-tag pull policy heuristic
	// (imagePullPolicyFor) and sets ImagePullPolicy: Always on every
	// task container regardless of tag. Default false → moving tags
	// (`v1`, `latest`, named channels) get Always, pinned semver and
	// digest refs get IfNotPresent. Useful for clusters fronted by a
	// registry mirror where a HEAD against the cache is cheap and the
	// operator wants every job to re-resolve the manifest — including
	// "pinned" tags that an internal registry may have been retagged
	// under the same name.
	ForceImagePullAlways bool
}

// Kubernetes is an Engine that runs each script as a Pod in the
// configured namespace. Thread-safe — RunScript has no shared
// mutable state.
type Kubernetes struct {
	client   kubernetes.Interface
	restCfg  *rest.Config // nil in fake-client paths; required for exec
	executor PodExecutor  // injected; defaults to NewSPDYExecutor when restCfg is set
	cfg      KubernetesConfig
	nowName  func() string // replaceable in tests
}

// NewKubernetes constructs the engine. Returns an error when
// kubeconfig is bad or the client can't be built; that lets the
// agent bail at startup instead of failing the first job.
func NewKubernetes(cfg KubernetesConfig) (*Kubernetes, error) {
	if cfg.Namespace == "" {
		return nil, errors.New("engine: kubernetes: namespace is required")
	}
	restCfg, err := loadRESTConfig(cfg.KubeconfigPath)
	if err != nil {
		return nil, err
	}
	client, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("engine: kubernetes: build client: %w", err)
	}
	k := NewKubernetesWithClient(client, cfg)
	k.restCfg = restCfg
	k.executor = NewSPDYExecutor(client, restCfg, cfg.Namespace)
	return k, nil
}

// NewKubernetesWithClient is the test seam: supply a (possibly fake)
// clientset directly. Tests that need exec inject a fake PodExecutor
// via SetExecutor; the default executor is nil so a test that exercises
// isolated-mode exec without injecting fails loudly instead of dialling
// a real cluster.
func NewKubernetesWithClient(client kubernetes.Interface, cfg KubernetesConfig) *Kubernetes {
	applyKubernetesDefaults(&cfg)
	return &Kubernetes{
		client:  client,
		cfg:     cfg,
		nowName: defaultPodName,
	}
}

// SetExecutor injects a PodExecutor — test seam for isolated-mode
// exec flows. Production code uses the SPDY executor wired in
// NewKubernetes.
func (k *Kubernetes) SetExecutor(e PodExecutor) { k.executor = e }

// Executor returns the configured PodExecutor (or nil if none was
// injected — fake-client paths default to nil).
func (k *Kubernetes) Executor() PodExecutor { return k.executor }

// Config returns a copy of the engine's config — read-only access
// for the runner to decide between shared and isolated dispatch
// without re-reading env vars.
func (k *Kubernetes) Config() KubernetesConfig { return k.cfg }

func applyKubernetesDefaults(cfg *KubernetesConfig) {
	if cfg.WorkspaceMountPath == "" {
		cfg.WorkspaceMountPath = "/workspace"
	}
	if cfg.DefaultImage == "" {
		cfg.DefaultImage = "alpine:3.19"
	}
	if cfg.DinDImage == "" {
		cfg.DinDImage = "docker:24-dind"
	}
	if cfg.HousekeeperImage == "" {
		cfg.HousekeeperImage = "alpine:3.19"
	}
	if cfg.WorkspaceMode == "" {
		cfg.WorkspaceMode = WorkspaceModeShared
	}
	if cfg.WorkspaceSize == "" {
		cfg.WorkspaceSize = "20Gi"
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = time.Second
	}
	if cfg.StartupTimeout <= 0 {
		cfg.StartupTimeout = 5 * time.Minute
	}
}

// TCP port the DinD sidecar listens on. localhost-only (same Pod =
// same netns), TLS off by way of DOCKER_TLS_CERTDIR="". MVP picks
// unencrypted intra-pod over the cert-dance because the daemon
// is already privileged — the trust boundary is the Pod, not the
// socket layer.
const dindTCPPort = 2375

// dindHost is what the task container's DOCKER_HOST points at.
// Exposed as a const so tests can assert on the value without
// hardcoding the port in two places.
const dindHost = "tcp://localhost:2375"

// Name identifies the engine for log/metric labels.
func (*Kubernetes) Name() string { return "kubernetes" }

// RunScript spawns a Pod running the script, streams its logs into
// OnLine, and returns the container's exit code. See Engine for the
// err contract.
func (k *Kubernetes) RunScript(ctx context.Context, spec ScriptSpec) (int, error) {
	// Outputs env injection happens inside BuildPodSpec so the
	// Pod is testable end-to-end without driving the fake Create
	// reactor. See BuildPodSpec for the workDir-anchored fix.
	pod := k.BuildPodSpec(spec)

	created, err := k.client.CoreV1().Pods(k.cfg.Namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		return -1, fmt.Errorf("engine: kubernetes: create pod: %w", err)
	}
	name := created.Name

	var finalExit int
	runErr := func() error {
		if err := k.waitForRunning(ctx, name); err != nil {
			return err
		}
		// Stream logs concurrently with waiting for terminal state.
		streamDone := make(chan struct{})
		go func() {
			defer close(streamDone)
			k.streamLogs(ctx, name, spec.OnLine)
		}()
		exit, err := k.waitForTaskTerminated(ctx, name)
		<-streamDone
		finalExit = exit
		return err
	}()

	// DinD never self-terminates — when the task exits, the Pod
	// phase would stay Running until the sidecar is killed. Force
	// cleanup on Docker jobs so the daemon doesn't leak into the
	// next scheduled Pod on the same node.
	success := runErr == nil && finalExit == 0
	if spec.Docker {
		_ = k.client.CoreV1().Pods(k.cfg.Namespace).Delete(ctx, name, metav1.DeleteOptions{})
	} else {
		k.maybeCleanup(ctx, name, success)
	}

	if runErr != nil {
		return -1, runErr
	}
	return finalExit, nil
}

// BuildPodSpec is exported for tests that want to assert on the
// materialised shape without actually submitting it. Pod name is
// generated here so each spec is reproducible enough to inspect.
func (k *Kubernetes) BuildPodSpec(spec ScriptSpec) *corev1.Pod {
	image := spec.Image
	if image == "" {
		image = k.cfg.DefaultImage
	}
	workDir := spec.WorkDir
	if workDir == "" {
		workDir = k.cfg.WorkspaceMountPath
	}

	// Outputs (issue #10): anchor GOCDNEXT_OUTPUT_FILE at workDir
	// (= scriptWorkDir), NOT at the mount root. Shared K8s mounts
	// the WHOLE PVC at ContainerWorkspaceMount, so a checkout with
	// target_dir puts scriptWorkDir at <mount>/<target_dir>;
	// prepareOutputsFile creates the file under THAT path. Joining
	// the env with the mount root instead would point
	// `> $GOCDNEXT_OUTPUT_FILE` at a sibling directory the agent
	// never created (same bug shape that bit isolated mode and
	// surfaced in review). Inject inline here so BuildPodSpec is
	// the single source of truth and tests can assert env without
	// going through the fake clientset's Create reactor.
	specEnv := spec.Env
	if spec.OutputsHostPath != "" && spec.OutputsRelPath != "" {
		specEnv = withOutputsEnv(specEnv, path.Join(workDir, spec.OutputsRelPath))
	}

	env := make([]corev1.EnvVar, 0, len(specEnv)+1)
	for k, v := range specEnv {
		env = append(env, corev1.EnvVar{Name: k, Value: v})
	}
	if spec.Docker {
		// Point the task container's Docker clients at the DinD
		// sidecar over localhost TCP. `DOCKER_TLS_CERTDIR=""` is
		// honoured by the official dind image to disable TLS,
		// which keeps the port the same on both sides.
		env = append(env,
			corev1.EnvVar{Name: "DOCKER_HOST", Value: dindHost},
			corev1.EnvVar{Name: "DOCKER_TLS_CERTDIR", Value: ""},
		)
	}

	pullSecrets := make([]corev1.LocalObjectReference, 0, len(k.cfg.ImagePullSecrets))
	for _, n := range k.cfg.ImagePullSecrets {
		pullSecrets = append(pullSecrets, corev1.LocalObjectReference{Name: n})
	}

	workspaceVolume := corev1.Volume{Name: "workspace"}
	if k.cfg.WorkspacePVCName != "" {
		workspaceVolume.VolumeSource = corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: k.cfg.WorkspacePVCName,
			},
		}
	} else {
		workspaceVolume.VolumeSource = corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{
				SizeLimit: func() *resource.Quantity {
					q := resource.MustParse("10Gi")
					return &q
				}(),
			},
		}
	}

	pullPolicy := imagePullPolicyFor(image)
	if k.cfg.ForceImagePullAlways {
		// Operator override: cluster routes pulls through a registry
		// cache (HEAD is cheap, body served from local mirror) and
		// wants every job to re-resolve the manifest regardless of
		// the tag's apparent immutability. Pinned tags too — defends
		// against an operator retagging a "pinned" version in their
		// internal registry under the same name.
		pullPolicy = corev1.PullAlways
	}
	taskContainer := corev1.Container{
		Name:            "task",
		Image:           image,
		ImagePullPolicy: pullPolicy,
		WorkingDir:      workDir,
		Env:             env,
		Resources:       buildResourceRequirements(spec.Resources),
		VolumeMounts: []corev1.VolumeMount{{
			Name:      "workspace",
			MountPath: k.cfg.WorkspaceMountPath,
		}},
	}
	// Empty Script = plugin task: the image's own ENTRYPOINT is the
	// logic and PLUGIN_* env vars carry the inputs. Leaving Command
	// (and Args) nil tells Kubernetes to use the image's ENTRYPOINT
	// + CMD as declared — matching the docker engine's behaviour at
	// engine/docker.go:253. Forcing `sh -c ""` here previously made
	// every plugin task no-op with exit 0 ("success but nothing
	// happened").
	if spec.Script != "" {
		// `--` after -c stops sh's option parsing — without it, a
		// user script literal starting with `-` (e.g. plugin command
		// like `-m uv sync`) would be interpreted as a flag and the
		// container would die at startup with `sh: - : invalid
		// option`. Same fix applied in the docker + shell engines.
		taskContainer.Command = []string{"sh", "-c", "--", spec.Script}
	}
	containers := []corev1.Container{taskContainer}

	if spec.Docker {
		// DinD runs as a plain sibling container (not an init
		// sidecar) for compatibility with k8s < 1.29 that don't
		// have the native sidecar pattern. The task typically
		// sleeps a beat before its first docker call, or uses a
		// retry loop — dind needs ~1-2s to come up. We do NOT
		// wait ourselves: the daemon readiness check belongs in
		// user code, same as real-world Woodpecker/GitLab setups.
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

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      k.nowName(),
			Namespace: k.cfg.Namespace,
			Labels:    podLabels(spec),
		},
		Spec: corev1.PodSpec{
			RestartPolicy:    corev1.RestartPolicyNever,
			NodeSelector:     k.cfg.NodeSelector,
			ImagePullSecrets: pullSecrets,
			Volumes:          []corev1.Volume{workspaceVolume},
			Containers:       containers,
			HostAliases:      hostAliases,
		},
	}
}

// waitForRunning blocks until the Pod reports Running OR reaches a
// terminal state (which means the container likely failed before it
// started — bad image, schedule failure). StartupTimeout caps the
// wait so a stuck Pending (image pull backoff, unschedulable) is
// surfaced as an error instead of hanging.
func (k *Kubernetes) waitForRunning(ctx context.Context, name string) error {
	startup, cancel := context.WithTimeout(ctx, k.cfg.StartupTimeout)
	defer cancel()
	return wait.PollUntilContextCancel(startup, k.cfg.PollInterval, true, func(ctx context.Context) (bool, error) {
		pod, err := k.client.CoreV1().Pods(k.cfg.Namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		switch pod.Status.Phase {
		case corev1.PodRunning, corev1.PodSucceeded, corev1.PodFailed:
			return true, nil
		}
		return false, nil
	})
}

// waitForTaskTerminated polls the task container's status and
// returns its exit code as soon as it reaches Terminated —
// regardless of whether other containers in the Pod are still
// running. This matters for DinD jobs: the dind sidecar doesn't
// self-exit, so Pod phase never reaches Succeeded/Failed, but
// the user's script (the task container) is already done and
// we can report the result. Falls back to pod phase for older
// fake clients in tests that only update phase without per-
// container status.
func (k *Kubernetes) waitForTaskTerminated(ctx context.Context, name string) (int, error) {
	var pod *corev1.Pod
	err := wait.PollUntilContextCancel(ctx, k.cfg.PollInterval, true, func(ctx context.Context) (bool, error) {
		p, err := k.client.CoreV1().Pods(k.cfg.Namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		pod = p
		for _, cs := range pod.Status.ContainerStatuses {
			if cs.Name == "task" && cs.State.Terminated != nil {
				return true, nil
			}
		}
		// Fallback for minimal test fixtures (or Pods that
		// failed so early no container status exists): if the
		// Pod itself reports terminal, stop polling.
		switch pod.Status.Phase {
		case corev1.PodSucceeded, corev1.PodFailed:
			return true, nil
		}
		return false, nil
	})
	if err != nil {
		return -1, err
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name == "task" && cs.State.Terminated != nil {
			return int(cs.State.Terminated.ExitCode), nil
		}
	}
	if pod.Status.Phase == corev1.PodSucceeded {
		return 0, nil
	}
	return -1, nil
}

// streamLogs follows the task container's log stream and emits one
// line per scanner.Text() into OnLine. Errors opening or reading the
// stream are logged to stderr by the agent host path — here we
// silently return because the caller still gets the exit code via
// waitForTerminal.
func (k *Kubernetes) streamLogs(ctx context.Context, name string, emit func(string, string)) {
	if emit == nil {
		return
	}
	req := k.client.CoreV1().Pods(k.cfg.Namespace).GetLogs(name, &corev1.PodLogOptions{
		Container: "task",
		Follow:    true,
	})
	stream, err := req.Stream(ctx)
	if err != nil {
		return
	}
	defer func() { _ = stream.Close() }()
	scanner := bufio.NewScanner(stream)
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	for scanner.Scan() {
		emit("stdout", scanner.Text())
	}
	_ = io.Discard
}

// maybeCleanup deletes the Pod if CleanupOn{Success,Failure} enables
// it. Best-effort; delete failures are swallowed (the Pod will be
// garbage-collected by K8s eventually, and a stuck Pod is a signal
// the operator wants to see anyway).
func (k *Kubernetes) maybeCleanup(ctx context.Context, name string, success bool) {
	keep := (!success && !k.cfg.CleanupOnFailure) || (success && !k.cfg.CleanupOnSuccess)
	if keep {
		return
	}
	_ = k.client.CoreV1().Pods(k.cfg.Namespace).Delete(ctx, name, metav1.DeleteOptions{})
}

// loadRESTConfig picks in-cluster config when kubeconfig is empty,
// else reads the kubeconfig path.
func loadRESTConfig(kubeconfigPath string) (*rest.Config, error) {
	if kubeconfigPath != "" {
		cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
		if err != nil {
			return nil, fmt.Errorf("engine: kubernetes: kubeconfig %s: %w", kubeconfigPath, err)
		}
		return cfg, nil
	}
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("engine: kubernetes: in-cluster config: %w", err)
	}
	return cfg, nil
}

// defaultPodName generates a DNS-1123 compliant name no longer than
// 63 chars.
func defaultPodName() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return "gocdnext-job-" + strings.ToLower(hex.EncodeToString(b))
}

// Silences staticcheck for unused import when IsAlreadyExists isn't
// needed anymore in a refactor.
var _ = kerrors.IsAlreadyExists

// buildResourceRequirements maps the engine-level Resources struct
// into a corev1.ResourceRequirements. Empty strings are skipped
// (so a partially-set spec round-trips into a partial PodSpec —
// kubelet defaults the rest). Bad quantities are dropped silently:
// the apply-time resolver validated them upstream, so reaching this
// branch means a caller bypassed the resolver — better to ship the
// pod with the well-formed half than fail the job over an
// invariant violation we can't surface as a useful error here.
func buildResourceRequirements(r Resources) corev1.ResourceRequirements {
	if r.IsZero() {
		return corev1.ResourceRequirements{}
	}
	out := corev1.ResourceRequirements{}
	if q, ok := parseQuantitySilent(r.CPURequest); ok {
		if out.Requests == nil {
			out.Requests = corev1.ResourceList{}
		}
		out.Requests[corev1.ResourceCPU] = q
	}
	if q, ok := parseQuantitySilent(r.MemoryRequest); ok {
		if out.Requests == nil {
			out.Requests = corev1.ResourceList{}
		}
		out.Requests[corev1.ResourceMemory] = q
	}
	if q, ok := parseQuantitySilent(r.CPULimit); ok {
		if out.Limits == nil {
			out.Limits = corev1.ResourceList{}
		}
		out.Limits[corev1.ResourceCPU] = q
	}
	if q, ok := parseQuantitySilent(r.MemoryLimit); ok {
		if out.Limits == nil {
			out.Limits = corev1.ResourceList{}
		}
		out.Limits[corev1.ResourceMemory] = q
	}
	return out
}

func parseQuantitySilent(s string) (resource.Quantity, bool) {
	if s == "" {
		return resource.Quantity{}, false
	}
	q, err := resource.ParseQuantity(s)
	if err != nil {
		return resource.Quantity{}, false
	}
	return q, true
}

// podLabels stamps the static gocdnext labels plus the resolved
// profile name + agent tags so operators can grep workloads by
// pool ("kubectl get pods -l gocdnext.io/profile=gpu") without
// trawling agent logs. Tag values that violate DNS-1123 land
// silently dropped — k8s would reject the Pod creation otherwise,
// and the diagnostic value (which-pool) is preserved by the
// surviving labels.
func podLabels(spec ScriptSpec) map[string]string {
	labels := map[string]string{
		"app.kubernetes.io/name":       "gocdnext-job",
		"app.kubernetes.io/component":  "task",
		"app.kubernetes.io/managed-by": "gocdnext-agent",
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

// sanitizeLabelValue is a best-effort DNS-1123 check: k8s allows
// label values matching `[a-z0-9A-Z]([-_.a-z0-9A-Z]*[a-z0-9A-Z])?`,
// up to 63 chars. Anything else is dropped — admin-provided tag
// names are typically already conformant; this guards against the
// rare typo from making a Pod uncreatable.
func sanitizeLabelValue(s string) (string, bool) {
	if s == "" || len(s) > 63 {
		return "", false
	}
	first := s[0]
	last := s[len(s)-1]
	if !isLabelEdge(first) || !isLabelEdge(last) {
		return "", false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !isLabelEdge(c) && c != '-' && c != '_' && c != '.' {
			return "", false
		}
	}
	return s, true
}

func isLabelEdge(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}
