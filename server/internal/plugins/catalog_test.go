package plugins

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCatalog_LoadSurfacesCategoryAndExamples(t *testing.T) {
	// Schema extension: category groups plugins on the UI, examples
	// surface ready-to-copy YAML snippets. Both are optional; the
	// loader must populate them when set and leave them empty
	// otherwise (older manifests that predate the fields still parse).
	root := t.TempDir()
	writePlugin(t, root, "helm", `
name: helm
category: deploy
description: deploy release
inputs:
  command:
    required: true
    description: helm subcmd.
examples:
  - name: upgrade prod
    description: typical release
    yaml: |
      uses: gocdnext/helm@v1
      with:
        command: upgrade --install api ./chart
`)
	writePlugin(t, root, "legacy", `
name: legacy
description: no extras
inputs: {}
`)

	c := New()
	if err := c.Load(root); err != nil {
		t.Fatalf("load: %v", err)
	}

	helm, ok := c.Lookup("gocdnext/helm")
	if !ok {
		t.Fatal("helm missing")
	}
	if helm.Category != "deploy" {
		t.Errorf("category = %q", helm.Category)
	}
	if len(helm.Examples) != 1 {
		t.Fatalf("examples = %d, want 1", len(helm.Examples))
	}
	ex := helm.Examples[0]
	if ex.Name != "upgrade prod" {
		t.Errorf("example name = %q", ex.Name)
	}
	if ex.YAML == "" || !strings.Contains(ex.YAML, "uses: gocdnext/helm@v1") {
		t.Errorf("example yaml missing content:\n%s", ex.YAML)
	}
	// Trailing newline from the YAML block scalar must be trimmed
	// so the frontend can render the snippet flush without an
	// empty line at the bottom.
	if strings.HasSuffix(ex.YAML, "\n") {
		t.Errorf("example yaml ends with trailing newline:\n%q", ex.YAML)
	}

	legacy, ok := c.Lookup("gocdnext/legacy")
	if !ok {
		t.Fatal("legacy missing")
	}
	if legacy.Category != "" {
		t.Errorf("legacy category should be empty, got %q", legacy.Category)
	}
	if len(legacy.Examples) != 0 {
		t.Errorf("legacy examples should be empty, got %d", len(legacy.Examples))
	}
}

func TestCatalog_LoadReadsManifests(t *testing.T) {
	// Seed a temp monorepo shape so the loader exercises the
	// real filesystem walk without depending on the actual
	// plugins/ dir layout (which will grow over time).
	root := t.TempDir()
	writePlugin(t, root, "node", `
name: node
description: |
  Run pnpm commands.
inputs:
  command:
    required: true
    description: pnpm subcommand.
  working-dir:
    required: false
    default: "."
    description: Subdir to cd into.
`)
	writePlugin(t, root, "go", `
name: go
description: Run go build/test.
inputs:
  command:
    required: true
    description: go subcommand + args.
`)

	c := New()
	if err := c.Load(root); err != nil {
		t.Fatalf("load: %v", err)
	}

	names := c.Names()
	if len(names) != 2 || names[0] != "go" || names[1] != "node" {
		t.Errorf("names = %+v, want [go node] (sorted)", names)
	}

	node, ok := c.Lookup("gocdnext/node@v1")
	if !ok {
		t.Fatal("node not found via uses:@v1 ref")
	}
	if !node.Inputs["command"].Required {
		t.Error("command input should be required")
	}
	if got := node.Inputs["working-dir"].Default; got != "." {
		t.Errorf("working-dir default = %q", got)
	}
}

func TestCatalog_LoadMissingRootIsNoOp(t *testing.T) {
	// Deployments without the monorepo plugin dir (bare server
	// image, third-party-only shops) must still boot. Missing
	// root is not an error — the catalog just stays empty and
	// the parser falls back to pass-through validation.
	c := New()
	if err := c.Load("/definitely/does/not/exist"); err != nil {
		t.Errorf("missing root should not error: %v", err)
	}
	if got := len(c.Names()); got != 0 {
		t.Errorf("names = %d, want 0 on missing root", got)
	}
}

func TestCatalog_LoadRejectsNameDirMismatch(t *testing.T) {
	// Manifest's `name:` must match its directory — otherwise
	// operators would write `uses: gocdnext/node` and hit a
	// silent "plugin not found, passing through" warning when
	// they expected schema validation.
	root := t.TempDir()
	writePlugin(t, root, "node", `
name: pinocchio
description: liar.
inputs: {}
`)
	c := New()
	err := c.Load(root)
	if err == nil {
		t.Fatal("expected error for name/dir mismatch")
	}
	if !strings.Contains(err.Error(), "does not match") {
		t.Errorf("err = %v; want something about name/dir mismatch", err)
	}
}

func TestCatalog_LoadSkipsDirsWithoutManifest(t *testing.T) {
	// `plugins/fixtures/` or similar scaffolding dirs shouldn't
	// break the loader; they just aren't plugins.
	root := t.TempDir()
	writePlugin(t, root, "node", `
name: node
description: ok.
inputs: {}
`)
	// Sibling dir without plugin.yaml.
	if err := os.MkdirAll(filepath.Join(root, "fixtures"), 0o755); err != nil {
		t.Fatal(err)
	}
	c := New()
	if err := c.Load(root); err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := len(c.Names()); got != 1 {
		t.Errorf("names = %d, want 1 (fixtures/ must be skipped)", got)
	}
}

func TestCatalog_Validate_MissingRequiredInputFails(t *testing.T) {
	c := New()
	c.Register(Spec{
		Name: "node",
		Inputs: map[string]Input{
			"command": {Required: true, Description: "pnpm cmd"},
		},
	})
	err := c.Validate("gocdnext/node@v1", map[string]string{})
	if err == nil {
		t.Fatal("expected error for missing required input")
	}
	if !strings.Contains(err.Error(), "command") {
		t.Errorf("err = %v; should name the missing input", err)
	}
}

func TestCatalog_Validate_UnknownInputFails(t *testing.T) {
	// Typos are the #1 reason schema validation exists — a
	// `workking-dir` vs `working-dir` would silently land in
	// PLUGIN_WORKKING_DIR env and never take effect. Catch at
	// apply time, name the typo, suggest the known names.
	c := New()
	c.Register(Spec{
		Name: "node",
		Inputs: map[string]Input{
			"command":     {Required: true},
			"working-dir": {Required: false},
		},
	})
	err := c.Validate("gocdnext/node@v1", map[string]string{
		"command":      "install",
		"workking-dir": "web",
	})
	if err == nil {
		t.Fatal("expected error for typo'd input")
	}
	msg := err.Error()
	if !strings.Contains(msg, "workking-dir") {
		t.Errorf("err = %v; should name the typo'd key", err)
	}
	if !strings.Contains(msg, "known inputs") {
		t.Errorf("err = %v; should list known inputs to help the fix", err)
	}
}

func TestCatalog_Validate_UnknownPluginPassesThrough(t *testing.T) {
	// Third-party image (`ghcr.io/someone/else@v1`) not in the
	// catalog must NOT fail — just pass through. Keeps the door
	// open for ad-hoc plugins without forcing operators to
	// register every image they try.
	c := New()
	err := c.Validate("ghcr.io/someone/else@v1", map[string]string{
		"anything": "goes",
	})
	if err != nil {
		t.Errorf("unknown plugin should pass through, got: %v", err)
	}
}

func TestCatalog_Validate_EmptyWithOnAllOptionalSpec(t *testing.T) {
	c := New()
	c.Register(Spec{
		Name: "quiet",
		Inputs: map[string]Input{
			"verbose": {Required: false},
		},
	})
	if err := c.Validate("gocdnext/quiet", nil); err != nil {
		t.Errorf("empty with + all-optional should pass: %v", err)
	}
}

func TestShortNameForLookup(t *testing.T) {
	cases := map[string]string{
		"node":                            "node",
		"gocdnext/node":                   "node",
		"gocdnext/node@v1":                "node",
		"gocdnext/node:v1":                "node",
		"gocdnext/node@sha256:abc":        "node",
		"ghcr.io/acme/foo@v1":             "foo",
		"ghcr.io/acme/foo@sha256:abc":     "foo",
		"registry.io:5000/acme/foo@v1":    "foo",
		"registry.io:5000/acme/foo:v1":    "foo",
	}
	for in, want := range cases {
		t.Run(in, func(t *testing.T) {
			if got := shortNameForLookup(in); got != want {
				t.Errorf("got %q, want %q", got, want)
			}
		})
	}
}

// TestCatalog_LoadAll_OverrideOrder pins the precedence rule:
// when the same plugin name appears in multiple roots, the LAST
// root wins. The chart relies on this so an operator's
// ConfigMap-mounted manifest overrides the baked-in one for
// matching names.
func TestCatalog_LoadAll_OverrideOrder(t *testing.T) {
	baked := t.TempDir()
	extra := t.TempDir()

	writePlugin(t, baked, "helm", `
name: helm
category: deploy
description: baked
inputs: {}
`)
	writePlugin(t, extra, "helm", `
name: helm
category: deploy
description: operator override
inputs: {}
`)

	c := New()
	if err := c.LoadAll([]string{baked, extra}); err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	got, ok := c.Lookup("gocdnext/helm")
	if !ok {
		t.Fatal("helm missing")
	}
	if got.Description != "operator override" {
		t.Errorf("description = %q, want last-wins override", got.Description)
	}

	// And the reverse — flipping order flips the winner.
	c = New()
	if err := c.LoadAll([]string{extra, baked}); err != nil {
		t.Fatalf("LoadAll reversed: %v", err)
	}
	got, _ = c.Lookup("gocdnext/helm")
	if got.Description != "baked" {
		t.Errorf("description = %q, want baked-wins on reversed order", got.Description)
	}
}

// TestCatalog_LoadAll_MergesDistinct verifies that same-named
// plugins follow override rules but distinct names from different
// roots merge cleanly (the catalog is the union).
func TestCatalog_LoadAll_MergesDistinct(t *testing.T) {
	a := t.TempDir()
	b := t.TempDir()
	writePlugin(t, a, "helm", "name: helm\ndescription: x\ninputs: {}\n")
	writePlugin(t, b, "kubectl", "name: kubectl\ndescription: y\ninputs: {}\n")

	c := New()
	if err := c.LoadAll([]string{a, b}); err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if _, ok := c.Lookup("gocdnext/helm"); !ok {
		t.Errorf("helm missing from merged catalog")
	}
	if _, ok := c.Lookup("gocdnext/kubectl"); !ok {
		t.Errorf("kubectl missing from merged catalog")
	}
}

func TestCatalog_LoadAll_SkipsEmpty(t *testing.T) {
	a := t.TempDir()
	writePlugin(t, a, "helm", "name: helm\ndescription: x\ninputs: {}\n")

	c := New()
	// Empty + whitespace-only entries should not abort the walk.
	if err := c.LoadAll([]string{"", "   ", a}); err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if _, ok := c.Lookup("gocdnext/helm"); !ok {
		t.Errorf("helm missing despite valid root present")
	}
}

func TestSplitCatalogDirs(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{":", nil},
		{"a", []string{"a"}},
		{"a:b", []string{"a", "b"}},
		{"  a  :  b  ", []string{"a", "b"}},
		{"/etc/x:/etc/y:/etc/z", []string{"/etc/x", "/etc/y", "/etc/z"}},
		{":/etc/x:", []string{"/etc/x"}},
	}
	for _, tc := range tests {
		got := SplitCatalogDirs(tc.in)
		if len(got) != len(tc.want) {
			t.Errorf("Split(%q) = %v, want %v", tc.in, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("Split(%q)[%d] = %q, want %q", tc.in, i, got[i], tc.want[i])
			}
		}
	}
}

func writePlugin(t *testing.T, root, name, yaml string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(dir, "plugin.yaml"),
		[]byte(strings.TrimSpace(yaml)+"\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
}
