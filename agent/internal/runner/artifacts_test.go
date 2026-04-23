package runner_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gocdnext/gocdnext/agent/internal/runner"
)

func TestTarGzPath_SingleFile(t *testing.T) {
	dir := t.TempDir()
	content := []byte("hello artifact")
	if err := os.WriteFile(filepath.Join(dir, "bin"), content, 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	sha, size, err := runner.TarGzPath(dir, "bin", &buf)
	if err != nil {
		t.Fatalf("TarGzPath: %v", err)
	}
	if size <= 0 {
		t.Errorf("size = %d", size)
	}
	if len(sha) != 64 {
		t.Errorf("sha len = %d, want 64", len(sha))
	}

	// Unpack and verify content.
	gr, err := gzip.NewReader(&buf)
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	tr := tar.NewReader(gr)
	h, err := tr.Next()
	if err != nil {
		t.Fatalf("tar next: %v", err)
	}
	if h.Name != "bin" {
		t.Errorf("tar header name = %q, want bin", h.Name)
	}
	got, _ := io.ReadAll(tr)
	if !bytes.Equal(got, content) {
		t.Errorf("content mismatch: got %q", got)
	}
}

func TestTarGzPath_Directory(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "out", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "out", "top.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "out", "sub", "nested.bin"), []byte("bb"), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	_, _, err := runner.TarGzPath(dir, "out", &buf)
	if err != nil {
		t.Fatalf("TarGzPath: %v", err)
	}

	gr, _ := gzip.NewReader(&buf)
	tr := tar.NewReader(gr)
	seen := map[string]int64{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar: %v", err)
		}
		body, _ := io.ReadAll(tr)
		seen[h.Name] = int64(len(body))
	}
	if seen["out/top.txt"] != 1 {
		t.Errorf("out/top.txt size = %d", seen["out/top.txt"])
	}
	if seen["out/sub/nested.bin"] != 2 {
		t.Errorf("out/sub/nested.bin size = %d", seen["out/sub/nested.bin"])
	}
}

func TestTarGzPath_NestedDirPreservesFullPath(t *testing.T) {
	// Regression: `artifacts: paths: [web/node_modules/]` used to
	// land as `node_modules/...` in the tar (basename-only), so a
	// consumer that `cd web` to use pnpm found nothing — the unpack
	// ended up at scriptWorkDir/node_modules instead of
	// scriptWorkDir/web/node_modules. Full relative path must be
	// preserved so the producer + consumer agree on the layout.
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "web", "node_modules", ".bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "web", "node_modules", ".bin", "tsc"),
		[]byte("#!/bin/sh\necho tsc"), 0o755); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if _, _, err := runner.TarGzPath(dir, "web/node_modules/", &buf); err != nil {
		t.Fatalf("TarGzPath: %v", err)
	}

	gr, _ := gzip.NewReader(&buf)
	tr := tar.NewReader(gr)
	found := false
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar: %v", err)
		}
		if h.Name == "web/node_modules/.bin/tsc" {
			found = true
		}
		if h.Name == "node_modules/.bin/tsc" {
			t.Errorf("tar entry %q is flattened — producer/consumer path mismatch", h.Name)
		}
	}
	if !found {
		t.Error("expected tar entry web/node_modules/.bin/tsc, not found")
	}
}

func TestTarGzPath_RoundTripsSymlinks(t *testing.T) {
	// Regression: pnpm's node_modules uses symlinks heavily
	// (`node_modules/react → .pnpm/react@X/node_modules/react`).
	// An earlier version wrote the tar header with an empty link
	// target and UntarGz ignored non-dir/non-reg entries entirely,
	// so every symlink got stripped. Consumers then saw a
	// `node_modules/` missing every top-level package → `pnpm exec`
	// couldn't find anything.
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "pkg", "real", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "pkg", "real", "bin", "tsc"),
		[]byte("#!/bin/sh\necho tsc"), 0o755); err != nil {
		t.Fatal(err)
	}
	// pnpm-style relative symlink: node_modules/react →
	// .pnpm/react@X/node_modules/react. Here we simplify to
	// `pkg/link -> real/bin` which exercises the same mechanism.
	if err := os.Symlink("real/bin", filepath.Join(src, "pkg", "link")); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	sha, _, err := runner.TarGzPath(src, "pkg", &buf)
	if err != nil {
		t.Fatalf("tar: %v", err)
	}

	dest := t.TempDir()
	if err := runner.UntarGz(dest, &buf, sha); err != nil {
		t.Fatalf("untar: %v", err)
	}

	// Link must exist and point at its original target.
	linkAbs := filepath.Join(dest, "pkg", "link")
	got, err := os.Readlink(linkAbs)
	if err != nil {
		t.Fatalf("readlink %q: %v", linkAbs, err)
	}
	if got != "real/bin" {
		t.Errorf("symlink target = %q, want %q", got, "real/bin")
	}
	// Following the symlink must resolve to the real file.
	tscAbs := filepath.Join(linkAbs, "tsc")
	content, err := os.ReadFile(tscAbs)
	if err != nil {
		t.Fatalf("read through symlink: %v", err)
	}
	if !strings.Contains(string(content), "echo tsc") {
		t.Errorf("content via symlink: %q", content)
	}
}

func TestUntarGz_RejectsAbsoluteSymlinks(t *testing.T) {
	// Guard: a producer that writes a symlink with target
	// `/etc/passwd` would let the extracted node_modules reach
	// sensitive host files. Reject on extract.
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{
		Name:     "evil",
		Typeflag: tar.TypeSymlink,
		Linkname: "/etc/passwd",
		Mode:     0o777,
	}); err != nil {
		t.Fatal(err)
	}
	_ = tw.Close()
	_ = gz.Close()

	err := runner.UntarGz(t.TempDir(), &buf, "")
	if err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("expected absolute-symlink refusal, got %v", err)
	}
}

func TestUntarGz_RejectsEscapingSymlinks(t *testing.T) {
	// Relative symlinks that resolve above dest are path traversal.
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{
		Name:     "sneaky",
		Typeflag: tar.TypeSymlink,
		Linkname: "../../etc/passwd",
		Mode:     0o777,
	}); err != nil {
		t.Fatal(err)
	}
	_ = tw.Close()
	_ = gz.Close()

	err := runner.UntarGz(t.TempDir(), &buf, "")
	if err == nil || !strings.Contains(err.Error(), "escapes dest") {
		t.Fatalf("expected escape refusal, got %v", err)
	}
}

func TestTarGzPath_Missing(t *testing.T) {
	dir := t.TempDir()
	_, _, err := runner.TarGzPath(dir, "nope", io.Discard)
	if err == nil {
		t.Error("expected error for missing path")
	}
}

func TestUntarGz_RoundTrip(t *testing.T) {
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "bin", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(src, "bin", "core"), []byte("hello"), 0o644)
	_ = os.WriteFile(filepath.Join(src, "bin", "sub", "tool"), []byte("world!"), 0o755)

	var buf bytes.Buffer
	sha, _, err := runner.TarGzPath(src, "bin", &buf)
	if err != nil {
		t.Fatalf("tar: %v", err)
	}

	dest := t.TempDir()
	if err := runner.UntarGz(dest, &buf, sha); err != nil {
		t.Fatalf("untar: %v", err)
	}

	got1, _ := os.ReadFile(filepath.Join(dest, "bin", "core"))
	if string(got1) != "hello" {
		t.Errorf("core: %q", got1)
	}
	got2, _ := os.ReadFile(filepath.Join(dest, "bin", "sub", "tool"))
	if string(got2) != "world!" {
		t.Errorf("tool: %q", got2)
	}
}

func TestUntarGz_ShaMismatchErrors(t *testing.T) {
	src := t.TempDir()
	_ = os.WriteFile(filepath.Join(src, "f"), []byte("x"), 0o644)
	var buf bytes.Buffer
	if _, _, err := runner.TarGzPath(src, "f", &buf); err != nil {
		t.Fatal(err)
	}
	// wrong sha
	err := runner.UntarGz(t.TempDir(), &buf, "deadbeef")
	if err == nil {
		t.Fatal("expected sha mismatch error")
	}
}

func TestUntarGz_RejectsPathTraversal(t *testing.T) {
	// Hand-craft a malicious tar with "../escape" entry; untar must
	// refuse rather than write outside dest.
	var tarBuf bytes.Buffer
	gz := gzip.NewWriter(&tarBuf)
	tw := tar.NewWriter(gz)
	hdr := &tar.Header{
		Name: "../escape.txt", Mode: 0o644, Size: int64(len("pwn")),
		Typeflag: tar.TypeReg,
	}
	_ = tw.WriteHeader(hdr)
	_, _ = tw.Write([]byte("pwn"))
	_ = tw.Close()
	_ = gz.Close()

	if err := runner.UntarGz(t.TempDir(), &tarBuf, ""); err == nil {
		t.Fatal("expected path traversal error")
	}
}

func TestTarGzPath_ShaDeterministicForSameBytes(t *testing.T) {
	// Same content tarred twice must *not* necessarily have the same sha
	// (gzip embeds mtime by default) — document the known limit via test.
	// If this fails someday, it'll be because we started zeroing headers;
	// update the test then.
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "f"), []byte("x"), 0o644)

	var a, b bytes.Buffer
	sha1, _, _ := runner.TarGzPath(dir, "f", &a)
	sha2, _, _ := runner.TarGzPath(dir, "f", &b)

	if sha1 == sha2 {
		// Fine if they match; not a bug either way. This test just exists
		// to document reality — if stable hashing becomes a need later,
		// we'd zero the tar header mtime + gzip header.
		t.Log("sha matched across two runs (same content, same mtime) — OK")
	}
}
