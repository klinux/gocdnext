package parser

import (
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

// Poll interval bounds on the git material. 1-minute floor keeps
// the poll worker from flooding provider APIs (GitHub 5k req/hr
// PAT limit divided by N materials). 24h ceiling is long enough
// for "daily sanity" polls but short enough that a misconfigured
// never-firing material gets noticed quickly. Zero means disabled.
const (
	MinPollInterval = time.Minute
	MaxPollInterval = 24 * time.Hour

	// approvalQuorumByLabelCap bounds how many distinct PR labels
	// can override the base quorum on a single approval gate.
	// 16 fits the most elaborate workflow (hotfix, risky,
	// breaking-change, security, ...) without being a footgun for
	// operators who'd otherwise encode label taxonomy in YAML when
	// it belongs in a wiki.
	approvalQuorumByLabelCap = 16
)

// approvalLabelRE bounds the charset of PR labels that can override
// the quorum. Matches the intersection of GitHub/GitLab label
// naming conventions (alphanumeric + `.` + `_` + `-` + `/`),
// rejecting shell metas + spaces. The labels coming OUT of the
// webhook are already lowercased + trimmed (see github.normaliseLabels);
// validating again at YAML parse time guards against YAML-side
// typos like `"hot fix"` or `"weird$tag"` that the operator would
// otherwise notice only when a PR's labels surprisingly fail to
// match.
var approvalLabelRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._/-]*$`)

// deployEnvRE bounds a deploy environment name (#39). Starts with an
// alphanumeric, then alphanumerics + `.` `_` `-`, max 64 chars. The
// bound + charset keep the name safe in URLs, audit payloads, and the
// unique (project_id, name) key — and reject shell metas/spaces that
// a `deploy: {environment: "prod; rm -rf"}` typo would smuggle in.
var deployEnvRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

// jobClusterRE bounds a `cluster:` reference — must match the store's
// clusterNameRE so a name that parses also resolves. Lowercase
// DNS-ish; keeps it safe in the jsonpath usage query + log lines.
var jobClusterRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

// deployVersionRefRE extracts strict `${{ ... }}` tokens from a
// deploy.version so we can validate their namespace at APPLY time
// rather than letting an unresolvable ref wedge the job in `queued`
// forever at dispatch (the scheduler retries config errors that
// aren't ErrNeedsRefUnresolved). Bounded inner capture; non-greedy.
var deployVersionRefRE = regexp.MustCompile(`\$\{\{\s*([^}]{1,256}?)\s*\}\}`)

// deployVersionRefOK accepts the only namespaces deploy.version
// resolves against at dispatch: upstream outputs (`needs.<job>.
// outputs.<alias>`) and CI built-ins (`CI_*`). It deliberately
// rejects `${{ MY_VAR }}` / `${{ SECRET }}` — variables aren't wired
// for version, and a secret in a version would leak into the
// deployment_revisions row + Environments UI.
//
// The needs branch MIRRORS the scheduler's needsRefPattern
// (refs.go): job name is `[A-Za-z0-9_-]+` (no dot — job names have
// none) and the alias is lowercase-leading kebab/snake, matching the
// output-alias grammar. Keeping the shapes identical means a version
// ref accepted here resolves at dispatch unless the upstream simply
// didn't produce the alias — and even that case is now terminal
// (ErrDeployVersionUnresolved), never a retry loop. Matrix-selector
// needs refs (needs.X.matrix[..].outputs.Y) are NOT accepted in a
// version — rare, and rejected loud at apply rather than silently.
var deployVersionRefOK = regexp.MustCompile(`^(needs\.[A-Za-z0-9_-]+\.outputs\.[a-z][a-zA-Z0-9_-]*|CI_[A-Z0-9_]+)$`)

// ParsePollInterval returns (duration, error) for a YAML-supplied
// poll_interval string. Empty is (0, nil) — caller treats zero as
// "polling disabled". Format is Go's time.ParseDuration ("5m",
// "1h30m", "2h"). Out-of-bounds values return an error so invalid
// config never silently becomes a runaway loop or effectively-off
// misconfiguration.
func ParsePollInterval(raw string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("poll_interval: parse %q: %w", raw, err)
	}
	if d < MinPollInterval {
		return 0, fmt.Errorf("poll_interval %q is below minimum %s", raw, MinPollInterval)
	}
	if d > MaxPollInterval {
		return 0, fmt.Errorf("poll_interval %q exceeds maximum %s", raw, MaxPollInterval)
	}
	return d, nil
}

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
	// ':' is the segment separator of the OIDC id_token `sub` claim
	// grammar (project:X:pipeline:Y:ref_type:...) — a name carrying
	// one could impersonate grammar segments in sloppy glob-based
	// cloud policies. The sub builder also percent-encodes as
	// defence in depth, but the front door rejects so operators see
	// the constraint at apply time, not in an IAM debugging session.
	if strings.Contains(name, ":") {
		return nil, fmt.Errorf("pipeline name %q must not contain ':' (reserved as the id_token subject-claim separator)", name)
	}

	concurrency := strings.ToLower(strings.TrimSpace(f.Concurrency))
	switch concurrency {
	case "", domain.ConcurrencyParallel, domain.ConcurrencySerial:
		// OK
	default:
		return nil, fmt.Errorf("concurrency: unknown value %q (want \"parallel\" or \"serial\")", f.Concurrency)
	}

	// _notifications is a reserved stage name: at run materialization
	// time the store appends a synthetic stage with that exact name
	// to hold notification jobs. Colliding with it would break
	// ordinal math + the cascade's "is this stage synthetic?" check.
	for _, s := range f.Stages {
		if s == domain.NotificationStageName {
			return nil, fmt.Errorf("stage name %q is reserved for server-synthesized notification jobs", s)
		}
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
		if err := validatePipelineEvents(f.When.Event); err != nil {
			return nil, fmt.Errorf("%s: when.event: %w", name, err)
		}
		p.TriggerEvents = append([]string(nil), f.When.Event...)
	}
	if f.When != nil && len(f.When.Branch) > 0 {
		p.TriggerBranches = append([]string(nil), f.When.Branch...)
	}
	if f.When != nil && len(f.When.Status) > 0 {
		return nil, fmt.Errorf(
			"%s: when.status is reserved and not enforced — remove it (issue #40)", name)
	}
	if f.When != nil && len(f.When.Paths) > 0 {
		if err := validateTriggerPaths(f.When.Paths); err != nil {
			return nil, fmt.Errorf("%s: when.paths: %w", name, err)
		}
		p.TriggerPaths = append([]string(nil), f.When.Paths...)
	}

	for _, sv := range f.Services {
		svc, err := toService(sv)
		if err != nil {
			return nil, err
		}
		p.Services = append(p.Services, svc)
	}

	// Preserve the nil-vs-empty distinction from the YAML layer:
	// no `notifications:` key at all → leave p.Notifications nil
	// (the run-create path reads that as "inherit project-level");
	// `notifications: []` → allocate an empty non-nil slice (the
	// run-create path reads that as "explicit opt-out, skip project
	// inheritance"). Go's `range nil` does nothing, so we'd lose
	// the distinction without the pre-allocation.
	if f.Notifications != nil {
		p.Notifications = []domain.Notification{}
		for i, ns := range f.Notifications {
			n, err := toNotification(i, ns)
			if err != nil {
				return nil, err
			}
			p.Notifications = append(p.Notifications, n)
		}
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

	// Resolve `extends:` chains before materializing jobs so
	// toJob only sees fully-flattened JobDefs. Hidden template
	// jobs (names starting with `.`) are removed by the resolver
	// so they never appear in the domain.Pipeline — they only
	// exist to be parents of real jobs.
	resolved, err := resolveExtends(f.Jobs)
	if err != nil {
		return nil, err
	}

	for name, jd := range resolved {
		if !declared[jd.Stage] {
			return nil, fmt.Errorf("job %q references undeclared stage %q", name, jd.Stage)
		}
		j, err := toJob(name, jd, f.Variables)
		if err != nil {
			return nil, err
		}
		p.Jobs = append(p.Jobs, j)
	}

	// Cross-validate `needs:` references AFTER all jobs are
	// resolved — needs to know the full (name → stage_ordinal)
	// mapping, which only exists post-loop. See validateNeeds for
	// the three rejection classes (unknown name, self-reference,
	// forward-stage reference) and validateNoCycles for the
	// runtime-deadlock guard.
	if err := validateNeeds(p.Jobs, f.Stages); err != nil {
		return nil, err
	}
	if err := validateNoCycles(p.Jobs); err != nil {
		return nil, err
	}

	return p, nil
}

func toMaterial(m MaterialSpec) (domain.Material, error) {
	switch {
	case m.Git != nil:
		branch := defaultStr(m.Git.Branch, "main")
		poll, err := ParsePollInterval(m.Git.PollInterval)
		if err != nil {
			return domain.Material{}, err
		}
		if err := validateGitMaterialEvents(m.Git.On); err != nil {
			return domain.Material{}, fmt.Errorf("git material %q: %w", m.Git.URL, err)
		}
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
				PollInterval:        poll,
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
