package rpc

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gocdnext/gocdnext/agent/internal/runner"
)

// TestArtifactTarCmd_ByCodec pins the in-pod artifact tar command per codec.
// gzip MUST stay the pre-zstd direct `tar -czf` (no shell) so nothing changes
// for the default; zstd pipes tar into `zstd -T0` under pipefail so a
// compressor failure on a truncated stream can't be masked by tar's exit 0.
// Gzipping an already-compressed artifact (a .zip, a tar.gz) is ~80s of pure
// CPU waste on 2 GB; zstd's fast path stores incompressible blocks near memcpy
// speed (#274 follow-up — artifacts, not just caches).
func TestArtifactTarCmd_ByCodec(t *testing.T) {
	const wd = "/workspace/src/abc"
	files := []string{"dist/a.jar", "dist/b.zip"}

	t.Run("gzip stays direct tar -czf (unchanged)", func(t *testing.T) {
		u := &ArtifactUploader{compression: codecGzip}
		cmd := u.artifactTarCmd(wd, files)
		want := []string{"tar", "-czf", "-", "-C", wd, "--", "dist/a.jar", "dist/b.zip"}
		if strings.Join(cmd, " ") != strings.Join(want, " ") {
			t.Errorf("gzip cmd = %v, want %v", cmd, want)
		}
	})

	t.Run("zstd pipes through zstd -T0 with pipefail", func(t *testing.T) {
		u := &ArtifactUploader{compression: codecZstd}
		cmd := u.artifactTarCmd(wd, files)
		if cmd[0] != "sh" || cmd[1] != "-c" {
			t.Fatalf("zstd cmd should be sh -c form, got %v", cmd)
		}
		script := cmd[2]
		for _, must := range []string{"pipefail", "zstd -T0", "tar -cf -"} {
			if !strings.Contains(script, must) {
				t.Errorf("zstd script missing %q: %q", must, script)
			}
		}
		// workDir + files reach the shell as positional args, never
		// interpolated into the script (injection-safe).
		if strings.Contains(script, wd) {
			t.Errorf("workDir must not be interpolated into the script: %q", script)
		}
		tail := strings.Join(cmd[len(cmd)-2:], " ")
		if tail != "dist/a.jar dist/b.zip" {
			t.Errorf("files should be the trailing positional args, got %v", cmd)
		}
	})
}

// TestArtifactUseCompression_FailSafeToGzip: unset/garbage → gzip, never a
// codec the housekeeper image can't run.
func TestArtifactUseCompression_FailSafeToGzip(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", codecGzip},
		{"gzip", codecGzip},
		{"zstd", codecZstd},
		{"ZSTD", codecZstd},
		{"  zstd ", codecZstd},
		{"br", codecGzip},
	}
	for _, tt := range tests {
		u := &ArtifactUploader{}
		u.UseCompression(tt.in)
		if u.compression != tt.want {
			t.Errorf("UseCompression(%q) = %q, want %q", tt.in, u.compression, tt.want)
		}
	}
}

// TestArtifactTarCmd_Zstd_RoundTrips runs the REAL zstd artifact tar command
// through a pipefail shell (as the housekeeper would) and feeds its output to
// the REAL restore reader (runner.UntarGz), proving the `-C "$dir" -- "$@"`
// form round-trips. Skips where zstd/pipefail-sh aren't available; CI's
// gocdnext-housekeeper image carries both.
func TestArtifactTarCmd_Zstd_RoundTrips(t *testing.T) {
	if _, err := exec.LookPath("zstd"); err != nil {
		t.Skip("zstd not installed")
	}
	// A shell whose `set -o pipefail` works; CI /bin/sh may be dash, so fall
	// back to bash (the housekeeper image's busybox sh supports it).
	shell := ""
	for _, name := range []string{"sh", "bash"} {
		if p, err := exec.LookPath(name); err == nil && exec.Command(p, "-c", "set -o pipefail").Run() == nil {
			shell = p
			break
		}
	}
	if shell == "" {
		t.Skip("no pipefail-capable shell")
	}

	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "dist"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "dist", "a.txt"), []byte("artifact-zstd"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Mirror uploadOneFromPod's exec: <shell> -c <script> _ <dir> <files...>.
	u := &ArtifactUploader{compression: codecZstd}
	cmd := u.artifactTarCmd(src, []string{"dist"})
	run := exec.Command(shell, append([]string{"-c", cmd[2]}, cmd[3:]...)...)
	var out, errb bytes.Buffer
	run.Stdout, run.Stderr = &out, &errb
	if err := run.Run(); err != nil {
		t.Fatalf("run artifact zstd script: %v (stderr=%s)", err, errb.String())
	}
	if out.Len() < 4 || out.Bytes()[0] != 0x28 {
		t.Fatalf("output is not a zstd stream (len=%d)", out.Len())
	}

	dest := t.TempDir()
	if err := runner.UntarGz(dest, bytes.NewReader(out.Bytes()), ""); err != nil {
		t.Fatalf("restore reader failed on zstd artifact output: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "dist", "a.txt"))
	if err != nil {
		t.Fatalf("read restored: %v", err)
	}
	if string(got) != "artifact-zstd" {
		t.Errorf("restored = %q, want %q", got, "artifact-zstd")
	}
}
