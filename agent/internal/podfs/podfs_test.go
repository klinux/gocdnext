package podfs_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/gocdnext/gocdnext/agent/internal/engine"
	"github.com/gocdnext/gocdnext/agent/internal/podfs"
)

// staticExec stubs PodExecutor for the podfs unit tests. It just
// emits `out` and `outErr` to the writer/error the caller passes.
type staticExec struct {
	out []byte
	err error
}

var _ engine.PodExecutor = (*staticExec)(nil)

func (s *staticExec) Exec(_ context.Context, _, _ string, _ []string,
	_ io.Reader, stdout, _ io.Writer) error {
	if len(s.out) > 0 {
		_, _ = stdout.Write(s.out)
	}
	return s.err
}

// TestCappedBuffer_TruncationFlag — MED-4 regression: a producer
// that writes past the cap leaves Truncated()=true so the consumer
// can refuse to use the partial buffer. Pre-fix this was silent.
func TestCappedBuffer_TruncationFlag(t *testing.T) {
	cb := &podfs.CappedBuffer{W: &bytes.Buffer{}, Max: 4}
	n, err := cb.Write([]byte("hello"))
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if n != 5 {
		t.Errorf("write returned n=%d, want 5 (callers count CONSUMED bytes, not buffered)", n)
	}
	if !cb.Truncated() {
		t.Error("Truncated() = false after over-cap write; cache-key hashing would silently use partial input")
	}
	if cb.W.String() != "hell" {
		t.Errorf("buffer = %q, want %q", cb.W.String(), "hell")
	}
}

func TestCappedBuffer_UnderCapStaysClean(t *testing.T) {
	cb := &podfs.CappedBuffer{W: &bytes.Buffer{}, Max: 100}
	_, _ = cb.Write([]byte("hi"))
	if cb.Truncated() {
		t.Error("Truncated() = true on under-cap write")
	}
}

func TestCappedBuffer_ExactCapFitsNoOverflow(t *testing.T) {
	cb := &podfs.CappedBuffer{W: &bytes.Buffer{}, Max: 4}
	_, _ = cb.Write([]byte("abcd"))
	if cb.Truncated() {
		t.Error("Truncated() = true on exact-fit write; only over-cap should trigger")
	}
	if cb.W.String() != "abcd" {
		t.Errorf("buffer = %q, want %q", cb.W.String(), "abcd")
	}
}

// TestListFiles_OverflowReturnsError is the MED-4 user-facing fix:
// when `find`'s output exceeds the buffer cap, ListFiles must NOT
// return a partial slice that downstream cache-key hashing would
// silently consume. Mirrors the safety check in
// `agent/internal/podfs/podfs.go`.
func TestListFiles_OverflowReturnsError(t *testing.T) {
	// Build a payload larger than the default 16 MiB cap. We're not
	// testing the real cap (would be slow); we test the principle
	// by forcing overflow on a small cap via direct Exec.
	exec := &staticExec{out: bytes.Repeat([]byte("/workspace/foo\n"), 2_000_000)}
	_, err := podfs.ListFiles(context.Background(), exec, "pod", "container", "/workspace")
	if err == nil {
		t.Fatal("want error on listing overflow, got nil")
	}
	if !strings.Contains(err.Error(), "exceeded") || !strings.Contains(err.Error(), "cap") {
		t.Errorf("err message should mention cap exceeded; got %q", err.Error())
	}
}

func TestListFiles_PropagatesExecError(t *testing.T) {
	exec := &staticExec{err: errors.New("kubelet down")}
	_, err := podfs.ListFiles(context.Background(), exec, "pod", "c", "/workspace")
	if err == nil {
		t.Fatal("want error on exec failure, got nil")
	}
	if !strings.Contains(err.Error(), "kubelet down") {
		t.Errorf("err should wrap underlying: %v", err)
	}
}

func TestListFiles_RejectsRelativeWorkDir(t *testing.T) {
	_, err := podfs.ListFiles(context.Background(), &staticExec{}, "p", "c", "relative")
	if err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Errorf("want absolute-required error, got %v", err)
	}
}

// TestMatchGlobs_Recursive — sanity check on the doublestar
// integration; the artifact and cache paths both rely on `**`
// working.
func TestMatchGlobs_Recursive(t *testing.T) {
	const wd = "/w"
	files := []string{
		wd + "/a/build/test-results/test/x.xml",
		wd + "/b/build/test-results/test/y.xml",
		wd + "/build.gradle",
	}
	got := podfs.MatchGlobs(wd, files, []string{"**/build/test-results/test/*.xml"})
	if len(got) != 2 {
		t.Errorf("got %v, want 2 matches", got)
	}
}

func TestMatchSingleGlob_LiteralPath(t *testing.T) {
	const wd = "/w"
	got := podfs.MatchSingleGlob(wd, []string{wd + "/main.go", wd + "/build.gradle"}, "main.go")
	if len(got) != 1 || got[0] != wd+"/main.go" {
		t.Errorf("got %v, want exactly /w/main.go", got)
	}
}

func TestHasGlobChars(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"main.go", false},
		{"*.go", true},
		{"build/**/x.xml", true},
		{"a?b", true},
		{"[a-z].txt", true},
		{"path/with{a,b}.txt", true},
		{"plain/path/no-meta", false},
	}
	for _, tc := range tests {
		if got := podfs.HasGlobChars(tc.in); got != tc.want {
			t.Errorf("HasGlobChars(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
