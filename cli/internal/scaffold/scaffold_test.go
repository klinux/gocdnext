package scaffold

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gocdnext/gocdnext/cli/internal/apply"
)

// writeFiles drops a set of files (relative path → content) into dir.
func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDetectAndGenerate_NodeBunDocker(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"package.json": `{"name":"@acme/web","engines":{"node":"24.x"},"scripts":{"lint":"eslint .","test":"vitest","build":"vite build"}}`,
		"bun.lock":     "",
		"Dockerfile":   "FROM oven/bun:1\n",
	})

	s, err := Detect(dir)
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if s.Language != "node" || s.NodeManager != "bun" || !s.HasDockerfile {
		t.Fatalf("stack = %+v, want node/bun/docker", s)
	}
	if s.AppName != "web" { // scope stripped
		t.Fatalf("appName = %q, want web", s.AppName)
	}

	files, err := Generate(s) // Generate parse-validates every file
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	got := map[string]string{}
	for _, f := range files {
		got[f.Name] = f.Content
	}
	for _, want := range []string{"build-pr.yaml", "build.yaml", "security.yaml"} {
		if _, ok := got[want]; !ok {
			t.Fatalf("missing %s (got %v)", want, keys(got))
		}
	}
	if !strings.Contains(got["build-pr.yaml"], "bun run lint && bun run test") {
		t.Errorf("build-pr missing chained lint+test:\n%s", got["build-pr.yaml"])
	}
	if !strings.Contains(got["build.yaml"], "gocdnext/buildx@v1") || !strings.Contains(got["build.yaml"], "bun run build") {
		t.Errorf("build.yaml missing image job or build cmd:\n%s", got["build.yaml"])
	}
	if !strings.Contains(got["build.yaml"], "${{ CI_COMMIT_SHORT_SHA }}") {
		t.Errorf("build.yaml should carry the literal sha substitution:\n%s", got["build.yaml"])
	}
}

func TestDetectAndGenerate_NodePnpmNoDocker(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"package.json":   `{"name":"svc","scripts":{"test":"jest"}}`,
		"pnpm-lock.yaml": "",
	})
	s, err := Detect(dir)
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if s.NodeManager != "pnpm" || s.HasDockerfile {
		t.Fatalf("stack = %+v, want pnpm, no docker", s)
	}
	files, err := Generate(s)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	got := byName(files)
	// only test script → check is just the test, no lint chain
	if !strings.Contains(got["build-pr.yaml"], "pnpm run test") || strings.Contains(got["build-pr.yaml"], "run lint") {
		t.Errorf("build-pr should be test-only:\n%s", got["build-pr.yaml"])
	}
	// no Dockerfile → no image job / no image stage
	if strings.Contains(got["build.yaml"], "image") {
		t.Errorf("build.yaml should have no image job without a Dockerfile:\n%s", got["build.yaml"])
	}
}

func TestDetectAndGenerate_Go(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"go.mod":     "module github.com/acme/tool\n\ngo 1.24\n",
		"Dockerfile": "FROM golang:1.24\n",
	})
	s, err := Detect(dir)
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if s.Language != "go" || s.AppName != "tool" {
		t.Fatalf("stack = %+v, want go/tool", s)
	}
	files, err := Generate(s)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	got := byName(files)
	if !strings.Contains(got["build-pr.yaml"], "go test ./...") {
		t.Errorf("go build-pr missing go test:\n%s", got["build-pr.yaml"])
	}
	if !strings.Contains(got["build.yaml"], "go build ./...") || !strings.Contains(got["build.yaml"], "gocdnext/go@v1") {
		t.Errorf("go build.yaml wrong:\n%s", got["build.yaml"])
	}
}

func TestGenerate_NodeNoScripts_InstallOnlyPRJob(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"package.json": `{"name":"bare"}`,
		"bun.lock":     "",
	})
	s, _ := Detect(dir)
	// no test/lint/build scripts → warnings recorded
	if len(s.Warnings) == 0 {
		t.Fatal("expected warnings for missing scripts")
	}
	files, err := Generate(s)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	got := byName(files)
	// build-pr has no `command:` → install-only job (still valid YAML)
	if strings.Contains(got["build-pr.yaml"], "command:") {
		t.Errorf("build-pr should be install-only (no command) with no scripts:\n%s", got["build-pr.yaml"])
	}
}

func TestGenerate_NodeNoLockfile_PinsManagerNonFrozen(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"package.json": `{"name":"x","scripts":{"test":"t","build":"b"}}`, // no lockfile
	})
	s, _ := Detect(dir)
	if s.HasLockfile {
		t.Fatal("no lockfile should mean HasLockfile=false")
	}
	if !hasWarning(s.Warnings, "no lockfile") {
		t.Fatalf("expected a no-lockfile warning, got %v", s.Warnings)
	}
	files, err := Generate(s)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	got := byName(files)
	// Without a lockfile, manager:auto would fail at runtime — so the
	// scaffold pins manager:npm + frozen:false (npm install, not ci).
	if !strings.Contains(got["build.yaml"], "manager: npm") || !strings.Contains(got["build.yaml"], "frozen: false") {
		t.Errorf("no-lockfile build.yaml must pin manager + frozen:false:\n%s", got["build.yaml"])
	}
	// And with a lockfile it must NOT (auto is cleaner).
	dir2 := t.TempDir()
	writeFiles(t, dir2, map[string]string{"package.json": `{"name":"y","scripts":{"test":"t","build":"b"}}`, "bun.lock": ""})
	s2, _ := Detect(dir2)
	f2, _ := Generate(s2)
	if strings.Contains(byName(f2)["build.yaml"], "manager:") {
		t.Errorf("with a lockfile, build.yaml should rely on manager:auto (no manager key):\n%s", byName(f2)["build.yaml"])
	}
}

func TestDetect_PackageManagerNoLockfile_AlignsManager(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		// packageManager pnpm but NO lockfile — forcing npm would trip the
		// plugin's lockfile/packageManager conflict guard at runtime.
		"package.json": `{"name":"x","packageManager":"pnpm@9.15.0","scripts":{"test":"t","build":"b"}}`,
	})
	s, _ := Detect(dir)
	if s.NodeManager != "pnpm" {
		t.Fatalf("manager = %q, want pnpm (from packageManager)", s.NodeManager)
	}
	got := byName(mustGenerate(t, s))
	if !strings.Contains(got["build.yaml"], "manager: pnpm") || !strings.Contains(got["build.yaml"], "frozen: false") {
		t.Errorf("expected manager: pnpm + frozen: false:\n%s", got["build.yaml"])
	}
	if strings.Contains(got["build.yaml"], "manager: npm") {
		t.Errorf("must not force npm when packageManager is pnpm:\n%s", got["build.yaml"])
	}
	if !strings.Contains(got["build-pr.yaml"], "pnpm run test") {
		t.Errorf("check command should use pnpm:\n%s", got["build-pr.yaml"])
	}
}

func TestDetect_YarnV1Warns_BerryDoesNot(t *testing.T) {
	// yarn.lock without .yarnrc.yml → Yarn v1 → warn (plugin rejects it).
	v1 := t.TempDir()
	writeFiles(t, v1, map[string]string{"package.json": `{"name":"x","scripts":{"test":"t"}}`, "yarn.lock": ""})
	s1, _ := Detect(v1)
	if !hasWarning(s1.Warnings, "Yarn v1") {
		t.Fatalf("expected a Yarn v1 warning, got %v", s1.Warnings)
	}
	// yarn.lock WITH .yarnrc.yml → Yarn 3+ → no v1 warning.
	berry := t.TempDir()
	writeFiles(t, berry, map[string]string{"package.json": `{"name":"x","scripts":{"test":"t"}}`, "yarn.lock": "", ".yarnrc.yml": "nodeLinker: node-modules\n"})
	s2, _ := Detect(berry)
	if hasWarning(s2.Warnings, "Yarn v1") {
		t.Fatalf("berry should not warn v1, got %v", s2.Warnings)
	}
	// packageManager yarn@4 but NO yarn.lock → Berry via corepack, not v1.
	pm := t.TempDir()
	writeFiles(t, pm, map[string]string{"package.json": `{"name":"x","packageManager":"yarn@4.1.0","scripts":{"test":"t"}}`})
	s3, _ := Detect(pm)
	if hasWarning(s3.Warnings, "Yarn v1") {
		t.Fatalf("yarn@4 without a lockfile must not warn v1, got %v", s3.Warnings)
	}
}

func mustGenerate(t *testing.T, s Stack) []apply.File {
	t.Helper()
	files, err := Generate(s)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	return files
}

func TestWrite_RefusesSymlinkedGocdnext(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"package.json": `{"name":"x","scripts":{"test":"t"}}`, "bun.lock": ""})
	s, _ := Detect(dir)
	files, _ := Generate(s)

	// Point .gocdnext at an outside dir — Write must refuse, not escape.
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, ".gocdnext")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if _, err := Write(dir, files, true); err == nil {
		t.Fatal("expected Write to refuse a symlinked .gocdnext")
	}
	// Nothing should have been written into the symlink target.
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("wrote through the symlink into %s: %v", outside, entries)
	}
}

func hasWarning(warns []string, sub string) bool {
	for _, w := range warns {
		if strings.Contains(w, sub) {
			return true
		}
	}
	return false
}

func TestDetect_Unknown(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"README.md": "hi"})
	if _, err := Detect(dir); err == nil {
		t.Fatal("expected error for an unsupported stack")
	}
}

func TestWrite_NoClobberWithoutForce(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"package.json": `{"name":"x","scripts":{"test":"t"}}`, "bun.lock": ""})
	s, _ := Detect(dir)
	files, _ := Generate(s)

	if _, err := Write(dir, files, false); err != nil {
		t.Fatalf("first write: %v", err)
	}
	// second write without force must refuse
	if _, err := Write(dir, files, false); err == nil {
		t.Fatal("expected refusal to overwrite without --force")
	}
	// with force it overwrites
	if _, err := Write(dir, files, true); err != nil {
		t.Fatalf("force write: %v", err)
	}
	// the files landed under .gocdnext/
	if _, err := os.Stat(filepath.Join(dir, ".gocdnext", "build-pr.yaml")); err != nil {
		t.Fatalf("build-pr.yaml not written: %v", err)
	}
}

func byName(files []apply.File) map[string]string {
	m := map[string]string{}
	for _, f := range files {
		m[f.Name] = f.Content
	}
	return m
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
