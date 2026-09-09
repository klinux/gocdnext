package runner_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"

	"github.com/gocdnext/gocdnext/agent/internal/runner"
)

// tarOneFile builds an uncompressed tar byte stream carrying a single
// regular file, so the codec round-trip tests can wrap it in gzip or zstd
// and assert UntarGz extracts it either way (the reader-first half of the
// zstd cache work, #274).
func tarOneFile(t *testing.T, name, body string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{
		Name:     name,
		Mode:     0o644,
		Size:     int64(len(body)),
		Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatalf("tar header: %v", err)
	}
	if _, err := tw.Write([]byte(body)); err != nil {
		t.Fatalf("tar write: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	return buf.Bytes()
}

func shaOf(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// TestUntarGz_AutoDetectsCodec is the core reader-first guarantee: the SAME
// UntarGz restores a tar whether it arrived gzip-compressed (every cache and
// artifact written today) or zstd-compressed (what the isolated store will
// write in release B). Detection is by content (magic bytes), so a blob
// already sitting in the bucket with no format metadata still restores.
func TestUntarGz_AutoDetectsCodec(t *testing.T) {
	raw := tarOneFile(t, "hello.txt", "conteudo-de-cache")

	gzipBlob := func() []byte {
		var b bytes.Buffer
		zw := gzip.NewWriter(&b)
		_, _ = zw.Write(raw)
		_ = zw.Close()
		return b.Bytes()
	}()

	zstdBlob := func() []byte {
		var b bytes.Buffer
		zw, err := zstd.NewWriter(&b)
		if err != nil {
			t.Fatalf("zstd writer: %v", err)
		}
		_, _ = zw.Write(raw)
		_ = zw.Close()
		return b.Bytes()
	}()

	tests := []struct {
		name string
		blob []byte
	}{
		{"gzip", gzipBlob},
		{"zstd", zstdBlob},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dest := t.TempDir()
			if err := runner.UntarGz(dest, bytes.NewReader(tt.blob), shaOf(tt.blob)); err != nil {
				t.Fatalf("UntarGz(%s): %v", tt.name, err)
			}
			got, err := os.ReadFile(filepath.Join(dest, "hello.txt"))
			if err != nil {
				t.Fatalf("read extracted: %v", err)
			}
			if string(got) != "conteudo-de-cache" {
				t.Errorf("content = %q, want %q", got, "conteudo-de-cache")
			}
		})
	}
}

// TestUntarGz_RejectsUnknownMagic keeps the detector honest: a blob that is
// neither gzip nor zstd must fail loudly, not be fed to the wrong decoder and
// silently produce a truncated tree.
func TestUntarGz_RejectsUnknownMagic(t *testing.T) {
	junk := []byte("not-a-compressed-stream-at-all")
	err := runner.UntarGz(t.TempDir(), bytes.NewReader(junk), "")
	if err == nil {
		t.Fatal("expected error for unknown-magic blob, got nil")
	}
}
