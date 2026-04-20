// Package webhook exposes HTTP endpoints that accept provider webhooks
// (starting with GitHub) and persist them as modifications. It glues together
// github.VerifySignature, github.ParsePushEvent and the store.
package webhook

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/gocdnext/gocdnext/server/internal/checks"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/internal/webhook/github"
)

const maxBodyBytes = 5 << 20 // 5 MiB — GitHub payloads are usually <1 MiB.

// extractCloneURL pulls repository.clone_url out of an unverified
// GitHub webhook body. Every event we care about (push,
// pull_request, ping-on-repo) carries this field at the top level
// of the "repository" object. Failing to parse → empty string →
// caller 400s.
func extractCloneURL(body []byte) string {
	var env struct {
		Repository struct {
			CloneURL string `json:"clone_url"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return ""
	}
	return env.Repository.CloneURL
}

// Handler serves the webhook endpoints. Register HandleGitHub on
// the router of your choice; the method signature is compatible
// with http.HandlerFunc.
//
// HMAC verification is per-repo: we peek at repository.clone_url
// in the (still-unverified) body, look up the scm_source bound to
// that URL, and validate the signature with its sealed secret.
// An unknown repo or a row without a registered secret → 401.
type Handler struct {
	store    *store.Store
	log      *slog.Logger
	fetcher  ConfigFetcher
	reporter *checks.Reporter
}

// NewHandler builds the webhook handler. The Store must have a
// cipher configured (GOCDNEXT_SECRET_KEY) — without it no repo can
// register a secret and every incoming webhook 401s.
func NewHandler(s *store.Store, log *slog.Logger) *Handler {
	if log == nil {
		log = slog.Default()
	}
	return &Handler{store: s, log: log}
}

// WithConfigFetcher opts the handler into drift detection: on a push whose
// clone_url matches a registered scm_source, the fetcher is called to re-read
// `.gocdnext/` and ApplyProject is invoked before the material-match path
// runs. Nil (default) disables drift.
func (h *Handler) WithConfigFetcher(f ConfigFetcher) *Handler {
	h.fetcher = f
	return h
}

// WithChecksReporter enables GitHub Checks API reporting. nil keeps
// the feature off; each webhook-triggered run will then silently
// skip the check create.
func (h *Handler) WithChecksReporter(r *checks.Reporter) *Handler {
	h.reporter = r
	return h
}

// HandleGitHub verifies the HMAC signature, parses a push event
// and persists a modification when the payload matches a known
// material. Non-push events and pushes with no matching material
// are answered with 204 No Content so GitHub does not retry.
func (h *Handler) HandleGitHub(w http.ResponseWriter, r *http.Request) {
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
		provider: "github",
		event:    event,
		headers:  headersJSON(r.Header),
		payload:  json.RawMessage(body),
		writer:   sc,
	}
	defer h.recordDelivery(r.Context(), rec)
	w = sc

	// Peek at the unverified body to pull repository.clone_url so
	// we can look up which scm_source's secret to check against.
	// This is safe: the attacker controls what URL we pick, but
	// they still have to produce a valid HMAC for whichever secret
	// that choice resolves to — and they don't have any of those.
	cloneURL := extractCloneURL(body)
	if cloneURL == "" {
		rec.status = store.WebhookStatusRejected
		rec.errText = "missing repository.clone_url"
		h.log.Warn("github webhook: no clone_url in payload",
			"event", event, "delivery", delivery)
		http.Error(w, "invalid payload: repository.clone_url required", http.StatusBadRequest)
		return
	}
	auth, err := h.store.FindSCMSourceWebhookSecret(r.Context(), cloneURL)
	if errors.Is(err, store.ErrSCMSourceNotFound) {
		rec.status = store.WebhookStatusRejected
		rec.errText = "no scm_source registered for this repo"
		h.log.Warn("github webhook: unknown repo", "event", event,
			"delivery", delivery, "clone_url", cloneURL)
		http.Error(w, "no scm_source registered for this repo", http.StatusUnauthorized)
		return
	}
	if err != nil {
		rec.status = store.WebhookStatusError
		rec.errText = "scm_source lookup: " + err.Error()
		h.log.Error("github webhook: scm_source lookup", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if auth.Secret == "" {
		rec.status = store.WebhookStatusRejected
		rec.errText = "no webhook secret registered for this scm_source"
		h.log.Warn("github webhook: scm_source has no secret yet",
			"delivery", delivery, "scm_source_id", auth.ID)
		http.Error(w, "no webhook secret registered for this repo", http.StatusUnauthorized)
		return
	}

	if err := github.VerifySignature(auth.Secret, body, signature); err != nil {
		rec.status = store.WebhookStatusRejected
		rec.errText = "invalid signature: " + err.Error()
		h.log.Warn("github webhook: signature rejected",
			"event", event, "delivery", delivery,
			"scm_source_id", auth.ID, "err", err)
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}

	switch event {
	case "push":
		// continue below
	case "pull_request":
		h.handlePullRequest(w, r, body, delivery, rec)
		return
	default:
		rec.status = store.WebhookStatusIgnored
		h.log.Info("github webhook: ignored event", "event", event, "delivery", delivery)
		w.WriteHeader(http.StatusNoContent)
		return
	}

	ev, err := github.ParsePushEvent(body)
	if err != nil {
		rec.status = store.WebhookStatusError
		rec.errText = "parse push: " + err.Error()
		h.log.Warn("github webhook: parse failed", "delivery", delivery, "err", err)
		http.Error(w, "invalid push payload: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Branch deletions carry no head commit — nothing to persist.
	if ev.Deleted || ev.HeadCommit == nil {
		rec.status = store.WebhookStatusIgnored
		h.log.Info("github webhook: skipping delete/no-head-commit", "delivery", delivery, "ref", ev.Ref)
		w.WriteHeader(http.StatusNoContent)
		return
	}

	branch := ev.Branch

	// Config drift: if the repo matches a registered scm_source and the
	// push is on its default branch, re-read `.gocdnext/` at this revision
	// and re-apply before we match materials against the (possibly new)
	// pipeline state. Failure here falls through to the legacy path.
	var driftOutcome DriftOutcome
	if scm, ok := h.driftLookup(r.Context(), ev.Repository.CloneURL); ok {
		driftOutcome = h.applyDrift(r.Context(), scm, branch, ev.After)
		if driftOutcome.Applied {
			h.log.Info("github webhook: config drift re-applied",
				"delivery", delivery, "scm_source_id", scm.ID, "revision", ev.After)
		} else if driftOutcome.Error != "" {
			h.log.Warn("github webhook: config drift failed",
				"delivery", delivery, "scm_source_id", scm.ID, "err", driftOutcome.Error)
		}
	}

	fp := store.FingerprintFor(ev.Repository.CloneURL, branch)

	material, err := h.store.FindMaterialByFingerprint(r.Context(), fp)
	if errors.Is(err, store.ErrMaterialNotFound) {
		h.log.Info("github webhook: no matching material",
			"delivery", delivery, "repo", ev.Repository.FullName, "ref", ev.Ref,
			"fingerprint", fp, "drift_applied", driftOutcome.Applied)
		// If drift ran (config-only push: scm_source matched but no material
		// tied to this URL+branch), acknowledge with 202 + the outcome so the
		// caller sees the sync happened. Legacy no-match pushes still 204.
		if driftOutcome.Attempted {
			rec.status = store.WebhookStatusAccepted
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"drift": map[string]any{
					"applied":  driftOutcome.Applied,
					"error":    driftOutcome.Error,
					"revision": driftOutcome.Revision,
				},
			})
			return
		}
		rec.status = store.WebhookStatusIgnored
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err != nil {
		rec.status = store.WebhookStatusError
		rec.errText = "material lookup: " + err.Error()
		h.log.Error("github webhook: material lookup failed", "delivery", delivery, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	rec.materialID = material.ID

	mod := store.Modification{
		MaterialID:  material.ID,
		Revision:    ev.After,
		Branch:      branch,
		Author:      ev.HeadCommit.Author.Name,
		Message:     ev.HeadCommit.Message,
		Payload:     json.RawMessage(body),
		CommittedAt: ev.HeadCommit.Timestamp,
	}

	res, err := h.store.InsertModification(r.Context(), mod)
	if err != nil {
		rec.status = store.WebhookStatusError
		rec.errText = "insert modification: " + err.Error()
		h.log.Error("github webhook: insert modification failed", "delivery", delivery, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	resp := map[string]any{
		"modification_id": res.ID,
		"created":         res.Created,
		"material_id":     material.ID.String(),
	}
	if driftOutcome.Attempted {
		resp["drift"] = map[string]any{
			"applied":  driftOutcome.Applied,
			"error":    driftOutcome.Error,
			"revision": driftOutcome.Revision,
		}
	}

	// Only trigger a fresh run on a newly-inserted modification. If run creation
	// fails after the modification was persisted, the retry on GitHub's side
	// will see Created=false and skip this branch — a known gap to plug when we
	// introduce EnsureRun (C1.6 or later).
	if res.Created {
		runRes, err := h.store.CreateRunFromModification(r.Context(), store.CreateRunFromModificationInput{
			PipelineID:     material.PipelineID,
			MaterialID:     material.ID,
			ModificationID: res.ID,
			Revision:       ev.After,
			Branch:         branch,
			Provider:       "github",
			Delivery:       delivery,
			TriggeredBy:    "system:webhook",
		})
		if err != nil {
			rec.status = store.WebhookStatusError
			rec.errText = "create run: " + err.Error()
			h.log.Error("github webhook: create run failed",
				"delivery", delivery, "modification_id", res.ID, "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		resp["run_id"] = runRes.RunID.String()
		resp["run_counter"] = runRes.Counter
		h.log.Info("github webhook: run queued",
			"delivery", delivery, "pipeline_id", material.PipelineID,
			"run_id", runRes.RunID, "counter", runRes.Counter,
			"stages", len(runRes.StageRuns), "jobs", len(runRes.JobRuns))
		h.reporter.ReportRunCreated(r.Context(), runRes.RunID)
	} else {
		h.log.Info("github webhook: modification already present, no run queued",
			"delivery", delivery, "modification_id", res.ID)
	}

	rec.status = store.WebhookStatusAccepted
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		// Already wrote 202 — nothing we can do, just log.
		h.log.Warn("github webhook: encode response failed", "err", fmt.Sprint(err))
	}
}
