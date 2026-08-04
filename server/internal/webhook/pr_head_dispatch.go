package webhook

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/store"
)

// prHeadResolveTimeout bounds the TOTAL head-config resolution (a folder can need
// many sequential requests, each with its own HTTP timeout). Without it a slow
// contributor branch could delay the base fan-out — including the mandatory
// system_managed pipelines — for the whole request budget. Derived from the
// request context, so a client disconnect still cancels promptly.
const prHeadResolveTimeout = 30 * time.Second

// applyPRHeadConfig handles the PR-head-config path for a matched PR. It returns
// (base, outcomes, resolveErr):
//   - base: the materials the caller must still fan out on the BASE flow;
//   - outcomes: the head runs it created (or per-pipeline create errors), to be
//     folded into the delivery's runs/status alongside the base outcomes;
//   - resolveErr: a resolution-level error (fetch/parse/validate/envelope) that
//     blocked the repo pipelines — the caller maps it to 503/422 when nothing
//     else ran.
//
// It is a strict no-op (returns materials, nil, nil — ZERO fetch) for anything
// but a GitHub same-repo PR on a project that opted in. Partition + partiality:
//   - system_managed ALWAYS runs base, independent of the head;
//   - repo pipelines run from the head only with an unambiguous binding + toggle
//     on; an ambiguous/missing binding or a resolution error blocks JUST the repo
//     pipelines (no silent base fallback) while system_managed still runs;
//   - toggle off → repo pipelines run base.
func (h *Handler) applyPRHeadConfig(
	ctx context.Context, ev pullRequestEvent, materials []store.Material,
	causeDetail json.RawMessage, delivery string, body []byte,
) (base []store.Material, outcomes []fanOutOutcome, resolveErr error) {
	if ev.Provider != "github" || !ev.SameRepo {
		return materials, nil, nil
	}

	ids := make([]uuid.UUID, len(materials))
	for i, m := range materials {
		ids[i] = m.ID
	}
	identities, err := h.store.ListMaterialPipelineIdentities(ctx, ids)
	if err != nil {
		h.log.Error("pr-head: material identity lookup failed — base flow", "err", err)
		return materials, nil, nil
	}
	byMaterial := make(map[uuid.UUID]store.MaterialPipelineIdentity, len(identities))
	for _, id := range identities {
		byMaterial[id.MaterialID] = id
	}

	// Partition: repo (non-system-managed) candidates vs base (system_managed, or
	// a material with no identity — never head-driven).
	var repo []store.Material
	for _, m := range materials {
		if id, ok := byMaterial[m.ID]; ok && !id.SystemManaged {
			repo = append(repo, m)
		} else {
			base = append(base, m)
		}
	}
	if len(repo) == 0 {
		return materials, nil, nil // only system_managed → base, ZERO fetch
	}

	bindings, err := h.store.FindPRHeadBindingsByURL(ctx, ev.CloneURL)
	if err != nil || len(bindings) != 1 {
		// Ambiguous (>1) or missing (0) binding → fail closed: repo pipelines do
		// NOT run (no silent base fallback); system_managed still runs base. It's
		// an operator misconfiguration, not a client error, so no resolveErr.
		h.log.Warn("pr-head: ambiguous or missing scm binding — repo pipelines blocked",
			"repo", ev.RepoLabel, "bindings", len(bindings), "err", err)
		return base, nil, nil
	}
	b := bindings[0]
	if !b.Trust {
		return materials, nil, nil // toggle off → repo runs base, unchanged
	}

	// Base-authorized set: repo materials in THIS project; a repo material in
	// another project (shared fingerprint) falls back to base.
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

	resolveCtx, cancel := context.WithTimeout(ctx, prHeadResolveTimeout)
	defer cancel()
	started := time.Now()
	plan, err := resolvePRHeadPlan(resolveCtx, h.fetcher, h.pluginCatalog, b.Source, b.ConfigPath, ev.HeadSHA, authorized)
	// Duration + result of the head resolution (observability the plan calls for).
	h.log.Info("pr-head: config resolution",
		"repo", ev.RepoLabel, "pipelines", len(authorized),
		"duration_ms", time.Since(started).Milliseconds(), "ok", err == nil)
	if err != nil {
		// Resolution error blocks the repo pipelines of this plan (no partial repo
		// runs); system_managed still runs base. The caller maps err to 503/422.
		h.log.Warn("pr-head: config resolution failed — repo pipelines blocked",
			"repo", ev.RepoLabel, "err", err)
		return base, nil, err
	}
	// One outcome per plan entry — per-material, preserving per-pipeline failure
	// semantics (a store admission error on one pipeline doesn't sink the others).
	outcomes = make([]fanOutOutcome, 0, len(plan))
	for _, e := range plan {
		outcomes = append(outcomes, h.createPRHeadRun(ctx, ev, b, e, causeDetail, delivery, body))
	}
	return base, outcomes, nil
}

func (h *Handler) createPRHeadRun(
	ctx context.Context, ev pullRequestEvent, b store.PRHeadBinding, e prHeadPlanEntry,
	causeDetail json.RawMessage, delivery string, body []byte,
) fanOutOutcome {
	oc := fanOutOutcome{PipelineID: e.PipelineID, MaterialID: e.MaterialID}
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
		oc.Err = err
		return oc
	}
	if created {
		oc.RunID = run.RunID
		oc.RunCounter = run.Counter
		oc.ModCreated = true
	}
	return oc
}
