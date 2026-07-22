package parser

import (
	"fmt"
	"strings"

	"github.com/gocdnext/gocdnext/proto/cachekey"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

func toJob(name string, jd JobDef, pipelineVars map[string]string) (domain.Job, error) {
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
		Cluster:   jd.Cluster,
	}
	if jd.Cluster != "" {
		if !jobClusterRE.MatchString(jd.Cluster) {
			return domain.Job{}, fmt.Errorf("job %q: cluster name %q invalid (lowercase alnum, '-'/'_', start with alnum, ≤63 chars)", name, jd.Cluster)
		}
		// One source of truth for the kubeconfig: a managed cluster
		// injects PLUGIN_KUBECONFIG itself, so a `with: {kubeconfig: …}`
		// on the same job would be ambiguous.
		if jd.With["kubeconfig"] != "" {
			return domain.Job{}, fmt.Errorf("job %q: set either cluster: or with.kubeconfig, not both", name)
		}
		// …and nothing else may define PLUGIN_KUBECONFIG either. A
		// variable / secret / id_token / matrix dimension with that
		// exact name would fight the injected credential (last-writer-
		// wins is non-deterministic across the dispatch assembly), and a
		// deploy that silently authenticates against the wrong cluster is
		// the worst possible failure. Reject the collision at parse.
		const injected = "PLUGIN_KUBECONFIG"
		if _, ok := jd.Variables[injected]; ok {
			return domain.Job{}, fmt.Errorf("job %q: cluster: injects %s — remove the conflicting variables.%s", name, injected, injected)
		}
		for _, s := range jd.Secrets {
			if s == injected {
				return domain.Job{}, fmt.Errorf("job %q: cluster: injects %s — remove the conflicting secret %q", name, injected, injected)
			}
		}
		if _, ok := jd.IDTokens[injected]; ok {
			return domain.Job{}, fmt.Errorf("job %q: cluster: injects %s — remove the conflicting id_tokens.%s", name, injected, injected)
		}
		if jd.Parallel != nil {
			for _, dim := range jd.Parallel.Matrix {
				if _, ok := dim[injected]; ok {
					return domain.Job{}, fmt.Errorf("job %q: cluster: injects %s — remove the conflicting parallel.matrix dimension %q", name, injected, injected)
				}
			}
		}
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
		// artifacts.when gates upload on task outcome. Empty defaults to
		// on_success (upload only on a green job). Reject unknown values
		// loudly — a typo like `on_faliure` must not silently fall back to
		// on_success and hide a red scan's findings from the dashboard.
		switch w := strings.TrimSpace(jd.Artifacts.When); w {
		case "", "on_success":
			// default: leave j.ArtifactsWhen empty (== on_success)
		case "on_failure", "always":
			j.ArtifactsWhen = w
		default:
			return domain.Job{}, fmt.Errorf("job %q: artifacts.when must be on_success | on_failure | always (got %q)", name, jd.Artifacts.When)
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
	if jd.CoverageReport != nil {
		cr, err := toCoverageReport(name, jd.CoverageReport)
		if err != nil {
			return domain.Job{}, err
		}
		j.CoverageReport = cr
	}
	if len(jd.TestReports) > 0 {
		j.TestReports = append([]string(nil), jd.TestReports...)
	}

	if len(jd.Outputs) > 0 {
		// Flatten YAML's two-shape OutputDef into the legacy
		// alias→env-var map for validation + downstream consumers
		// that don't care about masking. The masking flag lives
		// on a parallel map so adding it didn't have to ripple
		// through every consumer of Job.Outputs.
		flat := make(map[string]string, len(jd.Outputs))
		var masks map[string]bool
		for k, v := range jd.Outputs {
			flat[k] = v.Env
			if v.Masked {
				if masks == nil {
					masks = make(map[string]bool, 1)
				}
				masks[k] = true
			}
		}
		if err := validateOutputsDeclaration(name, flat); err != nil {
			return domain.Job{}, err
		}
		j.Outputs = flat
		j.OutputMasks = masks
	}

	if len(jd.IDTokens) > 0 {
		specs, err := validateIDTokensDeclaration(name, jd, pipelineVars)
		if err != nil {
			return domain.Job{}, err
		}
		j.IDTokens = specs
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
		// parallel.matrix on a gate is rejected too (#97): a per-cell manual
		// gate is semantically odd, and the supersede backstop's "all governing
		// gates passed" would have to reason over matrix cells. Disallow it up
		// front so the model stays one gate = one decision.
		if jd.Parallel != nil {
			return domain.Job{}, fmt.Errorf(
				"job %q: approval gate cannot declare parallel/matrix — a gate is a single human decision, not a per-cell fan-out",
				name,
			)
		}
		if len(jd.Script) > 0 || jd.Uses != "" || jd.Image != "" ||
			jd.Settings != nil || jd.Artifacts != nil ||
			len(jd.NeedsArtifacts) > 0 || len(jd.Cache) > 0 ||
			jd.Docker || len(jd.IDTokens) > 0 || jd.Deploy != nil ||
			jd.Cluster != "" {
			return domain.Job{}, fmt.Errorf(
				"job %q: approval gate cannot declare script/uses/image/artifacts/cache/docker/id_tokens/deploy/cluster — it only blocks on a human decision. "+
					"For a promote-then-deploy flow, put deploy: on a separate executable job with needs: [<this-gate>]",
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

	// Deploy marker (#39): tracking only — the job's tasks (set
	// above) still perform the deploy. Validate AFTER task assembly
	// so "executable" is a real check (len(Tasks) > 0), and reject a
	// deploy on a job that runs nothing (a `deploy:` with no script/
	// uses/plugin would record a revision for a deploy that never
	// happened). Approval+deploy was already rejected above.
	if jd.Deploy != nil {
		if len(j.Tasks) == 0 {
			return domain.Job{}, fmt.Errorf(
				"job %q: deploy: requires an executable job (script:, uses:, or image:+settings:) — the marker tracks a deploy the job performs, it does not perform one itself",
				name)
		}
		env := strings.TrimSpace(jd.Deploy.Environment)
		if env == "" {
			return domain.Job{}, fmt.Errorf("job %q: deploy.environment is required", name)
		}
		if !domain.ValidEnvironmentName(env) {
			return domain.Job{}, fmt.Errorf(
				"job %q: deploy.environment %q has forbidden characters — start alphanumeric, then alphanumeric + . _ - (max 64)",
				name, jd.Deploy.Environment)
		}
		version := strings.TrimSpace(jd.Deploy.Version)
		// Validate strict-ref namespaces at apply time. Shell-style
		// ${VAR} is left alone (soft-resolved at dispatch, literal on
		// miss — never wedges the job); only `${{ }}` tokens can fail
		// the strict pass and hang the dispatch, so those are what we
		// gate here.
		for _, m := range deployVersionRefRE.FindAllStringSubmatch(version, -1) {
			if !deployVersionRefOK.MatchString(m[1]) {
				return domain.Job{}, fmt.Errorf(
					"job %q: deploy.version reference ${{ %s }} is not allowed — version accepts only ${{ needs.<job>.outputs.<alias> }} and ${{ CI_* }} "+
						"(variables/secrets are rejected: the version is recorded and shown in the Environments UI)",
					name, m[1])
			}
		}
		// revision takes the SAME non-secret allow-list: it is persisted as the
		// watch's expected_revision and surfaces in the UI and in error messages,
		// so a secret must never be able to reach it.
		revision := strings.TrimSpace(jd.Deploy.Revision)
		for _, m := range deployVersionRefRE.FindAllStringSubmatch(revision, -1) {
			if !deployVersionRefOK.MatchString(m[1]) {
				return domain.Job{}, fmt.Errorf(
					"job %q: deploy.revision reference ${{ %s }} is not allowed — revision accepts only ${{ needs.<job>.outputs.<alias> }} and ${{ CI_* }} "+
						"(variables/secrets are rejected: the revision is recorded and shown in the UI)",
					name, m[1])
			}
		}
		j.Deploy = &domain.DeploySpec{
			Environment: env,
			Version:     version,
			Revision:    revision,
		}
	}

	if jd.Parallel != nil && len(jd.Parallel.Matrix) > 0 {
		// Reject empty entries (`matrix: [{}]`) BEFORE folding —
		// an entry that's `{}` contributes nothing to the flat
		// map and would pass `validateMatrixDimensions` (which
		// iterates dims) silently, then expandMatrix in the
		// store turns it into a single row with matrix_key="" —
		// violating the "matrix-declared job never produces an
		// empty-key row" invariant the substitution layer relies
		// on. Reject loud at the entry level so the operator
		// sees exactly which entry is malformed.
		for i, e := range jd.Parallel.Matrix {
			if len(e) == 0 {
				return domain.Job{}, fmt.Errorf("job %q: parallel.matrix entry %d is empty; remove the entry or supply at least one dimension", name, i)
			}
		}
		flat := flattenMatrix(jd.Parallel.Matrix)
		// Defence in depth: even with non-empty entries, if every
		// entry's dim list was empty (caught by
		// validateMatrixDimensions below) OR the user passed
		// `matrix: [{ }]` shapes the YAML parser unmarshalled as
		// non-empty but produced no keys, the flattened map is
		// empty. Either way, a declared `matrix:` MUST yield at
		// least one dimension; otherwise the row routing gets
		// the wrong contract.
		if len(flat) == 0 {
			return domain.Job{}, fmt.Errorf("job %q: parallel.matrix is declared but expands to zero dimensions; remove the matrix block or supply at least one dimension", name)
		}
		if err := validateMatrixDimensions(name, flat); err != nil {
			return domain.Job{}, err
		}
		if err := validateMatrixDimNames(name, flat, pipelineVars, jd.Variables, jd.Secrets, jd.IDTokens); err != nil {
			return domain.Job{}, err
		}
		j.Matrix = flat
	}

	// Unenforced keys are REJECTED, not silently parsed (#40): a
	// `rules:` block that gates nothing is a safety rail that
	// isn't there. domain.Rule stays for decoding definitions
	// persisted before this version; new applies must drop the key.
	if len(jd.Rules) > 0 {
		return domain.Job{}, fmt.Errorf(
			"job %s: rules: is not enforced at dispatch and would silently gate nothing — "+
				"remove it; for change-based triggering use when.paths, for promotion gates use approval: "+
				"(tracked in issue #40)", name)
	}
	if jd.When != nil {
		return domain.Job{}, fmt.Errorf(
			"job %s: job-level when: is accepted by the schema but not implemented — "+
				"remove it; pipeline-level when: (event/branch/paths) is the working trigger gate", name)
	}

	return j, nil
}
