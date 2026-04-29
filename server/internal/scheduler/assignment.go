// Package scheduler turns queued runs into JobAssignments dispatched to live
// agents. It listens on PostgreSQL `run_queued` notifications and falls back
// to a periodic tick so missed notifies don't leave work stuck. Current slice
// (C3) dispatches jobs in the active stage; result handling + stage progression
// arrives in C5.
package scheduler

import (
	"encoding/json"
	"fmt"

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
	profileEnv map[string]string,
	profileMasks []string,
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

	tasks := make([]*gocdnextv1.TaskSpec, 0, len(jobDef.Tasks))
	for _, tk := range jobDef.Tasks {
		switch {
		case tk.Script != "":
			tasks = append(tasks, &gocdnextv1.TaskSpec{
				Kind: &gocdnextv1.TaskSpec_Script{Script: tk.Script},
			})
		case tk.Plugin != nil:
			tasks = append(tasks, &gocdnextv1.TaskSpec{
				Kind: &gocdnextv1.TaskSpec_Plugin{Plugin: &gocdnextv1.PluginSpec{
					Image:    tk.Plugin.Image,
					Settings: tk.Plugin.Settings,
				}},
			})
		}
	}

	env := map[string]string{}
	// Profile env first — operator-level defaults that pipeline/job
	// vars can override below. Profile secrets are pre-decrypted by
	// the caller; they share the same map with profile plain env so
	// override precedence is uniform (later writes win).
	for k, v := range profileEnv {
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
	masks := make([]string, 0, len(secrets)+len(profileMasks))
	// Profile secret VALUES land in masks so the runner redacts them
	// from logs. Done before the job-secrets loop so the order is
	// "profile masks then job masks" — order doesn't matter for
	// correctness (the runner does substring replace) but it makes
	// test fixtures stable.
	for _, v := range profileMasks {
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

	checkouts := materialCheckouts(materials, revs)

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
		LogMasks:              masks,
		ArtifactPaths:         append([]string(nil), jobDef.ArtifactPaths...),
		OptionalArtifactPaths: append([]string(nil), jobDef.OptionalArtifactPaths...),
		ArtifactDownloads:     downloads,
		Docker:                jobDef.Docker,
		Services:              services,
		Caches:                caches,
		TestReports:           append([]string(nil), jobDef.TestReports...),
		Resources:             resourceRequirements(jobDef.Resources),
		Profile:               jobDef.Profile,
	}, nil
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

func materialCheckouts(materials []store.Material, revs map[string]revisionSnapshot) []*gocdnextv1.MaterialCheckout {
	out := make([]*gocdnextv1.MaterialCheckout, 0, len(materials))
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
		out = append(out, &gocdnextv1.MaterialCheckout{
			MaterialId: m.ID.String(),
			Url:        cfg.URL,
			Revision:   rev.Revision,
			Branch:     firstNonEmpty(rev.Branch, cfg.Branch),
			TargetDir:  targetDirFor(m.ID),
			SecretRef:  cfg.SecretRef,
		})
	}
	return out
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
