package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Secret-bearing env vars must never reach a Pod spec as a plain
// `EnvVar.Value`: `get pods` is a far cheaper RBAC grant than
// `get secrets`, so anyone who can `kubectl get pod -o yaml` in the
// runners' namespace would read every pipeline secret the job was
// given — a read path neither the UI nor LogMasks covers (#309).
//
// This file holds the three pure pieces the Kubernetes engine uses to
// close that path: classify (SplitSecretEnv), name (SecretEnvKey),
// materialise (buildTaskEnvVars). The Docker engine solved the same
// class differently — values travel via the CLI process's own env so
// they never land on argv (see docker_envleak_test.go).

// secretEnvKeyPrefix namespaces the data keys of secret-backed env
// vars inside the Secret. In isolated mode that Secret ALSO carries
// the serialised assignment under AssignmentSecretKey
// ("assignment.pb"); the prefix makes a collision impossible by
// construction, since every generated key starts with "env." and the
// assignment key does not.
const secretEnvKeyPrefix = "env."

// secretValueMinLen mirrors applyMasks' floor in
// agent/internal/runner/runner_io.go: masks shorter than 4 chars are
// ignored for log redaction because substring-replacing them mangles
// unrelated output. The same reasoning applies here — a 1-3 char
// secret would drag every env var that merely contains those
// characters into the Secret, and a secret that short is already
// unprotectable in the logs. Keeping the two floors identical means
// "what the log redacts" and "what the Pod spec hides" are the same
// set, with no third rule for an operator to learn.
const secretValueMinLen = 4

// SplitSecretEnv partitions env into the vars that may stay inline in
// the Pod spec and the vars whose values must be served from a
// Secret.
//
// secretValues is JobAssignment.log_masks: the ledger of every
// sensitive value the scheduler resolved into this job — declared
// `secrets:`, runner-profile secrets, OIDC id_tokens,
// PLUGIN_KUBECONFIG, clone tokens, opt-in masked outputs. Matching on
// VALUES rather than on names is what makes this complete without a
// wire change: it also catches the `PLUGIN_*` vars the agent itself
// derives from plugin settings that were resolved from `${{ SECRET }}`
// server-side — keys the server could not name here even if it tried.
//
// An env value is sensitive when it CONTAINS a mask, not only when it
// equals one: a composed value like
// `postgres://u:${{ PG_PASSWORD }}@host/db` leaks the password
// verbatim. False positives are harmless (a non-secret value merely
// moves to the Secret and still reaches the container unchanged), so
// the comparison errs toward over-protecting.
//
// Both returned maps are freshly allocated — ScriptSpec.Env is shared
// with the runner that built it, so mutating the input would corrupt
// a later retry's assignment (same rationale as withOutputsEnv).
func SplitSecretEnv(env map[string]string, secretValues []string) (plain, secret map[string]string) {
	plain = make(map[string]string, len(env))
	secret = make(map[string]string)
	if len(env) == 0 {
		return plain, secret
	}

	// Filter the mask list ONCE instead of re-checking the floor for
	// every (env, mask) pair. Job env is a few dozen entries and masks
	// a handful, but this is the dispatch hot path for every k8s job.
	masks := make([]string, 0, len(secretValues))
	for _, m := range secretValues {
		if len(m) >= secretValueMinLen {
			masks = append(masks, m)
		}
	}
	if len(masks) == 0 {
		for k, v := range env {
			plain[k] = v
		}
		return plain, secret
	}

	for k, v := range env {
		sensitive := false
		for _, m := range masks {
			if strings.Contains(v, m) {
				sensitive = true
				break
			}
		}
		if sensitive {
			secret[k] = v
			continue
		}
		plain[k] = v
	}
	return plain, secret
}

// SecretEnvKey maps an env var NAME to the Secret data key its value
// is stored under. Kubernetes accepts `[-._a-zA-Z0-9]+` as a Secret
// key; env var names the parser validated as identifiers always
// qualify, so the common case reads naturally in
// `kubectl get secret -o yaml` ("env.GHCR_TOKEN").
//
// Anything outside that charset gets a deterministic hashed alias
// ("env.x" + 8 hex of sha256) rather than being sanitised — two names
// differing only in a dropped character would otherwise collide and
// silently hand one var the other's value. The env var name INSIDE
// the container is unaffected either way; only the storage key
// changes. There is deliberately no inline fallback: an unmappable
// name must still not leak.
func SecretEnvKey(name string) string {
	if isValidSecretKeyName(name) {
		return secretEnvKeyPrefix + name
	}
	sum := sha256.Sum256([]byte(name))
	return secretEnvKeyPrefix + "x" + hex.EncodeToString(sum[:4])
}

func isValidSecretKeyName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z',
			c >= 'A' && c <= 'Z',
			c >= '0' && c <= '9',
			c == '-', c == '.', c == '_':
		default:
			return false
		}
	}
	return true
}

// buildTaskEnvVars materialises the task container's env list: plain
// values inline, sensitive ones as a secretKeyRef into secretName.
//
// Keys are emitted in sorted order over the MERGED set so two
// identical specs produce byte-identical Pod specs (test assertions,
// log diffs, and the AlreadyExists recovery path all benefit).
//
// Optional is left unset (= required): a key missing from the Secret
// must fail the container with CreateContainerConfigError, which the
// engine's startup waiter surfaces, instead of starting the task with
// a silently empty credential.
func buildTaskEnvVars(plain, secret map[string]string, secretName string) []corev1.EnvVar {
	merged := make(map[string]struct{}, len(plain)+len(secret))
	for k := range plain {
		merged[k] = struct{}{}
	}
	for k := range secret {
		merged[k] = struct{}{}
	}

	names := make([]string, 0, len(merged))
	for k := range merged {
		names = append(names, k)
	}
	sort.Strings(names)

	out := make([]corev1.EnvVar, 0, len(names))
	for _, name := range names {
		if _, ok := secret[name]; ok {
			out = append(out, corev1.EnvVar{
				Name: name,
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
						Key:                  SecretEnvKey(name),
					},
				},
			})
			continue
		}
		out = append(out, corev1.EnvVar{Name: name, Value: plain[name]})
	}
	return out
}

// EnvSecretName is the Secret a task pod reads its secret-backed env
// from, in BOTH workspace modes. Derived from the pod name so the
// builder that emits the refs (BuildPodSpec / BuildIsolatedJobPodSpec)
// and the caller that creates the object (RunScript /
// CreateIsolatedJobPod) agree without passing anything between them.
//
// Isolated mode does NOT fold this into the assignment Secret it
// already creates per job, even though that would save an object: the
// runner deletes the assignment Secret the moment prep terminates,
// which is before the kubelet resolves the task container's env. See
// IsolatedJobSpec.EnvSecretName.
func EnvSecretName(podName string) string { return podName + "-env" }

// BuildEnvSecret materialises the per-pod env Secret (both modes).
// Keys come from SecretEnvKey so they match exactly what
// buildTaskEnvVars referenced.
//
// Returns nil when there is nothing sensitive — the caller skips the
// API round trip entirely, keeping the common (no-secrets) job on the
// same number of calls as before #309.
//
// Lifetime is the pod's: the caller patches an ownerReference right
// after the pod is created, so the kubelet's GC removes this with the
// pod, including when jobCleanup retains a failed pod for debugging
// (a retained pod keeps a readable Secret, which is the point). That
// ownerRef is the ONLY thing that reaps it — nothing deletes this
// Secret explicitly on the happy path, which is what keeps it alive
// long enough for the task container to start.
func BuildEnvSecret(name, namespace string, secretEnv map[string]string) (*corev1.Secret, error) {
	if len(secretEnv) == 0 {
		return nil, nil
	}
	if name == "" {
		return nil, fmt.Errorf("env secret: empty name")
	}
	if namespace == "" {
		return nil, fmt.Errorf("env secret: empty namespace")
	}
	data := make(map[string][]byte, len(secretEnv))
	total := 0
	for k, v := range secretEnv {
		key := SecretEnvKey(k)
		data[key] = []byte(v)
		total += len(key) + len(v)
	}
	// Same cap + rationale as the assignment Secret: fail here with a
	// clear message instead of letting the apiserver reject the object
	// mid-dispatch against its 1 MiB limit.
	if total > AssignmentSecretMaxBytes {
		return nil, fmt.Errorf("env secret: %d bytes exceeds max %d", total, AssignmentSecretMaxBytes)
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			// Same managed-by label the task pod carries, so the
			// documented manual sweep
			// (`kubectl delete secret -l app.kubernetes.io/managed-by=gocdnext-agent`)
			// also reaches these if an agent dies before the ownerRef
			// patch lands.
			Labels: map[string]string{
				"app.kubernetes.io/name":       "gocdnext-job-env",
				"app.kubernetes.io/managed-by": "gocdnext-agent",
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: data,
	}, nil
}
