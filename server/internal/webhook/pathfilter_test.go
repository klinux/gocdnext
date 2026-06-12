package webhook

import "testing"

func TestPathsMatch(t *testing.T) {
	tests := []struct {
		name  string
		globs []string
		files []string
		known bool
		want  bool
	}{
		{"no globs always fires", nil, []string{"README.md"}, true, true},
		{"unknown set fails open", []string{"**/*.go"}, nil, false, true},
		{"unknown set fails open even with globs and files", []string{"**/*.go"}, []string{"docs/x.md"}, false, true},
		{"go file matches doublestar", []string{"**/*.go"}, []string{"internal/store/x.go"}, true, true},
		{"root-level go file matches **", []string{"**/*.go"}, []string{"main.go"}, true, true},
		{"docs-only change filtered out", []string{"**/*.go", "go.mod"}, []string{"README.md", "docs/a.md"}, true, false},
		{"dir glob matches nested", []string{"web/**"}, []string{"web/src/app/page.tsx"}, true, true},
		{"dir glob does not match sibling", []string{"web/**"}, []string{"api/openapi.yaml"}, true, false},
		{"exact file", []string{"go.mod"}, []string{"go.mod"}, true, true},
		{"one of many files matches", []string{"cmd/**"}, []string{"README.md", "cmd/server/main.go"}, true, true},
		{"empty changed set with globs filtered", []string{"**/*.go"}, []string{}, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := pathsMatch(tt.globs, tt.files, tt.known)
			if got != tt.want {
				t.Fatalf("pathsMatch(%v, %v, %v) = %v, want %v",
					tt.globs, tt.files, tt.known, got, tt.want)
			}
		})
	}
}
