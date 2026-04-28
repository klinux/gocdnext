// Package plugins maintains the in-memory catalog of known plugin
// manifests. Each `plugin.yaml` in the monorepo (read at startup)
// becomes a Spec the parser consults when a pipeline declares
// `uses: gocdnext/<name>`. Third-party images without a manifest
// are not rejected — they pass validation with a warn log so the
// catalog can grow organically without gating legitimate use.
package plugins

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Spec is the in-memory shape of one plugin's manifest. Keys
// mirror the YAML fields directly; the only normalisation is
// that Inputs gets indexed by name for O(1) validation.
type Spec struct {
	Name        string
	Description string
	Category    string
	Inputs      map[string]Input
	Examples    []Example
}

// Input describes one accepted entry of the job's `with:` map.
// `Required` is enforced at apply time; `Default` is informational
// only (the plugin's entrypoint owns the default — we don't
// inject it into the PLUGIN_* env, matching Woodpecker's hands-
// off posture).
type Input struct {
	Required    bool
	Default     string
	Description string
}

// Example is a documented usage snippet surfaced in the catalog
// UI. Kept deliberately simple — three fields, plain text — so
// operators read the YAML and copy/paste without translation.
// Plugins without examples still validate; the field is meta
// rather than enforcement.
type Example struct {
	Name        string
	Description string
	YAML        string
}

// Catalog is the server-side registry: name → Spec. The zero
// value is a valid empty catalog — operators without any plugin
// manifest dir can still run third-party images (with a warn).
type Catalog struct {
	specs map[string]Spec
}

// New builds an empty catalog. Call Load or register specs
// manually for tests.
func New() *Catalog {
	return &Catalog{specs: map[string]Spec{}}
}

// Load walks `root` looking for `<root>/<name>/plugin.yaml`
// files and registers each one. Missing `root` is a soft
// no-op — deployments without the monorepo plugin dir (e.g.,
// a bare server image) still boot; the parser falls back to
// "unknown plugin, pass through" for any `uses:`.
//
// A manifest whose `name` field disagrees with its directory is
// a config bug and returns an error — otherwise operators would
// chase ghost "why doesn't my schema apply?" tickets.
//
// When the same plugin name appears in multiple roots (because
// LoadAll was called or Load was called multiple times) the
// later read wins — by design, so an operator can override a
// baked manifest from a sidecar ConfigMap.
func (c *Catalog) Load(root string) error {
	info, err := os.Stat(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("plugins: stat %q: %w", root, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("plugins: %q is not a directory", root)
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		return fmt.Errorf("plugins: read %q: %w", root, err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		manifest := filepath.Join(root, e.Name(), "plugin.yaml")
		spec, err := readManifest(manifest)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				// Dir without a manifest: skip silently. Might
				// be a sibling asset (fixtures, docs) that
				// happens to live under plugins/.
				continue
			}
			return fmt.Errorf("plugins: load %s: %w", e.Name(), err)
		}
		if spec.Name == "" {
			return fmt.Errorf("plugins: %s: manifest missing `name`", manifest)
		}
		if spec.Name != e.Name() {
			return fmt.Errorf("plugins: %s: manifest name %q does not match dir %q",
				manifest, spec.Name, e.Name())
		}
		c.specs[spec.Name] = spec
	}
	return nil
}

// LoadAll calls Load for each path in `roots` in order. Roots
// past the first can override earlier ones — this is what lets
// the chart bake the official catalogue under one path and let
// operators drop their own manifests into a ConfigMap mounted at
// a second path. A failure in any root short-circuits with the
// underlying error so a typo'd manifest doesn't silently degrade
// to "no validation" mode.
func (c *Catalog) LoadAll(roots []string) error {
	for _, r := range roots {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		if err := c.Load(r); err != nil {
			return err
		}
	}
	return nil
}

// SplitCatalogDirs parses the GOCDNEXT_PLUGIN_CATALOG_DIR env
// value — a colon-separated list of paths, $PATH-style — into
// the slice LoadAll consumes. Empty entries are dropped so a
// trailing colon doesn't blow up Load with `stat ""`.
func SplitCatalogDirs(envValue string) []string {
	parts := strings.Split(envValue, ":")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Lookup resolves a `uses:` reference to its Spec, or (_, false)
// when the plugin isn't in the catalog. The parser uses the
// false return to decide "pass-through, no validation" instead
// of failing outright.
//
// The `uses:` value may carry a version (`gocdnext/node@v1`) or
// a digest (`gocdnext/node@sha256:abc`) — both strip to the
// short name for lookup. A fully-qualified registry path
// (`ghcr.io/acme/plugin@v1`) strips to the last path segment
// for lookup but will usually miss the catalog (third-party),
// which is the expected outcome.
func (c *Catalog) Lookup(uses string) (Spec, bool) {
	name := shortNameForLookup(uses)
	if name == "" {
		return Spec{}, false
	}
	spec, ok := c.specs[name]
	return spec, ok
}

// Names returns the catalog keys in insertion-order-independent
// sorted form. Used by the upcoming HTTP catalog endpoint and
// docs generators.
func (c *Catalog) Names() []string {
	out := make([]string, 0, len(c.specs))
	for n := range c.specs {
		out = append(out, n)
	}
	// stable for UI + docs — alphabetical is fine since plugins
	// are equal citizens, no "featured" bucket.
	sortStrings(out)
	return out
}

// Specs returns all registered specs by copy. Used by the HTTP
// catalog endpoint.
func (c *Catalog) Specs() []Spec {
	names := c.Names()
	out := make([]Spec, 0, len(names))
	for _, n := range names {
		out = append(out, c.specs[n])
	}
	return out
}

// Register inserts a spec directly. Test-only convenience so
// catalog consumers can be exercised without touching disk.
func (c *Catalog) Register(s Spec) {
	if c.specs == nil {
		c.specs = map[string]Spec{}
	}
	c.specs[s.Name] = s
}

// Validate checks a job's `with:` map against a plugin's Spec:
// every required input must be present; no unknown keys allowed.
// Empty `with:` with all-optional inputs is valid. Returns nil
// on unknown plugin so the parser can decide pass-through vs
// strict validation per policy.
func (c *Catalog) Validate(uses string, with map[string]string) error {
	spec, ok := c.Lookup(uses)
	if !ok {
		return nil
	}
	for name, in := range spec.Inputs {
		if !in.Required {
			continue
		}
		if _, set := with[name]; !set {
			return fmt.Errorf(
				"plugin %s: required input %q is missing (description: %s)",
				spec.Name, name, in.Description)
		}
	}
	for k := range with {
		if _, known := spec.Inputs[k]; !known {
			return fmt.Errorf(
				"plugin %s: unknown input %q (did you typo? known inputs: %s)",
				spec.Name, k, inputNames(spec.Inputs))
		}
	}
	return nil
}

// --- internals ---

type manifestYAML struct {
	Name        string                       `yaml:"name"`
	Description string                       `yaml:"description"`
	Category    string                       `yaml:"category"`
	Inputs      map[string]manifestInputYAML `yaml:"inputs"`
	Examples    []manifestExampleYAML        `yaml:"examples"`
}

type manifestInputYAML struct {
	Required    bool   `yaml:"required"`
	Default     string `yaml:"default"`
	Description string `yaml:"description"`
}

type manifestExampleYAML struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	YAML        string `yaml:"yaml"`
}

func readManifest(path string) (Spec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Spec{}, err
	}
	var raw manifestYAML
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return Spec{}, fmt.Errorf("yaml: %w", err)
	}
	spec := Spec{
		Name:        raw.Name,
		Description: strings.TrimSpace(raw.Description),
		Category:    strings.TrimSpace(raw.Category),
		Inputs:      make(map[string]Input, len(raw.Inputs)),
		Examples:    make([]Example, 0, len(raw.Examples)),
	}
	for n, in := range raw.Inputs {
		spec.Inputs[n] = Input{
			Required:    in.Required,
			Default:     in.Default,
			Description: in.Description,
		}
	}
	for _, ex := range raw.Examples {
		spec.Examples = append(spec.Examples, Example{
			Name:        strings.TrimSpace(ex.Name),
			Description: strings.TrimSpace(ex.Description),
			YAML:        strings.TrimRight(ex.YAML, "\n"),
		})
	}
	return spec, nil
}

// shortNameForLookup extracts the `<name>` out of a `uses:`
// reference. For `gocdnext/node@v1` → "node". For
// `ghcr.io/acme/foo@sha256:...` → "foo". Used only to key into
// the catalog; the full ref stays intact for actual image pulls.
func shortNameForLookup(uses string) string {
	// Strip `@...` (tag or digest) first.
	if at := strings.Index(uses, "@"); at >= 0 {
		uses = uses[:at]
	}
	// And `:tag` if the operator happened to use docker-colon form.
	if colon := strings.LastIndex(uses, ":"); colon >= 0 {
		// Be careful not to swallow `registry.io:5000/path`. A real
		// port sits before a `/`; a tag sits after the last `/`.
		lastSlash := strings.LastIndex(uses, "/")
		if colon > lastSlash {
			uses = uses[:colon]
		}
	}
	// Take the last path segment: `gocdnext/node` → "node".
	if slash := strings.LastIndex(uses, "/"); slash >= 0 {
		return uses[slash+1:]
	}
	return uses
}

func inputNames(inputs map[string]Input) string {
	names := make([]string, 0, len(inputs))
	for n := range inputs {
		names = append(names, n)
	}
	sortStrings(names)
	return strings.Join(names, ", ")
}

// sortStrings is a thin wrapper on sort.Strings so every
// ordering call site names the same helper — makes it easy to
// grep "everywhere catalog ordering happens" in one shot.
func sortStrings(s []string) { sort.Strings(s) }
