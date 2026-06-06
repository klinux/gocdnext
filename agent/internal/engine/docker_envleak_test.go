package engine

import (
	"strings"
	"testing"
)

// TestDockerArgs_EnvValueNeverLandsOnArgv is the regression guard
// for the leak path the v0.7.x security review caught: the
// previous docker engine emitted `-e KEY=VAL` on the docker CLI
// argv, exposing secret-bearing values (PEM keys, registry
// tokens, generic `secrets:` env) to anyone with `ps auxww` on
// the host. The fix references env vars by NAME only on argv
// (`-e KEY`) and propagates the actual values via the docker CLI
// process's own env (cmd.Env in RunScript) — docker reads them
// from there and injects into the container.
//
// This test asserts the invariant at the buildArgs layer without
// needing a real docker daemon. RunScript's cmd.Env wiring is
// covered by the integration suite when docker is available.
func TestDockerArgs_EnvValueNeverLandsOnArgv(t *testing.T) {
	d := &Docker{
		cfg: DockerConfig{SocketPath: "/var/run/docker.sock"},
	}

	const (
		secretToken = "ghp_abc123_should_never_appear_on_argv"
		// Multi-line PEM-style value — the previous --env-file path
		// would have broken on this; the env-propagation path is
		// indifferent to newlines because env vars don't have
		// line-based parsing.
		pemKey = "-----BEGIN COSIGN PRIVATE KEY-----\n" +
			"line1_secret_bytes_NEVER_ON_ARGV\n" +
			"line2_more_secret\n" +
			"-----END COSIGN PRIVATE KEY-----\n"
	)

	spec := ScriptSpec{
		Image:  "alpine:3.20",
		Script: "true",
		Env: map[string]string{
			"GHCR_TOKEN":          secretToken,
			"COSIGN_PRIVATE_KEY":  pemKey,
			"COSIGN_PASSWORD":     "passphrase",
			"INNOCENT_PUBLIC_VAR": "no-secret-here",
		},
	}

	args := d.buildArgs("alpine:3.20", spec, "/tmp/test.cid")
	joined := strings.Join(args, " ")

	// Every env KEY must appear on argv as `-e KEY` with no `=`.
	for key, val := range spec.Env {
		expectedFlag := "-e " + key
		if !strings.Contains(joined, expectedFlag) {
			t.Errorf("argv missing %q reference: %s", expectedFlag, joined)
		}
		// The value must NOT appear ANYWHERE on argv.
		if strings.Contains(joined, val) {
			t.Errorf("argv leaks value of %q: present in argv (full argv: %v)", key, args)
		}
		// Defence-in-depth: check the literal `-e KEY=` form never
		// appears either (catches a regression that switches to
		// `-e KEY=...truncated`).
		legacyFlag := "-e " + key + "="
		if strings.Contains(joined, legacyFlag) {
			t.Errorf("argv contains legacy `-e KEY=` form for %q — leak path returned", key)
		}
	}

	// Sanity: no raw secret bytes anywhere in the argv.
	for _, secret := range []string{secretToken, pemKey, "passphrase"} {
		for _, arg := range args {
			if strings.Contains(arg, secret) {
				t.Errorf("argv arg %q contains a known-secret substring", arg)
			}
		}
	}
}

// TestEnvKeysSorted asserts deterministic ordering — argv layout
// stability is a debug-affordance contract, not a correctness
// contract, but flakes here mean test-snapshot drift.
func TestEnvKeysSorted(t *testing.T) {
	in := map[string]string{
		"DELTA":   "d",
		"ALPHA":   "a",
		"CHARLIE": "c",
		"BRAVO":   "b",
	}
	got := envKeysSorted(in)
	want := []string{"ALPHA", "BRAVO", "CHARLIE", "DELTA"}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for i, k := range want {
		if got[i] != k {
			t.Errorf("got[%d] = %q, want %q", i, got[i], k)
		}
	}
}

// TestEnvPairsForCmd asserts the helper that RunScript uses to
// populate cmd.Env produces KEY=VAL strings exactly compatible
// with os/exec's contract. Order matches envKeysSorted so argv
// `-e KEY` references and the env-pair index line up across
// runs.
func TestEnvPairsForCmd(t *testing.T) {
	in := map[string]string{
		"BETA":  "two\nlines\nallowed",
		"ALPHA": "single",
	}
	got := envPairsForCmd(in)
	want := []string{"ALPHA=single", "BETA=two\nlines\nallowed"}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d (got %v)", len(got), len(want), got)
	}
	for i, p := range want {
		if got[i] != p {
			t.Errorf("got[%d] = %q, want %q", i, got[i], p)
		}
	}
}
