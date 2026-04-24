package parser

import (
	"fmt"
	"io"
	"strings"

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

	concurrency := strings.ToLower(strings.TrimSpace(f.Concurrency))
	switch concurrency {
	case "", domain.ConcurrencyParallel, domain.ConcurrencySerial:
		// OK
	default:
		return nil, fmt.Errorf("concurrency: unknown value %q (want \"parallel\" or \"serial\")", f.Concurrency)
	}

	p := &domain.Pipeline{
		ProjectID:   projectID,
		Name:        name,
		Stages:      f.Stages,
		Variables:   f.Variables,
		Template:    f.Template,
		Concurrency: concurrency,
	}
	if f.When != nil && len(f.When.Event) > 0 {
		p.TriggerEvents = append([]string(nil), f.When.Event...)
	}

	for _, sv := range f.Services {
		svc, err := toService(sv)
		if err != nil {
			return nil, err
		}
		p.Services = append(p.Services, svc)
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
		Docker:    jd.Docker,
	}
	if jd.Artifacts != nil {
		j.ArtifactPaths = append([]string(nil), jd.Artifacts.Paths...)
		// De-dup against required — a path in both lists is kept
		// only in ArtifactPaths (required wins). Rare but possible
		// when someone adds `optional:` to an existing job without
		// removing from `paths:`.
		required := make(map[string]struct{}, len(jd.Artifacts.Paths))
		for _, p := range jd.Artifacts.Paths {
			required[p] = struct{}{}
		}
		for _, p := range jd.Artifacts.Optional {
			if _, dup := required[p]; dup {
				continue
			}
			j.OptionalArtifactPaths = append(j.OptionalArtifactPaths, p)
		}
	}
	for _, c := range jd.Cache {
		if c.Key == "" {
			return domain.Job{}, fmt.Errorf("job %q: cache entry missing `key`", name)
		}
		if len(c.Paths) == 0 {
			return domain.Job{}, fmt.Errorf("job %q: cache entry %q has empty `paths` — nothing to cache", name, c.Key)
		}
		j.Cache = append(j.Cache, domain.CacheSpec{
			Key:   c.Key,
			Paths: append([]string(nil), c.Paths...),
		})
	}
	for _, na := range jd.NeedsArtifacts {
		if na.FromJob == "" {
			return domain.Job{}, fmt.Errorf("job %q: needs_artifacts entry missing from_job", name)
		}
		j.ArtifactDeps = append(j.ArtifactDeps, domain.ArtifactDep{
			FromJob:      na.FromJob,
			FromPipeline: na.FromPipeline,
			Paths:        append([]string(nil), na.Paths...),
			Dest:         na.Dest,
		})
	}

	// Approval gates are a pure state-machine construct — they
	// dispatch nothing. Reject any execution knob on the same
	// job: silently ignoring them would let a misconfigured YAML
	// look like it'd run something, when in fact it only parks
	// on the gate. Downstream jobs still wait on `needs:` of an
	// approval job; that's the whole point of the gate.
	if jd.Approval != nil {
		if len(jd.Script) > 0 || jd.Uses != "" || jd.Image != "" ||
			jd.Settings != nil || jd.Artifacts != nil ||
			len(jd.NeedsArtifacts) > 0 || len(jd.Cache) > 0 ||
			jd.Docker {
			return domain.Job{}, fmt.Errorf(
				"job %q: approval gate cannot declare script/uses/image/artifacts/cache/docker — it only blocks on a human decision",
				name,
			)
		}
		j.Approval = &domain.ApprovalSpec{
			Approvers:   append([]string(nil), jd.Approval.Approvers...),
			Description: jd.Approval.Description,
		}
		return j, nil
	}

	// Concatenate all `script:` entries into a single Task so they
	// execute in the same shell session. Previously each line
	// became its own Task → its own `docker run --rm`, so state
	// set by one line (env vars, installed tools like corepack +
	// pnpm) didn't survive to the next. `set -e` makes the chain
	// fail-fast: any line that exits non-zero aborts the rest,
	// matching what GitLab CI / Woodpecker do out of the box.
	if len(jd.Script) > 0 {
		joined := "set -e\n" + strings.Join(jd.Script, "\n")
		j.Tasks = append(j.Tasks, domain.Task{Script: joined})
	}

	// Plugin step via the explicit `uses:` + `with:` sugar (matches
	// the GitHub-Actions shape operators already know, but the
	// execution model is Woodpecker's — the agent translates `with:`
	// to PLUGIN_* env vars and runs the image's own entrypoint).
	// Disallow mixing with `script:` on the same job: the plugin's
	// entrypoint IS the logic, a trailing `script:` would just be
	// ignored and confuse the operator.
	if jd.Uses != "" {
		if len(jd.Script) > 0 {
			return domain.Job{}, fmt.Errorf(
				"job %q: `uses:` is mutually exclusive with `script:` — a plugin step runs the image's entrypoint on its own",
				name,
			)
		}
		if jd.Image != "" {
			return domain.Job{}, fmt.Errorf(
				"job %q: `uses:` and `image:` both set — pick one (`uses:` IS the image for plugin jobs)",
				name,
			)
		}
		image, err := resolvePluginRef(jd.Uses)
		if err != nil {
			return domain.Job{}, fmt.Errorf("job %q: %w", name, err)
		}
		j.Tasks = append(j.Tasks, domain.Task{
			Plugin: &domain.PluginStep{
				Image:    image,
				Settings: jd.With,
			},
		})
	} else if len(jd.Script) == 0 && jd.Image != "" && jd.Settings != nil {
		// Legacy single-step plugin shape: `image:` + `settings:`
		// with no script. Kept for backwards-compat with YAMLs
		// written before `uses:`/`with:` landed.
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

// toService validates a service spec and derives a default name
// when omitted. image is mandatory — a service without one can't
// start. Name defaults to the image's short form (repository
// basename, tag stripped) so `image: postgres:16-alpine` implies
// `name: postgres` without extra YAML. Duplicate names across
// the pipeline would collide on the docker network alias; that
// check lives in ApplyProject where all services are visible
// together.
func toService(s ServiceSpec) (domain.Service, error) {
	if strings.TrimSpace(s.Image) == "" {
		return domain.Service{}, fmt.Errorf("service: image is required")
	}
	name := strings.TrimSpace(s.Name)
	if name == "" {
		name = defaultServiceNameFromImage(s.Image)
	}
	if name == "" {
		return domain.Service{}, fmt.Errorf("service: couldn't derive name from image %q; set `name:` explicitly", s.Image)
	}
	return domain.Service{
		Name:    name,
		Image:   s.Image,
		Env:     s.Env,
		Command: append([]string(nil), s.Command...),
	}, nil
}

// defaultServiceNameFromImage picks a dns-label-friendly name from
// a container image reference. "postgres:16-alpine" → "postgres",
// "registry.local/foo/bar:1" → "bar". Strips registry+repo path
// and tag.
func defaultServiceNameFromImage(image string) string {
	s := image
	// Strip tag.
	if i := strings.LastIndex(s, ":"); i >= 0 {
		// Colons in host:port (registry) survive since they appear
		// BEFORE any slash — only strip when the last colon is
		// after the last slash.
		lastSlash := strings.LastIndex(s, "/")
		if i > lastSlash {
			s = s[:i]
		}
	}
	// Strip registry + repo path.
	if i := strings.LastIndex(s, "/"); i >= 0 {
		s = s[i+1:]
	}
	return strings.TrimSpace(s)
}

