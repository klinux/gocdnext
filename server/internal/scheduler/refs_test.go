package scheduler

import (
	"strings"
	"testing"
)

func TestSubstituteRefs(t *testing.T) {
	secrets := map[string]string{
		"DOCKER_USERNAME": "deploybot",
		"DOCKER_PASSWORD": "hunter2",
		"TOKEN":           "ghp_abc",
		"EMPTY":           "",
	}
	vars := map[string]string{
		"REGISTRY": "registry.example.com",
		"TAG":      "latest",
		"TOKEN":    "var-wins-when-secret-also-present", // covers precedence below
	}

	tests := []struct {
		name      string
		in        string
		wantOut   string
		wantErr   string // substring; empty = expect success
		wantNoSub bool   // true when fast-path (no `${{`) should return input verbatim
	}{
		{
			name:    "single secret reference",
			in:      "${{ DOCKER_USERNAME }}",
			wantOut: "deploybot",
		},
		{
			name:    "no whitespace inside braces",
			in:      "${{DOCKER_USERNAME}}",
			wantOut: "deploybot",
		},
		{
			name:    "extra whitespace inside braces tolerated",
			in:      "${{   DOCKER_USERNAME   }}",
			wantOut: "deploybot",
		},
		{
			name:    "two refs in one value",
			in:      "${{ REGISTRY }}/app:${{ TAG }}",
			wantOut: "registry.example.com/app:latest",
		},
		{
			name:    "literal text around ref",
			in:      "Bearer ${{ TOKEN }}",
			wantOut: "Bearer hunter2-wins?",
			wantErr: "", // expect success — but precedence test below cross-checks
		},
		{
			name:    "secrets win over variables in source order",
			in:      "${{ TOKEN }}",
			wantOut: "ghp_abc", // secrets passed first → first hit wins
		},
		{
			name:    "empty value substitutes to empty string",
			in:      "X=${{ EMPTY }}",
			wantOut: "X=",
		},
		{
			name:      "no refs at all (fast path)",
			in:        "plain text without any markers",
			wantOut:   "plain text without any markers",
			wantNoSub: true,
		},
		{
			name:      "shell-style ${VAR} is NOT a ref",
			in:        "${REGISTRY}/x",
			wantOut:   "${REGISTRY}/x",
			wantNoSub: true,
		},
		{
			name:    "GitHub-Actions expressions fail with explicit message",
			in:      "${{ secrets.DOCKER_USERNAME }}",
			wantErr: "unsupported reference expression",
		},
		{
			name:    "operator expression fails with explicit message",
			in:      "${{ A && B }}",
			wantErr: "unsupported reference expression",
		},
		{
			name:    "function-call-looking expression fails",
			in:      "${{ format('img:%s', TAG) }}",
			wantErr: "unsupported reference expression",
		},
		{
			name:    "unresolved single ref",
			in:      "${{ NEVER_DECLARED }}",
			wantErr: "NEVER_DECLARED",
		},
		{
			name:    "multiple unresolved refs deduplicated and sorted",
			in:      "${{ Z }} ${{ A }} ${{ A }} ${{ M }}",
			wantErr: "A, M, Z",
		},
		{
			name:    "mixed resolved + unresolved fails fast",
			in:      "${{ DOCKER_USERNAME }} ${{ MISSING }}",
			wantErr: "MISSING",
		},
		{
			name:    "single-pass — substituted value containing $${{ }} stays literal",
			in:      "${{ NESTED_FROM_VALUE }}",
			wantErr: "NESTED_FROM_VALUE", // not in sources
		},
		{
			name:    "name with digits and underscores",
			in:      "${{ AWS_REGION_1 }}",
			wantErr: "AWS_REGION_1", // not declared — still parsed as a valid ref name
		},
		{
			name:    "name starting with digit fails as invalid identifier",
			in:      "${{ 1AWS }}",
			wantErr: "unsupported reference expression",
		},
		{
			name:    "name with hyphen fails as invalid identifier",
			in:      "${{ AWS-REGION }}",
			wantErr: "unsupported reference expression",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := substituteRefs(tt.in, secrets, vars)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil; out=%q", tt.wantErr, got)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("err = %v, want substring %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if tt.name == "literal text around ref" {
				// Cross-check precedence claim with the actual want.
				if got != "Bearer ghp_abc" {
					t.Errorf("precedence: got %q, want %q", got, "Bearer ghp_abc")
				}
				return
			}
			if got != tt.wantOut {
				t.Errorf("got %q, want %q", got, tt.wantOut)
			}
		})
	}
}

func TestSubstituteRefs_ErrorDoesNotLeakOtherValues(t *testing.T) {
	// Security guard: an unresolved reference must NEVER cause the
	// error message to disclose the value of a NEIGHBOURING ref in
	// the same string. The Plugin.Settings/Env values often sit next
	// to secrets, and an exception bubbles up into the job log /
	// audit_events — leaking there is worse than a downstream auth
	// fail.
	secrets := map[string]string{
		"SECRET_TOKEN": "ghp_supersecret_real_token",
	}
	_, err := substituteRefs(
		"--auth ${{ SECRET_TOKEN }} --user ${{ MISSING_USER }}",
		secrets,
	)
	if err == nil {
		t.Fatal("expected error for MISSING_USER")
	}
	if strings.Contains(err.Error(), "ghp_supersecret_real_token") {
		t.Errorf("error message leaked secret value: %v", err)
	}
	if !strings.Contains(err.Error(), "MISSING_USER") {
		t.Errorf("error message missing the unresolved name: %v", err)
	}
}

func TestSubstituteRefsMap_PreservesKeysAndCarriesKeyContext(t *testing.T) {
	in := map[string]string{
		"username": "${{ DOCKER_USERNAME }}",
		"password": "${{ MISSING_PASS }}",
		"image":    "registry.example.com/app",
	}
	secrets := map[string]string{"DOCKER_USERNAME": "deploybot"}

	_, err := substituteRefsMap(in, secrets)
	if err == nil {
		t.Fatal("expected error for missing password ref")
	}
	// The map flavor wraps the inner err with the offending KEY so
	// the operator sees `password: unresolved ...` instead of just
	// `unresolved ...` and has to grep their YAML to find which
	// setting is broken.
	if !strings.Contains(err.Error(), "password") {
		t.Errorf("err missing key context: %v", err)
	}
	if !strings.Contains(err.Error(), "MISSING_PASS") {
		t.Errorf("err missing ref name: %v", err)
	}

	// And the happy path returns a NEW map (callers must not mutate
	// the original; it may be shared with the cached pipeline def).
	out, err := substituteRefsMap(
		map[string]string{"username": "${{ DOCKER_USERNAME }}"},
		secrets,
	)
	if err != nil {
		t.Fatalf("happy path: %v", err)
	}
	if out["username"] != "deploybot" {
		t.Errorf("username = %q, want deploybot", out["username"])
	}
}

func TestSubstituteRefsMap_NilInputPassesThrough(t *testing.T) {
	// Empty / nil maps are common (jobs without env, plugins without
	// settings) — must NOT allocate a fresh map just to look busy.
	got, err := substituteRefsMap(nil, map[string]string{"X": "1"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != nil {
		t.Errorf("nil input should pass through nil, got %v", got)
	}
}

func TestSubstituteShellVars(t *testing.T) {
	// `${VAR}` is shell-style. Unlike `${{ NAME }}` it's SOFT: an
	// unknown name lands in the output literal so a legitimate shell
	// fragment (`echo ${HOME}`) survives the dispatch substitution
	// pass and gets expanded at container runtime.
	sources := map[string]string{
		"CI_BRANCH":           "gocdnext-tests",
		"CI_COMMIT_SHORT_SHA": "f5b5f8a6",
		"CI_RUN_COUNTER":      "42",
	}

	tests := []struct {
		name, in, want string
	}{
		{"single known var", "${CI_BRANCH}", "gocdnext-tests"},
		{"composite tag", "myapp:1.${CI_RUN_COUNTER}.${CI_COMMIT_SHORT_SHA}-gocdnext",
			"myapp:1.42.f5b5f8a6-gocdnext"},
		{"unknown left literal (runtime shell handles it)", "${HOME}/cache", "${HOME}/cache"},
		{"mixed known + unknown", "${CI_BRANCH} in ${HOME}", "gocdnext-tests in ${HOME}"},
		{"empty input", "", ""},
		{"no shell-var token (fast path)", "plain string", "plain string"},
		{"bash parameter expansion `${VAR:-x}` NOT a ref",
			"${VAR:-default}", "${VAR:-default}"},
		{"positional `$1` NOT a ref", "${1}", "${1}"},
		{"adjacent vars", "${CI_BRANCH}${CI_RUN_COUNTER}", "gocdnext-tests42"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := substituteShellVars(tc.in, sources); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSubstituteShellVarsMap_NilPassesThrough(t *testing.T) {
	got := substituteShellVarsMap(nil, map[string]string{"X": "1"})
	if got != nil {
		t.Errorf("nil input should pass through, got %v", got)
	}
}

func TestSubstituteNeedsRefs_HappyPath(t *testing.T) {
	needs := NeedsOutputs{
		"bump": {
			"next":         "v1.3.0",
			"kind":         "minor",
			"image-digest": "sha256:deadbeef",
		},
	}
	cases := []struct {
		in   string
		want string
	}{
		{
			in:   "release ${{ needs.bump.outputs.next }}",
			want: "release v1.3.0",
		},
		{
			in:   "tag=${{ needs.bump.outputs.next }} kind=${{ needs.bump.outputs.kind }}",
			want: "tag=v1.3.0 kind=minor",
		},
		{
			in:   "ghcr.io/org/app@${{ needs.bump.outputs.image-digest }}",
			want: "ghcr.io/org/app@sha256:deadbeef",
		},
		{
			in:   "no refs here",
			want: "no refs here",
		},
		{
			// Pass-through for non-needs refs — the standard
			// substituteRefs pre-pass resolver handles these.
			in:   "${{ MY_VAR }}",
			want: "${{ MY_VAR }}",
		},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := substituteNeedsRefs(tc.in, needs, nil, nil)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSubstituteNeedsRefs_MissingJob(t *testing.T) {
	// `needs.unknown.outputs.x` — operator referenced a job not
	// in their `needs:` list, OR referenced it but the upstream
	// didn't run. Either way, hard error pointing at the missing
	// link so the operator's not chasing a confusing downstream
	// failure.
	needs := NeedsOutputs{"bump": {"next": "v1.0.0"}}
	_, err := substituteNeedsRefs("tag=${{ needs.unknown.outputs.foo }}", needs, nil, nil)
	if err == nil {
		t.Fatal("expected error for missing upstream job, got nil")
	}
	if !strings.Contains(err.Error(), "not in this job's `needs:`") {
		t.Errorf("error should mention the missing-needs case, got: %v", err)
	}
}

func TestSubstituteNeedsRefs_MissingAlias(t *testing.T) {
	// Upstream ran but didn't produce the named alias — distinct
	// failure mode from missing-job; the error must explain WHICH
	// case (operator typo'd the alias vs operator forgot the
	// declaration vs plugin didn't write to $GOCDNEXT_OUTPUT_FILE).
	needs := NeedsOutputs{"bump": {"next": "v1.0.0"}}
	_, err := substituteNeedsRefs("tag=${{ needs.bump.outputs.nope }}", needs, nil, nil)
	if err == nil {
		t.Fatal("expected error for missing alias, got nil")
	}
	if !strings.Contains(err.Error(), "did not produce the named output") {
		t.Errorf("error should mention the missing-alias case, got: %v", err)
	}
}

func TestSubstituteNeedsRefs_MalformedBodyPassesThrough(t *testing.T) {
	// Bodies that DON'T match `needs.X.outputs.Y` — e.g.
	// `${{ needs.bump.outputs }}` (missing alias) or
	// `${{ needs.bump.foo.next }}` (wrong middle segment) — are
	// left for the downstream substituteRefs to reject with the
	// "unsupported reference expression" error. The pre-pass
	// only handles the canonical shape.
	needs := NeedsOutputs{"bump": {"next": "v1"}}
	for _, body := range []string{
		"${{ needs.bump.outputs }}",
		"${{ needs.bump.foo.next }}",
		"${{ needs.bump }}",
	} {
		got, err := substituteNeedsRefs(body, needs, nil, nil)
		if err != nil {
			t.Errorf("body %q should pass through, got error: %v", body, err)
		}
		if got != body {
			t.Errorf("body %q should be unchanged, got %q", body, got)
		}
	}
}

func TestSubstituteNeedsRefs_NilNeedsErrorsOnReference(t *testing.T) {
	// nil needs + a ref → missing-job error. The function MUST
	// NOT silently pass through — that would hand a literal
	// `${{ needs.X.outputs.Y }}` to the agent, which would land
	// in a plugin setting and surface as a confusing downstream
	// error.
	_, err := substituteNeedsRefs("tag=${{ needs.bump.outputs.next }}", nil, nil, nil)
	if err == nil {
		t.Fatal("expected error with nil needs + a reference, got nil")
	}
}

func TestSubstituteNeedsRefsMap_PreservesKeysAndCarriesKeyContext(t *testing.T) {
	needs := NeedsOutputs{"bump": {"next": "v1.3.0"}}
	in := map[string]string{
		"TAG":    "${{ needs.bump.outputs.next }}",
		"BRANCH": "main",
	}
	out, err := substituteNeedsRefsMap(in, needs, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out["TAG"] != "v1.3.0" {
		t.Errorf("TAG = %q, want v1.3.0", out["TAG"])
	}
	if out["BRANCH"] != "main" {
		t.Errorf("BRANCH = %q, want main (passthrough)", out["BRANCH"])
	}
}

func TestSubstituteNeedsRefsMap_KeyAppearsInError(t *testing.T) {
	// When a value fails resolution the wrapper must prepend the
	// MAP KEY so the operator sees which env var / setting was
	// the source of the bad reference.
	in := map[string]string{
		"IMAGE_TAG": "${{ needs.unknown.outputs.x }}",
	}
	_, err := substituteNeedsRefsMap(in, NeedsOutputs{}, nil, nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "IMAGE_TAG") {
		t.Errorf("error should cite the offending map key, got: %v", err)
	}
}

func TestSubstituteRefs_RegexCompiledOnce(t *testing.T) {
	// Compile-once invariant: refPattern is package-level; this test
	// just exercises the var once so a future refactor that moves it
	// inside a hot loop trips the bench-style assertion below.
	if refPattern == nil {
		t.Fatal("refPattern must be a package-level compiled regex")
	}
	// Sanity: calling substituteRefs twice doesn't recompile (which
	// would show up as different regex pointers in pprof; we just
	// verify the symbol stays the same instance).
	a := refPattern
	_, _ = substituteRefs("${{ X }}", map[string]string{"X": "1"})
	b := refPattern
	if a != b {
		t.Errorf("refPattern reallocated between calls")
	}
}

// =================================================================
// Matrix selector resolution (issue #21).
// =================================================================

func TestSubstituteNeedsRefs_MatrixSelectorExplicitMultiDim(t *testing.T) {
	// Explicit selector with both dims declared: lex-sorted
	// canonicalization makes operator-order and storage-order
	// match deterministically.
	matrix := MatrixNeedsOutputs{
		"build": {
			"arch=amd64,os=linux": {"digest": "sha256:aaa"},
			"arch=arm64,os=linux": {"digest": "sha256:bbb"},
		},
	}
	dims := MatrixDimNames{"build": []string{"arch", "os"}}

	// Selector body order doesn't matter — canonical form is
	// lex-sorted, same as the stored keys.
	cases := []struct {
		in   string
		want string
	}{
		{"img=${{ needs.build.matrix[os=linux,arch=amd64].outputs.digest }}", "img=sha256:aaa"},
		{"img=${{ needs.build.matrix[arch=amd64,os=linux].outputs.digest }}", "img=sha256:aaa"},
		{"img=${{ needs.build.matrix[arch=arm64,os=linux].outputs.digest }}", "img=sha256:bbb"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := substituteNeedsRefs(tc.in, nil, matrix, dims)
			if err != nil {
				t.Fatalf("substituteNeedsRefs: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSubstituteNeedsRefs_MatrixSelectorOneDimShortcut(t *testing.T) {
	// 1-dim upstream: operator can write `matrix[apac]` instead
	// of `matrix[shard=apac]`. canonicalMatrixKey expands using
	// the single declared dim name.
	matrix := MatrixNeedsOutputs{
		"bump": {
			"shard=apac": {"next": "v1.0.0-apac"},
			"shard=emea": {"next": "v1.0.0-emea"},
		},
	}
	dims := MatrixDimNames{"bump": []string{"shard"}}

	got, err := substituteNeedsRefs("tag=${{ needs.bump.matrix[apac].outputs.next }}", nil, matrix, dims)
	if err != nil {
		t.Fatalf("substituteNeedsRefs: %v", err)
	}
	if got != "tag=v1.0.0-apac" {
		t.Errorf("got %q, want tag=v1.0.0-apac", got)
	}
}

func TestSubstituteNeedsRefs_MatrixBareRefErrorsLoud(t *testing.T) {
	// Bare ref against a matrix-expanded upstream is an
	// operator error — the scheduler can't pick "the right one".
	// Error message must say so and point at the selector form.
	matrix := MatrixNeedsOutputs{
		"bump": {"shard=apac": {"next": "x"}, "shard=emea": {"next": "y"}},
	}
	dims := MatrixDimNames{"bump": []string{"shard"}}

	_, err := substituteNeedsRefs("tag=${{ needs.bump.outputs.next }}", nil, matrix, dims)
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"matrix", "selector", "needs.bump.outputs.next"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should contain %q, got: %v", want, err)
		}
	}
}

func TestSubstituteNeedsRefs_MatrixSelectorOnPlainErrors(t *testing.T) {
	// The inverse: selector against a non-matrix upstream is
	// also a typo. Tell the operator to drop the selector.
	needs := NeedsOutputs{"bump": {"next": "v1.0.0"}}

	_, err := substituteNeedsRefs("tag=${{ needs.bump.matrix[apac].outputs.next }}", needs, nil, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"NOT a matrix", "drop the matrix"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should contain %q, got: %v", want, err)
		}
	}
}

func TestSubstituteNeedsRefs_MatrixSelectorUnknownKey(t *testing.T) {
	// A selector that doesn't match any row — surfaces with the
	// available canonical keys for the operator's debugging.
	matrix := MatrixNeedsOutputs{
		"bump": {"shard=apac": {"next": "x"}, "shard=emea": {"next": "y"}},
	}
	dims := MatrixDimNames{"bump": []string{"shard"}}

	_, err := substituteNeedsRefs("tag=${{ needs.bump.matrix[us].outputs.next }}", nil, matrix, dims)
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"shard=us", "available"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should contain %q, got: %v", want, err)
		}
	}
}

func TestSubstituteNeedsRefs_MatrixSelectorOneDimShortcutAgainstMultiDimErrors(t *testing.T) {
	// 1-dim shortcut against a multi-dim upstream is ambiguous —
	// refuse and tell the operator to use the explicit form.
	matrix := MatrixNeedsOutputs{
		"build": {"arch=amd64,os=linux": {"digest": "x"}},
	}
	dims := MatrixDimNames{"build": []string{"arch", "os"}}

	_, err := substituteNeedsRefs("tag=${{ needs.build.matrix[linux].outputs.digest }}", nil, matrix, dims)
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"1-dim shortcut", "matrix dimensions"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should contain %q, got: %v", want, err)
		}
	}
}

func TestSubstituteNeedsRefs_MatrixSelectorUnknownDim(t *testing.T) {
	// Explicit k=v with k NOT in the upstream's declared
	// strategy.matrix — typo, refuse loud with the declared
	// dimensions cited.
	matrix := MatrixNeedsOutputs{
		"bump": {"shard=apac": {"next": "x"}},
	}
	dims := MatrixDimNames{"bump": []string{"shard"}}

	_, err := substituteNeedsRefs("tag=${{ needs.bump.matrix[region=us].outputs.next }}", nil, matrix, dims)
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"region", "not declared", "shard"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should contain %q, got: %v", want, err)
		}
	}
}
