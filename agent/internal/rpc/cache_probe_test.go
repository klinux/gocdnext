package rpc_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gocdnext/gocdnext/agent/internal/rpc"
)

// runProbeScript drives the real shell with the same argv the
// production code builds: `sh -c <script> _ <workDir> <paths...>`.
// `_` is the conventional `$0` placeholder; `$1` is the workDir,
// then the paths flow as `$@`. Returns stdout, exit code, error.
func runProbeScript(t *testing.T, workDir string, paths []string) (string, int, error) {
	t.Helper()
	args := append([]string{"-c", rpc.CacheProbeScriptForTest(), "_", workDir}, paths...)
	cmd := exec.Command("sh", args...)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
		// Treat non-zero exit as data, not as test error.
		return stdout.String(), code, nil
	}
	return stdout.String(), code, err
}

// TestCacheProbeScript_AllPathsMissingExitsZero is the v0.14.8
// regression cover. Pre-fix the script's exit followed the LAST
// `[ -e "$p" ]` test — if every declared path was absent, every
// test returned 1 and the script exited 1. The caller wrapped that
// as `cache store failed (probe paths: exit 1)`. Alarming noise
// for a normal cache-miss-first-run (operator added a cache: block,
// `.gradle-home/` hasn't been populated yet). After the fix, the
// script ALWAYS exits 0; missing-everywhere is signalled by empty
// stdout, not by exit code.
func TestCacheProbeScript_AllPathsMissingExitsZero(t *testing.T) {
	workDir := t.TempDir()
	stdout, code, err := runProbeScript(t, workDir, []string{
		".gradle-home/caches",
		".gradle-home/wrapper",
		".gradle-home/notifications",
	})
	if err != nil {
		t.Fatalf("run probe: %v", err)
	}
	if code != 0 {
		t.Errorf("probe exit = %d, want 0 (empty stdout signals 'no paths', NOT exit 1)", code)
	}
	if strings.TrimSpace(stdout) != "" {
		t.Errorf("stdout = %q, want empty (no paths exist)", stdout)
	}
}

// TestCacheProbeScript_SomePathsExistEmitsOnlyThose validates the
// happy path: when only some paths exist, stdout lists them
// (one per line), and exit is 0. Mirrors what the production code
// expects for the normal cache-hit-then-store flow.
func TestCacheProbeScript_SomePathsExistEmitsOnlyThose(t *testing.T) {
	workDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workDir, "node_modules"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "node_modules", "marker"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	stdout, code, err := runProbeScript(t, workDir, []string{
		"node_modules",
		".gradle-home/caches", // doesn't exist
		"missing-thing",
	})
	if err != nil {
		t.Fatalf("run probe: %v", err)
	}
	if code != 0 {
		t.Errorf("probe exit = %d, want 0", code)
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 1 || lines[0] != "node_modules" {
		t.Errorf("stdout lines = %v, want [node_modules]", lines)
	}
}

// TestCacheProbeScript_LeadingDashDefanged verifies the defang
// for paths starting with `-` (would otherwise be misread as
// options by downstream tar). Pre-existing behaviour, regression
// cover that the v0.14.8 trailing `exit 0` didn't break it.
func TestCacheProbeScript_LeadingDashDefanged(t *testing.T) {
	workDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workDir, "-dist"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	stdout, code, err := runProbeScript(t, workDir, []string{"-dist"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if code != 0 {
		t.Errorf("exit = %d", code)
	}
	if strings.TrimSpace(stdout) != "./-dist" {
		t.Errorf("stdout = %q, want './-dist' (defanged)", stdout)
	}
}

// TestCacheProbeScript_BadWorkDirFailsLoud — the leading `cd "$1"
// || exit 1` is preserved across the fix: an unreachable workDir
// (typo, race with workspace cleanup) MUST exit non-zero so the
// caller routes via the error branch. The fix ONLY made "all
// paths missing" benign; "workspace itself doesn't exist" stays
// a real failure.
func TestCacheProbeScript_BadWorkDirFailsLoud(t *testing.T) {
	_, code, err := runProbeScript(t, "/this/path/should/not/exist/anywhere", []string{"x"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if code == 0 {
		t.Errorf("exit = 0 for missing workDir; want non-zero (cd failed)")
	}
}
