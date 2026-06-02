package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gocdnext/gocdnext/proto/cachekey"
)

// writeFiles materialises a {relpath → content} map under root.
// Mirrors the agent's post-checkout workspace shape so the
// resolver tests are realistic (the actual agent runs against a
// just-cloned tree).
func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %q: %v", filepath.Dir(full), err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("write %q: %v", full, err)
		}
	}
}

func TestWorkspaceHashResolver_DeterministicSingleFile(t *testing.T) {
	t.Parallel()
	workDir := t.TempDir()
	writeFiles(t, workDir, map[string]string{
		"pnpm-lock.yaml": "lockfile-v1\nresolution-v1\n",
	})
	r := &workspaceHashResolver{ctx: context.Background(), workDir: workDir}

	first, err := r.Hash("pnpm-lock.yaml")
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	second, err := r.Hash("pnpm-lock.yaml")
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if first != second {
		t.Errorf("non-deterministic: %q != %q", first, second)
	}
	if len(first) != cachekey.HashOutputLength {
		t.Errorf("output length = %d, want %d", len(first), cachekey.HashOutputLength)
	}
	// Content sanity: each hex char is 0-9a-f.
	for _, c := range first {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Errorf("non-hex char %q in output %q", c, first)
		}
	}
}

func TestWorkspaceHashResolver_DifferentContentDifferentHash(t *testing.T) {
	t.Parallel()
	a := t.TempDir()
	b := t.TempDir()
	writeFiles(t, a, map[string]string{"lock.yaml": "version-1"})
	writeFiles(t, b, map[string]string{"lock.yaml": "version-2"})

	ra := &workspaceHashResolver{ctx: context.Background(), workDir: a}
	rb := &workspaceHashResolver{ctx: context.Background(), workDir: b}

	ha, err := ra.Hash("lock.yaml")
	if err != nil {
		t.Fatalf("a: %v", err)
	}
	hb, err := rb.Hash("lock.yaml")
	if err != nil {
		t.Fatalf("b: %v", err)
	}
	if ha == hb {
		t.Errorf("same hash for different content: %q (a=version-1, b=version-2)", ha)
	}
}

func TestWorkspaceHashResolver_GlobSortedAcrossWalks(t *testing.T) {
	t.Parallel()
	workDir := t.TempDir()
	// Three packages with different package.json content; order
	// of filesystem traversal is unspecified, so sort must
	// stabilise the hash.
	writeFiles(t, workDir, map[string]string{
		"apps/web/package.json":    `{"name":"web","version":"1.0.0"}`,
		"apps/api/package.json":    `{"name":"api","version":"1.0.0"}`,
		"apps/worker/package.json": `{"name":"worker","version":"1.0.0"}`,
	})
	r := &workspaceHashResolver{ctx: context.Background(), workDir: workDir}

	results := make([]string, 5)
	for i := range results {
		h, err := r.Hash("apps/*/package.json")
		if err != nil {
			t.Fatalf("iter %d: %v", i, err)
		}
		results[i] = h
	}
	for i := 1; i < len(results); i++ {
		if results[i] != results[0] {
			t.Errorf("hash drift across iterations: %v", results)
			break
		}
	}
}

func TestWorkspaceHashResolver_PathChangeChangesHash(t *testing.T) {
	t.Parallel()
	// Same content under two different relative paths must
	// produce different hashes — renaming a file should bust
	// the cache.
	a := t.TempDir()
	b := t.TempDir()
	writeFiles(t, a, map[string]string{"original.yaml": "same content"})
	writeFiles(t, b, map[string]string{"renamed.yaml": "same content"})

	ra := &workspaceHashResolver{ctx: context.Background(), workDir: a}
	rb := &workspaceHashResolver{ctx: context.Background(), workDir: b}

	ha, _ := ra.Hash("original.yaml")
	hb, _ := rb.Hash("renamed.yaml")
	if ha == hb {
		t.Errorf("rename didn't change hash: same key for original.yaml and renamed.yaml")
	}
}

func TestWorkspaceHashResolver_ZeroMatchesFails(t *testing.T) {
	t.Parallel()
	workDir := t.TempDir()
	r := &workspaceHashResolver{ctx: context.Background(), workDir: workDir}

	_, err := r.Hash("missing-lock.yaml")
	if err == nil {
		t.Fatalf("expected error for zero matches, got nil")
	}
	if !strings.Contains(err.Error(), "matched 0 files") {
		t.Errorf("err = %v, want 'matched 0 files'", err)
	}
}

func TestWorkspaceHashResolver_TooManyMatchesFails(t *testing.T) {
	t.Parallel()
	workDir := t.TempDir()
	files := make(map[string]string, MaxCacheKeyGlobFiles+5)
	for i := 0; i < MaxCacheKeyGlobFiles+5; i++ {
		files[filepath.Join("pkgs", "p"+leftPad(i)+".json")] = "x"
	}
	writeFiles(t, workDir, files)
	r := &workspaceHashResolver{ctx: context.Background(), workDir: workDir}

	_, err := r.Hash("pkgs/*.json")
	if err == nil {
		t.Fatalf("expected error for too many matches, got nil")
	}
	if !strings.Contains(err.Error(), "exceeds max") {
		t.Errorf("err = %v, want 'exceeds max'", err)
	}
}

func leftPad(n int) string {
	// 4-char zero-padded; enough for 100+ files in the test.
	s := "0000" + intToString(n)
	return s[len(s)-4:]
}

// intToString — small helper so we don't import strconv for one
// call.
func intToString(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [16]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// TestWorkspaceHashResolver_PreCanceledContextFailsFast proves
// CancelJob propagation: a job canceled before/while the resolver
// is hashing aborts on the next chunk boundary instead of running
// to completion. Pre-cancel the ctx; the resolver must return
// ctx.Err immediately at the per-file gate.
func TestWorkspaceHashResolver_PreCanceledContextFailsFast(t *testing.T) {
	t.Parallel()
	workDir := t.TempDir()
	writeFiles(t, workDir, map[string]string{"lock.yaml": "content"})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel
	r := &workspaceHashResolver{ctx: ctx, workDir: workDir}

	_, err := r.Hash("lock.yaml")
	if err == nil {
		t.Fatalf("Hash returned nil error for canceled ctx")
	}
	if !strings.Contains(err.Error(), "context canceled") {
		t.Errorf("err = %v, want it to mention context canceled", err)
	}
}

// TestWorkspaceHashResolver_PerFileByteCap proves a single
// oversized file is rejected before consuming pre-task minutes.
// Uses tiny caps (defined in the test) by writing a file just
// past MaxCacheKeyHashBytesPerFile — would otherwise need a
// 16 MiB write, so we stage the test against the public const.
func TestWorkspaceHashResolver_PerFileByteCap(t *testing.T) {
	t.Parallel()
	workDir := t.TempDir()
	// File 1 byte past the per-file cap.
	big := strings.Repeat("a", MaxCacheKeyHashBytesPerFile+1)
	writeFiles(t, workDir, map[string]string{"huge.bin": big})

	r := &workspaceHashResolver{ctx: context.Background(), workDir: workDir}
	_, err := r.Hash("huge.bin")
	if err == nil {
		t.Fatalf("Hash returned nil for over-cap file")
	}
	if !strings.Contains(err.Error(), "per-file cap") {
		t.Errorf("err = %v, want it to mention per-file cap", err)
	}
}

// TestWorkspaceHashResolver_RejectsDirectorySymlinkEscape closes
// the chain-symlink hole that a leaf-only Lstat misses:
//
//	workspace/locks -> /etc            (directory symlink)
//	cache.key: k-{{ hash "locks/passwd" }}
//
// filepath.Glob resolves the directory symlink during expansion
// and yields `workspace/locks/passwd`. A naive Lstat on that
// path follows the directory symlink and sees `/etc/passwd` as
// a regular file — the leaf-only check passes and the hasher
// streams content from outside the workspace. The 12-hex
// digest is a weak oracle, but it's enough to make this a
// real workspace-boundary escape. EvalSymlinks + containment
// against the resolved realWorkDir is the closing piece.
func TestWorkspaceHashResolver_RejectsDirectorySymlinkEscape(t *testing.T) {
	t.Parallel()
	workDir := t.TempDir()
	outside := t.TempDir()
	target := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(target, []byte("not yours"), 0o644); err != nil {
		t.Fatalf("write target: %v", err)
	}
	// Symlink the WHOLE outside dir into the workspace as a
	// subdirectory. The leaf (secret.txt) is a regular file
	// inside outside/, so leaf-only checks would pass.
	if err := os.Symlink(outside, filepath.Join(workDir, "linkdir")); err != nil {
		t.Fatalf("symlink dir: %v", err)
	}

	r := &workspaceHashResolver{ctx: context.Background(), workDir: workDir}
	_, err := r.Hash("linkdir/secret.txt")
	if err == nil {
		t.Fatalf("Hash returned nil for dir-symlink escape (read %q via workspace/linkdir)", target)
	}
	if !strings.Contains(err.Error(), "resolves outside workspace") {
		t.Errorf("err = %v, want 'resolves outside workspace'", err)
	}
}

// TestWorkspaceHashResolver_RejectsDirectorySymlinkEscapeViaGlob
// — same escape vector but via a wildcard pattern. filepath.Glob
// walks the linked directory and returns matches inside it; the
// per-match containment check has to fire on EACH glob result,
// not just the user-typed pattern.
func TestWorkspaceHashResolver_RejectsDirectorySymlinkEscapeViaGlob(t *testing.T) {
	t.Parallel()
	workDir := t.TempDir()
	outside := t.TempDir()
	for _, name := range []string{"a.txt", "b.txt"} {
		if err := os.WriteFile(filepath.Join(outside, name), []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(workDir, "linkdir")); err != nil {
		t.Fatalf("symlink dir: %v", err)
	}

	r := &workspaceHashResolver{ctx: context.Background(), workDir: workDir}
	_, err := r.Hash("linkdir/*.txt")
	if err == nil {
		t.Fatalf("Hash returned nil for glob through dir-symlink")
	}
	if !strings.Contains(err.Error(), "resolves outside workspace") {
		t.Errorf("err = %v, want 'resolves outside workspace'", err)
	}
}

// TestWorkspaceHashResolver_DirectorySymlinkInsideWorkspaceOK is
// the inverse guard: a directory symlink pointing AT another
// directory INSIDE the workspace stays inside containment. The
// check should not flag this — it's a legitimate workspace
// navigation aid (monorepo convenience links, etc.).
func TestWorkspaceHashResolver_DirectorySymlinkInsideWorkspaceOK(t *testing.T) {
	t.Parallel()
	workDir := t.TempDir()
	realDir := filepath.Join(workDir, "real")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(realDir, "lock.yaml"), []byte("inside"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Symlink(realDir, filepath.Join(workDir, "link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	r := &workspaceHashResolver{ctx: context.Background(), workDir: workDir}
	got, err := r.Hash("link/lock.yaml")
	if err != nil {
		t.Fatalf("Hash returned %v for legitimate in-workspace dir-symlink", err)
	}
	if len(got) != cachekey.HashOutputLength {
		t.Errorf("output length = %d, want %d", len(got), cachekey.HashOutputLength)
	}
}

// TestWorkspaceHashResolver_RejectsLeafSymlinkInsideWorkspace
// exercises the LEAF-Lstat check independently from containment.
// The leaf is a symlink pointing at another file WITHIN the
// workspace, so containment passes (target is contained) but the
// leaf is still a symlink — rejected at Lstat. This keeps the
// digest a pure function of regular-file content (a symlink swap
// would otherwise change the digest based on link metadata vs
// real content, hurting determinism).
//
// Companion to TestWorkspaceHashResolver_RejectsDirectorySymlinkEscape
// which covers the OUTSIDE-workspace case via the containment
// check. Both checks must fire on their respective paths.
func TestWorkspaceHashResolver_RejectsLeafSymlinkInsideWorkspace(t *testing.T) {
	t.Parallel()
	workDir := t.TempDir()
	// Target inside the workspace, so containment passes.
	target := filepath.Join(workDir, "real.yaml")
	if err := os.WriteFile(target, []byte("inside"), 0o644); err != nil {
		t.Fatalf("write target: %v", err)
	}
	link := filepath.Join(workDir, "lock.yaml")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	r := &workspaceHashResolver{ctx: context.Background(), workDir: workDir}
	_, err := r.Hash("lock.yaml")
	if err == nil {
		t.Fatalf("Hash returned nil for leaf symlink (containment passes; Lstat must still reject)")
	}
	if !strings.Contains(err.Error(), "regular file") {
		t.Errorf("err = %v, want 'regular file' (leaf-Lstat path)", err)
	}
}

// TestWorkspaceHashResolver_DigestMatchesManualSHA pins the
// concrete hash output for a known input so a future refactor of
// the digest construction (separator change, ordering tweak)
// fails loud instead of silently invalidating every operator's
// cache.
func TestWorkspaceHashResolver_DigestMatchesManualSHA(t *testing.T) {
	t.Parallel()
	workDir := t.TempDir()
	const content = "fixed content\n"
	writeFiles(t, workDir, map[string]string{"lock.yaml": content})

	// Reproduce the resolver's hashing recipe by hand:
	//   "lock.yaml\n" + content + "\n"
	h := sha256.New()
	h.Write([]byte("lock.yaml\n"))
	h.Write([]byte(content))
	h.Write([]byte("\n"))
	want := hex.EncodeToString(h.Sum(nil))[:cachekey.HashOutputLength]

	r := &workspaceHashResolver{ctx: context.Background(), workDir: workDir}
	got, err := r.Hash("lock.yaml")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if got != want {
		t.Errorf("digest recipe drift: got %q, want %q", got, want)
	}
}
