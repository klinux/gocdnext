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
//	gocdnext-svc-<12-hex-of-jobid>-<svcname>
//
// stays under the 63-char DNS label limit. Critical: the value comes
// from pipeline YAML — without strict validation, a malicious name
// like `x` * 200 or `Invalid.NAME` would either fail at API submit
// time with a noisy error or, worse, sneak into a label value where
// it can collide with another job's resources.
var k8sServiceNameRE = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,30}[a-z0-9])?$`)

// EnsureServices brings up one Pod per declared service in the
// configured namespace and returns hostAliases mapping each service
// name to its pod IP. The runner threads the aliases into the task
// pod's ScriptSpec.HostAliases so a `postgres:5432`-style hostname
// resolves via /etc/hosts (no DNS query, no Service object needed,
// no port spec in the pipeline YAML).
//
// Pod naming: gocdnext-svc-<jobshort>-<svcname>. Deterministic per
// (jobID, svcName) so a runner restart can identify (and force-
// delete) leftovers from a previous attempt.
//
// Errors:
//   - empty services → no-op ServicesWireup with a noop cleanup.
//   - invalid name / empty image → fail before any API call, no
//     cleanup needed.
//   - pod creation conflict → likely a leftover from a previous
//     job; surface with a hint to delete + retry. The cleanup we
//     return still tears down anything we DID create in this call.
//   - podIP wait timeout (StartupTimeout) → cleanup is called
//     internally so the caller sees a clean error path with
//     no leaked pods to mop up.
//
// Concurrency: pods are created sequentially (cheap API calls);
// the wait-for-podIP step runs in parallel via errgroup since
// image pulls dominate and serialising would multiply latency by
// the number of services.
func (k *Kubernetes) EnsureServices(
	ctx context.Context,
	services []ServiceSpec,
	jobID string,
	log func(stream, text string),
) (ServicesWireup, error) {
	noop := ServicesWireup{Cleanup: func() {}}
	if len(services) == 0 {
		return noop, nil
	}
	if jobID == "" {
		return noop, errors.New("kubernetes engine: EnsureServices needs a non-empty jobID for collision-free pod naming")
	}

	jobShort := shortDockerID(jobID)
	prefix := "gocdnext-svc-" + jobShort + "-"

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

	// created tracks every pod we successfully POSTed so cleanup
	// can target exactly what this call brought up — important when
	// a later service's Create fails partway through and we don't
	// want to wipe an unrelated pod that happens to match the name
	// prefix from a concurrent job (jobShort makes that unlikely
	// but the explicit list keeps cleanup correctness independent of
	// the naming scheme).
	var (
		mu      sync.Mutex
		created []string
	)
	cleanup := func() {
		mu.Lock()
		names := append([]string(nil), created...)
		mu.Unlock()
		// Use a background context so cleanup still runs after
		// the caller's ctx was canceled (typical reason cleanup
		// is invoked). Force-delete (grace period 0) because the
		// services don't need graceful shutdown and the alternative
		// is a 30s lag between job end and pod-IP recycling.
		gp := int64(0)
		bg := context.Background()
		for _, name := range names {
			_ = k.client.CoreV1().Pods(k.cfg.Namespace).Delete(bg, name, metav1.DeleteOptions{
				GracePeriodSeconds: &gp,
			})
		}
	}

	for _, svc := range services {
		name := prefix + svc.Name
		pod := k.buildServicePod(name, svc, jobID)
		if log != nil {
			log("stdout", fmt.Sprintf("$ starting service %s (%s)", svc.Name, svc.Image))
		}
		if _, err := k.client.CoreV1().Pods(k.cfg.Namespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
			cleanup()
			if kerrors.IsAlreadyExists(err) {
				return noop, fmt.Errorf(
					"kubernetes engine: service pod %s already exists — leftover from a previous job? delete it and retry",
					name)
			}
			return noop, fmt.Errorf("kubernetes engine: create service pod %s: %w", name, err)
		}
		mu.Lock()
		created = append(created, name)
		mu.Unlock()
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
				waitCancel()
				return
			}
			aliases[i] = HostAlias{IP: ip, Hostnames: []string{svc.Name}}
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

// buildServicePod materialises a ServiceSpec into a corev1.Pod. The
// pod is labelled with the originating job id so an operator running
// `kubectl get pods -l gocdnext.io/job=<id>` sees both the task pod
// and every service pod that backs it.
func (k *Kubernetes) buildServicePod(name string, svc ServiceSpec, jobID string) *corev1.Pod {
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
