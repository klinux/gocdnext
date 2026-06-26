// Command schemagen generates the JSON Schema for the gocdnext pipeline YAML
// spec directly from the canonical parser structs (server/pkg/parser), so the
// schema can never drift from the parser. The struct doc comments become
// `description` fields — that is what powers hover docs in the editor.
//
// Two roots are emitted (self-contained files, regenerated together):
//   - gocdnext-pipeline.schema.json — the full `.gocdnext/*.yaml` pipeline.
//   - gocdnext-policy-fragment.schema.json — the compliance policy body, which
//     CompilePolicy reduces to stages+jobs (see server/pkg/compliance).
//
// Run from the server module root (the Makefile / go:generate directive does):
//
//	go run ./cmd/schemagen -out ../schema
//
// CI regenerates and fails on a diff (see Makefile `schema-check`), so the
// committed schema is always in lock-step with schema.go.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/gocdnext/gocdnext/server/pkg/parser"
	"github.com/invopop/jsonschema"
)

const (
	modulePath = "github.com/gocdnext/gocdnext/server"
	// baseURI is the canonical $id prefix. `latest` tracks main; release
	// publishing copies the same files under a version segment (see #98 D4).
	baseURI = "https://raw.githubusercontent.com/klinux/gocdnext/main/schema"
)

// policyFragment mirrors what compliance.CompilePolicy actually keeps from a
// policy body: only stages + jobs. Every other top-level pipeline field is
// discarded at compile time, so the fragment schema must NOT surface them —
// otherwise an admin thinks `materials`/`services`/`notifications` take effect.
type policyFragment struct {
	Stages []string                 `yaml:"stages"`
	Jobs   map[string]parser.JobDef `yaml:"jobs"`
}

func main() {
	out := flag.String("out", "schema", "output directory for the schema files")
	source := flag.String("source", "./pkg/parser", "path to the parser package source (for doc comments)")
	version := flag.String("version", "latest", "schema version segment (e.g. v0.58.0 or 'latest')")
	flag.Parse()

	if err := run(*out, *source, *version); err != nil {
		fmt.Fprintln(os.Stderr, "schemagen:", err)
		os.Exit(1)
	}
}

func run(outDir, source, version string) error {
	dir := filepath.Join(outDir, version)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}

	roots := []struct {
		file     string
		title    string
		v        any
		fragment bool
	}{
		{"gocdnext-pipeline.schema.json", "gocdnext pipeline", &parser.File{}, false},
		{"gocdnext-policy-fragment.schema.json", "gocdnext compliance policy fragment", &policyFragment{}, true},
	}

	for _, root := range roots {
		s, err := reflectSchema(source, root.title, root.v, root.fragment)
		if err != nil {
			return fmt.Errorf("reflect %s: %w", root.file, err)
		}
		s.ID = jsonschema.ID(fmt.Sprintf("%s/%s/%s", baseURI, version, root.file))
		if err := writeSchema(filepath.Join(dir, root.file), s); err != nil {
			return err
		}
		fmt.Printf("wrote %s\n", filepath.Join(dir, root.file))
	}
	return nil
}

func reflectSchema(source, title string, v any, fragment bool) (*jsonschema.Schema, error) {
	r := new(jsonschema.Reflector)
	// Field names come from the yaml tags, not json — that is the wire format.
	r.FieldNameTag = "yaml"
	// Emit shared definitions under $defs with $ref (keeps the file small and
	// readable; JobDef appears once).
	r.DoNotReference = false
	// Doc comments → descriptions (hover docs). Requires the source on disk.
	if err := r.AddGoComments(modulePath, source); err != nil {
		return nil, fmt.Errorf("add go comments: %w", err)
	}
	s := r.Reflect(v)
	s.Title = title
	applyOverrides(s)
	if fragment {
		applyPolicyFragmentOverrides(s)
	}
	return s, nil
}

func writeSchema(path string, s *jsonschema.Schema) error {
	// MarshalIndent over invopop's ordered maps → deterministic output, so the
	// CI drift check (git diff --exit-code) is stable across runs.
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	b = append(b, '\n')
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
