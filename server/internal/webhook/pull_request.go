package webhook

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"

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
//   revision    = PR head SHA
//   branch      = PR head ref (so git checkout of the run pulls the
//                 PR code, not main)
//   cause       = "pull_request"
//   cause_detail = { pr_number, pr_title, pr_author, pr_url,
//                    pr_head_ref, pr_head_sha, pr_base_ref,
//                    pr_action }
//
// Only opened / synchronize / reopened trigger runs. Close/merge are
// ack'd with 204 — the subsequent push to base handles itself.
func (h *Handler) handlePullRequest(w http.ResponseWriter, r *http.Request, body []byte, delivery string) {
	ev, err := github.ParsePullRequestEvent(body)
	if err != nil {
		h.log.Warn("github webhook: PR parse failed", "delivery", delivery, "err", err)
		http.Error(w, "invalid pull_request payload: "+err.Error(), http.StatusBadRequest)
		return
	}

	if !ev.IsTriggerableAction() {
		h.log.Info("github webhook: PR action ignored",
			"delivery", delivery, "action", ev.Action, "number", ev.Number)
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Match by (url, base_ref). Fingerprint is the same normalisation
	// the material uses on its own side, so this finds the material a
	// user declared with `branch: main on: [push, pull_request]`.
	fp := store.FingerprintFor(ev.Repository.CloneURL, ev.BaseRef)
	material, err := h.store.FindMaterialByFingerprint(r.Context(), fp)
	if errors.Is(err, store.ErrMaterialNotFound) {
		h.log.Info("github webhook: no material for PR base",
			"delivery", delivery, "repo", ev.Repository.FullName,
			"base_ref", ev.BaseRef, "fingerprint", fp)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err != nil {
		h.log.Error("github webhook: material lookup failed (PR)",
			"delivery", delivery, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Does this material opt into PR events? `on: [pull_request]`.
	// Events live inside the JSONB config (Git material); decode only
	// that subset.
	var gitCfg domain.GitMaterial
	if err := json.Unmarshal(material.Config, &gitCfg); err != nil {
		h.log.Warn("github webhook: decode material config (PR)",
			"delivery", delivery, "material_id", material.ID, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !slices.Contains(gitCfg.Events, "pull_request") {
		h.log.Info("github webhook: material does not listen for pull_request",
			"delivery", delivery, "material_id", material.ID,
			"events", gitCfg.Events)
		w.WriteHeader(http.StatusNoContent)
		return
	}

	mod := store.Modification{
		MaterialID:  material.ID,
		Revision:    ev.HeadSHA,
		Branch:      ev.HeadRef,
		Author:      ev.Author,
		Message:     ev.Title,
		Payload:     json.RawMessage(body),
		CommittedAt: ev.At,
	}
	res, err := h.store.InsertModification(r.Context(), mod)
	if err != nil {
		h.log.Error("github webhook: insert PR modification failed",
			"delivery", delivery, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	resp := map[string]any{
		"modification_id": res.ID,
		"created":         res.Created,
		"material_id":     material.ID.String(),
		"pr_number":       ev.Number,
	}

	if res.Created {
		causeDetail, _ := json.Marshal(map[string]any{
			"pr_number":   ev.Number,
			"pr_title":    ev.Title,
			"pr_author":   ev.Author,
			"pr_url":      ev.HTMLURL,
			"pr_head_ref": ev.HeadRef,
			"pr_head_sha": ev.HeadSHA,
			"pr_base_ref": ev.BaseRef,
			"pr_action":   ev.Action,
		})
		runRes, err := h.store.CreateRunFromModification(r.Context(), store.CreateRunFromModificationInput{
			PipelineID:     material.PipelineID,
			MaterialID:     material.ID,
			ModificationID: res.ID,
			Revision:       ev.HeadSHA,
			Branch:         ev.HeadRef,
			Provider:       "github",
			Delivery:       delivery,
			TriggeredBy:    "system:webhook",
			Cause:          "pull_request",
			CauseDetail:    causeDetail,
		})
		if err != nil {
			h.log.Error("github webhook: PR run create failed",
				"delivery", delivery, "modification_id", res.ID, "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		resp["run_id"] = runRes.RunID.String()
		resp["run_counter"] = runRes.Counter
		h.log.Info("github webhook: PR run queued",
			"delivery", delivery, "pipeline_id", material.PipelineID,
			"run_id", runRes.RunID, "counter", runRes.Counter,
			"pr_number", ev.Number, "head_sha", ev.HeadSHA, "head_ref", ev.HeadRef)
	} else {
		h.log.Info("github webhook: PR modification already present, no run queued",
			"delivery", delivery, "modification_id", res.ID, "pr_number", ev.Number)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		h.log.Warn("github webhook: encode response failed", "err", fmt.Sprint(err))
	}
}
