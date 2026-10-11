package engine

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestSplitSecretEnv(t *testing.T) {
	tests := []struct {
		name         string
		env          map[string]string
		secretValues []string
		wantPlain    map[string]string
		wantSecret   map[string]string
	}{
		{
			name:       "nil env",
			env:        nil,
			wantPlain:  map[string]string{},
			wantSecret: map[string]string{},
		},
		{
			name:         "nil masks keeps everything inline",
			env:          map[string]string{"CI_BRANCH": "main"},
			secretValues: nil,
			wantPlain:    map[string]string{"CI_BRANCH": "main"},
			wantSecret:   map[string]string{},
		},
		{
			name:         "job secret moves out, plain var stays",
			env:          map[string]string{"CI_BRANCH": "main", "GHCR_TOKEN": "ghp_abc123"},
			secretValues: []string{"ghp_abc123"},
			wantPlain:    map[string]string{"CI_BRANCH": "main"},
			wantSecret:   map[string]string{"GHCR_TOKEN": "ghp_abc123"},
		},
		{
			name: "plugin setting carrying the same secret value moves too",
			env: map[string]string{
				"PLUGIN_PASSWORD": "ghp_abc123",
				"GHCR_TOKEN":      "ghp_abc123",
				"PLUGIN_REGISTRY": "ghcr.io",
			},
			secretValues: []string{"ghp_abc123"},
			wantPlain:    map[string]string{"PLUGIN_REGISTRY": "ghcr.io"},
			wantSecret: map[string]string{
				"PLUGIN_PASSWORD": "ghp_abc123",
				"GHCR_TOKEN":      "ghp_abc123",
			},
		},
		{
			name:         "empty mask never matches an empty env value",
			env:          map[string]string{"EMPTY": "", "TOKEN": "s3cr3t"},
			secretValues: []string{"", "s3cr3t"},
			wantPlain:    map[string]string{"EMPTY": ""},
			wantSecret:   map[string]string{"TOKEN": "s3cr3t"},
		},
		{
			name:         "multiline PEM value",
			env:          map[string]string{"KEY": "-----BEGIN KEY-----\nabc\n-----END KEY-----\n"},
			secretValues: []string{"-----BEGIN KEY-----\nabc\n-----END KEY-----\n"},
			wantPlain:    map[string]string{},
			wantSecret:   map[string]string{"KEY": "-----BEGIN KEY-----\nabc\n-----END KEY-----\n"},
		},
		{
			name: "substring of a mask is NOT a match (exact value only)",
			// applyMasks does substring replacement in log lines, but an
			// env var whose value is merely a PREFIX of a secret is not
			// itself sensitive — moving it would be noise. The reverse
			// (an env value that CONTAINS a secret) is covered below.
			env:          map[string]string{"PREFIX": "ghp_"},
			secretValues: []string{"ghp_abc123"},
			wantPlain:    map[string]string{"PREFIX": "ghp_"},
			wantSecret:   map[string]string{},
		},
		{
			name: "env value that embeds a secret moves out",
			// e.g. DATABASE_URL built from a password secret in YAML:
			// `postgres://u:${{ PG_PASSWORD }}@host/db`. The composed
			// value leaks the secret verbatim, so it must not stay inline.
			env:          map[string]string{"DATABASE_URL": "postgres://u:s3cr3tpass@host/db"},
			secretValues: []string{"s3cr3tpass"},
			wantPlain:    map[string]string{},
			wantSecret:   map[string]string{"DATABASE_URL": "postgres://u:s3cr3tpass@host/db"},
		},
		{
			name: "short mask does not drag unrelated values along",
			// applyMasks ignores masks under 4 chars to avoid mangling
			// log lines; the same floor applies here, otherwise a 1-char
			// secret would move every env var containing that character.
			env:          map[string]string{"PORT": "5432", "TINY": "ab"},
			secretValues: []string{"ab"},
			wantPlain:    map[string]string{"PORT": "5432", "TINY": "ab"},
			wantSecret:   map[string]string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plain, secret := SplitSecretEnv(tt.env, tt.secretValues)
			assertEnvMap(t, "plain", plain, tt.wantPlain)
			assertEnvMap(t, "secret", secret, tt.wantSecret)
		})
	}
}

// TestSplitSecretEnv_DoesNotMutateInput guards the contract every
// engine relies on: ScriptSpec.Env is shared with the runner that
// built it (see withOutputsEnv's doc), so mutating it would corrupt
// a retry's assignment.
func TestSplitSecretEnv_DoesNotMutateInput(t *testing.T) {
	env := map[string]string{"A": "plain", "B": "s3cr3t"}
	SplitSecretEnv(env, []string{"s3cr3t"})
	if len(env) != 2 || env["A"] != "plain" || env["B"] != "s3cr3t" {
		t.Fatalf("input env mutated: %v", env)
	}
}

func TestSecretEnvKey(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "identifier", in: "GHCR_TOKEN", want: "env.GHCR_TOKEN"},
		{name: "plugin prefixed", in: "PLUGIN_PASSWORD", want: "env.PLUGIN_PASSWORD"},
		{name: "digits and dashes", in: "a-b.c_9", want: "env.a-b.c_9"},
		// Anything outside k8s's Secret-key charset `[-._a-zA-Z0-9]+`
		// gets a deterministic hashed alias. The env var NAME inside the
		// container is unaffected — only the Secret's data key changes.
		{name: "space", in: "WEIRD NAME", want: "env.xffed24a8"},
		{name: "slash", in: "A/B", want: "env.x998d3ed8"},
		{name: "unicode", in: "CHAVÉ", want: "env.xe8cb0912"},
		{name: "empty", in: "", want: "env.xe3b0c442"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SecretEnvKey(tt.in); got != tt.want {
				t.Errorf("SecretEnvKey(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestSecretEnvKey_NeverCollidesWithAssignmentKey is load-bearing for
// isolated mode: the secret-backed env entries share the Secret that
// carries the serialised JobAssignment, so a key collision would
// overwrite the payload the prep init container reads.
func TestSecretEnvKey_NeverCollidesWithAssignmentKey(t *testing.T) {
	for _, name := range []string{AssignmentSecretKey, "assignment", "assignment.pb", "ASSIGNMENT_PB"} {
		if got := SecretEnvKey(name); got == AssignmentSecretKey {
			t.Errorf("SecretEnvKey(%q) = %q, collides with the assignment key", name, got)
		}
	}
}

// TestSecretEnvKey_Distinct asserts the hashed-alias branch keeps
// distinct names distinct — a collision would silently give two env
// vars the same value.
func TestSecretEnvKey_Distinct(t *testing.T) {
	seen := map[string]string{}
	for _, name := range []string{"A B", "A/B", "A.B", "A B ", "", "ç", "é"} {
		key := SecretEnvKey(name)
		if prev, dup := seen[key]; dup {
			t.Errorf("SecretEnvKey(%q) == SecretEnvKey(%q) == %q", name, prev, key)
		}
		seen[key] = name
	}
}

func TestBuildTaskEnvVars(t *testing.T) {
	plain := map[string]string{"CI_BRANCH": "main", "AAA": "first"}
	secret := map[string]string{"GHCR_TOKEN": "ghp_abc", "WEIRD NAME": "pw"}

	got := buildTaskEnvVars(plain, secret, "pod-assignment")

	// Deterministic order across the merged key set.
	wantOrder := []string{"AAA", "CI_BRANCH", "GHCR_TOKEN", "WEIRD NAME"}
	if len(got) != len(wantOrder) {
		t.Fatalf("len = %d, want %d (%+v)", len(got), len(wantOrder), got)
	}
	for i, name := range wantOrder {
		if got[i].Name != name {
			t.Errorf("got[%d].Name = %q, want %q", i, got[i].Name, name)
		}
	}

	byName := map[string]corev1.EnvVar{}
	for _, e := range got {
		byName[e.Name] = e
	}

	if e := byName["CI_BRANCH"]; e.Value != "main" || e.ValueFrom != nil {
		t.Errorf("plain var not inline: %+v", e)
	}
	for name, wantKey := range map[string]string{
		"GHCR_TOKEN": "env.GHCR_TOKEN",
		"WEIRD NAME": SecretEnvKey("WEIRD NAME"),
	} {
		e := byName[name]
		if e.Value != "" {
			t.Errorf("%s: Value is set (%q) — the leak this fix removes", name, e.Value)
		}
		if e.ValueFrom == nil || e.ValueFrom.SecretKeyRef == nil {
			t.Fatalf("%s: no secretKeyRef", name)
		}
		ref := e.ValueFrom.SecretKeyRef
		if ref.Name != "pod-assignment" || ref.Key != wantKey {
			t.Errorf("%s: ref = {%s, %s}, want {pod-assignment, %s}", name, ref.Name, ref.Key, wantKey)
		}
		if ref.Optional != nil && *ref.Optional {
			t.Errorf("%s: optional secretKeyRef — a missing key must fail the container loudly", name)
		}
	}

	// No secret VALUE may appear anywhere in the materialised env list.
	for _, e := range got {
		for _, v := range secret {
			if strings.Contains(e.Value, v) {
				t.Errorf("env var %q leaks a secret value", e.Name)
			}
		}
	}
}

func TestBuildTaskEnvVars_NoSecretsIsPureInline(t *testing.T) {
	got := buildTaskEnvVars(map[string]string{"A": "1"}, nil, "")
	if len(got) != 1 || got[0].Value != "1" || got[0].ValueFrom != nil {
		t.Fatalf("got %+v, want a single inline var", got)
	}
}

// TestBuildTaskEnvVars_NeverFallsBackToInline pins the one behaviour
// that must hold even when a caller passes a broken secret name: the
// value NEVER goes back inline. An empty name yields a ref the API
// server rejects at create time (loud), which is the correct failure
// — a silent inline fallback would be the leak returning.
func TestBuildTaskEnvVars_NeverFallsBackToInline(t *testing.T) {
	got := buildTaskEnvVars(nil, map[string]string{"TOKEN": "s3cr3t"}, "")
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1 (%+v)", len(got), got)
	}
	if got[0].Value != "" || got[0].ValueFrom == nil || got[0].ValueFrom.SecretKeyRef == nil {
		t.Fatalf("fell back to an inline secret value: %+v", got[0])
	}
}

func assertEnvMap(t *testing.T, label string, got, want map[string]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: len = %d, want %d (got %v, want %v)", label, len(got), len(want), got, want)
	}
	for k, v := range want {
		gv, ok := got[k]
		if !ok {
			t.Errorf("%s: missing key %q", label, k)
			continue
		}
		if gv != v {
			t.Errorf("%s[%q] = %q, want %q", label, k, gv, v)
		}
	}
}
