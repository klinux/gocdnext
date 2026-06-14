package refs_test

import (
	"strings"
	"testing"

	"github.com/gocdnext/gocdnext/server/pkg/refs"
)

func TestSubstituteRefs(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		sources []map[string]string
		want    string
		wantErr string
	}{
		{name: "no refs passes through", in: "plain value", want: "plain value"},
		{name: "single ref", in: "${{ TOKEN }}", sources: []map[string]string{{"TOKEN": "abc"}}, want: "abc"},
		{name: "ref inside text", in: "img:${{ TAG }}-x", sources: []map[string]string{{"TAG": "v1"}}, want: "img:v1-x"},
		{
			name: "first source wins (override > base)",
			in:   "${{ NAME }}",
			sources: []map[string]string{
				{"NAME": "override"},
				{"NAME": "base"},
			},
			want: "override",
		},
		{
			name:    "single-pass: resolved value with a ref stays literal",
			in:      "${{ A }}",
			sources: []map[string]string{{"A": "${{ B }}", "B": "x"}},
			want:    "${{ B }}",
		},
		{name: "unresolved errors", in: "${{ MISSING }}", wantErr: "unresolved reference(s): MISSING"},
		{name: "unsupported expression errors", in: "${{ secrets.X }}", wantErr: "unsupported reference expression(s): secrets.X"},
		{
			name:    "errors list deduped + sorted",
			in:      "${{ B }}${{ A }}${{ B }}",
			wantErr: "unresolved reference(s): A, B",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := refs.SubstituteRefs(tt.in, tt.sources...)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSubstituteRefs_ErrorDoesNotLeakOtherValues(t *testing.T) {
	// Security contract: the error names the unresolved ref only —
	// never the resolved value of the secret sitting next to the typo.
	_, err := refs.SubstituteRefs("${{ TYPOE }} ${{ SECRET }}",
		map[string]string{"SECRET": "super-secret-value"})
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "super-secret-value") {
		t.Fatalf("error leaked a resolved value: %v", err)
	}
}

func TestSubstituteShellVars(t *testing.T) {
	// Soft pass: known ${VAR} resolve, unknown stay LITERAL (the
	// opposite of the strict ${{ }} hard-fail).
	got := refs.SubstituteShellVars("${KNOWN}/${UNKNOWN}", map[string]string{"KNOWN": "x"})
	if got != "x/${UNKNOWN}" {
		t.Errorf("got %q, want x/${UNKNOWN}", got)
	}
	// Bash parameter-expansion forms are not touched.
	if out := refs.SubstituteShellVars("${PATH:-/usr}", map[string]string{"PATH": "x"}); out != "${PATH:-/usr}" {
		t.Errorf("modifier form should be left alone, got %q", out)
	}
}

func TestSubstituteRefsMap_PreservesKeysAndCarriesKeyContext(t *testing.T) {
	out, err := refs.SubstituteRefsMap(
		map[string]string{"img": "${{ TAG }}"},
		map[string]string{"TAG": "v1"})
	if err != nil || out["img"] != "v1" {
		t.Fatalf("out = %v, err = %v", out, err)
	}
	// The failing key is named in the error so the operator finds it.
	_, err = refs.SubstituteRefsMap(map[string]string{"bad": "${{ MISSING }}"})
	if err == nil || !strings.Contains(err.Error(), "bad:") {
		t.Fatalf("err = %v, want it to name key 'bad'", err)
	}
}

func TestSubstituteMaps_NilPassThrough(t *testing.T) {
	if got, err := refs.SubstituteRefsMap(nil); got != nil || err != nil {
		t.Errorf("SubstituteRefsMap(nil) = %v, %v", got, err)
	}
	if got := refs.SubstituteShellVarsMap(nil, map[string]string{"X": "1"}); got != nil {
		t.Errorf("SubstituteShellVarsMap(nil) = %v, want nil", got)
	}
}

func TestRefPattern_CompiledOnce(t *testing.T) {
	// Compile-once invariant (CLAUDE.md: "regex compiled once in init,
	// not per call"). RefPattern is package-level; SubstituteRefs runs
	// on every env + setting value of every dispatched job, so a
	// refactor that moved compilation into the function would be a hot-
	// path regression. Guards the symbol staying the same instance.
	if refs.RefPattern == nil {
		t.Fatal("RefPattern must be a package-level compiled regex")
	}
	a := refs.RefPattern
	_, _ = refs.SubstituteRefs("${{ X }}", map[string]string{"X": "1"})
	if refs.RefPattern != a {
		t.Errorf("RefPattern reallocated between calls")
	}
}

func TestDedupeSorted(t *testing.T) {
	got := refs.DedupeSorted([]string{"b", "a", "b", "c", "a"})
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}
