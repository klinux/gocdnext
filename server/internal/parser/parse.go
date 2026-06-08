package parser

import (
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/gocdnext/gocdnext/proto/cachekey"
	"github.com/gocdnext/gocdnext/server/internal/domain"
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
		j, err := toJob(name, jd)
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

// validateNoCycles detects cycles in the `needs:` graph via DFS
// with three-color marking (classic algorithm). A cycle would
// otherwise deadlock the scheduler at runtime: every job in the
// cycle waits on another that's also waiting, all stay queued
// indefinitely, and nothing makes progress.
//
// Why this isn't covered by validateNeeds:
//   - forward-stage rejection only catches cycles that cross stages
//     in the wrong direction. Same-stage cycles (`a needs b`,
//     `b needs a` both in `build`) pass that check.
//   - self-reference rejection catches the 1-node cycle (`a needs a`).
//     But 2-cycle, 3-cycle, ... are not.
//
// Algorithm:
//   - white (unvisited) = 0
//   - gray (in current DFS stack) = 1
//   - black (fully explored) = 2
//     Hitting a gray node means we found a back-edge → cycle. The
//     stack at that moment is the cycle path; we slice from the
//     first occurrence of the revisited node to get a clean trace.
//
// Iteration order: caller may pass jobs in map-iteration order
// (non-deterministic). DFS visits jobs alphabetically so the
// error message is stable across runs — important when a
// pipeline fails apply in CI and the operator compares error
// strings across attempts.
func validateNoCycles(jobs []domain.Job) error {
	needs := make(map[string][]string, len(jobs))
	names := make([]string, 0, len(jobs))
	for _, j := range jobs {
		needs[j.Name] = j.Needs
		names = append(names, j.Name)
	}
	sort.Strings(names)

	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := make(map[string]int, len(jobs))
	var stack []string

	var visit func(name string) error
	visit = func(name string) error {
		switch color[name] {
		case black:
			return nil
		case gray:
			// Back-edge → cycle. Slice the stack from where the
			// revisited name first appears so the trace is just
			// the cycle, not the whole DFS path leading to it.
			start := 0
			for i, n := range stack {
				if n == name {
					start = i
					break
				}
			}
			cycle := append([]string(nil), stack[start:]...)
			cycle = append(cycle, name)
			return fmt.Errorf("`needs:` cycle detected — jobs would deadlock at dispatch: %s", strings.Join(cycle, " → "))
		}
		color[name] = gray
		stack = append(stack, name)
		// Sort the dep list too so the error message is
		// deterministic when multiple cycles exist.
		deps := append([]string(nil), needs[name]...)
		sort.Strings(deps)
		for _, dep := range deps {
			if _, exists := needs[dep]; !exists {
				// Unknown name — already rejected by validateNeeds;
				// skip the recursion to avoid a nil-map traversal.
				// In the post-validateNeeds happy path this branch
				// never fires.
				continue
			}
			if err := visit(dep); err != nil {
				return err
			}
		}
		stack = stack[:len(stack)-1]
		color[name] = black
		return nil
	}

	for _, name := range names {
		if err := visit(name); err != nil {
			return err
		}
	}
	return nil
}

// validateNeeds checks every job's `needs:` list against the set of
// jobs in the same pipeline. Rejects three classes of bug at apply
// time so the scheduler doesn't have to defend against them at
// dispatch:
//
//   - Unknown name: needs references a job that doesn't exist. The
//     scheduler's gate (server/internal/scheduler/needs.go) would
//     treat this as "terminal not-in-run" and silently SKIP the
//     downstream with `error="needs unmet: ghost: not in this run"`.
//     Stage/run cascade only counts `failed` (not `skipped`) toward
//     run failure (see results.sql GetStageProgress / GetRunProgress),
//     so a typo here would let the run finalize GREEN even though a
//     job was effectively unrunnable. Rejecting at apply means the
//     operator sees the typo before any run starts.
//
//   - Self-reference: `needs: [self]`. Same shape as "unknown" at
//     runtime (the job's own status drives the gate into a self-
//     wait), but a clearer error at apply.
//
//   - Forward-stage reference: a job in an earlier stage needs a
//     job in a later stage. The scheduler dispatches stages in
//     ordinal order; the later-stage job never starts until the
//     earlier stage closes, but the earlier-stage job can't close
//     because the gate is waiting on the later-stage job to
//     reach success. Hard deadlock — Kleber's pipeline would hang.
//     Same-stage and earlier-stage references are fine (the latter
//     is redundant given the stage gate but harmless).
func validateNeeds(jobs []domain.Job, stages []string) error {
	stageOrdinal := make(map[string]int, len(stages))
	for i, s := range stages {
		stageOrdinal[s] = i
	}
	byName := make(map[string]domain.Job, len(jobs))
	for _, j := range jobs {
		byName[j.Name] = j
	}
	for _, j := range jobs {
		myOrd, hasStage := stageOrdinal[j.Stage]
		for _, dep := range j.Needs {
			if dep == j.Name {
				return fmt.Errorf("job %q: `needs:` contains itself", j.Name)
			}
			target, exists := byName[dep]
			if !exists {
				return fmt.Errorf("job %q: `needs:` references unknown job %q (no job by that name in this pipeline)", j.Name, dep)
			}
			// If either job's stage isn't declared, the earlier
			// stage-check already rejected it; skip the ordinal
			// comparison to keep the error message focused on the
			// undeclared stage instead of compounding two errors.
			if !hasStage {
				continue
			}
			targetOrd, ok := stageOrdinal[target.Stage]
			if !ok {
				continue
			}
			if targetOrd > myOrd {
				return fmt.Errorf("job %q (stage %q, ordinal %d): `needs:` references %q in later stage %q (ordinal %d) — forward references would deadlock the dispatcher",
					j.Name, j.Stage, myOrd, dep, target.Stage, targetOrd)
			}
		}
	}
	return nil
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
	if jd.Agent != nil {
		j.Profile = jd.Agent.Profile
		// Profile-supplied tags merge later (apply-time resolver),
		// not here — `j.Tags` is what the user typed at job scope.
		// AgentDef.Tags supplements job.Tags for users who want to
		// add a tag without leaving it implicit.
		if len(jd.Agent.Tags) > 0 {
			j.Tags = append(append([]string(nil), j.Tags...), jd.Agent.Tags...)
		}
	}
	if jd.Resources != nil {
		if jd.Resources.Requests != nil {
			j.Resources.Requests = domain.ResourceQuantities{
				CPU:    jd.Resources.Requests.CPU,
				Memory: jd.Resources.Requests.Memory,
			}
		}
		if jd.Resources.Limits != nil {
			j.Resources.Limits = domain.ResourceQuantities{
				CPU:    jd.Resources.Limits.CPU,
				Memory: jd.Resources.Limits.Memory,
			}
		}
	}
	if jd.Artifacts != nil {
		// Dedupe by CANONICAL form (trailing slashes trimmed) so the
		// downstream storage layer's partial unique index on
		// (job_run_id, path) — migration 00035 — can't be tripped by
		// `dist` and `dist/` appearing in the same job. Without this:
		//   paths:    [dist]
		//   optional: [dist/, screenshots]
		// would survive parse → assignment → agent → server's batch
		// insert blows up on the canonical-form unique index (the
		// required `dist` row blocks the optional `dist/` insert that
		// the server normalizes to `dist`). The batch txn rolls back
		// and `screenshots` is lost as collateral. Deduping at the
		// PARSER means the proto wire shape carries a clean
		// separation and neither agent run nor server batch ever sees
		// the conflict.
		//
		// First-occurrence shape wins within each list so the
		// operator's typing round-trips back to the UI; required
		// wins over optional on cross-list collisions (the existing
		// contract).
		canonRequired := make(map[string]struct{}, len(jd.Artifacts.Paths))
		for _, p := range jd.Artifacts.Paths {
			canon := canonicalArtifactPath(p)
			if _, dup := canonRequired[canon]; dup {
				continue
			}
			canonRequired[canon] = struct{}{}
			j.ArtifactPaths = append(j.ArtifactPaths, p)
		}
		canonOptional := make(map[string]struct{}, len(jd.Artifacts.Optional))
		for _, p := range jd.Artifacts.Optional {
			canon := canonicalArtifactPath(p)
			if _, dup := canonRequired[canon]; dup {
				continue
			}
			if _, dup := canonOptional[canon]; dup {
				continue
			}
			canonOptional[canon] = struct{}{}
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
		// Cache key may carry `{{ hash "..." }}` templates; validate
		// syntax here so bad config (typo in function name, missing
		// closing brace, path traversal in arg) fails the apply
		// rather than dispatching jobs that'd error at agent
		// expansion time. The parsed template is discarded server-
		// side; the agent re-parses and expands at runtime when the
		// workspace is materialised. Plain literals (no `{{`) round-
		// trip unchanged, keeping backwards-compat with pre-v0.4.37
		// cache keys.
		if _, err := cachekey.Parse(c.Key); err != nil {
			return domain.Job{}, fmt.Errorf("job %q: cache entry %q: %w", name, c.Key, err)
		}
		j.Cache = append(j.Cache, domain.CacheSpec{
			Key:   c.Key,
			Paths: append([]string(nil), c.Paths...),
		})
	}
	if len(jd.TestReports) > 0 {
		j.TestReports = append([]string(nil), jd.TestReports...)
	}

	if len(jd.Outputs) > 0 {
		if err := validateOutputsDeclaration(name, jd.Outputs); err != nil {
			return domain.Job{}, err
		}
		// Copy so a later mutation of the parsed YAML map can't
		// reach the domain object.
		j.Outputs = make(map[string]string, len(jd.Outputs))
		for k, v := range jd.Outputs {
			j.Outputs[k] = v
		}
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
		required := jd.Approval.Required
		if required < 0 {
			return domain.Job{}, fmt.Errorf(
				"job %q: approval.required must be >= 1 (got %d)",
				name, required)
		}
		if required == 0 {
			required = 1
		}
		// Sanity cap so a typo (`required: 100`) with only a couple
		// approvers listed doesn't produce an un-passable gate.
		// Allow room for generous quorums but fail fast when the
		// combined list couldn't possibly satisfy Required.
		listSize := len(jd.Approval.Approvers) + len(jd.Approval.ApproverGroups)
		if listSize > 0 && required > listSize {
			return domain.Job{}, fmt.Errorf(
				"job %q: approval.required=%d exceeds approvers+approver_groups=%d — gate would be un-passable",
				name, required, listSize)
		}
		// quorum_by_label: PR-label-driven quorum override. Validated
		// here so misconfigured maps don't reach the run materialiser.
		// Charset matches GitHub/GitLab label naming conventions
		// (alphanum + dash + dot + underscore + slash); rejecting
		// shell metas + spaces also keeps the field safe for the
		// audit event payload and URL-ish surfaces.
		var quorumByLabel map[string]int
		if len(jd.Approval.QuorumByLabel) > 0 {
			if got := len(jd.Approval.QuorumByLabel); got > approvalQuorumByLabelCap {
				return domain.Job{}, fmt.Errorf(
					"job %q: approval.quorum_by_label has %d entries — cap is %d (operator is encoding policy in YAML the wrong way past that)",
					name, got, approvalQuorumByLabelCap)
				}
			quorumByLabel = make(map[string]int, len(jd.Approval.QuorumByLabel))
			for label, override := range jd.Approval.QuorumByLabel {
				// Normalise BEFORE charset check so `HotFix` in YAML
				// is accepted (matches GitHub's case-insensitive
				// label model) and stored alongside the lowercased
				// labels that come out of the webhook normaliser.
				lower := strings.ToLower(label)
				if lower == "" {
					return domain.Job{}, fmt.Errorf(
						"job %q: approval.quorum_by_label has an empty label key", name)
				}
				if !approvalLabelRE.MatchString(lower) {
					return domain.Job{}, fmt.Errorf(
						"job %q: approval.quorum_by_label key %q has forbidden characters — accepted: alphanumeric + . _ - /",
						name, label)
				}
				if override < 1 {
					return domain.Job{}, fmt.Errorf(
						"job %q: approval.quorum_by_label[%q]=%d must be >= 1 — a gate with quorum 0 would auto-pass without any approver",
						name, label, override)
				}
				if listSize > 0 && override > listSize {
					return domain.Job{}, fmt.Errorf(
						"job %q: approval.quorum_by_label[%q]=%d exceeds approvers+approver_groups=%d — gate would be un-passable for runs with this label",
						name, label, override, listSize)
				}
				if _, dup := quorumByLabel[lower]; dup {
					return domain.Job{}, fmt.Errorf(
						"job %q: approval.quorum_by_label has duplicate label key %q (case-insensitive)",
						name, label)
				}
				quorumByLabel[lower] = override
			}
		}
		j.Approval = &domain.ApprovalSpec{
			Approvers:      append([]string(nil), jd.Approval.Approvers...),
			ApproverGroups: append([]string(nil), jd.Approval.ApproverGroups...),
			Required:       required,
			QuorumByLabel:  quorumByLabel,
			Description:    jd.Approval.Description,
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

// pipelineEvents enumerates the values accepted in top-level
// `when.event:` — webhook-driven (push, pull_request, tag), trigger-
// based (manual, cron), and dependency-based (upstream). Adding to
// this set is the explicit handshake operators trip when wiring a
// new trigger type, vs. the silent-accept-and-never-fire bug a free-
// text list lets through.
var pipelineEvents = map[string]struct{}{
	"push":         {},
	"pull_request": {},
	"tag":          {},
	"manual":       {},
	"cron":         {},
	"upstream":     {},
}

// gitMaterialEvents are the subset that mean anything to a git
// material's `on:` filter — only SCM events can actually arrive on
// that material. cron/manual/upstream don't have an "on a git
// material" semantic; `on: [cron]` was always a no-op + a typo.
var gitMaterialEvents = map[string]struct{}{
	"push":         {},
	"pull_request": {},
	"tag":          {},
}

func validatePipelineEvents(ev []string) error {
	for _, e := range ev {
		if _, ok := pipelineEvents[e]; !ok {
			return fmt.Errorf("unknown event %q (accepted: push, pull_request, tag, manual, cron, upstream)", e)
		}
	}
	return nil
}

func validateGitMaterialEvents(on []string) error {
	for _, e := range on {
		if _, ok := gitMaterialEvents[e]; !ok {
			return fmt.Errorf("unknown event %q in `on:` (accepted: push, pull_request, tag)", e)
		}
	}
	return nil
}

// outputAliasRE is the allowed character set for an `outputs:` map
// key (the YAML alias the operator types). Same shape as a shell
// identifier so substitution refs `${{ needs.X.outputs.<alias> }}`
// parse predictably and the alias can appear in a downstream env
// var name without escaping. ^[a-z] forces lowercase-leading per
// the gocdnext YAML convention.
var outputAliasRE = regexp.MustCompile(`^[a-z][a-zA-Z0-9_-]*$`)

// outputEnvRE is the allowed character set for the RIGHT-hand
// value of an `outputs:` map entry — the plugin's env-var name
// written to $GOCDNEXT_OUTPUT_FILE. Standard POSIX env-var-name
// shape: starts with letter/underscore, then alphanumerics +
// underscores. No lowercase requirement because the operator
// might be mirroring a third-party plugin's naming convention
// (NEXT, PROMOTED_DIGEST, etc.).
var outputEnvRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// validateOutputsDeclaration enforces the alias / env-name shape +
// a soft per-job limit so a misbehaving pipeline can't declare an
// open-ended outputs blob. The 64KB cap on actual output VALUES
// applies at agent + server layers; here we just bound the
// declaration count (operator-facing) so a 10000-entry block
// surfaces at apply rather than at dispatch.
func validateOutputsDeclaration(jobName string, outputs map[string]string) error {
	const maxOutputs = 64
	if len(outputs) > maxOutputs {
		return fmt.Errorf("job %q: outputs declares %d entries, cap is %d (open an issue if you legitimately need more)",
			jobName, len(outputs), maxOutputs)
	}
	for alias, envName := range outputs {
		if !outputAliasRE.MatchString(alias) {
			return fmt.Errorf("job %q: outputs alias %q must match %s — typically lowercase + dashes (e.g. `next`, `image-digest`)",
				jobName, alias, outputAliasRE.String())
		}
		if envName == "" {
			return fmt.Errorf("job %q: outputs alias %q maps to an empty env-var name — must name the variable the plugin writes to $GOCDNEXT_OUTPUT_FILE",
				jobName, alias)
		}
		if !outputEnvRE.MatchString(envName) {
			return fmt.Errorf("job %q: outputs[%s] env-var name %q must match %s — POSIX env-var shape (e.g. NEXT, PROMOTED_DIGEST)",
				jobName, alias, envName, outputEnvRE.String())
		}
	}
	return nil
}

// toService validates a service spec and derives a default name
// when omitted. image is mandatory — a service without one can't
// start. Name defaults to the image's short form (repository
// basename, tag stripped) so `image: postgres:16-alpine` implies
// `name: postgres` without extra YAML. Duplicate names across
// the pipeline would collide on the docker network alias; that
// check lives in ApplyProject where all services are visible
// together.
// notificationTriggers is the closed set of `on:` values. Keep
// in sync with domain.NotificationTrigger constants.
var notificationTriggers = map[string]domain.NotificationTrigger{
	"failure":  domain.NotifyOnFailure,
	"success":  domain.NotifyOnSuccess,
	"always":   domain.NotifyOnAlways,
	"canceled": domain.NotifyOnCanceled,
}

func toNotification(idx int, n NotificationSpec) (domain.Notification, error) {
	on := strings.TrimSpace(strings.ToLower(n.On))
	trig, ok := notificationTriggers[on]
	if !ok {
		return domain.Notification{}, fmt.Errorf(
			"notifications[%d]: unknown on %q (allowed: failure, success, always, canceled)",
			idx, n.On,
		)
	}
	if strings.TrimSpace(n.Uses) == "" {
		return domain.Notification{}, fmt.Errorf("notifications[%d]: `uses:` is required", idx)
	}
	return domain.Notification{
		On:      trig,
		Uses:    strings.TrimSpace(n.Uses),
		With:    cloneStrMap(n.With),
		Secrets: append([]string(nil), n.Secrets...),
	}, nil
}

func cloneStrMap(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

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

// canonicalArtifactPath strips trailing slashes so `dist` and
// `dist/` collapse to the same key when deduping artifact entries.
// Mirrors store.NormalizeArtifactPath (kept inline rather than
// importing — the parser shouldn't depend on the storage layer).
// Only the trailing slash is touched — we deliberately do NOT
// resolve `.`/`..` or otherwise rewrite the path, since the
// agent's tar/untar loop preserves operator-declared shape verbatim.
func canonicalArtifactPath(p string) string {
	for len(p) > 1 && p[len(p)-1] == '/' {
		p = p[:len(p)-1]
	}
	return p
}
