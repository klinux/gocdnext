package main

import "github.com/invopop/jsonschema"

// reservedPrefix mirrors compliance.ReservedPrefix — every stage and job a
// policy contributes must carry it. Encoding it in the fragment schema turns
// the "must start with _compliance_" rule into an inline editor error instead
// of a submit-time surprise.
const reservedPrefix = "_compliance_"

// applyOverrides patches the reflected schema for the handful of cases struct
// reflection can't express: custom UnmarshalYAML shapes and the
// exactly-one-of material. Each patch is guarded by a $def existence check so
// the same function is safe to run over either root (the policy fragment shares
// JobDef and its sub-defs with the full pipeline, but has no MaterialSpec).
//
// Deliberately NOT patched:
//   - DeployDef — its custom UnmarshalYAML only enforces strictness; the
//     reflected {environment, version} object already matches the wire shape.
//   - numeric/bool fields (retry, required, count, …) — `${{ }}` substitution
//     resolves only inside env:/variables:/with: (string maps), never in these
//     scalars, so they stay strictly typed.
func applyOverrides(s *jsonschema.Schema) {
	relaxStringMaps(s)
	overrideMaterialSpec(s)
	overrideJobOutputs(s)
	overrideIDTokenAud(s)
}

func strType() *jsonschema.Schema { return &jsonschema.Schema{Type: "string"} }

// applyPolicyFragmentOverrides constrains the fragment to compliance policy
// shapes: every stage entry and every job name must start with the reserved
// prefix. Applied only to the policy-fragment root (the full pipeline has no
// such restriction).
func applyPolicyFragmentOverrides(s *jsonschema.Schema) {
	def, ok := s.Definitions["policyFragment"]
	if !ok || def.Properties == nil {
		return
	}
	if stages, ok := def.Properties.Get("stages"); ok && stages.Items != nil {
		stages.Items.Pattern = "^" + reservedPrefix
	}
	if jobs, ok := def.Properties.Get("jobs"); ok {
		// patternProperties + additionalProperties:false, NOT propertyNames —
		// the editor's validator (codemirror-json-schema) runs Draft-04, which
		// ignores propertyNames. patternProperties is honoured across Draft-04
		// → 2020-12, so the editor AND the Go validator enforce the same rule.
		jobs.PatternProperties = map[string]*jsonschema.Schema{
			"^" + reservedPrefix: {Ref: "#/$defs/JobDef"},
		}
		jobs.AdditionalProperties = jsonschema.FalseSchema
	}
}

// scalarUnion mirrors the parser's coercion: a `map[string]string` value
// (plugin `with`/`settings`, `variables`, service `env`) is decoded into a
// string even when the YAML scalar is an unquoted bool or number
// (`install: false`, `replicas: 3`). The schema must accept the same scalars or
// it rejects valid configs the parser happily reads.
func scalarUnion() *jsonschema.Schema {
	return &jsonschema.Schema{OneOf: []*jsonschema.Schema{
		{Type: "string"},
		{Type: "boolean"},
		{Type: "number"},
	}}
}

// relaxStringMaps finds every reflected `map[string]string` (an object whose
// additionalProperties is a bare string, with no fixed properties of its own)
// and widens the value to scalarUnion. Targeted by shape, so it covers `with`,
// `settings`, `variables`, service `env`, notification `with`, etc. — and any
// future string map — without touching object maps (`jobs`, `outputs`) or typed
// maps (`quorum_by_label` is integer-valued).
func relaxStringMaps(s *jsonschema.Schema) {
	for _, def := range s.Definitions {
		if def.Properties == nil {
			continue
		}
		for pair := def.Properties.Oldest(); pair != nil; pair = pair.Next() {
			prop := pair.Value
			ap := prop.AdditionalProperties
			if prop.Type == "object" && prop.Properties == nil && ap != nil && ap.Type == "string" {
				prop.AdditionalProperties = scalarUnion()
			}
		}
	}
}

// overrideMaterialSpec requires at least one of the four source fields
// (git/upstream/cron/manual) — reflection leaves them all optional, which would
// accept an empty `{}` the parser rejects. We deliberately do NOT enforce
// "exactly one": the parser (parse.go) picks the first set field when several
// are present, so a strict oneOf would reject configs the server accepts. The
// schema stays a permissive superset of server behaviour (D3); the parser is
// the authority on the remaining edges (multi-key precedence, `manual: false`).
func overrideMaterialSpec(s *jsonschema.Schema) {
	def, ok := s.Definitions["MaterialSpec"]
	if !ok {
		return
	}
	one := uint64(1)
	def.MinProperties = &one
}

// overrideJobOutputs teaches the `outputs:` map its short form. OutputDef has a
// custom UnmarshalYAML accepting either a bare string (alias: ENV_VAR) or the
// object form ({env, masked}); reflection only sees the object.
func overrideJobOutputs(s *jsonschema.Schema) {
	job, ok := s.Definitions["JobDef"]
	if !ok || job.Properties == nil {
		return
	}
	outputs, ok := job.Properties.Get("outputs")
	if !ok {
		return
	}
	outputs.AdditionalProperties = &jsonschema.Schema{
		OneOf: []*jsonschema.Schema{
			strType(),
			{Ref: "#/$defs/OutputDef"},
		},
	}
}

// overrideIDTokenAud accepts both a scalar aud and a sequence (GitLab parity —
// the custom UnmarshalYAML decodes either into []string).
func overrideIDTokenAud(s *jsonschema.Schema) {
	def, ok := s.Definitions["IDTokenDef"]
	if !ok || def.Properties == nil {
		return
	}
	aud, ok := def.Properties.Get("aud")
	if !ok {
		return
	}
	desc := aud.Description
	def.Properties.Set("aud", &jsonschema.Schema{
		Description: desc,
		OneOf: []*jsonschema.Schema{
			strType(),
			{Type: "array", Items: strType()},
		},
	})
}
