package parser

import (
	"fmt"
	"io"

	"gopkg.in/yaml.v3"

	"github.com/gocdnext/gocdnext/server/internal/domain"
)

// Parse reads a single pipeline file and returns a domain.Pipeline.
// The pipelineName argument is used verbatim (caller is responsible for naming).
//
// Most callers should use ParseNamed or LoadFolder instead.
//
// Responsibilities kept here (MVP):
//   - YAML decode
//   - Material fingerprint (for dedup across pipelines)
//   - Validation: stages declared, jobs reference a declared stage
//
// Deferred (future): include resolution, extends/anchors merging, template expansion,
// rules evaluation semantics. Those live in separate files to keep this one small.
func Parse(r io.Reader, projectID, pipelineName string) (*domain.Pipeline, error) {
	return ParseNamed(r, projectID, pipelineName)
}

// ParseNamed is the canonical entry point. Pipeline name is resolved as:
//  1. `name:` field in the YAML (preferred)
//  2. fallbackName (usually the filename without extension)
func ParseNamed(r io.Reader, projectID, fallbackName string) (*domain.Pipeline, error) {
	var f File
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("yaml decode: %w", err)
	}

	name := f.Name
	if name == "" {
		name = fallbackName
	}
	if name == "" {
		return nil, fmt.Errorf("pipeline has no name (set top-level `name:` or pass a filename)")
	}

	p := &domain.Pipeline{
		ProjectID: projectID,
		Name:      name,
		Stages:    f.Stages,
		Variables: f.Variables,
		Template:  f.Template,
	}

	for _, m := range f.Materials {
		mat, err := toMaterial(m)
		if err != nil {
			return nil, err
		}
		p.Materials = append(p.Materials, mat)
	}

	declared := make(map[string]bool, len(f.Stages))
	for _, s := range f.Stages {
		declared[s] = true
	}

	for name, jd := range f.Jobs {
		if !declared[jd.Stage] {
			return nil, fmt.Errorf("job %q references undeclared stage %q", name, jd.Stage)
		}
		j, err := toJob(name, jd)
		if err != nil {
			return nil, err
		}
		p.Jobs = append(p.Jobs, j)
	}

	return p, nil
}

func toMaterial(m MaterialSpec) (domain.Material, error) {
	switch {
	case m.Git != nil:
		branch := defaultStr(m.Git.Branch, "main")
		return domain.Material{
			Type:        domain.MaterialGit,
			Fingerprint: domain.GitFingerprint(m.Git.URL, branch),
			AutoUpdate:  true,
			Git: &domain.GitMaterial{
				URL:                 m.Git.URL,
				Branch:              branch,
				Events:              defaultEvents(m.Git.On),
				AutoRegisterWebhook: m.Git.AutoRegisterWebhook,
				SecretRef:           m.Git.SecretRef,
			},
		}, nil
	case m.Upstream != nil:
		return domain.Material{
			Type:        domain.MaterialUpstream,
			Fingerprint: domain.UpstreamFingerprint(m.Upstream.Pipeline, m.Upstream.Stage),
			AutoUpdate:  true,
			Upstream: &domain.UpstreamMaterial{
				Pipeline: m.Upstream.Pipeline,
				Stage:    m.Upstream.Stage,
				Status:   defaultStr(m.Upstream.Status, "success"),
			},
		}, nil
	case m.Cron != nil:
		return domain.Material{
			Type:        domain.MaterialCron,
			Fingerprint: domain.CronFingerprint(m.Cron.Expression),
			AutoUpdate:  true,
			Cron:        &domain.CronMaterial{Expression: m.Cron.Expression},
		}, nil
	case m.Manual:
		return domain.Material{
			Type:        domain.MaterialManual,
			Fingerprint: domain.ManualFingerprint(),
		}, nil
	default:
		return domain.Material{}, fmt.Errorf("material must set one of: git, upstream, cron, manual")
	}
}

func toJob(name string, jd JobDef) (domain.Job, error) {
	j := domain.Job{
		Name:      name,
		Stage:     jd.Stage,
		Image:     jd.Image,
		Needs:     jd.Needs,
		Settings:  jd.Settings,
		Variables: jd.Variables,
		Secrets:   jd.Secrets,
		Tags:      jd.Tags,
	}

	for _, line := range jd.Script {
		j.Tasks = append(j.Tasks, domain.Task{Script: line})
	}

	// If image starts with "plugins/" treat the whole job as a single plugin step.
	// (Woodpecker-style single-step jobs are common; multi-step jobs mix `script:` tasks.)
	if len(jd.Script) == 0 && jd.Image != "" && jd.Settings != nil {
		j.Tasks = append(j.Tasks, domain.Task{
			Plugin: &domain.PluginStep{
				Image:    jd.Image,
				Settings: jd.Settings,
			},
		})
	}

	if jd.Parallel != nil && len(jd.Parallel.Matrix) > 0 {
		j.Matrix = flattenMatrix(jd.Parallel.Matrix)
	}

	for _, r := range jd.Rules {
		j.Rules = append(j.Rules, domain.Rule{
			IfExpr:  r.If,
			When:    r.When,
			Changes: r.Changes,
		})
	}

	return j, nil
}

func flattenMatrix(entries []map[string][]string) map[string][]string {
	out := map[string][]string{}
	for _, e := range entries {
		for k, vs := range e {
			out[k] = append(out[k], vs...)
		}
	}
	return out
}

func defaultStr(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func defaultEvents(ev []string) []string {
	if len(ev) == 0 {
		return []string{"push"}
	}
	return ev
}

