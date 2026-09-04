package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/google/uuid"

	ghscm "github.com/gocdnext/gocdnext/server/internal/scm/github"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/internal/webhook/github"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

const refPrefixHeads = "refs/heads/"

var (
	errMergeGroupInstallationMismatch = errors.New("github app installation mismatch")
	errMergeGroupRepositoryInvalid    = errors.New("invalid merge_group repository")
)

// handleMergeGroup processes GitHub App-delivered merge_group events. This is
// not a queue implementation: GitHub owns ordering/merging; gocdnext only runs
// the same PR-required pipelines against the merge-group SHA GitHub provides.
func (h *Handler) handleMergeGroup(w http.ResponseWriter, r *http.Request, body []byte, delivery string, rec *deliveryRec, auth *appWebhookAuth) {
	ev, err := github.ParseMergeGroupEvent(body)
	if err != nil {
		rec.status = store.WebhookStatusError
		rec.errText = "parse merge_group: " + err.Error()
		h.log.Warn("github app webhook: merge_group parse failed", "delivery", delivery, "err", err)
		http.Error(w, "invalid merge_group payload: "+err.Error(), http.StatusBadRequest)
		return
	}
	if auth == nil {
		rec.status = store.WebhookStatusRejected
		rec.errText = "github app webhook auth missing"
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}
	if auth.app == nil {
		rec.status = store.WebhookStatusError
		if auth.appErr != nil {
			rec.errText = "github app client unavailable: " + auth.appErr.Error()
		} else {
			rec.errText = "github app client unavailable"
		}
		h.log.Error("github app webhook: merge_group cannot build authenticated app client",
			"delivery", delivery, "app_id", auth.integration.AppID, "err", auth.appErr)
		http.Error(w, "github app client unavailable", http.StatusServiceUnavailable)
		return
	}
	if err := verifyMergeGroupAppInstallation(r.Context(), auth.app, ev); err != nil {
		switch {
		case errors.Is(err, errMergeGroupRepositoryInvalid):
			rec.status = store.WebhookStatusError
			rec.errText = err.Error()
			http.Error(w, "invalid merge_group repository", http.StatusBadRequest)
		case errors.Is(err, ghscm.ErrNoInstallation), errors.Is(err, errMergeGroupInstallationMismatch):
			rec.status = store.WebhookStatusRejected
			rec.errText = err.Error()
			h.log.Warn("github app webhook: merge_group app/repo binding rejected",
				"delivery", delivery, "app_id", auth.integration.AppID,
				"repo", ev.Repository.FullName, "installation_id", ev.InstallationID, "err", err)
			http.Error(w, "github app installation mismatch", http.StatusUnauthorized)
		default:
			rec.status = store.WebhookStatusError
			rec.errText = "merge_group installation lookup: " + err.Error()
			h.log.Error("github app webhook: merge_group installation lookup failed",
				"delivery", delivery, "app_id", auth.integration.AppID,
				"repo", ev.Repository.FullName, "err", err)
			http.Error(w, "github installation lookup failed", http.StatusServiceUnavailable)
		}
		return
	}

	switch ev.Action {
	case "checks_requested":
		h.dispatchMergeGroup(w, r, body, delivery, rec, ev, auth)
	case "destroyed":
		h.cancelMergeGroup(w, r, delivery, rec, ev)
	default:
		rec.status = store.WebhookStatusIgnored
		h.log.Info("github app webhook: merge_group action ignored",
			"delivery", delivery, "action", ev.Action)
		w.WriteHeader(http.StatusNoContent)
	}
}

func verifyMergeGroupAppInstallation(ctx context.Context, app *ghscm.AppClient, ev github.MergeGroupEvent) error {
	if ev.InstallationID <= 0 {
		return fmt.Errorf("%w: missing installation id", errMergeGroupInstallationMismatch)
	}
	owner, repo, err := ghscm.ParseRepoURL(ev.Repository.CloneURL)
	if err != nil {
		return fmt.Errorf("%w: %v", errMergeGroupRepositoryInvalid, err)
	}
	got, err := app.InstallationID(ctx, owner, repo)
	if err != nil {
		return err
	}
	if got != ev.InstallationID {
		return fmt.Errorf("%w: payload=%d resolved=%d", errMergeGroupInstallationMismatch, ev.InstallationID, got)
	}
	return nil
}

func (h *Handler) dispatchMergeGroup(w http.ResponseWriter, r *http.Request, body []byte, delivery string, rec *deliveryRec, ev github.MergeGroupEvent, auth *appWebhookAuth) {
	baseRef := stripHeadsPrefix(ev.BaseRef)
	headRef := stripHeadsPrefix(ev.HeadRef)
	fp := store.FingerprintFor(ev.Repository.CloneURL, baseRef)
	destroyed, err := h.store.MergeGroupDestroyed(r.Context(), fp, ev.HeadSHA)
	if err != nil {
		rec.status = store.WebhookStatusError
		rec.errText = "merge_group destroyed lookup: " + err.Error()
		h.log.Error("github app webhook: merge_group destroyed lookup failed",
			"delivery", delivery, "fingerprint", fp, "head_sha", ev.HeadSHA, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if destroyed {
		rec.status = store.WebhookStatusIgnored
		h.log.Info("github app webhook: stale merge_group checks_requested ignored after destroyed",
			"delivery", delivery, "fingerprint", fp, "head_sha", ev.HeadSHA)
		w.WriteHeader(http.StatusNoContent)
		return
	}

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
	seenPipelines := map[uuid.UUID]bool{}
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
		if seenPipelines[m.PipelineID] {
			h.log.Info("github app webhook: merge_group duplicate material for pipeline ignored",
				"delivery", delivery, "pipeline_id", m.PipelineID, "material_id", m.ID)
			continue
		}
		seenPipelines[m.PipelineID] = true
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
		"mg_head_sha":    ev.HeadSHA,
		"mg_head_ref":    headRef,
		"mg_base_sha":    ev.BaseSHA,
		"mg_base_ref":    baseRef,
		"mg_action":      ev.Action,
		"mg_fingerprint": fp,
	}
	causeDetail, _ := json.Marshal(detail)
	outcomes := h.fanOutMergeGroupMaterials(r.Context(), fp, fanOutInput{
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
	destroyedDuringFanout := false
	for _, oc := range outcomes {
		if oc.Err != nil {
			if errors.Is(oc.Err, store.ErrMergeGroupDestroyed) {
				destroyedDuringFanout = true
			}
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
			if err := h.reporter.CreateCheckWithAppInstallation(r.Context(), oc.RunID, auth.app, ev.InstallationID); err != nil {
				errorCount++
				h.log.Error("github app webhook: merge_group required check create failed",
					"delivery", delivery, "pipeline_id", oc.PipelineID,
					"run_id", oc.RunID, "err", err)
			}
		}
	}
	if destroyedDuringFanout {
		if _, err := h.store.CancelMergeGroupRuns(r.Context(), fp, ev.HeadSHA, "checks_requested raced destroyed"); err != nil {
			rec.status = store.WebhookStatusError
			rec.errText = "merge_group cancel after destroyed race: " + err.Error()
			h.log.Error("github app webhook: merge_group destroyed race cleanup failed",
				"delivery", delivery, "fingerprint", fp, "head_sha", ev.HeadSHA, "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		rec.status = store.WebhookStatusIgnored
		h.log.Info("github app webhook: merge_group checks_requested lost race with destroyed",
			"delivery", delivery, "fingerprint", fp, "head_sha", ev.HeadSHA)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if errorCount > 0 {
		rec.status = store.WebhookStatusError
		rec.errText = fmt.Sprintf("merge_group fan-out/report: %d/%d pipelines errored", errorCount, len(outcomes))
		h.log.Error("github app webhook: merge_group fan-out/report failed partially",
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
	baseRef := stripHeadsPrefix(ev.BaseRef)
	fp := store.FingerprintFor(ev.Repository.CloneURL, baseRef)
	runIDs, err := h.store.CancelMergeGroupRuns(r.Context(), fp, ev.HeadSHA, ev.Reason)
	if err != nil {
		rec.status = store.WebhookStatusError
		rec.errText = "merge_group cancel: " + err.Error()
		h.log.Error("github app webhook: merge_group cancel failed",
			"delivery", delivery, "fingerprint", fp, "head_sha", ev.HeadSHA, "reason", ev.Reason, "err", err)
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
		"fingerprint":   fp,
		"reason":        ev.Reason,
	})
}

func (h *Handler) fanOutMergeGroupMaterials(ctx context.Context, fingerprint string, in fanOutInput) []fanOutOutcome {
	out := make([]fanOutOutcome, 0, len(in.Materials))
	for _, m := range in.Materials {
		oc := fanOutOutcome{PipelineID: m.PipelineID, MaterialID: m.ID}
		res, err := h.store.CreateOrFindMergeGroupRun(ctx, store.MergeGroupRunInput{
			Fingerprint: fingerprint,
			PipelineID:  m.PipelineID,
			MaterialID:  m.ID,
			Revision:    in.Revision,
			Branch:      in.Branch,
			Author:      in.Author,
			Message:     in.Message,
			Payload:     in.Payload,
			CommittedAt: in.CommittedAt,
			Provider:    in.Provider,
			Delivery:    in.Delivery,
			TriggeredBy: in.TriggeredBy,
			CauseDetail: in.CauseDetail,
		})
		if err != nil {
			oc.Err = err
			h.log.Warn("github app webhook: merge_group create/find run failed",
				"pipeline_id", m.PipelineID, "material_id", m.ID,
				"delivery", in.Delivery, "err", err)
			out = append(out, oc)
			continue
		}
		oc.ModificationID = res.ModificationID
		oc.ModCreated = res.ModificationCreated
		oc.RunID = res.Run.RunID
		oc.RunCounter = res.Run.Counter
		out = append(out, oc)
	}
	return out
}

func stripHeadsPrefix(ref string) string {
	return strings.TrimPrefix(ref, refPrefixHeads)
}
