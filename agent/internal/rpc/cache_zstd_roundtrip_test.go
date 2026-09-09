package rpc_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/gocdnext/gocdnext/agent/internal/rpc"
	"github.com/gocdnext/gocdnext/agent/internal/runner"
)

// TestCacheTarScript_Zstd_RoundTrips runs the REAL zstd store script through
// `sh` (as the housekeeper would) and feeds its output to the REAL restore
// reader (runner.UntarGz), proving the writer and reader halves of the zstd
// cache work agree end-to-end (#274). Skips where zstd/sh aren't installed;
// CI's dedicated housekeeper image carries both.
func TestCacheTarScript_Zstd_RoundTrips(t *testing.T) {
	if _, err := exec.LookPath("zstd"); err != nil {
		t.Skip("zstd not installed; skipping real round-trip")
	}

	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "deps"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "deps", "a.txt"), []byte("cache-payload-zstd"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Mirror StoreFromPod's argv: sh -c <script> _ <workDir> <paths...>
	script := rpc.CacheTarScriptForTest(rpc.CodecZstdForTest)
	cmd := exec.Command("sh", "-c", script, "_", src, "deps")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &bytes.Buffer{}
	if err := cmd.Run(); err != nil {
		t.Fatalf("run zstd store script: %v (stderr=%s)", err, cmd.Stderr)
	}
	if out.Len() < 4 || out.Bytes()[0] != 0x28 {
		t.Fatalf("output is not a zstd stream (magic=%x, len=%d)", out.Bytes()[:min(4, out.Len())], out.Len())
	}

	dest := t.TempDir()
	if err := runner.UntarGz(dest, bytes.NewReader(out.Bytes()), ""); err != nil {
		t.Fatalf("restore reader failed on zstd store output: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "deps", "a.txt"))
	if err != nil {
		t.Fatalf("read restored: %v", err)
	}
	if string(got) != "cache-payload-zstd" {
		t.Errorf("restored content = %q, want %q", got, "cache-payload-zstd")
	}
}
