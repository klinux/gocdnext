package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/audit"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/internal/webhook/github"
)

// appDeliveryStaleAfter is how long a claimed-but-unfinished delivery may sit in
// 'processing' before a redelivery assumes the prior attempt crashed and
// re-claims it. Well above a rerun's few-second runtime.
const appDeliveryStaleAfter = 5 * time.Minute

// HandleGitHubApp receives webhooks delivered to the GitHub APP (not the
// per-repo webhook): `check_run`/`check_suite` `rerequested` (the PR "Re-run"
// button) and the setup `ping`. These are signed with the App's own webhook
// secret — NOT the per-repo scm_source secret HandleGitHub verifies against —
// so this is a separate route with its own secret resolution.
//
// Security posture mirrors HandleGitHub: MaxBytesReader before any parse, a
// mandatory X-GitHub-Delivery, and NOTHING in the payload is trusted until the
// HMAC over the App secret passes (the payload's app id only SELECTS the
// candidate secret).
func (h *Handler) HandleGitHubApp(w http.ResponseWriter, r *http.Request) {
	event := r.Header.Get("X-GitHub-Event")
	if event == "" {
		http.Error(w, "missing X-GitHub-Event header", http.StatusBadRequest)
		return
	}
	delivery := r.Header.Get("X-GitHub-Delivery")
	signature := r.Header.Get("X-Hub-Signature-256")

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}

	sc := &statusCapture{ResponseWriter: w}
	rec := &deliveryRec{
		provider: "github_app",
		event:    event,
		headers:  headersJSON(r.Header),
		payload:  json.RawMessage(body),
		writer:   sc,
	}
	defer h.recordDelivery(r.Context(), rec)
	w = sc

	if delivery == "" {
		rec.status = store.WebhookStatusRejected
		rec.errText = "missing X-GitHub-Delivery"
		http.Error(w, "missing X-GitHub-Delivery header", http.StatusBadRequest)
		return
	}

	// Resolve the candidate App webhook secret from the UNVERIFIED payload's app
	// id (or the sole configured App). Trusted for nothing until VerifySignature
	// passes below — same posture as the scm path's clone_url selection.
	secret, ok := h.resolveAppSecret(r.Context(), event, body, rec, w)
	if !ok {
		return // resolveAppSecret already set rec + wrote the response
	}
	if err := github.VerifySignature(secret, body, signature); err != nil {
		rec.status = store.WebhookStatusRejected
		rec.errText = "invalid signature: " + err.Error()
		h.log.Warn("github app webhook: signature rejected",
			"event", event, "delivery", delivery, "err", err)
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}

	switch event {
	case "ping":
		rec.status = store.WebhookStatusAccepted
		w.WriteHeader(http.StatusNoContent)
	case "check_run":
		h.handleCheckRerun(w, r, body, delivery, rec)
	default:
		// check_suite (deferred) + check_run created/completed → nothing to do.
		rec.status = store.WebhookStatusIgnored
		h.log.Info("github app webhook: ignored event", "event", event, "delivery", delivery)
		w.WriteHeader(http.StatusNoContent)
	}
}

// resolveAppSecret picks the App webhook secret to verify against, from the
// (unverified) payload's app id — by id when present, else the sole configured
// App integration (the ping fallback). Fails closed: writes a 401 + returns
// false when nothing resolves.
func (h *Handler) resolveAppSecret(ctx context.Context, event string, body []byte, rec *deliveryRec, w http.ResponseWriter) (string, bool) {
	var appID int64
	switch event {
	case "ping":
		if ev, perr := github.ParsePingEvent(body); perr == nil {
			appID = ev.HookAppID
		}
	default: // check_run / check_suite
		if ev, perr := github.ParseCheckRunEvent(body); perr == nil {
			appID = ev.AppID
		}
	}

	var secret string
	var err error
	if appID != 0 {
		secret, err = h.store.AppWebhookSecretByAppID(ctx, appID)
	}
	// Fall back to the sole configured App when the id is absent OR didn't
	// resolve (e.g. the integration row has no app_id). SoleAppWebhookSecret is
	// itself fail-closed (0 or many → error).
	if appID == 0 || err != nil {
		secret, err = h.store.SoleAppWebhookSecret(ctx)
	}
	if err != nil || secret == "" {
		rec.status = store.WebhookStatusRejected
		rec.errText = "app webhook secret not resolved"
		h.log.Warn("github app webhook: secret not resolved",
			"event", event, "app_id", appID, "err", err)
		http.Error(w, "unknown or unconfigured github app", http.StatusUnauthorized)
		return "", false
	}
	return secret, true
}

// handleCheckRerun processes a verified `check_run` event. Only `rerequested`
// acts; everything is cross-checked against the persisted run↔check identity,
// deduped by delivery id, and re-run through the shared store.RerunRun (whose
// terminal guard both this and the HTTP path rely on).
func (h *Handler) handleCheckRerun(w http.ResponseWriter, r *http.Request, body []byte, delivery string, rec *deliveryRec) {
	ev, err := github.ParseCheckRunEvent(body)
	if err != nil {
		rec.status = store.WebhookStatusError
		rec.errText = "parse check_run: " + err.Error()
		http.Error(w, "invalid check_run payload", http.StatusBadRequest)
		return
	}
	if ev.Action != "rerequested" {
		rec.status = store.WebhookStatusIgnored
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// external_id is the gocdnext run UUID we stamped on the check run.
	runID, err := uuid.Parse(ev.ExternalID)
	if err != nil {
		rec.status = store.WebhookStatusIgnored
		h.log.Info("github app re-run: unparseable external_id",
			"delivery", delivery, "external_id", ev.ExternalID)
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Cross-check ALL of the persisted identity before acting: check-run id
	// (nil = commit_status-only run, which has no re-run button), installation,
	// and repo (case-insensitive — GitHub treats owner/repo that way).
	link, err := h.store.GetGithubCheckRun(r.Context(), runID)
	if err != nil {
		rec.status = store.WebhookStatusIgnored
		h.log.Info("github app re-run: no check link for run",
			"delivery", delivery, "run_id", runID, "err", err)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if link.CheckRunID == nil || *link.CheckRunID != ev.CheckRunID ||
		link.InstallationID != ev.InstallationID ||
		!strings.EqualFold(link.Owner, ev.Owner) ||
		!strings.EqualFold(link.Repo, ev.Repo) {
		rec.status = store.WebhookStatusIgnored
		h.log.Warn("github app re-run: identity cross-check mismatch",
			"delivery", delivery, "run_id", runID)
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Idempotency gate: claim the delivery. Redelivery / concurrent → skip.
	claim, err := h.store.ClaimAppDelivery(r.Context(), delivery, "check_run", appDeliveryStaleAfter)
	if err != nil {
		rec.status = store.WebhookStatusError
		rec.errText = "claim delivery: " + err.Error()
		// 5xx so GitHub redelivers; stale-reclaim recovers any half-claim.
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if claim != store.AppDeliveryClaimed {
		rec.status = store.WebhookStatusIgnored
		h.log.Info("github app re-run: duplicate delivery",
			"delivery", delivery, "run_id", runID)
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Re-run. The terminal guard lives in RerunRun (shared with the HTTP path).
	res, rerr := h.store.RerunRun(r.Context(), store.RerunRunInput{
		RunID:       runID,
		TriggeredBy: "github:rerun:" + delivery,
	})
	if rerr != nil {
		// Release the claim so a manual GitHub redelivery (same id) — or an
		// automatic one on the 5xx below — can retry.
		_ = h.store.ReleaseAppDelivery(r.Context(), delivery)
		switch {
		case errors.Is(rerr, store.ErrRunActive):
			// The run is still going — a redundant re-run request. Drop (204).
			rec.status = store.WebhookStatusIgnored
			h.log.Info("github app re-run: run still active", "delivery", delivery, "run_id", runID)
			w.WriteHeader(http.StatusNoContent)
		case errors.Is(rerr, store.ErrRunNotFound),
			errors.Is(rerr, store.ErrNoModificationForPipeline),
			errors.Is(rerr, store.ErrRunRevisionsMissing):
			// Not rerunnable (deleted run / pruned revision). Drop (204).
			rec.status = store.WebhookStatusIgnored
			h.log.Info("github app re-run: not rerunnable", "delivery", delivery, "run_id", runID, "err", rerr)
			w.WriteHeader(http.StatusNoContent)
		default:
			rec.status = store.WebhookStatusError
			rec.errText = "rerun: " + rerr.Error()
			h.log.Error("github app re-run failed", "delivery", delivery, "run_id", runID, "err", rerr)
			http.Error(w, "internal error", http.StatusInternalServerError)
		}
		return
	}

	// Success: mark the delivery done, report the reopen (post-commit), audit.
	if err := h.store.MarkAppDeliveryDone(r.Context(), delivery, res.RunID); err != nil {
		h.log.Warn("github app re-run: mark delivery done failed", "delivery", delivery, "err", err)
	}
	if h.reporter != nil {
		h.reporter.ReportRunReopened(r.Context(), res.RunID)
	}
	// No gocdnext RBAC here: a verified check_run rerequested is trusted because
	// GitHub gates the re-run button by repo write + the App-secret HMAC proves
	// authenticity. Recorded as a system event; the sender/cause ride metadata.
	audit.Emit(r.Context(), h.log, h.store, store.AuditActionRunRerun, "run", res.RunID.String(),
		map[string]any{
			"rerun_of": runID.String(),
			"counter":  res.Counter,
			"cause":    "github_rerun",
			"delivery": delivery,
		})
	rec.status = store.WebhookStatusAccepted
	h.log.Info("github app re-run", "delivery", delivery, "rerun_of", runID, "run_id", res.RunID)
	w.WriteHeader(http.StatusNoContent)
}
