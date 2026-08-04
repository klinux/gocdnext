package webhook

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/store"
)

// applyPRHeadConfig handles the PR-head-config path for a matched PR and returns
// the materials the caller must still fan out on the BASE flow. It is a strict
// no-op (returns `materials`, ZERO fetch) for anything but a GitHub same-repo PR
// on a project that opted in — the base flow stays byte-for-byte intact.
//
// Partition + partiality:
//   - system_managed pipelines ALWAYS run on the base flow, independent of the
//     head — a head that deletes/breaks `.gocdnext/` can't suppress them;
//   - repo pipelines run from the head ONLY when the clone URL binds to exactly
//     one scm_source and that project's toggle is on;
//   - an ambiguous/missing binding or a resolution error blocks JUST the repo
//     pipelines (no silent base fallback) while system_managed still runs;
//   - toggle OFF → the repo pipelines run on the base flow (not blocked).
func (h *Handler) applyPRHeadConfig(
	ctx context.Context, ev pullRequestEvent, materials []store.Material,
	causeDetail json.RawMessage, delivery string, body []byte,
) []store.Material {
	// Only the GitHub same-repo path — fork / GitLab / Bitbucket never fetch.
	if ev.Provider != "github" || !ev.SameRepo {
		return materials
	}

	ids := make([]uuid.UUID, len(materials))
	for i, m := range materials {
		ids[i] = m.ID
	}
	identities, err := h.store.ListMaterialPipelineIdentities(ctx, ids)
	if err != nil {
		h.log.Error("pr-head: material identity lookup failed — base flow", "err", err)
		return materials
	}
	byMaterial := make(map[uuid.UUID]store.MaterialPipelineIdentity, len(identities))
	for _, id := range identities {
		byMaterial[id.MaterialID] = id
	}

	// Partition: repo (non-system-managed) candidates vs base (system_managed, or
	// a material with no identity — never head-driven).
	var repo, base []store.Material
	for _, m := range materials {
		if id, ok := byMaterial[m.ID]; ok && !id.SystemManaged {
			repo = append(repo, m)
		} else {
			base = append(base, m)
		}
	}
	if len(repo) == 0 {
		// Nothing to resolve from the head → base flow, and NO fetch: a PR of only
		// system_managed work never touches the contributor branch.
		return materials
	}

	bindings, err := h.store.FindPRHeadBindingsByURL(ctx, ev.CloneURL)
	if err != nil || len(bindings) != 1 {
		// Ambiguous (>1) or missing (0) binding → fail closed: repo pipelines do
		// NOT run (no silent base fallback); system_managed still runs base.
		h.log.Warn("pr-head: ambiguous or missing scm binding — repo pipelines blocked",
			"repo", ev.RepoLabel, "bindings", len(bindings), "err", err)
		return base
	}
	b := bindings[0]
	if !b.Trust {
		// Toggle off → the repo pipelines run on the base flow, unchanged.
		return materials
	}

	// Build the base-authorized set from the repo materials in THIS project; a
	// repo material in another project (shared fingerprint) falls back to base.
	var authorized []authorizedPipeline
	for _, m := range repo {
		id := byMaterial[m.ID]
		if id.ProjectID != b.Source.ProjectID {
			base = append(base, m)
			continue
		}
		authorized = append(authorized, authorizedPipeline{
			Name: id.PipelineName, PipelineID: id.PipelineID, MaterialID: id.MaterialID,
		})
	}

	plan, err := resolvePRHeadPlan(ctx, h.fetcher, b.Source, b.ConfigPath, ev.HeadSHA, authorized)
	if err != nil {
		// Resolution error blocks the repo pipelines of this plan; system_managed
		// still runs base. No partial repo runs (the plan is all-or-nothing).
		h.log.Warn("pr-head: config resolution failed — repo pipelines blocked",
			"repo", ev.RepoLabel, "err", err)
		return base
	}
	// One run per plan entry — per-material, preserving the base flow's per-
	// pipeline failure semantics for concurrent store admission errors.
	for _, e := range plan {
		h.createPRHeadRun(ctx, ev, b, e, causeDetail, delivery, body)
	}
	return base
}

func (h *Handler) createPRHeadRun(
	ctx context.Context, ev pullRequestEvent, b store.PRHeadBinding, e prHeadPlanEntry,
	causeDetail json.RawMessage, delivery string, body []byte,
) {
	run, created, err := h.store.CreatePRHeadRun(ctx, store.CreatePRHeadRunInput{
		MaterialID:  e.MaterialID,
		ProjectID:   b.Source.ProjectID,
		RawDef:      e.HeadDef,
		Revision:    ev.HeadSHA,
		Branch:      ev.HeadRef,
		Author:      ev.Author,
		Message:     ev.Title,
		Payload:     json.RawMessage(body),
		CommittedAt: ev.At,
		TriggeredBy: "system:webhook",
		Provider:    ev.Provider,
		Delivery:    delivery,
		CauseDetail: causeDetail,
	})
	if err != nil {
		h.log.Error("pr-head: create run failed",
			"pipeline_id", e.PipelineID, "material_id", e.MaterialID, "err", err)
		return
	}
	if created {
		h.log.Info("pr-head run queued",
			"pipeline_id", e.PipelineID, "run_id", run.RunID, "head_sha", ev.HeadSHA)
		if h.reporter != nil {
			h.reporter.ReportRunCreated(ctx, run.RunID)
		}
	}
}
