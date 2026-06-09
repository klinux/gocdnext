// Package podfs centralises the agent-side helpers for reaching into
// a job pod's ephemeral PVC via PodExecutor.Exec in workspace-
// isolated mode. Three shapes shared between consumers:
//
//   - ListFiles — enumerate every regular file under a workspace
//     root with a single `find -type f` exec.
//   - MatchGlobs — apply doublestar (with `**` recursion) against
//     a file list and a set of YAML-declared glob patterns.
//   - CappedBuffer — io.Writer that bounds its backing buffer so a
//     pathological exec stdout can't pin the agent's memory.
//
// Lives in its own package so test_reports and artifact upload
// (and future consumers) can share the implementation without
// circular imports between `runner` and `rpc`. The shared-mode
// equivalent walks the agent host filesystem directly via
// `os.Stat` / `filepath.Glob` — that path doesn't need these
// helpers and stays where it is.
package podfs

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/gocdnext/gocdnext/agent/internal/engine"
)

// DefaultListBufCap is the upper bound on `find` stdout. 16 MiB is
// enough for ~150k file paths at ~100 chars each — well above any
// real CI workspace. A workspace pathologically larger gets its
// listing truncated; downstream globs match what's in the truncated
// set and the operator sees fewer matches than expected. Tradeoff
// favours bounding agent memory over completeness.
const DefaultListBufCap = 16 << 20

// CappedBuffer is a thin io.Writer that bounds a backing buffer at
// `Max` bytes — extra bytes are silently dropped so the SPDY exec
// stream stays clean. Closing the writer mid-stream surfaces as a
// transport error indistinguishable from a real network failure,
// hence the silent-drop policy. Callers gate behaviour on Len()
// against the cap (`Len() == Max` → overflowed).
type CappedBuffer struct {
	W   *bytes.Buffer
	Max int
}

func (c *CappedBuffer) Write(p []byte) (int, error) {
	remaining := c.Max - c.W.Len()
	if remaining <= 0 {
		return len(p), nil
	}
	if len(p) > remaining {
		_, _ = c.W.Write(p[:remaining])
		return len(p), nil
	}
	return c.W.Write(p)
}

// NewListBuffer is a convenience that builds a CappedBuffer over a
// fresh bytes.Buffer sized for the typical `find` output. Use this
// when the caller doesn't need to peek at the underlying buffer
// before exec.
func NewListBuffer() *CappedBuffer {
	return &CappedBuffer{W: &bytes.Buffer{}, Max: DefaultListBufCap}
}

// ListFiles runs `find <workDir> -type f` inside the named container
// of the pod and returns absolute paths of every regular file in
// the workspace.
//
// Globbing is done by callers via MatchGlobs so one `find` invocation
// can drive multiple declared patterns (test_reports + artifacts +
// caches), and the agent stays shell-agnostic — busybox (alpine),
// bash, and dash accept this invocation unchanged.
//
// workDir MUST be absolute; relative roots would resolve against the
// container's CWD, which is operator-controlled YAML and would let a
// surprise `cd` in the user's task break enumeration.
func ListFiles(
	ctx context.Context,
	exec engine.PodExecutor,
	pod, container, workDir string,
) ([]string, error) {
	if workDir == "" || !path.IsAbs(workDir) {
		return nil, fmt.Errorf("podfs: workDir must be absolute, got %q", workDir)
	}
	if exec == nil {
		return nil, fmt.Errorf("podfs: nil PodExecutor")
	}
	stdout := NewListBuffer()
	var stderr bytes.Buffer
	if err := exec.Exec(ctx, pod, container,
		[]string{"find", workDir, "-type", "f"},
		nil, stdout, &stderr,
	); err != nil {
		return nil, fmt.Errorf("podfs: exec find in %s/%s: %w (stderr=%q)",
			pod, container, err, stderr.String())
	}
	lines := strings.Split(strings.TrimRight(stdout.W.String(), "\n"), "\n")
	out := lines[:0]
	for _, l := range lines {
		if l == "" {
			continue
		}
		out = append(out, l)
	}
	return out, nil
}

// MatchGlobs filters `files` to those matching any of the declared
// `globs` (joined under workDir). Uses doublestar.PathMatch so `**`
// recursion works the same as the shared-mode `expandGlobs` path,
// keeping the two workspace modes glob-equivalent.
//
// Deduplicates by absolute path. Empty patterns are skipped.
// Returns nil when either inputs are empty.
func MatchGlobs(workDir string, files, globs []string) []string {
	if len(files) == 0 || len(globs) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(files))
	out := make([]string, 0, len(files))
	for _, g := range globs {
		if g == "" {
			continue
		}
		pat := path.Join(workDir, g)
		for _, f := range files {
			if seen[f] {
				continue
			}
			ok, err := doublestar.PathMatch(pat, f)
			if err != nil || !ok {
				continue
			}
			seen[f] = true
			out = append(out, f)
		}
	}
	return out
}

// MatchSingleGlob is the per-pattern variant of MatchGlobs — keeps
// the 1:N relationship between a declared path and its resolved
// files explicit. Used by callers that need to PRESERVE which
// declared pattern produced which matches (e.g., artifact upload,
// where each declared path becomes one archive containing its
// matches).
//
// Returns nil for empty inputs or no matches.
func MatchSingleGlob(workDir string, files []string, glob string) []string {
	if len(files) == 0 || glob == "" {
		return nil
	}
	pat := path.Join(workDir, glob)
	out := make([]string, 0, len(files))
	for _, f := range files {
		ok, err := doublestar.PathMatch(pat, f)
		if err != nil || !ok {
			continue
		}
		out = append(out, f)
	}
	return out
}

// HasGlobChars reports whether `s` contains any of the meta-
// characters MatchGlobs / doublestar treats specially. Lets a caller
// short-circuit the `find` + match path when the declared path is a
// literal — saving an exec round-trip on the common case.
//
// Conservative on `[`: a literal `[` would still trigger the fast-
// path miss and route to glob; doublestar.PathMatch on a literal
// pattern degrades to string equality, so the worst case is one
// extra exec call. Cheap enough.
func HasGlobChars(s string) bool {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '*', '?', '[', ']', '{', '}':
			return true
		}
	}
	return false
}

// Compile-time guard: callers should be reading from the buffer via
// its embedded bytes.Buffer, but expose String() in case a future
// caller wants it without reaching in.
var _ io.Writer = (*CappedBuffer)(nil)
