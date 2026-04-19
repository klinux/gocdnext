package runner_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
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

func TestTarGzPath_Missing(t *testing.T) {
	dir := t.TempDir()
	_, _, err := runner.TarGzPath(dir, "nope", io.Discard)
	if err == nil {
		t.Error("expected error for missing path")
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
