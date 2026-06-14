package scheduler

import (
	"strings"
	"testing"
)

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
