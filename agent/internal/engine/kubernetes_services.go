package engine

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
)

// k8sServiceNameRE enforces DNS-1123 label rules and adds a tighter
// length cap (32 chars) so the resulting pod name
//
//	gocdnext-svc-<12-hex-of-runid>-<svcname>
//
// stays under the 63-char DNS label limit. Critical: the value comes
// from pipeline YAML — without strict validation, a malicious name
// like `x` * 200 or `Invalid.NAME` would either fail at API submit
// time with a noisy error or, worse, sneak into a label value where
// it can collide with another job's resources.
var k8sServiceNameRE = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,30}[a-z0-9])?$`)

// EnsureServices brings up one Pod per declared service, SCOPED TO
// THE RUN (not to the job). Pod name is derived from runID so every
// job of the same run resolves to the same pod via the deterministic
// name — first creator wins, every subsequent job's Create returns
// AlreadyExists and the engine just Gets the existing PodIP. The
// HostAliases wireup the runner threads into ScriptSpec lets the
// job pod resolve `postgres:5432` via /etc/hosts without needing a
// k8s Service object or per-job DNS.
//
// Pod naming: gocdnext-svc-<runshort>-<svcname>. Pods are labelled
// with `gocdnext.io/run-id=<runID>` so the server's
// CleanupRunServices RPC (issued on run terminal) can sweep them
// with a single label-selector delete instead of needing to know
// per-pod names.
//
// Lifecycle:
//   - Per-job teardown is GONE: the returned Cleanup is a no-op.
//     Killing the service when ONE of N jobs finishes would break
//     the other N-1 jobs still using it.
//   - Run-terminal teardown is the server's job: when the run
//     reaches terminal (CompleteJob cascade OR Cancel path), the
//     server BROADCASTS CleanupRunServices(runID) to a wide target
//     set — agents that ran a job of the run UNION every connected
//     agent — and each agent's k8s engine does
//     `kubectl delete pods -l app.kubernetes.io/managed-by=gocdnext-agent,
//     app.kubernetes.io/component=service,gocdnext.io/run-id=<runID>`.
//     Multi-agent races resolve via NotFound (success).
//   - Pods leak (until manual cleanup with `kubectl delete pods
//     -l app.kubernetes.io/managed-by=gocdnext-agent,gocdnext.io/run-id=<id>`)
//     only when the server's engine-filtered target set is empty
//     OR every targeted dispatch errors. A non-k8s agent's no-op
//     does NOT cause a leak here because the server's SQL/in-mem
//     filters route the message away from those engines.
//
// Errors:
//   - empty services → no-op ServicesWireup with a noop cleanup.
//   - invalid name / empty image → fail before any API call.
//   - pod creation conflict NOT due to label match → real error
//     (a previous leak from a prior gocdnext version with the same
//     runID — extremely unlikely, but surfaced loudly).
//   - podIP wait timeout (StartupTimeout) → bubble up; the pod is
//     left in place. A sibling job of the same run will see the
//     failed-state pod via AlreadyExists and surface a refuse-to-
//     reuse error so the operator can delete it manually.
//
// Concurrency: the wait-for-podIP step runs in parallel via
// errgroup since image pulls dominate and serialising would
// multiply latency by the number of services.
func (k *Kubernetes) EnsureServices(
	ctx context.Context,
	services []ServiceSpec,
	runID, jobID string,
	log func(stream, text string),
	onLifecycle func(ServiceLifecycleEvent),
) (ServicesWireup, error) {
	noop := ServicesWireup{Cleanup: func() {}}
	if len(services) == 0 {
		return noop, nil
	}
	if runID == "" {
		return noop, errors.New("kubernetes engine: EnsureServices needs a non-empty runID for run-scoped pod naming")
	}
	emit := func(evt ServiceLifecycleEvent) {
		if onLifecycle == nil {
			return
		}
		onLifecycle(evt)
	}

	runShort := shortDockerID(runID)
	prefix := "gocdnext-svc-" + runShort + "-"

	// seen guards against the same name being declared twice in
	// one pipeline — the server-side parser SHOULD reject this,
	// but defending here avoids a confusing "pod already exists"
	// error mid-create.
	seen := make(map[string]struct{}, len(services))
	for _, svc := range services {
		if _, dup := seen[svc.Name]; dup {
			return noop, fmt.Errorf("kubernetes engine: service name %q declared twice", svc.Name)
		}
		seen[svc.Name] = struct{}{}
		if !k8sServiceNameRE.MatchString(svc.Name) {
			return noop, fmt.Errorf(
				"kubernetes engine: service name %q is invalid — expected DNS-1123 label (lowercase, dashes, starts with a letter), max 32 chars",
				svc.Name)
		}
		if svc.Image == "" {
			return noop, fmt.Errorf("kubernetes engine: service %q has empty image", svc.Name)
		}
	}

	// Cleanup is a no-op in the run-scoped model: per-job teardown
	// would kill services other jobs of the same run still need.
	// Run-terminal cleanup is the server's job — see
	// dispatchRunServiceCleanup in connect.go.
	cleanup := func() {}

	// Track whether this agent CREATED a pod vs reused a sibling's.
	// Emitting `starting` for the creator + skipping for the reuser
	// keeps the server-side row's started_at anchored to the FIRST
	// agent's create, not whichever sibling happened to call last.
	created := make(map[string]bool, len(services))
	for _, svc := range services {
		name := prefix + svc.Name
		pod := k.buildServicePod(name, svc, runID, jobID)
		if log != nil {
			log("stdout", fmt.Sprintf("$ starting service %s (%s)", svc.Name, svc.Image))
		}
		_, err := k.client.CoreV1().Pods(k.cfg.Namespace).Create(ctx, pod, metav1.CreateOptions{})
		switch {
		case err == nil:
			// We created it. waitForPodIP below will block on its
			// status; nothing else to do here.
			created[svc.Name] = true
			emit(ServiceLifecycleEvent{
				Name:    svc.Name,
				Image:   svc.Image,
				PodName: name,
				Status:  "starting",
			})
		case kerrors.IsAlreadyExists(err):
			// Another job of THIS run got here first — ideally.
			// Before trusting reuse, Get the existing pod and
			// validate it carries OUR labels: same managed-by,
			// component=service, run-id=our-run, service=our-svc.
			// Without this check a stale pod from a previous
			// gocdnext version, a name collision (12-hex prefix is
			// ~10^14 space but not infinite), or an unrelated
			// operator-deployed pod sharing the namespace could be
			// silently adopted — and never cleaned up since our
			// label-selector delete wouldn't match it back.
			existing, getErr := k.client.CoreV1().Pods(k.cfg.Namespace).Get(ctx, name, metav1.GetOptions{})
			if getErr != nil {
				return noop, fmt.Errorf("kubernetes engine: pod %s exists but Get failed: %w", name, getErr)
			}
			if err := assertOurServicePod(existing, runID, svc.Name); err != nil {
				return noop, fmt.Errorf("kubernetes engine: refusing to reuse pod %s: %w — delete it manually and retry", name, err)
			}
			if log != nil {
				log("stdout", fmt.Sprintf("$ reusing service %s from sibling job (run-scoped)", svc.Name))
			}
		default:
			return noop, fmt.Errorf("kubernetes engine: create service pod %s: %w", name, err)
		}
	}

	// Wait in parallel for each service's podIP. Image pulls (the
	// slow part) run concurrently on the kubelet anyway; serialising
	// the wait here would gate total startup on the SUM of pull
	// times instead of the MAX.
	//
	// First-error-wins semantics: a child cancel func ties all
	// goroutines together so the first failure unblocks the
	// remaining waitForPodIP polls instead of letting them run to
	// timeout. errChan capacity == len(services) keeps the producer
	// goroutines non-blocking even when no consumer drains.
	startup, cancel := context.WithTimeout(ctx, k.cfg.StartupTimeout)
	defer cancel()
	waitCtx, waitCancel := context.WithCancel(startup)
	defer waitCancel()
	aliases := make([]HostAlias, len(services))
	errCh := make(chan error, len(services))
	var wg sync.WaitGroup
	for i, svc := range services {
		i, svc := i, svc
		podName := prefix + svc.Name
		wg.Add(1)
		go func() {
			defer wg.Done()
			ip, err := k.waitForPodIP(waitCtx, podName)
			if err != nil {
				errCh <- fmt.Errorf("service %q: %w", svc.Name, err)
				// Only the creator owns the `failed` event — the
				// reuser would race on the same row, and either
				// "this agent failed to observe ready" OR "the
				// pod never came up" map to the same observable
				// state for the server.
				if created[svc.Name] {
					emit(ServiceLifecycleEvent{
						Name:    svc.Name,
						Image:   svc.Image,
						PodName: podName,
						Status:  "failed",
						Error:   err.Error(),
					})
				}
				waitCancel()
				return
			}
			aliases[i] = HostAlias{IP: ip, Hostnames: []string{svc.Name}}
			if created[svc.Name] {
				emit(ServiceLifecycleEvent{
					Name:    svc.Name,
					Image:   svc.Image,
					PodName: podName,
					Status:  "ready",
				})
			}
		}()
	}
	wg.Wait()
	close(errCh)
	if err := <-errCh; err != nil {
		cleanup()
		return noop, fmt.Errorf("kubernetes engine: wait service podIP: %w", err)
	}

	return ServicesWireup{HostAliases: aliases, Cleanup: cleanup}, nil
}

// buildServicePod materialises a ServiceSpec into a corev1.Pod.
//
// Labels:
//   - gocdnext.io/run-id: the LOAD-BEARING label. The server's
//     CleanupRunServices RPC deletes by this selector (in
//     combination with managed-by + component), so any future
//     code that creates service pods MUST stamp it or risk
//     leaving leaks behind.
//   - gocdnext.io/job: the originating job id — observability only.
//     A subsequent job of the same run will Get an existing pod
//     whose job label points at whichever sibling brought it up,
//     not at the current job. That's fine for `kubectl get pods
//     -l gocdnext.io/job=<id>` debugging (it still surfaces the
//     pod the job is actually using, just under a different
//     originator).
func (k *Kubernetes) buildServicePod(name string, svc ServiceSpec, runID, jobID string) *corev1.Pod {
	env := make([]corev1.EnvVar, 0, len(svc.Env))
	// Iterate the sorted key order so two identical specs produce
	// byte-identical PodSpecs. Helps test assertions + log diffs.
	for _, kv := range envPairsSorted(svc.Env) {
		idx := strings.IndexByte(kv, '=')
		env = append(env, corev1.EnvVar{Name: kv[:idx], Value: kv[idx+1:]})
	}

	policy := imagePullPolicyFor(svc.Image)
	if k.cfg.ForceImagePullAlways {
		policy = corev1.PullAlways
	}

	pullSecrets := make([]corev1.LocalObjectReference, 0, len(k.cfg.ImagePullSecrets))
	for _, n := range k.cfg.ImagePullSecrets {
		pullSecrets = append(pullSecrets, corev1.LocalObjectReference{Name: n})
	}

	labels := map[string]string{
		"app.kubernetes.io/name":       "gocdnext-service",
		"app.kubernetes.io/component":  "service",
		"app.kubernetes.io/managed-by": "gocdnext-agent",
		"gocdnext.io/service":          svc.Name,
	}
	if v, ok := sanitizeLabelValue(runID); ok {
		// Load-bearing for CleanupRunServices selector.
		labels["gocdnext.io/run-id"] = v
	}
	if v, ok := sanitizeLabelValue(jobID); ok {
		labels["gocdnext.io/job"] = v
	}

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: k.cfg.Namespace,
			Labels:    labels,
		},
		Spec: corev1.PodSpec{
			RestartPolicy:    corev1.RestartPolicyNever,
			NodeSelector:     k.cfg.NodeSelector,
			ImagePullSecrets: pullSecrets,
			Containers: []corev1.Container{{
				Name:  "service",
				Image: svc.Image,
				Env:   env,
				// svc.Command maps to Container.Args (NOT Command).
				// Docker semantics: `docker run image foo bar` makes
				// `foo bar` the CMD, appended to the image's
				// ENTRYPOINT (e.g. postgres' docker-entrypoint.sh).
				// K8s mirror: Container.Command overrides ENTRYPOINT;
				// Container.Args is the CMD-equivalent. Using Command
				// here would shadow the image's entrypoint and try to
				// exec `-c` as a binary (the original bug:
				// `command: [-c, fsync=off]` worked under Docker
				// engine but broke containerd with
				// `exec: "-c": executable file not found`).
				Args:            append([]string(nil), svc.Command...),
				ImagePullPolicy: policy,
			}},
		},
	}
}

// assertOurServicePod refuses to adopt a pre-existing pod that
// doesn't carry the full label set EnsureServices stamps. A bare
// `gocdnext.io/run-id` match isn't enough — could be:
//
//   - A pod from a previous gocdnext version that doesn't share our
//     CleanupRunServices contract (won't get reaped on run terminal).
//   - A name collision in the rare-but-possible 12-hex-prefix space.
//   - An unrelated operator-deployed pod whose labels happen to
//     include our run-id (unlikely but defensive).
//
// Requires the full label tuple: managed-by + component + run-id +
// service. Returns nil when the pod is ours; an error otherwise so
// the caller can surface a precise "refusing to reuse" message.
func assertOurServicePod(pod *corev1.Pod, runID, svcName string) error {
	if pod == nil {
		return fmt.Errorf("nil pod")
	}
	labels := pod.GetLabels()
	if labels == nil {
		return fmt.Errorf("missing labels")
	}
	if labels["app.kubernetes.io/managed-by"] != "gocdnext-agent" {
		return fmt.Errorf("managed-by label = %q, want gocdnext-agent", labels["app.kubernetes.io/managed-by"])
	}
	if labels["app.kubernetes.io/component"] != "service" {
		return fmt.Errorf("component label = %q, want service", labels["app.kubernetes.io/component"])
	}
	if labels["gocdnext.io/service"] != svcName {
		return fmt.Errorf("service label = %q, want %q", labels["gocdnext.io/service"], svcName)
	}
	wantRunID, ok := sanitizeLabelValue(runID)
	if !ok {
		return fmt.Errorf("internal: bad runID %q for label validation", runID)
	}
	if labels["gocdnext.io/run-id"] != wantRunID {
		return fmt.Errorf("run-id label = %q, want %q", labels["gocdnext.io/run-id"], wantRunID)
	}
	return nil
}

// CleanupRunServices deletes every service pod labelled with the
// given runID. Called by the agent on receipt of a server-side
// CleanupRunServices RPC (issued when CompleteJob's cascade marks
// a run terminal). Grace period 0 because services don't need
// graceful shutdown — the run is over.
//
// Selector is TIGHT: matches our managed-by + component + run-id
// triple, not just run-id. Without the first two an
// operator-deployed pod that happened to inherit our run-id
// label (unlikely but defensive) would be eligible for deletion;
// scoping to our component-service marker shuts that door.
//
// Error handling:
//   - List error bubbles up unchanged (RBAC misconfig is loud).
//   - NotFound on Delete counts as success (another agent raced).
//   - Other Delete errors are joined into a single returned error
//     so the caller (and the server's eventual log) sees the
//     full picture. Without aggregation a partial RBAC deny
//     (list ok, delete denied) would log "deleted N" while the
//     pods stayed alive — silent failure.
//
// Returns the count of pods that actually went away (delete
// succeeded OR was NotFound). Combined with the error, the
// caller can distinguish "all good" from "partial failure".
func (k *Kubernetes) CleanupRunServices(ctx context.Context, runID string, onLifecycle func(ServiceLifecycleEvent)) (int, error) {
	v, ok := sanitizeLabelValue(runID)
	if !ok || v == "" {
		return 0, fmt.Errorf("kubernetes engine: invalid runID %q for label selector", runID)
	}
	selector := strings.Join([]string{
		"app.kubernetes.io/managed-by=gocdnext-agent",
		"app.kubernetes.io/component=service",
		"gocdnext.io/run-id=" + v,
	}, ",")
	pods, err := k.client.CoreV1().Pods(k.cfg.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil {
		return 0, fmt.Errorf("list service pods for run %s: %w", runID, err)
	}
	gp := int64(0)
	var (
		deleted int
		errs    []error
	)
	for _, pod := range pods.Items {
		delErr := k.client.CoreV1().Pods(k.cfg.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{
			GracePeriodSeconds: &gp,
		})
		switch {
		case delErr == nil:
			deleted++
			// Only emit `stopped` for OUR successful delete —
			// NotFound (race with sibling agent) doesn't emit so
			// the stopped_at timestamp isn't multiply-claimed
			// across agents.
			if onLifecycle != nil {
				svcName := pod.GetLabels()["gocdnext.io/service"]
				if svcName != "" {
					onLifecycle(ServiceLifecycleEvent{
						Name:    svcName,
						PodName: pod.Name,
						Status:  "stopped",
					})
				}
			}
		case kerrors.IsNotFound(delErr):
			deleted++
		default:
			errs = append(errs, fmt.Errorf("delete %s: %w", pod.Name, delErr))
		}
	}
	if len(errs) > 0 {
		return deleted, fmt.Errorf("kubernetes engine: cleanup run services partial: %w", errors.Join(errs...))
	}
	return deleted, nil
}

// waitForPodIP polls the Pod status until status.podIP is populated
// OR the pod reaches a terminal phase (which means scheduling /
// image-pull / startup failed before networking was wired). Returns
// the IP string or an error describing what went wrong.
func (k *Kubernetes) waitForPodIP(ctx context.Context, name string) (string, error) {
	var ip string
	err := wait.PollUntilContextCancel(ctx, k.cfg.PollInterval, true, func(ctx context.Context) (bool, error) {
		pod, err := k.client.CoreV1().Pods(k.cfg.Namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		if pod.Status.PodIP != "" {
			ip = pod.Status.PodIP
			return true, nil
		}
		switch pod.Status.Phase {
		case corev1.PodFailed:
			return false, fmt.Errorf("pod %s reached phase Failed before podIP was assigned", name)
		case corev1.PodSucceeded:
			return false, fmt.Errorf("pod %s exited before podIP was assigned — image entrypoint immediate-exit?", name)
		}
		return false, nil
	})
	if err != nil {
		return "", err
	}
	if ip == "" {
		// PollUntilContextCancel succeeded with ip still unset only if
		// the predicate returned (true, nil) without populating ip —
		// shouldn't happen, but defending against a future code path
		// makes this caller's contract explicit.
		return "", fmt.Errorf("pod %s: podIP unexpectedly empty after wait succeeded", name)
	}
	return ip, nil
}
