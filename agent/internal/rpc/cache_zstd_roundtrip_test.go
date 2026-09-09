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

// TestCacheTarScript_Zstd_RoundTrips runs the REAL zstd store script through a
// shell that supports pipefail and feeds its output to the REAL restore reader
// (runner.UntarGz), proving the writer and reader halves of the zstd cache work
// agree end-to-end (#274). The production housekeeper image uses BusyBox sh,
// which supports pipefail; GitHub's Ubuntu /bin/sh may be dash, so this unit
// test falls back to bash when the host sh can't run the script faithfully.
func TestCacheTarScript_Zstd_RoundTrips(t *testing.T) {
	if _, err := exec.LookPath("zstd"); err != nil {
		t.Skip("zstd not installed; skipping real round-trip")
	}
	shell := shellWithPipefail(t)

	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "deps"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "deps", "a.txt"), []byte("cache-payload-zstd"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Mirror StoreFromPod's argv: <shell> -c <script> _ <workDir> <paths...>
	script := rpc.CacheTarScriptForTest(rpc.CodecZstdForTest)
	cmd := exec.Command(shell, "-c", script, "_", src, "deps")
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

func shellWithPipefail(t *testing.T) string {
	t.Helper()
	for _, name := range []string{"sh", "bash"} {
		path, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		if err := exec.Command(path, "-c", "set -o pipefail").Run(); err == nil {
			return path
		}
	}
	t.Skip("no shell with pipefail support; skipping real zstd round-trip")
	return ""
}
