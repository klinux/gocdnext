// Package scheduler turns queued runs into JobAssignments dispatched to live
// agents. It listens on PostgreSQL `run_queued` notifications and falls back
// to a periodic tick so missed notifies don't leave work stuck. Current slice
// (C3) dispatches jobs in the active stage; result handling + stage progression
// arrives in C5.
package scheduler

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"

	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
	"github.com/gocdnext/gocdnext/server/internal/domain"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

type revisionSnapshot struct {
	Revision string `json:"revision"`
	Branch   string `json:"branch"`
}

// BuildAssignment composes a JobAssignment proto from the run's pipeline
// snapshot + the dispatchable job row + the pipeline's material rows +
// already-resolved secrets (keyed by name) + pre-signed artifact
// downloads. Secret values land in env alongside the pipeline's own
// variables AND are echoed into LogMasks so the runner can replace them
// with *** in every log line. `downloads` is the list of upstream-job
// artefacts this job declares via `needs_artifacts:`; nil/empty when
// the job has no deps.
//
// `profileEnv` is the merged env (plain + decrypted secrets) from the
// job's runner profile; it lays in FIRST so explicit pipeline / job /
// project-secret values can override profile defaults. `profileMasks`
// is the matching list of profile secret VALUES — same redaction
// contract as job secrets.
func BuildAssignment(
	run store.RunForDispatch,
	job store.DispatchableJob,
	materials []store.Material,
	secrets map[string]string,
	downloads []*gocdnextv1.ArtifactDownload,
	profile store.ResolvedProfile,
	cloneTokens map[string]string,
	needsOutputs NeedsOutputs,
) (*gocdnextv1.JobAssignment, error) {
	var def domain.Pipeline
	if err := json.Unmarshal(run.Definition, &def); err != nil {
		return nil, fmt.Errorf("scheduler: decode pipeline: %w", err)
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
				return nil, fmt.Errorf("scheduler: notification idx %d out of range", idx)
			}
			image, err := domain.ResolvePluginRef(n.Uses)
			if err != nil {
				return nil, fmt.Errorf("scheduler: notification %d: %w", idx, err)
			}
			jobDef = domain.Job{
				Name:    job.Name,
				Stage:   domain.NotificationStageName,
				Image:   image,
				Tasks:   []domain.Task{{Plugin: &domain.PluginStep{Image: image, Settings: n.With}}},
				Secrets: append([]string(nil), n.Secrets...),
			}
		} else {
			return nil, fmt.Errorf("scheduler: job %q not in pipeline definition", job.Name)
		}
	}

	revs := map[string]revisionSnapshot{}
	if len(run.Revisions) > 0 {
		if err := json.Unmarshal(run.Revisions, &revs); err != nil {
			return nil, fmt.Errorf("scheduler: decode revisions: %w", err)
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
		// Expose the matrix key so scripts can branch on it. Full matrix
		// variable decomposition (OS=linux → $OS) is deferred.
		env["GOCDNEXT_MATRIX"] = job.MatrixKey
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
			return nil, fmt.Errorf("scheduler: secret %q not resolved for job %s", name, job.Name)
		}
		env[name] = v
		if v != "" {
			masks = append(masks, v)
		}
	}

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
	if needsResolved, err := substituteNeedsRefsMap(env, needsOutputs); err != nil {
		return nil, fmt.Errorf("scheduler: needs refs in env for job %s: %w", job.Name, err)
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
		return nil, fmt.Errorf("scheduler: env for job %s: %w", job.Name, err)
	}
	env = resolvedEnv

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
			pluginSettings, err := substituteNeedsRefsMap(tk.Plugin.Settings, needsOutputs)
			if err != nil {
				return nil, fmt.Errorf("scheduler: needs refs in plugin %q settings: %w", tk.Plugin.Image, err)
			}
			settings, err := substituteRefsMap(pluginSettings, secrets, env)
			if err != nil {
				return nil, fmt.Errorf("scheduler: plugin %q settings: %w", tk.Plugin.Image, err)
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
		Resources:             resourceRequirements(jobDef.Resources),
		Profile:               jobDef.Profile,
		Outputs:               copyStringMap(jobDef.Outputs),
		NodeSelector:          copyStringMap(profile.NodeSelector),
		Tolerations:           tolerationsToProto(profile.Tolerations),
	}, nil
}

// tolerationsToProto maps the store-side Toleration list (validated
// + normalised at write time) to the proto wire shape. Returns nil
// on empty input so the wire stays minimal — engines treat absent +
// empty list identically.
func tolerationsToProto(in []store.Toleration) []*gocdnextv1.Toleration {
	if len(in) == 0 {
		return nil
	}
	out := make([]*gocdnextv1.Toleration, len(in))
	for i, t := range in {
		out[i] = &gocdnextv1.Toleration{
			Key:               t.Key,
			Operator:          t.Operator,
			Value:             t.Value,
			Effect:            t.Effect,
			TolerationSeconds: t.TolerationSeconds,
		}
	}
	return out
}

// copyStringMap returns a fresh copy of the input — nil-tolerant.
// Used for the JobAssignment.Outputs field so a later mutation of
// the parsed-pipeline cache doesn't leak into in-flight assignments.
func copyStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// resourceRequirements maps the resolved domain ResourceSpec to its
// proto twin. Returns nil when nothing is set so the wire stays
// minimal — engines treat absent + all-empty identically.
func resourceRequirements(r domain.ResourceSpec) *gocdnextv1.ResourceRequirements {
	if r.IsZero() {
		return nil
	}
	return &gocdnextv1.ResourceRequirements{
		CpuRequest:    r.Requests.CPU,
		CpuLimit:      r.Limits.CPU,
		MemoryRequest: r.Requests.Memory,
		MemoryLimit:   r.Limits.Memory,
	}
}

// JobSecretsFromDefinition returns the list of secret names a job declares,
// by parsing the stored pipeline definition JSONB. Exported so the scheduler
// can resolve secrets before calling BuildAssignment.
func JobSecretsFromDefinition(definition []byte, jobName string) ([]string, error) {
	jobDef, err := jobDefFromDefinition(definition, jobName)
	if err != nil {
		return nil, err
	}
	return append([]string(nil), jobDef.Secrets...), nil
}

// JobTagsFromDefinition returns the list of required agent tags for a job.
// Exported so the scheduler can filter live sessions before picking one.
func JobTagsFromDefinition(definition []byte, jobName string) ([]string, error) {
	jobDef, err := jobDefFromDefinition(definition, jobName)
	if err != nil {
		return nil, err
	}
	return append([]string(nil), jobDef.Tags...), nil
}

// JobArtifactDepsFromDefinition returns the `needs_artifacts:` entries
// for a job, decoded into domain.ArtifactDep. Scheduler calls this
// ahead of BuildAssignment so it can fetch the upstream artefact rows
// and sign download URLs to embed in the assignment.
func JobArtifactDepsFromDefinition(definition []byte, jobName string) ([]domain.ArtifactDep, error) {
	jobDef, err := jobDefFromDefinition(definition, jobName)
	if err != nil {
		return nil, err
	}
	return append([]domain.ArtifactDep(nil), jobDef.ArtifactDeps...), nil
}

func jobDefFromDefinition(definition []byte, jobName string) (domain.Job, error) {
	var def domain.Pipeline
	if err := json.Unmarshal(definition, &def); err != nil {
		return domain.Job{}, fmt.Errorf("scheduler: decode pipeline: %w", err)
	}
	jobDef, ok := findJob(def.Jobs, jobName)
	if !ok {
		return domain.Job{}, fmt.Errorf("scheduler: job %q not in pipeline definition", jobName)
	}
	return jobDef, nil
}

// notificationAtIndex resolves the Nth entry in the pipeline
// definition's Notifications list. Used by the dispatch path to
// read back a synth job's `on:` trigger + `with:` without having
// to expand every synth job into def.Jobs at run-create time.
// Returns ok=false when the index is out of range — the caller
// treats that as "spec vanished after apply", dropping the
// dispatch rather than crashing.
func notificationAtIndex(definition []byte, idx int) (domain.Notification, bool) {
	var def struct {
		Notifications []domain.Notification `json:"Notifications"`
	}
	if err := json.Unmarshal(definition, &def); err != nil {
		return domain.Notification{}, false
	}
	if idx < 0 || idx >= len(def.Notifications) {
		return domain.Notification{}, false
	}
	return def.Notifications[idx], true
}

// effectiveNotificationAtIndex mirrors the run-create precedence:
// the pipeline's own Notifications list wins whenever it was
// declared (non-nil, even empty); the project's list only acts as
// a fallback when the pipeline never mentioned `notifications:`.
// Used by both the scheduler's trigger check and BuildAssignment
// so the dispatch path talks to exactly the same spec the run
// creator persisted.
func effectiveNotificationAtIndex(pipelineNotifs []domain.Notification, projectNotifsRaw []byte, idx int) (domain.Notification, bool) {
	source := pipelineNotifs
	if source == nil && len(projectNotifsRaw) > 0 {
		var projectNs []domain.Notification
		if err := json.Unmarshal(projectNotifsRaw, &projectNs); err == nil {
			source = projectNs
		}
	}
	if idx < 0 || idx >= len(source) {
		return domain.Notification{}, false
	}
	return source[idx], true
}

// resolveNotificationSpec is a thin wrapper over
// effectiveNotificationAtIndex that works off RunForDispatch —
// decodes the pipeline definition once, then defers to the
// shared precedence helper. Exposed so the scheduler's dispatch
// loop reads at the right abstraction ("resolve by run") without
// copy-pasting the Definition decode.
func resolveNotificationSpec(run store.RunForDispatch, idx int) (domain.Notification, bool) {
	var def struct {
		Notifications []domain.Notification `json:"Notifications"`
	}
	if err := json.Unmarshal(run.Definition, &def); err != nil {
		return domain.Notification{}, false
	}
	return effectiveNotificationAtIndex(def.Notifications, run.ProjectNotifications, idx)
}

// concurrencyFromDefinition pulls the pipeline-level concurrency
// setting from the JSONB snapshot. Empty / malformed / unknown
// values fall back to "" (parallel) so a bad definition never
// makes the scheduler wait forever — it's safer to race than
// deadlock.
func concurrencyFromDefinition(definition []byte) (string, error) {
	var def struct {
		Concurrency string `json:"Concurrency"`
	}
	if err := json.Unmarshal(definition, &def); err != nil {
		return "", fmt.Errorf("scheduler: decode pipeline: %w", err)
	}
	return def.Concurrency, nil
}

func findJob(jobs []domain.Job, name string) (domain.Job, bool) {
	for _, j := range jobs {
		if j.Name == name {
			return j, true
		}
	}
	return domain.Job{}, false
}

// materialCheckouts emits the gRPC MaterialCheckout entries the agent
// needs to clone each git material. When cloneTokens carries a token
// for the material id, the token is embedded in the URL as
// `https://x-access-token:TOKEN@host/...` so plain `git clone` picks
// it up without a credential helper, and the token is returned in the
// masks slice so the caller can append it to LogMasks. Non-https URLs
// are passed through untouched — SSH URLs need an in-pod SSH key, not
// a bearer.
func materialCheckouts(
	materials []store.Material,
	revs map[string]revisionSnapshot,
	cloneTokens map[string]string,
) ([]*gocdnextv1.MaterialCheckout, []string) {
	out := make([]*gocdnextv1.MaterialCheckout, 0, len(materials))
	var masks []string
	for _, m := range materials {
		if m.Type != string(domain.MaterialGit) {
			// Non-git materials don't need agent-side checkout (upstream/cron
			// are pure triggers; manual has no source code).
			continue
		}
		var cfg domain.GitMaterial
		if err := json.Unmarshal(m.Config, &cfg); err != nil {
			continue
		}
		rev := revs[m.ID.String()]
		// Restore a clonable scheme on URLs that were canonicalised
		// at apply time. The store layer matches scm_sources rows by
		// the canonical scheme-less form (`github.com/owner/repo`);
		// `git clone` can't speak that, so HTTPCloneURL hands it the
		// HTTPS variant. URLs that already carry a scheme (or are
		// SSH shorthand) pass through.
		url := domain.HTTPCloneURL(cfg.URL)
		if tok := cloneTokens[m.ID.String()]; tok != "" {
			if rewritten, ok := injectBearerInHTTPSURL(url, tok); ok {
				url = rewritten
				masks = append(masks, tok)
			}
		}
		out = append(out, &gocdnextv1.MaterialCheckout{
			MaterialId: m.ID.String(),
			Url:        url,
			Revision:   rev.Revision,
			Branch:     firstNonEmpty(rev.Branch, cfg.Branch),
			TargetDir:  targetDirFor(m.ID),
			SecretRef:  cfg.SecretRef,
		})
	}
	return out, masks
}

// injectBearerInHTTPSURL rewrites `https://host/...` into
// `https://x-access-token:TOKEN@host/...` so plain git picks the
// credential up without a helper. Returns (original, false) for any
// shape that isn't a plain `https://` URL: SSH, ssh://, scheme-less
// canonical form, or already-embedded credentials all fall through
// to the unauthenticated clone path the operator can debug from logs.
func injectBearerInHTTPSURL(raw, token string) (string, bool) {
	const prefix = "https://"
	if !strings.HasPrefix(raw, prefix) || token == "" {
		return raw, false
	}
	rest := raw[len(prefix):]
	if strings.Contains(rest, "@") {
		// Pre-existing user:pass — leave it to the operator's intent.
		return raw, false
	}
	return prefix + "x-access-token:" + token + "@" + rest, true
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func targetDirFor(id uuid.UUID) string {
	// Deterministic + short; agents create this dir under the workspace.
	return "src/" + id.String()[:8]
}

// dedupeArtifactPaths cleans the (required, optional) pair the
// agent receives so neither list contains canonically-identical
// duplicates AND optional never overlaps required. Defensive
// duplicate of the parser's apply-time dedupe (parse.go), applied
// here at dispatch so pipelines whose definition was persisted
// BEFORE the parser fix shipped still get a clean assignment.
//
// First-occurrence shape wins (operator's typing round-trips to
// the agent's tar entry name). Required wins over optional on
// cross-list collisions — the existing semantic.
//
// Uses store.NormalizeArtifactPath as the canonical form so the
// dedupe key here matches the one the storage layer's partial
// unique index enforces — drift between the two would let an
// "agent-deduped" assignment still trip the index. The package
// already imports store for other reasons, so reusing the helper
// has no dep cost.
func dedupeArtifactPaths(required, optional []string) (req, opt []string) {
	canonReq := make(map[string]struct{}, len(required))
	req = make([]string, 0, len(required))
	for _, p := range required {
		canon := store.NormalizeArtifactPath(p)
		if _, dup := canonReq[canon]; dup {
			continue
		}
		canonReq[canon] = struct{}{}
		req = append(req, p)
	}
	canonOpt := make(map[string]struct{}, len(optional))
	opt = make([]string, 0, len(optional))
	for _, p := range optional {
		canon := store.NormalizeArtifactPath(p)
		if _, dup := canonReq[canon]; dup {
			continue
		}
		if _, dup := canonOpt[canon]; dup {
			continue
		}
		canonOpt[canon] = struct{}{}
		opt = append(opt, p)
	}
	return req, opt
}
