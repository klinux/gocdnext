package webhook

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/internal/webhook/github"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

const refPrefixHeads = "refs/heads/"

// handleMergeGroup processes GitHub App-delivered merge_group events. This is
// not a queue implementation: GitHub owns ordering/merging; gocdnext only runs
// the same PR-required pipelines against the merge-group SHA GitHub provides.
func (h *Handler) handleMergeGroup(w http.ResponseWriter, r *http.Request, body []byte, delivery string, rec *deliveryRec) {
	ev, err := github.ParseMergeGroupEvent(body)
	if err != nil {
		rec.status = store.WebhookStatusError
		rec.errText = "parse merge_group: " + err.Error()
		h.log.Warn("github app webhook: merge_group parse failed", "delivery", delivery, "err", err)
		http.Error(w, "invalid merge_group payload: "+err.Error(), http.StatusBadRequest)
		return
	}

	switch ev.Action {
	case "checks_requested":
		h.dispatchMergeGroup(w, r, body, delivery, rec, ev)
	case "destroyed":
		h.cancelMergeGroup(w, r, delivery, rec, ev)
	default:
		rec.status = store.WebhookStatusIgnored
		h.log.Info("github app webhook: merge_group action ignored",
			"delivery", delivery, "action", ev.Action)
		w.WriteHeader(http.StatusNoContent)
	}
}

func (h *Handler) dispatchMergeGroup(w http.ResponseWriter, r *http.Request, body []byte, delivery string, rec *deliveryRec, ev github.MergeGroupEvent) {
	baseRef := stripHeadsPrefix(ev.BaseRef)
	headRef := stripHeadsPrefix(ev.HeadRef)
	fp := store.FingerprintFor(ev.Repository.CloneURL, baseRef)

	allMaterials, err := h.store.FindMaterialsByFingerprint(r.Context(), fp)
	if err != nil {
		rec.status = store.WebhookStatusError
		rec.errText = "material lookup (merge_group): " + err.Error()
		h.log.Error("github app webhook: merge_group material lookup failed",
			"delivery", delivery, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if len(allMaterials) == 0 {
		rec.status = store.WebhookStatusIgnored
		h.log.Info("github app webhook: no material for merge_group base",
			"delivery", delivery,
			"repo", ev.Repository.FullName,
			"base_ref", ev.BaseRef,
			"fingerprint", fp)
		w.WriteHeader(http.StatusNoContent)
		return
	}

	materials := make([]store.Material, 0, len(allMaterials))
	pathFilteredWouldApply := 0
	for _, m := range allMaterials {
		var cfg domain.GitMaterial
		if err := json.Unmarshal(m.Config, &cfg); err != nil {
			h.log.Warn("github app webhook: decode material config (merge_group)",
				"delivery", delivery, "material_id", m.ID, "err", err)
			continue
		}
		if !slices.Contains(cfg.Events, "merge_group") && !slices.Contains(cfg.Events, "pull_request") {
			continue
		}
		if len(cfg.Paths) > 0 {
			pathFilteredWouldApply++
		}
		materials = append(materials, m)
	}
	if len(materials) == 0 {
		rec.status = store.WebhookStatusIgnored
		h.log.Info("github app webhook: no merge_group-listening material for this base ref",
			"delivery", delivery, "material_candidates", len(allMaterials))
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if pathFilteredWouldApply > 0 {
		h.log.Info("github app webhook: merge_group ignores when.paths to ensure required check reports",
			"delivery", delivery, "materials_with_paths", pathFilteredWouldApply)
	}
	if h.reporter == nil {
		rec.status = store.WebhookStatusError
		rec.errText = "merge_group check reporter disabled"
		h.log.Error("github app webhook: merge_group cannot create required checks because reporter is disabled",
			"delivery", delivery, "materials", len(materials), "head_sha", ev.HeadSHA)
		http.Error(w, "github check reporter disabled", http.StatusServiceUnavailable)
		return
	}

	detail := map[string]any{
		"mg_head_sha": ev.HeadSHA,
		"mg_head_ref": headRef,
		"mg_base_sha": ev.BaseSHA,
		"mg_base_ref": baseRef,
		"mg_action":   ev.Action,
	}
	causeDetail, _ := json.Marshal(detail)
	outcomes := fanOutMaterials(r.Context(), h.log, h.store, fanOutInput{
		Materials:   materials,
		Revision:    ev.HeadSHA,
		Branch:      headRef,
		Author:      "github-merge-queue",
		Message:     ev.Commit.Message,
		Payload:     json.RawMessage(body),
		CommittedAt: ev.Commit.Timestamp,
		Provider:    "github",
		Delivery:    delivery,
		TriggeredBy: "system:webhook",
		Cause:       string(domain.CauseMergeGroup),
		CauseDetail: causeDetail,
	})
	rec.materialID = firstCreatedRunMaterialID(outcomes)

	runs := runsPayload(outcomes)
	errorCount := 0
	for _, oc := range outcomes {
		if oc.Err != nil {
			errorCount++
			h.log.Warn("github app webhook: merge_group pipeline fan-out failed",
				"delivery", delivery, "pipeline_id", oc.PipelineID,
				"material_id", oc.MaterialID, "err", oc.Err)
			continue
		}
		if oc.RunID != uuid.Nil {
			h.log.Info("github app webhook: merge_group run queued",
				"delivery", delivery, "pipeline_id", oc.PipelineID,
				"run_id", oc.RunID, "counter", oc.RunCounter,
				"head_sha", ev.HeadSHA, "head_ref", headRef, "base_ref", baseRef)
			if h.reporter != nil {
				h.reporter.ReportRunCreated(r.Context(), oc.RunID)
			}
		}
	}
	if errorCount > 0 {
		rec.status = store.WebhookStatusError
		rec.errText = fmt.Sprintf("merge_group fan-out: %d/%d pipelines errored", errorCount, len(outcomes))
		h.log.Error("github app webhook: merge_group fan-out failed partially",
			"delivery", delivery, "materials", len(materials), "errors", errorCount)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	rec.status = store.WebhookStatusAccepted
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	resp := map[string]any{
		"runs":          runs,
		"materials":     len(materials),
		"head_sha":      ev.HeadSHA,
		"head_ref":      headRef,
		"base_ref":      baseRef,
		"paths_ignored": pathFilteredWouldApply,
	}
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		h.log.Warn("github app webhook: merge_group response encode failed", "err", fmt.Sprint(err))
	}
}

func (h *Handler) cancelMergeGroup(w http.ResponseWriter, r *http.Request, delivery string, rec *deliveryRec, ev github.MergeGroupEvent) {
	runIDs, err := h.store.CancelMergeGroupRuns(r.Context(), ev.HeadSHA, ev.Reason)
	if err != nil {
		rec.status = store.WebhookStatusError
		rec.errText = "merge_group cancel: " + err.Error()
		h.log.Error("github app webhook: merge_group cancel failed",
			"delivery", delivery, "head_sha", ev.HeadSHA, "reason", ev.Reason, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	runs := make([]string, 0, len(runIDs))
	for _, id := range runIDs {
		runs = append(runs, id.String())
	}
	rec.status = store.WebhookStatusAccepted
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"canceled_runs": runs,
		"head_sha":      ev.HeadSHA,
		"reason":        ev.Reason,
	})
}

func stripHeadsPrefix(ref string) string {
	return strings.TrimPrefix(ref, refPrefixHeads)
}
