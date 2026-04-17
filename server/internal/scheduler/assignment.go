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
// snapshot + the dispatchable job row + the pipeline's material rows. All
// lookups are in-memory — the caller is expected to have fetched those once.
func BuildAssignment(run store.RunForDispatch, job store.DispatchableJob, materials []store.Material) (*gocdnextv1.JobAssignment, error) {
	var def domain.Pipeline
	if err := json.Unmarshal(run.Definition, &def); err != nil {
		return nil, fmt.Errorf("scheduler: decode pipeline: %w", err)
	}

	jobDef, ok := findJob(def.Jobs, job.Name)
	if !ok {
		return nil, fmt.Errorf("scheduler: job %q not in pipeline definition", job.Name)
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

	checkouts := materialCheckouts(materials, revs)

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
	}, nil
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
