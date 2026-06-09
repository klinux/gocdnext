package runner

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/gocdnext/gocdnext/agent/internal/engine"
)

// fakeResolverExec is the test stub for engine.PodExecutor used by
// the podHashResolver tests. Distinct from the rpc-package fake
// because it routes only `find` and `cat`, which is all the
// resolver issues.
type fakeResolverExec struct {
	mu       sync.Mutex
	findOut  string
	findErr  error
	files    map[string][]byte // absolute path → cat stdout
	catErrs  map[string]error
	gotFinds int
	gotCats  []string
}

var _ engine.PodExecutor = (*fakeResolverExec)(nil)

func (f *fakeResolverExec) Exec(_ context.Context, _, _ string, cmd []string,
	_ io.Reader, stdout, stderr io.Writer) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(cmd) == 0 {
		return errors.New("empty command")
	}
	switch cmd[0] {
	case "find":
		f.gotFinds++
		if f.findErr != nil {
			_, _ = stderr.Write([]byte("find sim error"))
			return f.findErr
		}
		_, _ = stdout.Write([]byte(f.findOut))
		return nil
	case "cat":
		if len(cmd) < 3 {
			return errors.New("cat needs path")
		}
		p := cmd[2]
		f.gotCats = append(f.gotCats, p)
		if e, ok := f.catErrs[p]; ok {
			_, _ = stderr.Write([]byte("cat sim error"))
			return e
		}
		if b, ok := f.files[p]; ok {
			_, _ = stdout.Write(b)
			return nil
		}
		return errors.New("file not found")
	default:
		return errors.New("unexpected: " + cmd[0])
	}
}

// TestPodHashResolver_DeterministicAcrossListingOrder is the
// load-bearing assertion: the same workspace content produces the
// same key regardless of `find`'s iteration order. Without sort
// inside Hash, two hosts could produce different keys for the
// SAME pnpm-lock.yaml — and operators chasing cache misses would
// see ghost invalidations.
func TestPodHashResolver_DeterministicAcrossListingOrder(t *testing.T) {
	const workDir = "/workspace/src/abc"

	// Same files, different order out of `find`. The resolver
	// sorts internally, so both should hash to the same value.
	exec1 := &fakeResolverExec{
		findOut: strings.Join([]string{
			workDir + "/a.gradle",
			workDir + "/b.gradle",
			workDir + "/main.kt", // irrelevant — glob won't match
		}, "\n") + "\n",
		files: map[string][]byte{
			workDir + "/a.gradle": []byte("apply plugin: 'kotlin'\n"),
			workDir + "/b.gradle": []byte("dependencies { ... }\n"),
		},
	}
	exec2 := &fakeResolverExec{
		findOut: strings.Join([]string{
			workDir + "/b.gradle", // reversed
			workDir + "/main.kt",
			workDir + "/a.gradle",
		}, "\n") + "\n",
		files: exec1.files,
	}

	r1 := newPodHashResolver(context.Background(), exec1, "pod", "cache-fetch", workDir)
	r2 := newPodHashResolver(context.Background(), exec2, "pod", "cache-fetch", workDir)

	h1, err := r1.Hash("*.gradle")
	if err != nil {
		t.Fatalf("r1.Hash: %v", err)
	}
	h2, err := r2.Hash("*.gradle")
	if err != nil {
		t.Fatalf("r2.Hash: %v", err)
	}
	if h1 != h2 {
		t.Errorf("deterministic contract violated: h1=%q h2=%q", h1, h2)
	}
	if len(h1) != 12 {
		t.Errorf("hash length = %d, want 12 (HashOutputLength)", len(h1))
	}
}

// TestPodHashResolver_DifferentContentDifferentKey — the inverse
// of the determinism contract. Same path-set, different content =>
// different key. This is the WHOLE POINT of cache key templating;
// silently colliding on different bytes would defeat invalidation.
func TestPodHashResolver_DifferentContentDifferentKey(t *testing.T) {
	const workDir = "/w"
	mk := func(content string) *fakeResolverExec {
		return &fakeResolverExec{
			findOut: workDir + "/lock.yaml\n",
			files:   map[string][]byte{workDir + "/lock.yaml": []byte(content)},
		}
	}
	r1 := newPodHashResolver(context.Background(), mk("v1.2.3"), "pod", "c", workDir)
	r2 := newPodHashResolver(context.Background(), mk("v1.2.4"), "pod", "c", workDir)

	h1, _ := r1.Hash("lock.yaml")
	h2, _ := r2.Hash("lock.yaml")
	if h1 == h2 {
		t.Errorf("different content produced same key (%q == %q)", h1, h2)
	}
}

// TestPodHashResolver_ZeroMatchesFailLoud — the operator declared
// invalidation by these file(s); a silent "" would let two
// different lockfiles share a key. Same posture as the host
// resolver (cachekey_expand.go).
func TestPodHashResolver_ZeroMatchesFailLoud(t *testing.T) {
	const workDir = "/w"
	exec := &fakeResolverExec{
		findOut: workDir + "/main.go\n",
		files:   map[string][]byte{workDir + "/main.go": []byte("package main")},
	}
	r := newPodHashResolver(context.Background(), exec, "pod", "c", workDir)
	_, err := r.Hash("pnpm-lock.yaml")
	if err == nil {
		t.Fatal("expected error on zero matches, got nil")
	}
	if !strings.Contains(err.Error(), "matched 0 files") {
		t.Errorf("err = %q, want 'matched 0 files'", err)
	}
}

// TestPodHashResolver_DoubleStarRecursion — the key Card pattern
// case: `**/build/test-results/**/*.xml`-style globs need
// doublestar `**` recursion. filepath.Glob's broken `**` semantics
// would zero-match here too — verifying we're using podfs.
func TestPodHashResolver_DoubleStarRecursion(t *testing.T) {
	const workDir = "/w"
	exec := &fakeResolverExec{
		findOut: strings.Join([]string{
			workDir + "/domain/build.gradle",
			workDir + "/secondary/mysql/build.gradle",
			workDir + "/main.kt",
		}, "\n") + "\n",
		files: map[string][]byte{
			workDir + "/domain/build.gradle":          []byte("x"),
			workDir + "/secondary/mysql/build.gradle": []byte("y"),
		},
	}
	r := newPodHashResolver(context.Background(), exec, "pod", "c", workDir)
	got, err := r.Hash("**/build.gradle")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if len(got) != 12 {
		t.Errorf("hash length = %d, want 12", len(got))
	}
	// 2 files catted (matches), 1 find. main.kt skipped.
	if len(exec.gotCats) != 2 {
		t.Errorf("cat calls = %d, want 2 (matched files only); got %v", len(exec.gotCats), exec.gotCats)
	}
	if exec.gotFinds != 1 {
		t.Errorf("find calls = %d, want 1", exec.gotFinds)
	}
}

// TestPodHashResolver_ReuseEnumerationAcrossCalls — multiple
// Hash() calls (e.g. a template with two `hash(...)` tokens)
// SHOULD reuse the cached `find` listing. Without that, each token
// re-runs `find` — N×M exec round-trips on a workspace listing.
func TestPodHashResolver_ReuseEnumerationAcrossCalls(t *testing.T) {
	const workDir = "/w"
	exec := &fakeResolverExec{
		findOut: strings.Join([]string{
			workDir + "/a.txt",
			workDir + "/b.txt",
		}, "\n") + "\n",
		files: map[string][]byte{
			workDir + "/a.txt": []byte("a"),
			workDir + "/b.txt": []byte("b"),
		},
	}
	r := newPodHashResolver(context.Background(), exec, "pod", "c", workDir)

	if _, err := r.Hash("a.txt"); err != nil {
		t.Fatalf("first Hash: %v", err)
	}
	if _, err := r.Hash("b.txt"); err != nil {
		t.Fatalf("second Hash: %v", err)
	}
	if exec.gotFinds != 1 {
		t.Errorf("find calls = %d after 2 Hash() invocations, want 1 (cached)", exec.gotFinds)
	}
}
