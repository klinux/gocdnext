// Package scheduler turns queued runs into JobAssignments dispatched to live
// agents. It listens on PostgreSQL `run_queued` notifications and falls back
// to a periodic tick so missed notifies don't leave work stuck. Current slice
// (C3) dispatches jobs in the active stage; result handling + stage progression
// arrives in C5.
package scheduler

import (
	"encoding/json"
	"fmt"
	"sort"

	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

func BuildAssignment(
	run store.RunForDispatch,
	job store.DispatchableJob,
	materials []store.Material,
	secrets map[string]string,
	downloads []*gocdnextv1.ArtifactDownload,
	profile store.ResolvedProfile,
	cloneTokens map[string]string,
	needsOutputs NeedsOutputs,
	matrixNeedsOutputs MatrixNeedsOutputs,
	idTokens map[string]string,
	clusterKubeconfig string,
	clusterMasks []string,
) (*gocdnextv1.JobAssignment, *DeployTarget, error) {
	var def domain.Pipeline
	if err := json.Unmarshal(run.Definition, &def); err != nil {
		return nil, nil, fmt.Errorf("scheduler: decode pipeline: %w", err)
	}

	jobDef, ok := findJob(def.Jobs, job.Name)
	if !ok {
		// Synth notification jobs aren't in def.Jobs — they're
		// materialized at run-create time from the effective
		// notifications list (pipeline's own or, when silent,
		// the project's). Resolve via the same priority the run
		// creator used so a dispatch always talks to the same
		// spec the synth came from.
		if idx, isNotif := domain.NotificationIndexFromName(job.Name); isNotif {
			n, ok := effectiveNotificationAtIndex(def.Notifications, run.ProjectNotifications, idx)
			if !ok {
				return nil, nil, fmt.Errorf("scheduler: notification idx %d out of range", idx)
			}
			image, err := domain.ResolvePluginRef(n.Uses)
			if err != nil {
				return nil, nil, fmt.Errorf("scheduler: notification %d: %w", idx, err)
			}
			jobDef = domain.Job{
				Name:    job.Name,
				Stage:   domain.NotificationStageName,
				Image:   image,
				Tasks:   []domain.Task{{Plugin: &domain.PluginStep{Image: image, Settings: n.With}}},
				Secrets: append([]string(nil), n.Secrets...),
			}
		} else {
			return nil, nil, fmt.Errorf("scheduler: job %q not in pipeline definition", job.Name)
		}
	}

	revs := map[string]revisionSnapshot{}
	if len(run.Revisions) > 0 {
		if err := json.Unmarshal(run.Revisions, &revs); err != nil {
			return nil, nil, fmt.Errorf("scheduler: decode revisions: %w", err)
		}
	}

	// Build env FIRST so plugin Settings (below) can resolve
	// `${{ NAME }}` refs against the same pool variables+secrets
	// land in. Pre-fix, settings shipped to the agent verbatim and
	// the literal `${{ DOCKER_USERNAME }}` reached `docker login`.
	env := map[string]string{}
	// Profile env first — operator-level defaults that pipeline/job
	// vars can override below. Profile secrets are pre-decrypted by
	// the caller; they share the same map with profile plain env so
	// override precedence is uniform (later writes win).
	for k, v := range profile.Env {
		env[k] = v
	}
	for k, v := range def.Variables {
		env[k] = v
	}
	for k, v := range jobDef.Variables {
		env[k] = v
	}
	if job.MatrixKey != "" {
		// Expose the combined matrix key (GOCDNEXT_MATRIX="ARCH=...,OS=...")
		// AND decompose it into one env var per dimension (#42): OS=linux
		// → $OS=linux, so scripts read `$OS` directly. The parser
		// validated the names (valid identifier, no reserved prefix, no
		// collision with variables/secrets) and values (no ,/= which are
		// the key separators), so the split below is unambiguous. Lands
		// AFTER pipeline/job variables (a matrix dim is the per-row axis,
		// more specific) and BEFORE secrets (a secret of the same name —
		// only possible on a pre-#42 persisted definition — still wins).
		env["GOCDNEXT_MATRIX"] = job.MatrixKey
		for k, v := range decomposeMatrixKey(job.MatrixKey) {
			env[k] = v
		}
	}

	// Secrets: layer on top of the pipeline-declared env. A secret with the
	// same name as a plain variable wins — we trust the user to not shadow
	// a secret name with a plain variable by accident.
	masks := make([]string, 0, len(secrets)+len(profile.SecretValues))
	// Profile secret VALUES land in masks so the runner redacts them
	// from logs. Done before the job-secrets loop so the order is
	// "profile masks then job masks" — order doesn't matter for
	// correctness (the runner does substring replace) but it makes
	// test fixtures stable.
	for _, v := range profile.SecretValues {
		if v != "" {
			masks = append(masks, v)
		}
	}
	for _, name := range jobDef.Secrets {
		v, ok := secrets[name]
		if !ok {
			// Caller should have resolved every declared secret before
			// calling BuildAssignment; anything missing here is a contract
			// bug we surface as an error.
			return nil, nil, fmt.Errorf("scheduler: secret %q not resolved for job %s", name, job.Name)
		}
		env[name] = v
		if v != "" {
			masks = append(masks, v)
		}
	}

	// OIDC id_tokens: pre-minted by the caller (mintIDTokens), same
	// pure-function pattern as cloneTokens. Injected AFTER secrets so
	// a token wins any residual name collision — the parser already
	// rejects declared collisions, this is defence in depth against
	// future env layers. Every JWT goes into masks: it's a bearer
	// credential and must never survive into a log stream.
	for name, token := range idTokens {
		env[name] = token
		if token != "" {
			masks = append(masks, token)
		}
	}

	// Managed cluster kubeconfig (pre-resolved by the caller from the
	// clusters registry). Injected as PLUGIN_KUBECONFIG — the same input
	// the kubectl/helm plugins already consume — so a `cluster:` job
	// authenticates without a pasted kubeconfig secret. Empty when the
	// job names no cluster, or names an in_cluster one (the pod's SA is
	// used). The resolver returns the full mask set (whole blob + each
	// sensitive scalar + raw token) so the agent's line-by-line log
	// redaction catches the credential even though the kubeconfig is
	// multiline — see store.ResolveClusterForDispatch.
	if clusterKubeconfig != "" {
		env["PLUGIN_KUBECONFIG"] = clusterKubeconfig
	}
	masks = append(masks, clusterMasks...)

	// Outputs (issue #10): values resolved from upstream
	// `${{ needs.X.outputs.Y }}` refs flow into env/plugin
	// settings AND become candidates for log lines. Treat them
	// as POTENTIALLY-SENSITIVE by default — promotion plugins
	// emit digests (low risk) but operators can put short-lived
	// tokens / signed URLs / etc. in outputs and downstream
	// shouldn't echo them in plain text.
	//
	// Defence in depth: append every output VALUE that's long
	// enough to survive the runner's len(m) < 4 floor (see
	// applyMasks in agent/runner.go) so a short version string
	// doesn't cause false-positive replacements across log
	// lines. The doc tells operators that outputs aren't a
	// secret channel and they should use `secrets:` for real
	// tokens — this masking is the safety net, not the
	// recommended path.
	for _, group := range needsOutputs {
		for _, v := range group {
			if len(v) >= 8 {
				masks = append(masks, v)
			}
		}
	}
	// Same heuristic for matrix-expanded outputs (issue #21): every
	// row of every matrix upstream contributes its long values to
	// the auto-mask backstop. The scheduler doesn't know yet which
	// rows the downstream's selector will pick at substitution
	// time, so we err on the safe side and include them all.
	for _, rows := range matrixNeedsOutputs {
		for _, group := range rows {
			for _, v := range group {
				if len(v) >= 8 {
					masks = append(masks, v)
				}
			}
		}
	}

	// Opt-in masking (issue #22): for each upstream that flagged
	// `outputs.<alias>.masked: true`, add the resolved value to
	// LogMasks unconditionally — bypassing the 8+-char heuristic
	// above. The heuristic is a backstop; the explicit declaration
	// is the operator's contract for "this value IS sensitive".
	// Look up OutputMasks on each upstream's domain.Job in the
	// pipeline definition snapshot (already deserialised into
	// `def` above).
	for upstreamName, group := range needsOutputs {
		upstream, ok := findJob(def.Jobs, upstreamName)
		if !ok || len(upstream.OutputMasks) == 0 {
			continue
		}
		for alias, value := range group {
			if upstream.OutputMasks[alias] && value != "" {
				masks = append(masks, value)
			}
		}
	}
	// Matrix variant of the opt-in mask: walk every row of every
	// flagged upstream and add masked values across the board.
	// Same selector-uncertainty rationale as the heuristic block
	// — we mask all rows since the downstream may pick any of them.
	for upstreamName, rows := range matrixNeedsOutputs {
		upstream, ok := findJob(def.Jobs, upstreamName)
		if !ok || len(upstream.OutputMasks) == 0 {
			continue
		}
		for _, group := range rows {
			for alias, value := range group {
				if upstream.OutputMasks[alias] && value != "" {
					masks = append(masks, value)
				}
			}
		}
	}

	// Build the MatrixDimNames table the substitution layer needs
	// to resolve the `matrix[apac]` 1-dim shortcut (issue #21).
	// Only upstreams that actually have rows in matrixNeedsOutputs
	// need an entry — for other jobs the lookup is irrelevant.
	// Dimension order in the slice is lex-sorted so 1-dim
	// shortcut behaviour stays deterministic regardless of YAML
	// declaration order.
	matrixDims := make(MatrixDimNames, len(matrixNeedsOutputs))
	for upstreamName := range matrixNeedsOutputs {
		upstream, ok := findJob(def.Jobs, upstreamName)
		if !ok || len(upstream.Matrix) == 0 {
			continue
		}
		dims := make([]string, 0, len(upstream.Matrix))
		for d := range upstream.Matrix {
			dims = append(dims, d)
		}
		sort.Strings(dims)
		matrixDims[upstreamName] = dims
	}

	// CI_* built-ins (CI_BRANCH, CI_COMMIT_SHORT_SHA, CI_RUN_COUNTER, …)
	// land in env BEFORE substitution so a pipeline variable like
	// `IMAGE_TAG: 1.${CI_RUN_COUNTER}.${CI_COMMIT_SHORT_SHA}` resolves
	// against them at dispatch time. They also flow to the container
	// at runtime so script tasks can read them directly.
	ciVars := buildCIVars(run, job.Name)
	for k, v := range ciVars {
		env[k] = v
	}

	// Pre-pass: resolve `${{ needs.<job>.outputs.<alias> }}` refs
	// BEFORE the standard substituteRefs pass. Separation rationale
	// + matrix-ambiguity contract live in refs.go::NeedsOutputs
	// doc; here we just thread the pre-built table through. Nil
	// NeedsOutputs short-circuits the pre-pass for jobs with no
	// needs (the common case).
	if needsResolved, err := substituteNeedsRefsMap(env, needsOutputs, matrixNeedsOutputs, matrixDims); err != nil {
		return nil, nil, fmt.Errorf("scheduler: needs refs in env for job %s: %w", job.Name, err)
	} else {
		env = needsResolved
	}

	// Resolve `${{ NAME }}` refs in env values against secrets +
	// CI built-ins. Variables-referencing-variables would need a
	// topological sort to be deterministic (Go map iteration is
	// random) — we don't want that complexity here. Documented
	// contract: variables MAY reference secrets and CI vars,
	// settings MAY reference variables + secrets + CI vars,
	// variables MAY NOT reference other plain variables.
	resolvedEnv, err := substituteRefsMap(env, secrets, ciVars)
	if err != nil {
		return nil, nil, fmt.Errorf("scheduler: env for job %s: %w", job.Name, err)
	}
	env = resolvedEnv

	// Deploy marker (#39): resolve the recorded version with the SAME
	// needs.outputs pre-pass + CI built-ins the env used — but NOT
	// secrets, since the version is persisted in deployment_revisions
	// and surfaced in the Environments UI (a secret-bearing version
	// would leak). An empty version defaults to the commit short sha.
	// The caller records the revision once the job actually dispatches.
	var deployTarget *DeployTarget
	if jobDef.Deploy != nil {
		version := jobDef.Deploy.Version
		if version == "" {
			// Default: the commit short sha. buildCIVars omits the key
			// entirely when the run has no git revision (manual run, no
			// material), so an empty result here is a real config error
			// for THIS run — fail terminally (ErrDeployVersionEmpty in
			// the dispatcher) rather than recording a blank version.
			version = ciVars["CI_COMMIT_SHORT_SHA"]
		} else {
			version, err = resolveDeployVersion(version, needsOutputs, matrixNeedsOutputs, matrixDims, ciVars)
			if err != nil {
				// Wrapped in ErrDeployVersionUnresolved — terminal.
				return nil, nil, fmt.Errorf("scheduler: job %s: %w", job.Name, err)
			}
		}
		if version == "" {
			return nil, nil, fmt.Errorf("%w: job %s deploy.environment %q",
				ErrDeployVersionEmpty, job.Name, jobDef.Deploy.Environment)
		}
		deployTarget = &DeployTarget{Environment: jobDef.Deploy.Environment, Version: version}
	}

	// Tasks: plugin Settings can pull from the resolved env (which
	// carries variables + secrets + CI vars) PLUS the raw secrets
	// map. Secrets first so a secret with the same NAME as a variable
	// wins — same precedence the env merge above enforced. After the
	// strict `${{ NAME }}` pass, a second pass resolves shell-style
	// `${VAR}` refs (CI built-ins, env values) — soft so a literal
	// `${HOME}` in a setting reaches the plugin entrypoint verbatim
	// for runtime use.
	tasks := make([]*gocdnextv1.TaskSpec, 0, len(jobDef.Tasks))
	for _, tk := range jobDef.Tasks {
		switch {
		case tk.Script != "":
			tasks = append(tasks, &gocdnextv1.TaskSpec{
				Kind: &gocdnextv1.TaskSpec_Script{Script: tk.Script},
			})
		case tk.Plugin != nil:
			// Pre-pass on plugin settings too — same contract as env.
			pluginSettings, err := substituteNeedsRefsMap(tk.Plugin.Settings, needsOutputs, matrixNeedsOutputs, matrixDims)
			if err != nil {
				return nil, nil, fmt.Errorf("scheduler: needs refs in plugin %q settings: %w", tk.Plugin.Image, err)
			}
			settings, err := substituteRefsMap(pluginSettings, secrets, env)
			if err != nil {
				return nil, nil, fmt.Errorf("scheduler: plugin %q settings: %w", tk.Plugin.Image, err)
			}
			settings = substituteShellVarsMap(settings, secrets, env)
			tasks = append(tasks, &gocdnextv1.TaskSpec{
				Kind: &gocdnextv1.TaskSpec_Plugin{Plugin: &gocdnextv1.PluginSpec{
					Image:    tk.Plugin.Image,
					Settings: settings,
				}},
			})
		}
	}

	checkouts, cloneMasks := materialCheckouts(materials, revs, cloneTokens)
	// Append clone-token values to LogMasks so the bearer never
	// appears verbatim in the agent's `$ git clone <url>` echo or in
	// `git remote -v` output. applyMasks does a plain substring
	// replace; duplicate entries are harmless.
	masks = append(masks, cloneMasks...)

	// Pipeline-level services travel per-assignment so the agent
	// decides the network shape without re-reading the pipeline
	// definition. Empty slice (len==0) becomes a nil on the wire —
	// same as omission — so engines that don't support services
	// keep their current fast path.
	var services []*gocdnextv1.ServiceSpec
	if len(def.Services) > 0 {
		services = make([]*gocdnextv1.ServiceSpec, 0, len(def.Services))
		for _, s := range def.Services {
			services = append(services, &gocdnextv1.ServiceSpec{
				Name:    s.Name,
				Image:   s.Image,
				Env:     s.Env,
				Command: append([]string(nil), s.Command...),
			})
		}
	}

	// Cache entries ride per-job (not pipeline-level) — each job
	// declares what it wants to persist. Agent will GET each
	// before tasks and PUT after success. Same shape as services:
	// empty = nil on wire, fast path stays hot.
	var caches []*gocdnextv1.CacheEntry
	if len(jobDef.Cache) > 0 {
		caches = make([]*gocdnextv1.CacheEntry, 0, len(jobDef.Cache))
		for _, c := range jobDef.Cache {
			caches = append(caches, &gocdnextv1.CacheEntry{
				Key:   c.Key,
				Paths: append([]string(nil), c.Paths...),
			})
		}
	}

	dedupedArtifactPaths, dedupedOptionalPaths :=
		dedupeArtifactPaths(jobDef.ArtifactPaths, jobDef.OptionalArtifactPaths)

	return &gocdnextv1.JobAssignment{
		RunId:          run.ID.String(),
		JobId:          job.ID.String(),
		Name:           job.Name,
		Image:          job.Image,
		Tasks:          tasks,
		Env:            env,
		Checkouts:      checkouts,
		Workspace:      "/workspace",
		TimeoutSeconds: 0,
		LogMasks:       masks,
		// Dedupe artifact paths by canonical form (trailing slashes
		// trimmed) WITHIN each list AND ACROSS the two lists. The
		// parser already does this at apply time, but `run.Definition`
		// is the persisted snapshot taken when the pipeline was first
		// applied — pre-fix pipelines stored before this release still
		// carry the raw (potentially-duplicated) shape. Deduping here
		// in BuildAssignment means the wire format the agent sees is
		// always clean, regardless of which release applied the
		// pipeline.
		//
		// Without this layer: a pipeline applied pre-fix with
		// `paths: [dist]` / `optional: [dist/]` would still trip the
		// optional batch's INSERT against the partial unique index
		// (storage layer canonicalizes `dist/` to `dist`, collides
		// with the required `dist` row), rolling back the optional
		// txn and silently dropping any other optional artifacts.
		ArtifactPaths:         dedupedArtifactPaths,
		OptionalArtifactPaths: dedupedOptionalPaths,
		ArtifactDownloads:     downloads,
		Docker:                jobDef.Docker,
		Services:              services,
		Caches:                caches,
		TestReports:           append([]string(nil), jobDef.TestReports...),
		CoverageReport:        coverageSpec(jobDef.CoverageReport),
		Resources:             resourceRequirements(jobDef.Resources),
		Profile:               jobDef.Profile,
		Outputs:               copyStringMap(jobDef.Outputs),
		NodeSelector:          copyStringMap(profile.NodeSelector),
		Tolerations:           tolerationsToProto(profile.Tolerations),
	}, deployTarget, nil
}
