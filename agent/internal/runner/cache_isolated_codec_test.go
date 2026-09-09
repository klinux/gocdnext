package runner

import (
	"strings"
	"testing"
)

// TestPodUntarCmd_ByMagic pins the in-pod restore command the agent chooses
// from the blob's leading magic bytes (#274, reader-first). The gzip branch
// MUST stay byte-identical to the pre-zstd command (`tar -xzf`) so release A
// changes nothing for the caches that exist today; only a zstd blob takes the
// new decompress-pipe path.
func TestPodUntarCmd_ByMagic(t *testing.T) {
	const workDir = "/workspace/src/abc"

	t.Run("gzip stays tar -xzf (unchanged)", func(t *testing.T) {
		cmd, err := podUntarCmd([]byte{0x1f, 0x8b, 0x08, 0x00}, workDir)
		if err != nil {
			t.Fatalf("gzip: %v", err)
		}
		want := []string{"tar", "-xzf", "-", "-C", workDir}
		if strings.Join(cmd, " ") != strings.Join(want, " ") {
			t.Errorf("gzip cmd = %v, want %v", cmd, want)
		}
	})

	t.Run("zstd pipes through zstd -dc with pipefail", func(t *testing.T) {
		cmd, err := podUntarCmd([]byte{0x28, 0xb5, 0x2f, 0xfd}, workDir)
		if err != nil {
			t.Fatalf("zstd: %v", err)
		}
		if cmd[0] != "sh" || cmd[1] != "-c" {
			t.Fatalf("zstd cmd should be sh -c form, got %v", cmd)
		}
		script := cmd[2]
		for _, must := range []string{"pipefail", "zstd -dc", "tar -xf"} {
			if !strings.Contains(script, must) {
				t.Errorf("zstd script missing %q: %q", must, script)
			}
		}
		// workDir must reach the shell as a positional arg, never
		// interpolated into the script (injection-safe).
		if cmd[len(cmd)-1] != workDir {
			t.Errorf("workDir should be the last positional arg, got %v", cmd)
		}
		if strings.Contains(script, workDir) {
			t.Errorf("workDir must not be interpolated into the script: %q", script)
		}
	})

	t.Run("unknown magic errors", func(t *testing.T) {
		if _, err := podUntarCmd([]byte{0x00, 0x01, 0x02, 0x03}, workDir); err == nil {
			t.Fatal("expected error for unknown magic, got nil")
		}
	})
}
