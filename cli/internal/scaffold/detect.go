// Package scaffold detects a repo's stack from its local checkout and
// generates a starter set of `.gocdnext/` pipelines (build-pr, build,
// security — no deploy) for the author to review, edit, and commit.
//
// Detection is STATIC: it only reads known manifest files and checks
// paths — it never executes the repo's code. The scan is shallow
// (top-level files only), so it's fast and safe to run anywhere.
package scaffold

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// maxManifestBytes caps any manifest we read into memory.
const maxManifestBytes = 1 << 20 // 1 MiB

// Stack is what detection infers about a repo. A zero Language means
// nothing supported was found.
type Stack struct {
	Language      string          // "node" | "go"
	AppName       string          // for image-name placeholders
	NodeManager   string          // bun | pnpm | yarn | npm (node only)
	NodeVersion   string          // engines.node value, informational
	HasLockfile   bool            // a package manager lockfile was found (node)
	Scripts       map[string]bool // which package.json scripts exist (node)
	HasDockerfile bool
	DefaultBranch string // best-effort ("main")
	Warnings      []string
}

// Detect inspects the checkout at root and returns the inferred stack.
func Detect(root string) (Stack, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return Stack{}, err
	}
	s := Stack{AppName: sanitizeName(filepath.Base(abs)), DefaultBranch: "main"}
	s.HasDockerfile = fileExists(filepath.Join(root, "Dockerfile"))

	hasNode := fileExists(filepath.Join(root, "package.json"))
	hasGo := fileExists(filepath.Join(root, "go.mod"))

	switch {
	case hasNode && hasGo:
		s.Warnings = append(s.Warnings,
			"both package.json and go.mod found — generated for Node; review, or re-run in the sub-app directory")
		s.Language = "node"
		detectNode(root, &s)
	case hasNode:
		s.Language = "node"
		detectNode(root, &s)
	case hasGo:
		s.Language = "go"
		detectGo(root, &s)
	default:
		return s, fmt.Errorf("no supported stack detected under %s (looked for package.json, go.mod)", root)
	}
	return s, nil
}

func detectNode(root string, s *Stack) {
	s.NodeManager, s.HasLockfile = detectNodeManager(root)
	s.Scripts = map[string]bool{}

	var pkg struct {
		Name           string `json:"name"`
		PackageManager string `json:"packageManager"`
		Engines        struct {
			Node string `json:"node"`
		} `json:"engines"`
		Scripts map[string]string `json:"scripts"`
	}
	if b, ok := readCapped(filepath.Join(root, "package.json")); ok {
		// Best-effort: a malformed package.json just yields no scripts,
		// handled by the warnings below — never a hard failure.
		_ = json.Unmarshal(b, &pkg)
	}
	if n := sanitizeName(pkg.Name); n != "" {
		s.AppName = n
	}
	s.NodeVersion = strings.TrimSpace(pkg.Engines.Node)
	for k := range pkg.Scripts {
		s.Scripts[k] = true
	}

	if !s.HasLockfile {
		// Align the manager with an explicit `packageManager` (if any) so
		// the plugin's lockfile/packageManager conflict guard doesn't fire
		// (e.g. packageManager pnpm@9 but we'd forced npm); else keep the
		// npm fallback. Either way it's a non-frozen install (no lockfile).
		if pm := managerFromPackageManager(pkg.PackageManager); pm != "" {
			s.NodeManager = pm
		}
		s.Warnings = append(s.Warnings, fmt.Sprintf(
			"no lockfile found — generated with manager: %s + frozen: false (non-reproducible install); commit a lockfile and re-run for a frozen, reproducible CI",
			s.NodeManager))
	}

	// Yarn v1 (a yarn.lock without .yarnrc.yml) is rejected by node@v3 — the
	// generated pipeline would validate but die at runtime with the plugin's
	// "yarn v1 is not supported". Keyed on the LOCKFILE, not just the manager:
	// a `packageManager: "yarn@4"` with no lockfile is Berry (corepack) and
	// runs fine, so it must NOT warn.
	if fileExists(filepath.Join(root, "yarn.lock")) && !fileExists(filepath.Join(root, ".yarnrc.yml")) {
		s.Warnings = append(s.Warnings,
			"looks like Yarn v1 (yarn.lock without .yarnrc.yml) — node@v3 rejects it; migrate to Yarn 3+ (add .yarnrc.yml) or switch to pnpm/npm before using these pipelines")
	}

	if !s.Scripts["test"] && !s.Scripts["lint"] {
		s.Warnings = append(s.Warnings,
			"no test/lint script in package.json — the PR job only installs deps; add a test script and re-run")
	}
	if !s.Scripts["build"] {
		s.Warnings = append(s.Warnings,
			"no build script in package.json — the build job assumes one; add it or edit build.yaml")
	}
}

func detectGo(root string, s *Stack) {
	if b, ok := readCapped(filepath.Join(root, "go.mod")); ok {
		if mod := goModulePath(b); mod != "" {
			s.AppName = sanitizeName(lastPathSegment(mod))
		}
	}
}

// detectNodeManager picks the manager from the lockfile present; bun's
// lockfile wins (it's unambiguous), then pnpm > yarn > npm. Returns
// found=false when NO lockfile exists — the caller then generates an
// explicit `manager: npm` + `frozen: false` (npm install, not ci), since
// the node plugin's `manager: auto` needs a lockfile to detect from.
func detectNodeManager(root string) (mgr string, found bool) {
	switch {
	case fileExists(filepath.Join(root, "bun.lock")), fileExists(filepath.Join(root, "bun.lockb")):
		return "bun", true
	case fileExists(filepath.Join(root, "pnpm-lock.yaml")):
		return "pnpm", true
	case fileExists(filepath.Join(root, "yarn.lock")):
		return "yarn", true
	case fileExists(filepath.Join(root, "package-lock.json")):
		return "npm", true
	default:
		return "npm", false
	}
}

// managerFromPackageManager extracts the tool from a `packageManager`
// field ("pnpm@9.15.0" → "pnpm"), limited to the managers the node plugin
// supports. Empty when absent or unrecognized.
func managerFromPackageManager(pm string) string {
	tool, _, _ := strings.Cut(strings.TrimSpace(pm), "@")
	switch tool {
	case "bun", "pnpm", "yarn", "npm":
		return tool
	default:
		return ""
	}
}

func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}

// readCapped reads up to maxManifestBytes of a file. ok is false when the
// file can't be read; a truncated read still returns ok (best-effort).
func readCapped(p string) ([]byte, bool) {
	f, err := os.Open(p)
	if err != nil {
		return nil, false
	}
	defer func() { _ = f.Close() }()
	b := make([]byte, maxManifestBytes)
	n, _ := f.Read(b)
	if n <= 0 {
		return nil, false
	}
	return b[:n], true
}

// goModulePath returns the module path from a go.mod's `module` directive.
func goModulePath(b []byte) string {
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(line, "module "); ok {
			return strings.TrimSpace(rest)
		}
	}
	return ""
}

func lastPathSegment(s string) string {
	s = strings.TrimRight(s, "/")
	if i := strings.LastIndex(s, "/"); i >= 0 {
		return s[i+1:]
	}
	return s
}

// sanitizeName lowercases, drops an npm scope (@org/pkg → pkg), and keeps
// only chars valid in an image name — so it's safe to drop into a
// registry placeholder. Empty input → empty output.
func sanitizeName(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndex(s, "/"); i >= 0 { // @scope/pkg → pkg
		s = s[i+1:]
	}
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return strings.Trim(b.String(), "-")
}
