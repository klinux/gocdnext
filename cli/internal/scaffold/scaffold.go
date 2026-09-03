package scaffold

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/gocdnext/gocdnext/cli/internal/apply"
	"github.com/gocdnext/gocdnext/server/pkg/parser"
)

// Generate renders the starter pipeline set for a detected stack and
// validates every file with the REAL server parser before returning it —
// so a template bug fails here (and in tests), never at apply/webhook
// time. The returned files plug straight into `apply` / `apply.Post`.
func Generate(s Stack) ([]apply.File, error) {
	if s.Language == "" {
		return nil, fmt.Errorf("no supported stack to scaffold")
	}
	d := dataFor(s)

	set := []struct {
		name string
		tmpl *template.Template
	}{
		{"build-pr.yaml", buildPRTmpl},
		{"build.yaml", buildTmpl},
		{"security.yaml", securityTmpl},
	}

	files := make([]apply.File, 0, len(set))
	for _, item := range set {
		content, err := render(item.tmpl, d)
		if err != nil {
			return nil, fmt.Errorf("render %s: %w", item.name, err)
		}
		fallback := strings.TrimSuffix(item.name, filepath.Ext(item.name))
		if _, err := parser.ParseNamed(strings.NewReader(content), "scaffold", fallback); err != nil {
			// A generated file that doesn't parse is a bug in the template,
			// not user input — fail loud rather than write invalid YAML.
			return nil, fmt.Errorf("internal: generated %s is invalid: %w", item.name, err)
		}
		files = append(files, apply.File{Name: item.name, Content: content})
	}
	return files, nil
}

// Write persists the files under <root>/.gocdnext/. It refuses to
// overwrite any existing file unless force is set, and checks ALL targets
// before writing ANY, so a partial clobber can't happen. Returns the
// repo-relative paths written.
func Write(root string, files []apply.File, force bool) ([]string, error) {
	dir := filepath.Join(root, ".gocdnext")

	// Refuse a symlinked .gocdnext — a `.gocdnext -> /elsewhere` in an
	// untrusted checkout would make us write OUTSIDE the repo (even
	// MkdirAll happily follows it). Lstat sees the link itself.
	if fi, err := os.Lstat(dir); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf(".gocdnext is a symlink — refusing to write through it")
		}
		if !fi.IsDir() {
			return nil, fmt.Errorf(".gocdnext exists but is not a directory")
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create .gocdnext: %w", err)
	}

	// Pre-flight every target BEFORE writing any (no partial clobber):
	// a symlinked target is refused even with --force (writing through it
	// escapes the dir); a regular existing file needs --force.
	for _, f := range files {
		p := filepath.Join(dir, f.Name)
		rel := filepath.Join(".gocdnext", f.Name)
		fi, err := os.Lstat(p)
		if errors.Is(err, os.ErrNotExist) {
			continue // absent — fine
		}
		if err != nil {
			// A non-"not-exist" stat error (permissions, odd FS) must abort
			// the pre-flight — otherwise "check all before writing any" lies.
			return nil, fmt.Errorf("stat %s: %w", rel, err)
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("%s is a symlink — refusing to overwrite", rel)
		}
		if !force {
			return nil, fmt.Errorf("%s already exists — re-run with --force to overwrite", rel)
		}
	}

	written := make([]string, 0, len(files))
	for _, f := range files {
		p := filepath.Join(dir, f.Name)
		if err := os.WriteFile(p, []byte(f.Content), 0o644); err != nil {
			return written, fmt.Errorf("write %s: %w", p, err)
		}
		written = append(written, filepath.Join(".gocdnext", f.Name))
	}
	return written, nil
}

// Summary is the one-line "detected: …" description of a stack.
func (s Stack) Summary() string {
	var parts []string
	if s.Language == "node" && s.NodeVersion != "" {
		parts = append(parts, "node "+s.NodeVersion) // fold version in, don't repeat "node"
	} else {
		parts = append(parts, s.Language)
	}
	if s.NodeManager != "" {
		parts = append(parts, s.NodeManager)
	}
	if s.HasDockerfile {
		parts = append(parts, "Dockerfile")
	}
	return strings.Join(parts, ", ")
}
