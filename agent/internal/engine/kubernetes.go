package engine

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
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

// KubernetesConfig configures the K8s engine. Deployment pattern:
// the agent itself runs in-cluster with an emptyDir OR PVC at
// WorkspaceMountPath, and each job Pod it creates mounts the same
// volume. The agent prepares the workspace on its local filesystem
// (which IS the mounted volume), then creates a Pod whose main
// container's workingDir is that same path.
//
// ReadWriteMany PVC lets the agent and job Pods sit on different
// nodes. ReadWriteOnce works too but pins both to the same node via
// nodeAffinity (operator's responsibility).
type KubernetesConfig struct {
	Namespace          string
	KubeconfigPath     string // empty = in-cluster
	WorkspacePVCName   string // empty = emptyDir (same-Pod only)
	WorkspaceMountPath string // default "/workspace"
	DefaultImage       string // fallback when ScriptSpec.Image is empty
	ImagePullSecrets   []string
	NodeSelector       map[string]string

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
}

// Kubernetes is an Engine that runs each script as a Pod in the
// configured namespace. Thread-safe — RunScript has no shared
// mutable state.
type Kubernetes struct {
	client  kubernetes.Interface
	cfg     KubernetesConfig
	nowName func() string // replaceable in tests
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
	return NewKubernetesWithClient(client, cfg), nil
}

// NewKubernetesWithClient is the test seam: supply a (possibly fake)
// clientset directly.
func NewKubernetesWithClient(client kubernetes.Interface, cfg KubernetesConfig) *Kubernetes {
	applyKubernetesDefaults(&cfg)
	return &Kubernetes{
		client:  client,
		cfg:     cfg,
		nowName: defaultPodName,
	}
}

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

	env := make([]corev1.EnvVar, 0, len(spec.Env)+1)
	for k, v := range spec.Env {
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

	containers := []corev1.Container{{
		Name:       "task",
		Image:      image,
		Command:    []string{"sh", "-c", spec.Script},
		WorkingDir: workDir,
		Env:        env,
		VolumeMounts: []corev1.VolumeMount{{
			Name:      "workspace",
			MountPath: k.cfg.WorkspaceMountPath,
		}},
	}}

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

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      k.nowName(),
			Namespace: k.cfg.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":       "gocdnext-job",
				"app.kubernetes.io/component":  "task",
				"app.kubernetes.io/managed-by": "gocdnext-agent",
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:    corev1.RestartPolicyNever,
			NodeSelector:     k.cfg.NodeSelector,
			ImagePullSecrets: pullSecrets,
			Volumes:          []corev1.Volume{workspaceVolume},
			Containers:       containers,
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
