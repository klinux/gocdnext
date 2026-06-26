package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/gocdnext/gocdnext/server/pkg/parser"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"
)

// parserSrc / repoRoot are relative to this package dir (server/cmd/schemagen).
const (
	parserSrc = "../../pkg/parser"
	repoRoot  = "../../.."
)

func compileRoot(t *testing.T, title string, v any, fragment bool) *jsonschema.Schema {
	t.Helper()
	s, err := reflectSchema(parserSrc, title, v, fragment)
	if err != nil {
		t.Fatalf("reflect schema: %v", err)
	}
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal schema: %v", err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("unmarshal schema doc: %v", err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("mem://schema.json", doc); err != nil {
		t.Fatalf("add resource: %v", err)
	}
	sch, err := c.Compile("mem://schema.json")
	if err != nil {
		t.Fatalf("compile schema: %v", err)
	}
	return sch
}

// yamlValue converts a YAML document into a JSON-normalised value the validator
// accepts (json.Number etc.), the same path real YAML takes before validation.
func yamlValue(t *testing.T, src []byte) any {
	t.Helper()
	var y any
	if err := yaml.Unmarshal(src, &y); err != nil {
		t.Fatalf("yaml unmarshal: %v", err)
	}
	jb, err := json.Marshal(y)
	if err != nil {
		t.Fatalf("json marshal: %v", err)
	}
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(jb))
	if err != nil {
		t.Fatalf("json renormalise: %v", err)
	}
	return v
}

// TestRoundTripRealExamples is the anti-over-strict guard: every real pipeline
// shipped in the repo (curated examples + our own CI) MUST validate. A failure
// here means the schema rejects a config the parser accepts.
func TestRoundTripRealExamples(t *testing.T) {
	sch := compileRoot(t, "pipeline", &parser.File{}, false)

	var files []string
	for _, glob := range []string{
		filepath.Join(repoRoot, "examples", "*", ".gocdnext", "*.yaml"),
		filepath.Join(repoRoot, ".gocdnext", "*.yaml"),
	} {
		m, _ := filepath.Glob(glob)
		files = append(files, m...)
	}
	if len(files) == 0 {
		t.Skip("no example pipelines found")
	}

	for _, f := range files {
		t.Run(filepath.Base(filepath.Dir(filepath.Dir(f)))+"/"+filepath.Base(f), func(t *testing.T) {
			src, err := os.ReadFile(f)
			if err != nil {
				t.Fatalf("read %s: %v", f, err)
			}
			if err := sch.Validate(yamlValue(t, src)); err != nil {
				t.Errorf("real example %s rejected by schema (schema too strict?):\n%v", f, err)
			}
		})
	}
}

func TestPipelineFixtures(t *testing.T) {
	sch := compileRoot(t, "pipeline", &parser.File{}, false)

	valid := map[string]string{
		"minimal":            "stages: [build]\njobs:\n  b:\n    stage: build\n    script: [echo hi]\n",
		"plugin job":         "stages: [s]\njobs:\n  j:\n    stage: s\n    uses: ghcr.io/acme/plugin@v1\n    with:\n      key: value\n",
		"material git":       "stages: [s]\nmaterials:\n  - git: {url: https://example.com/r.git}\njobs:\n  j: {stage: s, script: [x]}\n",
		"material upstream":  "stages: [s]\nmaterials:\n  - upstream: {pipeline: up, stage: build}\njobs:\n  j: {stage: s, script: [x]}\n",
		"outputs short":      "stages: [s]\njobs:\n  j:\n    stage: s\n    script: [x]\n    outputs: {next: NEXT}\n",
		"outputs object":     "stages: [s]\njobs:\n  j:\n    stage: s\n    script: [x]\n    outputs:\n      tok: {env: TOK, masked: true}\n",
		"id_tokens scalar":   "stages: [s]\njobs:\n  j:\n    stage: s\n    script: [x]\n    id_tokens: {T: {aud: https://x}}\n",
		"id_tokens sequence": "stages: [s]\njobs:\n  j:\n    stage: s\n    script: [x]\n    id_tokens: {T: {aud: [https://a, https://b]}}\n",
		"approval gate":      "stages: [gate]\njobs:\n  approve:\n    stage: gate\n    approval: {required: 1}\n",
		// Two source fields: the parser picks the first, so the schema accepts
		// it too (permissive superset — see overrideMaterialSpec).
		"material two of": "stages: [s]\nmaterials:\n  - {git: {url: https://x}, cron: {expression: '* * * * *'}}\njobs:\n  j: {stage: s, script: [x]}\n",
	}
	for name, src := range valid {
		t.Run("valid/"+name, func(t *testing.T) {
			if err := sch.Validate(yamlValue(t, []byte(src))); err != nil {
				t.Errorf("expected valid, got: %v", err)
			}
		})
	}

	invalid := map[string]string{
		"missing stages":     "jobs:\n  j: {stage: s, script: [x]}\n",
		"unknown top key":    "stages: [s]\njobs:\n  j: {stage: s, script: [x]}\nbogus: 1\n",
		"material none":      "stages: [s]\nmaterials:\n  - {}\njobs:\n  j: {stage: s, script: [x]}\n",
		"id_token wrong key": "stages: [s]\njobs:\n  j:\n    stage: s\n    script: [x]\n    id_tokens: {T: {audience: https://x}}\n",
	}
	for name, src := range invalid {
		t.Run("invalid/"+name, func(t *testing.T) {
			if err := sch.Validate(yamlValue(t, []byte(src))); err == nil {
				t.Errorf("expected schema rejection, got none")
			}
		})
	}
}

func TestPolicyFragmentFixtures(t *testing.T) {
	sch := compileRoot(t, "policy fragment", &policyFragment{}, true)

	if err := sch.Validate(yamlValue(t, []byte(
		"stages: [_compliance_scan]\njobs:\n  _compliance_scan: {stage: _compliance_scan, uses: ghcr.io/acme/trivy@v1}\n",
	))); err != nil {
		t.Errorf("valid policy fragment rejected: %v", err)
	}

	// A policy must not carry pipeline-level fields it cannot use — the
	// fragment schema rejects them so the admin isn't misled.
	if err := sch.Validate(yamlValue(t, []byte(
		"stages: [_compliance_x]\njobs:\n  j: {stage: _compliance_x, script: [x]}\nmaterials:\n  - git: {url: https://x}\n",
	))); err == nil {
		t.Error("policy fragment should reject `materials`, but accepted it")
	}

	// Stage and job names must carry the reserved prefix — surfaced inline in
	// the editor instead of only on submit.
	if err := sch.Validate(yamlValue(t, []byte(
		"stages: [_compliance_x]\njobs:\n  scan: {stage: _compliance_x, uses: ghcr.io/acme/trivy@v1}\n",
	))); err == nil {
		t.Error("policy fragment should reject a job name without the _compliance_ prefix")
	}
	if err := sch.Validate(yamlValue(t, []byte(
		"stages: [build]\njobs:\n  _compliance_scan: {stage: build, uses: ghcr.io/acme/trivy@v1}\n",
	))); err == nil {
		t.Error("policy fragment should reject a stage name without the _compliance_ prefix")
	}
}
