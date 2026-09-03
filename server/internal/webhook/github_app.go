package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/audit"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/internal/webhook/github"
)

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

	if !h.verifyAppSignature(r.Context(), event, delivery, body, signature, rec, w) {
		return
	}

	switch event {
	case "ping":
		rec.status = store.WebhookStatusAccepted
		w.WriteHeader(http.StatusNoContent)
	case "check_run":
		h.handleCheckRerun(w, r, body, delivery, rec)
	case "merge_group":
		h.handleMergeGroup(w, r, body, delivery, rec)
	default:
		// check_suite (deferred) + check_run created/completed → nothing to do.
		rec.status = store.WebhookStatusIgnored
		h.log.Info("github app webhook: ignored event", "event", event, "delivery", delivery)
		w.WriteHeader(http.StatusNoContent)
	}
}

// verifyAppSignature verifies an App-delivered webhook. When the payload carries
// an app id (ping/check_run/check_suite, and defensively any future merge_group
// shape with app.id), it selects that candidate secret first. Some App events,
// including GitHub's documented merge_group payload, only carry installation.id;
// for those we try every enabled App webhook secret and let HMAC be the
// authority. The body is still trusted for nothing until this returns true.
func (h *Handler) verifyAppSignature(ctx context.Context, event, delivery string, body []byte, signature string, rec *deliveryRec, w http.ResponseWriter) bool {
	var appID int64
	switch event {
	case "ping":
		if ev, perr := github.ParsePingEvent(body); perr == nil {
			appID = ev.HookAppID
		}
	case "merge_group":
		if ev, perr := github.ParseMergeGroupEvent(body); perr == nil {
			appID = ev.AppID
		}
	default: // check_run / check_suite
		if ev, perr := github.ParseCheckRunEvent(body); perr == nil {
			appID = ev.AppID
		}
	}

	if appID != 0 {
		secret, err := h.store.AppWebhookSecretByAppID(ctx, appID)
		if err == nil && secret != "" {
			if err := github.VerifySignature(secret, body, signature); err != nil {
				rec.status = store.WebhookStatusRejected
				rec.errText = "invalid signature: " + err.Error()
				h.log.Warn("github app webhook: signature rejected",
					"event", event, "delivery", delivery, "app_id", appID, "err", err)
				http.Error(w, "invalid signature", http.StatusUnauthorized)
				return false
			}
			return true
		}
		rec.status = store.WebhookStatusRejected
		rec.errText = "app webhook secret not resolved"
		h.log.Warn("github app webhook: app id secret not resolved",
			"event", event, "delivery", delivery, "app_id", appID, "err", err)
		http.Error(w, "unknown or unconfigured github app", http.StatusUnauthorized)
		return false
	}

	secrets, err := h.store.AppWebhookSecrets(ctx)
	if err != nil || len(secrets) == 0 {
		rec.status = store.WebhookStatusRejected
		rec.errText = "app webhook secret not resolved"
		h.log.Warn("github app webhook: secret not resolved",
			"event", event, "app_id", appID, "err", err)
		http.Error(w, "unknown or unconfigured github app", http.StatusUnauthorized)
		return false
	}
	for _, secret := range secrets {
		if github.VerifySignature(secret, body, signature) == nil {
			return true
		}
	}
	rec.status = store.WebhookStatusRejected
	rec.errText = "invalid signature"
	h.log.Warn("github app webhook: signature rejected",
		"event", event, "delivery", delivery, "app_id", appID)
	http.Error(w, "invalid signature", http.StatusUnauthorized)
	return false
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

	// Re-run + record the delivery ATOMICALLY (claim + create-run + link run_id
	// commit together — see store.RerunForAppDelivery). The terminal guard lives
	// in the shared rerun path, so a still-active run surfaces ErrRunActive here
	// before any claim. A duplicate/concurrent delivery → ErrAppDeliveryAlreadyClaimed
	// (its tx rolled back, no second run).
	res, rerr := h.store.RerunForAppDelivery(r.Context(), runID, delivery, "check_run", "github:rerun:"+delivery)
	switch {
	case rerr == nil:
		if h.reporter != nil {
			h.reporter.ReportRunReopened(r.Context(), res.RunID)
		}
		// No gocdnext RBAC here: a verified check_run rerequested is trusted —
		// GitHub gates the re-run button by repo write + the App-secret HMAC
		// proves authenticity. Recorded as a system event; sender/cause ride
		// the metadata.
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
	case errors.Is(rerr, store.ErrAppDeliveryAlreadyClaimed):
		rec.status = store.WebhookStatusIgnored
		h.log.Info("github app re-run: duplicate delivery", "delivery", delivery, "run_id", runID)
		w.WriteHeader(http.StatusNoContent)
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
}
