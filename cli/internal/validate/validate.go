// Package validate implements `gocdnext validate` — parse every
// pipeline under .gocdnext/ with the REAL server parser (extracted
// to server/pkg/parser exactly for this) so a YAML typo dies on the
// laptop instead of at apply/webhook time.
package validate

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gocdnext/gocdnext/server/pkg/parser"
)

// Run validates the pipelines reachable from path and prints one
// line per file. path resolution, in order:
//
//	a single .yaml/.yml file → validate just it
//	a directory containing .gocdnext/ → validate .gocdnext/*.y(a)ml
//	any other directory → validate its *.y(a)ml directly (lets
//	  `gocdnext validate .gocdnext` and fixture dirs work)
//
// Every file is parsed even after the first failure — one broken
// pipeline must not hide the state of the others. Returns an error
// (→ exit 1) when any file fails or none are found.
func Run(w io.Writer, path string) error {
	files, err := resolveFiles(path)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("no pipeline files (*.yaml / *.yml) found under %s", path)
	}

	failures := 0
	// seen mirrors the apply path's set-level check: two files
	// declaring the same pipeline name pass file-by-file parsing
	// but the server rejects the set (apply.go parseFiles).
	seen := map[string]string{}
	for _, f := range files {
		base := filepath.Base(f)
		fh, err := os.Open(f)
		if err != nil {
			failures++
			fmt.Fprintf(w, "FAIL %s: %v\n", base, err)
			continue
		}
		// Fallback name = file name sans extension, same as apply.
		fallback := strings.TrimSuffix(base, filepath.Ext(base))
		p, err := parser.ParseNamed(fh, "validate", fallback)
		_ = fh.Close()
		if err != nil {
			failures++
			fmt.Fprintf(w, "FAIL %s: %v\n", base, err)
			continue
		}
		if prev, dup := seen[p.Name]; dup {
			failures++
			fmt.Fprintf(w, "FAIL %s: pipeline %q already defined in %s — the server rejects this set\n", base, p.Name, prev)
			continue
		}
		seen[p.Name] = base
		fmt.Fprintf(w, "OK   %s — pipeline %q, %d job(s)\n", base, p.Name, len(p.Jobs))
	}
	if failures > 0 {
		return fmt.Errorf("%d of %d pipeline file(s) failed validation", failures, len(files))
	}
	return nil
}

func resolveFiles(path string) ([]string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}
	if !info.IsDir() {
		return []string{path}, nil
	}
	dir := path
	if sub := filepath.Join(path, ".gocdnext"); dirExists(sub) {
		dir = sub
	}
	yaml, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, err
	}
	yml, err := filepath.Glob(filepath.Join(dir, "*.yml"))
	if err != nil {
		return nil, err
	}
	files := append(yaml, yml...)
	sort.Strings(files)
	return files, nil
}

func dirExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}
