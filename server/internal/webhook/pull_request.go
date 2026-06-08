package webhook

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/domain"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/internal/webhook/github"
)

// handlePullRequest reacts to GitHub's pull_request webhook. Matching
// rule: the material that would fire on a push to the PR's BASE ref
// also fires on the PR (when its events list includes "pull_request"
// in the YAML). Rationale — pipelines are typically defined as
// `branch: main`, and people intend "lint these PRs that target main"
// without having to mirror the material to every feature branch.
//
// Run metadata:
//
//	revision    = PR head SHA
//	branch      = PR head ref (so git checkout of the run pulls the
//	              PR code, not main)
//	cause       = "pull_request"
//	cause_detail = { pr_number, pr_title, pr_author, pr_url,
//	                 pr_head_ref, pr_head_sha, pr_base_ref,
//	                 pr_action }
//
// Only opened / synchronize / reopened trigger runs. Close/merge are
// ack'd with 204 — the subsequent push to base handles itself.
func (h *Handler) handlePullRequest(w http.ResponseWriter, r *http.Request, body []byte, delivery string, rec *deliveryRec) {
	ev, err := github.ParsePullRequestEvent(body)
	if err != nil {
		rec.status = store.WebhookStatusError
		rec.errText = "parse pull_request: " + err.Error()
		h.log.Warn("github webhook: PR parse failed", "delivery", delivery, "err", err)
		http.Error(w, "invalid pull_request payload: "+err.Error(), http.StatusBadRequest)
		return
	}

	if !ev.IsTriggerableAction() {
		rec.status = store.WebhookStatusIgnored
		h.log.Info("github webhook: PR action ignored",
			"delivery", delivery, "action", ev.Action, "number", ev.Number)
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Match by (url, base_ref). Fingerprint is the same normalisation
	// the material uses on its own side, so this finds the material a
	// user declared with `branch: main on: [push, pull_request]`.
	// N pipelines can share the same fingerprint — fan-out one run
	// per pipeline that ALSO opts into pull_request via its events
	// list.
	fp := store.FingerprintFor(ev.Repository.CloneURL, ev.BaseRef)
	allMaterials, err := h.store.FindMaterialsByFingerprint(r.Context(), fp)
	if err != nil {
		rec.status = store.WebhookStatusError
		rec.errText = "material lookup (PR): " + err.Error()
		h.log.Error("github webhook: material lookup failed (PR)",
			"delivery", delivery, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if len(allMaterials) == 0 {
		rec.status = store.WebhookStatusIgnored
		h.log.Info("github webhook: no material for PR base",
			"delivery", delivery, "repo", ev.Repository.FullName,
			"base_ref", ev.BaseRef, "fingerprint", fp)
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Per-material event filter: only the materials that declare
	// `pull_request` in their events list are eligible. A pipeline
	// that hasn't opted in (push-only) is silently skipped — the
	// other pipelines still fire.
	materials := make([]store.Material, 0, len(allMaterials))
	for _, m := range allMaterials {
		var cfg domain.GitMaterial
		if err := json.Unmarshal(m.Config, &cfg); err != nil {
			h.log.Warn("github webhook: decode material config (PR)",
				"delivery", delivery, "material_id", m.ID, "err", err)
			continue
		}
		if !slices.Contains(cfg.Events, "pull_request") {
			continue
		}
		materials = append(materials, m)
	}
	if len(materials) == 0 {
		rec.status = store.WebhookStatusIgnored
		h.log.Info("github webhook: no PR-listening material for this base ref",
			"delivery", delivery, "material_candidates", len(allMaterials))
		w.WriteHeader(http.StatusNoContent)
		return
	}

	detail := map[string]any{
		"pr_number":   ev.Number,
		"pr_title":    ev.Title,
		"pr_author":   ev.Author,
		"pr_url":      ev.HTMLURL,
		"pr_head_ref": ev.HeadRef,
		"pr_head_sha": ev.HeadSHA,
		"pr_base_ref": ev.BaseRef,
		"pr_action":   ev.Action,
	}
	// pr_labels: only stamped when the PR actually has labels, so
	// runs from label-less PRs don't bloat cause_detail with a
	// `"pr_labels": []` field. Downstream consumers
	// (civars CI_PULL_REQUEST_LABELS + quorum_by_label resolver)
	// treat missing-key and empty-list the same way.
	if len(ev.Labels) > 0 {
		detail["pr_labels"] = ev.Labels
	}
	causeDetail, _ := json.Marshal(detail)
	outcomes := fanOutMaterials(r.Context(), h.log, h.store, fanOutInput{
		Materials:   materials,
		Revision:    ev.HeadSHA,
		Branch:      ev.HeadRef,
		Author:      ev.Author,
		Message:     ev.Title,
		Payload:     json.RawMessage(body),
		CommittedAt: ev.At,
		Provider:    "github",
		Delivery:    delivery,
		TriggeredBy: "system:webhook",
		Cause:       string(domain.CausePullRequest),
		CauseDetail: causeDetail,
	})
	rec.materialID = firstCreatedRunMaterialID(outcomes)

	runs := runsPayload(outcomes)
	allErrored := len(outcomes) > 0
	for _, oc := range outcomes {
		if oc.Err == nil {
			allErrored = false
			if oc.RunID != uuid.Nil {
				h.log.Info("github webhook: PR run queued",
					"delivery", delivery, "pipeline_id", oc.PipelineID,
					"run_id", oc.RunID, "counter", oc.RunCounter,
					"pr_number", ev.Number, "head_sha", ev.HeadSHA, "head_ref", ev.HeadRef)
				h.reporter.ReportRunCreated(r.Context(), oc.RunID)
			}
		}
	}
	if allErrored {
		rec.status = store.WebhookStatusError
		rec.errText = "PR fan-out: every pipeline errored"
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	rec.status = store.WebhookStatusAccepted
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	resp := map[string]any{
		"runs":      runs,
		"materials": len(materials),
		"pr_number": ev.Number,
	}
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		h.log.Warn("github webhook: encode response failed", "err", fmt.Sprint(err))
	}
}
