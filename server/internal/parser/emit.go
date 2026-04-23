package parser

import (
	"bytes"
	"fmt"
	"regexp"
	"sort"

	"gopkg.in/yaml.v3"

	"github.com/gocdnext/gocdnext/server/internal/domain"
)

// Emit reverses Parse: a domain.Pipeline becomes a YAML document
// whose shape matches the parser's schema. Round-tripping is the
// contract — parsing the output with ParseNamed must yield an
// equivalent pipeline (see emit_test.go).
//
// Used by the UI's "yaml" tab to render what's actually stored in
// pipelines.definition, instead of a thin two-field sketch. When
// the original on-disk YAML is eventually persisted, this becomes
// the fallback for pipelines applied before that feature shipped.
//
// Jobs are emitted bucketed by declared stage order and sorted
// alphabetically within each bucket so two calls produce
// byte-identical output (stable diff / no flicker on reloads).
func Emit(p *domain.Pipeline) ([]byte, error) {
	if p == nil {
		return nil, fmt.Errorf("emit: nil pipeline")
	}

	f := File{
		Name:        p.Name,
		Stages:      append([]string(nil), p.Stages...),
		Variables:   p.Variables,
		Template:    p.Template,
		Concurrency: p.Concurrency,
	}
	if len(p.TriggerEvents) > 0 {
		f.When = &WhenDef{Event: append([]string(nil), p.TriggerEvents...)}
	}
	for _, m := range p.Materials {
		// Implicit materials (project-repo synthesized from
		// scm_source at apply time) are deliberately hidden — the
		// yaml tab should mirror the operator's source, not the
		// expanded+stored form. They'll be re-synthesized on the
		// next apply/sync either way.
		if m.Implicit {
			continue
		}
		f.Materials = append(f.Materials, materialToSpec(m))
	}

	f.Jobs = make(map[string]JobDef, len(p.Jobs))
	for _, j := range p.Jobs {
		f.Jobs[j.Name] = jobToDef(j)
	}

	// yaml.v3 emits map keys in insertion-independent (hash) order.
	// Build the final document through a yaml.Node tree so stages
	// and jobs come out in a deterministic, human-readable order:
	//   - top-level fields in a fixed canonical order
	//   - jobs bucketed by p.Stages ordinal, alphabetical within bucket
	root, err := buildRootNode(p, f)
	if err != nil {
		return nil, fmt.Errorf("emit: build node: %w", err)
	}

	// Post-process the node tree: small scalar sequences go to flow
	// style (`[a, b, c]`) and string values that could be mistaken
	// for YAML scalars (numbers, bools, nulls) get explicitly
	// quoted. Makes the emitted YAML read closer to what an
	// operator would hand-write — without these passes short lists
	// spread across multiple lines and `"1.25"` degrades to
	// `1.25`, losing the string intent.
	flowifySmallSequences(root)
	quoteAmbiguousScalars(root)

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(root); err != nil {
		return nil, fmt.Errorf("emit: encode: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("emit: close: %w", err)
	}
	return buf.Bytes(), nil
}

func materialToSpec(m domain.Material) MaterialSpec {
	switch m.Type {
	case domain.MaterialGit:
		if m.Git == nil {
			return MaterialSpec{}
		}
		return MaterialSpec{Git: &GitSpec{
			URL:                 m.Git.URL,
			Branch:              m.Git.Branch,
			On:                  m.Git.Events,
			AutoRegisterWebhook: m.Git.AutoRegisterWebhook,
			SecretRef:           m.Git.SecretRef,
		}}
	case domain.MaterialUpstream:
		if m.Upstream == nil {
			return MaterialSpec{}
		}
		return MaterialSpec{Upstream: &UpstreamSpec{
			Pipeline: m.Upstream.Pipeline,
			Stage:    m.Upstream.Stage,
			Status:   m.Upstream.Status,
		}}
	case domain.MaterialCron:
		if m.Cron == nil {
			return MaterialSpec{}
		}
		return MaterialSpec{Cron: &CronSpec{Expression: m.Cron.Expression}}
	case domain.MaterialManual:
		return MaterialSpec{Manual: true}
	}
	return MaterialSpec{}
}

func jobToDef(j domain.Job) JobDef {
	def := JobDef{
		Stage:     j.Stage,
		Image:     j.Image,
		Needs:     j.Needs,
		Settings:  j.Settings,
		Variables: j.Variables,
		Secrets:   j.Secrets,
		Tags:      j.Tags,
		Docker:    j.Docker,
	}
	for _, t := range j.Tasks {
		if t.Script != "" {
			def.Script = append(def.Script, t.Script)
		}
		// Plugin-only jobs are re-serialised via image+settings on the
		// JobDef itself (see parser.go where a single plugin task is
		// synthesised from image+settings). We don't emit the task
		// here because JobDef has no list-of-tasks slot.
	}
	if len(j.ArtifactPaths) > 0 || len(j.OptionalArtifactPaths) > 0 {
		def.Artifacts = &Artifacts{
			Paths:    j.ArtifactPaths,
			Optional: j.OptionalArtifactPaths,
		}
	}
	for _, dep := range j.ArtifactDeps {
		def.NeedsArtifacts = append(def.NeedsArtifacts, NeedsArtifactDef{
			FromJob:      dep.FromJob,
			FromPipeline: dep.FromPipeline,
			Paths:        dep.Paths,
			Dest:         dep.Dest,
		})
	}
	if len(j.Matrix) > 0 {
		entry := map[string][]string{}
		for k, vs := range j.Matrix {
			entry[k] = append([]string(nil), vs...)
		}
		def.Parallel = &Parallel{Matrix: []map[string][]string{entry}}
	}
	for _, r := range j.Rules {
		def.Rules = append(def.Rules, RuleDef{
			If:      r.IfExpr,
			Changes: r.Changes,
			When:    r.When,
		})
	}
	return def
}

// buildRootNode emits a MappingNode with a fixed key order:
//   name, version (skipped, File has none set), concurrency,
//   stages, variables, materials, jobs.
// Jobs are grouped by the pipeline's declared stage order, then
// sorted alphabetically within each stage. Unknown-stage jobs
// (shouldn't happen post-parse validation, but keep it defensive)
// trail at the end in name order.
func buildRootNode(p *domain.Pipeline, f File) (*yaml.Node, error) {
	root := &yaml.Node{Kind: yaml.MappingNode}

	addScalar(root, "name", f.Name)
	if f.Concurrency != "" {
		addScalar(root, "concurrency", f.Concurrency)
	}
	if f.When != nil {
		whenNode, err := marshalInto(f.When)
		if err != nil {
			return nil, err
		}
		appendKV(root, "when", whenNode)
	}
	if len(f.Stages) > 0 {
		stagesNode, err := marshalInto(f.Stages)
		if err != nil {
			return nil, err
		}
		stagesNode.Style = yaml.FlowStyle
		appendKV(root, "stages", stagesNode)
	}
	if len(f.Variables) > 0 {
		varsNode, err := marshalSortedStringMap(f.Variables)
		if err != nil {
			return nil, err
		}
		appendKV(root, "variables", varsNode)
	}
	if len(f.Materials) > 0 {
		matsNode, err := marshalInto(f.Materials)
		if err != nil {
			return nil, err
		}
		appendKV(root, "materials", matsNode)
	}

	if len(f.Jobs) > 0 {
		jobsNode, err := orderedJobsNode(p, f.Jobs)
		if err != nil {
			return nil, err
		}
		appendKV(root, "jobs", jobsNode)
	}

	return root, nil
}

func orderedJobsNode(p *domain.Pipeline, jobs map[string]JobDef) (*yaml.Node, error) {
	node := &yaml.Node{Kind: yaml.MappingNode}

	stageOrder := map[string]int{}
	for i, s := range p.Stages {
		stageOrder[s] = i
	}

	names := make([]string, 0, len(jobs))
	for n := range jobs {
		names = append(names, n)
	}
	sort.SliceStable(names, func(i, j int) bool {
		a, b := jobs[names[i]], jobs[names[j]]
		oa, oka := stageOrder[a.Stage]
		ob, okb := stageOrder[b.Stage]
		// Unknown stages sort after known ones; keeps output
		// deterministic even if a stage was renamed after apply.
		if oka != okb {
			return oka
		}
		if oa != ob {
			return oa < ob
		}
		return names[i] < names[j]
	})

	for _, n := range names {
		jd := jobs[n]
		jNode, err := marshalInto(jd)
		if err != nil {
			return nil, fmt.Errorf("emit job %q: %w", n, err)
		}
		appendKV(node, n, jNode)
	}
	return node, nil
}

// marshalInto is a tiny adapter: serialise any yaml-tagged value to
// a yaml.Node via round-tripping through the encoder. Cheaper than
// building the node tree by hand for each JobDef field and keeps
// `omitempty` honoured.
func marshalInto(v any) (*yaml.Node, error) {
	var n yaml.Node
	if err := n.Encode(v); err != nil {
		return nil, err
	}
	return &n, nil
}

func marshalSortedStringMap(m map[string]string) (*yaml.Node, error) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	n := &yaml.Node{Kind: yaml.MappingNode}
	for _, k := range keys {
		appendKV(n, k, &yaml.Node{Kind: yaml.ScalarNode, Value: m[k]})
	}
	return n, nil
}

func addScalar(parent *yaml.Node, key, value string) {
	appendKV(parent, key, &yaml.Node{Kind: yaml.ScalarNode, Value: value})
}

func appendKV(parent *yaml.Node, key string, value *yaml.Node) {
	parent.Content = append(parent.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: key},
		value,
	)
}

// flowSequenceMaxLen is the upper bound for "short enough to inline".
// Six entries covers every sequence the pipeline YAML routinely
// carries (stages, events, needs, paths); longer lists stay in block
// style where they're easier to diff.
const flowSequenceMaxLen = 6

// flowifySmallSequences walks a yaml.Node tree and rewrites any
// sequence of ≤flowSequenceMaxLen scalar entries to flow style.
// Matches the way a human tends to write short lists
// (`stages: [lint, test, build]` / `needs: [vet]`) while leaving
// multi-line script arrays and richer list-of-map structures
// untouched.
func flowifySmallSequences(n *yaml.Node) {
	if n == nil {
		return
	}
	if n.Kind == yaml.SequenceNode &&
		len(n.Content) > 0 &&
		len(n.Content) <= flowSequenceMaxLen &&
		allSimpleScalars(n.Content) {
		n.Style = yaml.FlowStyle
	}
	for _, c := range n.Content {
		flowifySmallSequences(c)
	}
}

func allSimpleScalars(nodes []*yaml.Node) bool {
	for _, n := range nodes {
		if n == nil || n.Kind != yaml.ScalarNode {
			return false
		}
		// Multi-line / very long scalars force block style
		// downstream anyway, and mixing them into a flow sequence
		// reads worse than block.
		if len(n.Value) > 64 {
			return false
		}
		for _, r := range n.Value {
			if r == '\n' {
				return false
			}
		}
	}
	return true
}

// yamlReservedScalar matches strings that would re-parse as a
// non-string scalar without explicit quotes: numbers (including
// scientific notation), booleans, nulls. Anything that matches gets
// forced to double-quoted style in the output.
var yamlReservedScalar = regexp.MustCompile(
	`^(?:-?\d+(?:\.\d+)?(?:[eE][+-]?\d+)?|true|false|yes|no|on|off|null|~)$`,
)

// quoteAmbiguousScalars flips the style of scalar string values
// whose content would otherwise be parsed back as a different YAML
// type. E.g. `GO_VERSION: "1.25"` survives the round-trip instead
// of collapsing to `GO_VERSION: 1.25` (float) on re-emit — the
// source YAML explicitly quoted it, and losing that intent makes
// the tab misrepresent the stored definition.
//
// Only touches ScalarNode values we know are strings (we flip them
// regardless of current style, since yaml.v3's default scalar-style
// detection drops the quotes for these). Keys are left alone.
func quoteAmbiguousScalars(n *yaml.Node) {
	if n == nil {
		return
	}
	switch n.Kind {
	case yaml.MappingNode:
		// MappingNode.Content is [key, value, key, value, …] —
		// quote values only.
		for i := 1; i < len(n.Content); i += 2 {
			quoteIfAmbiguous(n.Content[i])
			quoteAmbiguousScalars(n.Content[i])
		}
	case yaml.SequenceNode, yaml.DocumentNode:
		for _, c := range n.Content {
			quoteIfAmbiguous(c)
			quoteAmbiguousScalars(c)
		}
	}
}

func quoteIfAmbiguous(n *yaml.Node) {
	if n == nil || n.Kind != yaml.ScalarNode {
		return
	}
	// Leave real bools / ints / floats alone — encoders stamp
	// those with an explicit !!bool / !!int / !!float tag, and
	// quoting them would break round-trip (re-parse fails when
	// the target field is a typed Go bool/int/float).
	if n.Tag != "" && n.Tag != "!!str" {
		return
	}
	if yamlReservedScalar.MatchString(n.Value) {
		n.Style = yaml.DoubleQuotedStyle
	}
}
