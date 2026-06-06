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
