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
// `Max` bytes — extra bytes are silently dropped from the buffer
// (closing the writer mid-stream would surface as a transport
// error indistinguishable from a real network failure), but the
// Overflowed flag is set so callers can detect and surface the
// truncation. Critical for cache-key hashing, where a partial
// listing would silently produce a key based on incomplete input
// and could restore the WRONG cache content on a future run.
//
// `Truncated()` is the public predicate; check it before consuming
// the buffer when correctness depends on completeness.
type CappedBuffer struct {
	W *bytes.Buffer
	Max int

	overflowed bool
}

func (c *CappedBuffer) Write(p []byte) (int, error) {
	remaining := c.Max - c.W.Len()
	if remaining <= 0 {
		c.overflowed = true
		return len(p), nil
	}
	if len(p) > remaining {
		_, _ = c.W.Write(p[:remaining])
		c.overflowed = true
		return len(p), nil
	}
	return c.W.Write(p)
}

// Truncated reports whether at least one byte was dropped because
// the buffer hit its Max cap. Callers whose downstream logic
// requires completeness (e.g., cache-key hashing) MUST check this
// after the producer finishes writing and fail loud if true —
// silently consuming a truncated buffer is the bug the
// determinism contract exists to prevent.
func (c *CappedBuffer) Truncated() bool { return c.overflowed }

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
	// Fail loud on truncation: a partial listing would silently
	// drop matches downstream — cache-key hashing would compute
	// over the surviving subset and produce a key that
	// "successfully" restores the WRONG cache content next run.
	// Better a hard error here than silent corruption later.
	if stdout.Truncated() {
		return nil, fmt.Errorf(
			"podfs: list output exceeded %d-byte cap in %s/%s (workspace likely has >150k files); "+
				"results would be partial — caller should bail or raise the cap",
			DefaultListBufCap, pod, container)
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

// PathsMissingError is returned by callers that resolve a set of
// declared paths/globs (artifact upload, future cache store) when
// one or more declarations resolved to zero files. Lives in podfs
// (not rpc/runner) because the two packages can't import each
// other (rpc → runner already, no cycle the other way) and both
// the producer (rpc.UploadFromPod) and consumer (runner.PostJob,
// which decides required-fails vs optional-swallows) need to
// type-check via errors.As.
//
// Note: the wording on caller-side logs is intentionally NEUTRAL
// for the optional path. "matched zero files" is honest;
// "failed (continuing)" reads as alarming for what the YAML
// explicitly declared as optional.
type PathsMissingError struct {
	Paths []string
}

func (e *PathsMissingError) Error() string {
	if len(e.Paths) == 1 {
		return fmt.Sprintf("path %q matched zero files in workspace", e.Paths[0])
	}
	return fmt.Sprintf("paths matched zero files in workspace: %s",
		strings.Join(e.Paths, ", "))
}
