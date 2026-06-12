package webhook

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/domain"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/internal/webhook/github"
)

// handleTagPush reacts to GitHub push events whose ref is `refs/tags/*`.
// Matching rule: every git material whose URL canonicalises to the same
// form AND whose `events:` list contains "tag" fires a run. Tags don't
// carry a base branch so the URL+branch fingerprint used by branch
// pushes can't match — see store.FindMaterialsByCloneURL for the
// rationale.
//
// Run metadata:
//
//	revision    = tag SHA (ev.After)
//	branch      = the tag name (so the agent's git clone fetches a
//	              detached HEAD at the tag — pipelines that need to
//	              reference the tag get it via $CI_BRANCH today and
//	              $CI_TAG_NAME via scheduler/civars.go)
//	cause       = "tag"
//	cause_detail = { tag_name, tag_message, tag_sha, tagger }
//
// Where tag_message / tagger come from ev.HeadCommit (annotated tags
// don't surface the annotation directly in the push payload; for
// lightweight tags the head commit IS the tag target). Fields are
// best-effort — empty values are silently omitted so the substitution
// layer keeps `${CI_TAG_MESSAGE}` literal on a lightweight tag.
func (h *Handler) handleTagPush(w http.ResponseWriter, r *http.Request, body []byte, delivery string, rec *deliveryRec, ev github.PushEvent) {
	// Best-effort [skip ci]: lightweight tags carry the target
	// commit (and its message) in head_commit; annotated tags arrive
	// with head_commit empty, so there is no message to inspect and
	// the tag fires normally — same caveat GitHub Actions has.
	if ev.HeadCommit != nil {
		if marker, ok := skipCIMarker(ev.HeadCommit.Message); ok {
			h.respondSkipCI(w, rec, "github", delivery, ev.Ref, marker)
			return
		}
	}

	allMaterials, err := h.store.FindMaterialsByCloneURL(r.Context(), ev.Repository.CloneURL)
	if err != nil {
		rec.status = store.WebhookStatusError
		rec.errText = "material lookup (tag): " + err.Error()
		h.log.Error("github webhook: material lookup failed (tag)",
			"delivery", delivery, "tag", ev.Tag, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if len(allMaterials) == 0 {
		rec.status = store.WebhookStatusIgnored
		h.log.Info("github webhook: no material for tag-push URL",
			"delivery", delivery, "repo", ev.Repository.FullName, "tag", ev.Tag)
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Per-material event filter: only materials that opted in to
	// `tag` fire. Push-only materials are silently skipped — keeps
	// the URL-only lookup from accidentally double-triggering CI
	// pipelines on every tag.
	materials := make([]store.Material, 0, len(allMaterials))
	for _, m := range allMaterials {
		var cfg domain.GitMaterial
		if err := json.Unmarshal(m.Config, &cfg); err != nil {
			h.log.Warn("github webhook: decode material config (tag)",
				"delivery", delivery, "material_id", m.ID, "err", err)
			continue
		}
		if !slices.Contains(cfg.Events, "tag") {
			continue
		}
		materials = append(materials, m)
	}
	if len(materials) == 0 {
		rec.status = store.WebhookStatusIgnored
		h.log.Info("github webhook: no tag-listening material for this repo",
			"delivery", delivery, "tag", ev.Tag,
			"material_candidates", len(allMaterials))
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// HeadCommit is best-effort: annotated tags arrive without it,
	// while lightweight tags + branch-fast-forward tags include it.
	// All three downstream consumers (Author, Message, CommittedAt)
	// degrade gracefully on empty strings / zero time.
	var (
		tagger      string
		tagMessage  string
		committedAt time.Time
	)
	if ev.HeadCommit != nil {
		tagger = ev.HeadCommit.Author.Name
		tagMessage = ev.HeadCommit.Message
		committedAt = ev.HeadCommit.Timestamp
	}
	causeDetail, _ := json.Marshal(map[string]any{
		"tag_name":    ev.Tag,
		"tag_message": tagMessage,
		"tag_sha":     ev.After,
		"tagger":      tagger,
	})
	outcomes := fanOutMaterials(r.Context(), h.log, h.store, fanOutInput{
		Materials:   materials,
		Revision:    ev.After,
		Branch:      ev.Tag, // agent checks out the tag as a detached HEAD
		Author:      tagger,
		Message:     tagMessage,
		Payload:     json.RawMessage(body),
		CommittedAt: committedAt,
		Provider:    "github",
		Delivery:    delivery,
		TriggeredBy: "system:webhook",
		Cause:       string(domain.CauseTag),
		CauseDetail: causeDetail,
	})
	rec.materialID = firstCreatedRunMaterialID(outcomes)

	runs := runsPayload(outcomes)
	allErrored := len(outcomes) > 0
	for _, oc := range outcomes {
		if oc.Err == nil {
			allErrored = false
			if oc.RunID != uuid.Nil {
				h.log.Info("github webhook: tag run queued",
					"delivery", delivery, "pipeline_id", oc.PipelineID,
					"run_id", oc.RunID, "counter", oc.RunCounter,
					"tag", ev.Tag, "sha", ev.After)
				h.reporter.ReportRunCreated(r.Context(), oc.RunID)
			}
		}
	}
	if allErrored {
		rec.status = store.WebhookStatusError
		rec.errText = "tag fan-out: every pipeline errored"
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	rec.status = store.WebhookStatusAccepted
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	resp := map[string]any{
		"runs":      runs,
		"materials": len(materials),
		"tag":       ev.Tag,
	}
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		h.log.Warn("github webhook: encode response failed", "err", fmt.Sprint(err))
	}
}
