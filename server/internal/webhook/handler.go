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

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/checks"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/internal/webhook/github"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
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
	// prFiles resolves PR changed-file lists for `when.paths`
	// filtering (push payloads embed the lists; PR payloads don't).
	// Nil = PR path filtering fails open. See pathfilter.go.
	prFiles PRFilesFetcher
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

	// Deletion events (branch OR tag) carry no useful state to fan
	// out — `before` is the prior SHA, `after` is the zero SHA, no
	// head_commit. Acknowledge and stop.
	if ev.Deleted {
		rec.status = store.WebhookStatusIgnored
		h.log.Info("github webhook: skipping ref deletion", "delivery", delivery, "ref", ev.Ref)
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Tag pushes route differently: no base branch (a tag points at a
	// SHA that may not be on any branch), so the URL+branch fingerprint
	// match used by the branch-push path can't fire. handleTagPush does
	// URL-only lookup + per-material `Events: [tag]` filter and stamps
	// cause="tag" + cause_detail={tag_name, tag_message, tag_sha}.
	//
	// Routed BEFORE the head_commit nil check below because some tag
	// payloads (notably annotated tags) arrive with head_commit empty
	// and we still want them to fire — the tag SHA lives in ev.After
	// regardless.
	if ev.IsTag {
		h.handleTagPush(w, r, body, delivery, rec, ev)
		return
	}

	// Branch pushes without a head_commit have nothing to persist
	// (the head_commit drives modification.author/message/committed_at).
	if ev.HeadCommit == nil {
		rec.status = store.WebhookStatusIgnored
		h.log.Info("github webhook: skipping branch push with no head_commit",
			"delivery", delivery, "ref", ev.Ref)
		w.WriteHeader(http.StatusNoContent)
		return
	}

	branch := ev.Branch

	// Config drift: if the repo matches a registered scm_source and the
	// push is on its default branch, re-read `.gocdnext/` at this revision
	// and re-apply before we match materials against the (possibly new)
	// pipeline state. Failure here falls through to the legacy path.
	// applyDrift also enforces the default-branch guard internally —
	// see drift.go for the rationale (broadening to non-default
	// branches is a separate follow-up, gated on a registered-material
	// check so a feature branch can't overwrite the project's global
	// definition).
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

	// [skip ci] check sits AFTER drift on purpose: the marker means
	// "don't create runs", not "don't observe the push" — a skip-ci
	// commit that edits .gocdnext/ still syncs project config, it
	// just doesn't trigger pipelines. Never applied to pull_request
	// events (see skipci.go for the security rationale).
	if marker, ok := skipCIMarker(ev.HeadCommit.Message); ok {
		if h.repoIsGoverned(r.Context(), ev.Repository.CloneURL) {
			h.log.Info("github webhook: [skip ci] ignored — project governed by compliance policies",
				"delivery", delivery, "ref", ev.Ref)
		} else {
			h.respondSkipCI(w, rec, "github", delivery, ev.Ref, marker)
			return
		}
	}

	normalizedURL := domain.NormalizeGitURL(ev.Repository.CloneURL)
	fp := store.FingerprintFor(ev.Repository.CloneURL, branch)

	materials, err := h.store.FindMaterialsByFingerprint(r.Context(), fp)
	if err != nil {
		rec.status = store.WebhookStatusError
		rec.errText = "material lookup: " + err.Error()
		h.log.Error("github webhook: material lookup failed", "delivery", delivery, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if len(materials) == 0 {
		// Log every dimension the lookup used so an operator can
		// diff against the apply-time material row (visible via
		// /api/v1/projects/{slug}). The most common no-match cause
		// is the branch differing between push and the pipeline's
		// material declaration; second is a URL form mismatch the
		// canonicaliser doesn't yet handle.
		h.log.Info("github webhook: no matching material",
			"delivery", delivery,
			"repo", ev.Repository.FullName,
			"clone_url", ev.Repository.CloneURL,
			"normalized_url", normalizedURL,
			"ref", ev.Ref,
			"branch", branch,
			"fingerprint", fp,
			"drift_applied", driftOutcome.Applied)
		// If drift ran (config-only push: scm_source matched but no material
		// tied to this URL+branch), acknowledge with 202 + the outcome so the
		// caller sees the sync happened. Surface a warning in the response
		// body so the operator catches the silent "applied but no run" path
		// from the GitHub webhook delivery viewer without tailing logs.
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
				"warning": fmt.Sprintf(
					"no pipeline material matched %s@%s — push did not trigger a run",
					normalizedURL, branch),
			})
			return
		}
		rec.status = store.WebhookStatusIgnored
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// when.event: the fingerprint match (URL+branch) authorizes the
	// repo, but a pipeline only fires on push if its events include
	// "push". A pipeline declaring [tag] / [manual] keeps an implicit
	// material on this branch; without this filter it fans out on every
	// push. Symmetric with the tag-push / pull_request paths. Runs AFTER
	// drift (config-sync still observes the push) and BEFORE when.paths
	// (paths only matter once the event is eligible).
	eventCandidates := len(materials)
	materials, eventFiltered := filterMaterialsByEvent(h.log, materials, "push", "github", delivery)
	if len(materials) == 0 {
		h.log.Info("github webhook: fingerprint matched but no push-listening material",
			"delivery", delivery, "branch", branch,
			"material_candidates", eventCandidates, "filtered_by_event", eventFiltered, "event", "push")
		// Config sync may still have observed this push even though no
		// pipeline fires — surface it as Accepted + the drift block so
		// the delivery doesn't read as a plain "ignored".
		resp := map[string]any{"runs": []any{}, "filtered_by_event": eventFiltered}
		status := http.StatusOK
		rec.status = store.WebhookStatusIgnored
		if driftOutcome.Attempted {
			resp["drift"] = map[string]any{
				"applied":  driftOutcome.Applied,
				"error":    driftOutcome.Error,
				"revision": driftOutcome.Revision,
			}
			status = http.StatusAccepted
			rec.status = store.WebhookStatusAccepted
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(resp)
		return
	}

	// when.paths: drop materials whose globs don't match the push's
	// changed files. Unknown set (truncated payload, force push with
	// no embedded commits) fails open inside pathsMatch.
	changedFiles, filesKnown := ev.ChangedFiles()
	materials, pathFiltered := filterMaterialsByPaths(
		h.log, materials, changedFiles, filesKnown, "github", delivery)
	if len(materials) == 0 {
		rec.status = store.WebhookStatusIgnored
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"runs":              []any{},
			"filtered_by_paths": pathFiltered,
		})
		return
	}

	outcomes := fanOutMaterials(r.Context(), h.log, h.store, fanOutInput{
		Materials:   materials,
		Revision:    ev.After,
		Branch:      branch,
		Author:      ev.HeadCommit.Author.Name,
		Message:     ev.HeadCommit.Message,
		Payload:     json.RawMessage(body),
		CommittedAt: ev.HeadCommit.Timestamp,
		Provider:    "github",
		Delivery:    delivery,
		TriggeredBy: "system:webhook",
	})
	rec.materialID = firstCreatedRunMaterialID(outcomes)

	runs := runsPayload(outcomes)
	resp := map[string]any{
		"runs":      runs,
		"materials": len(materials),
	}
	if pathFiltered > 0 {
		resp["filtered_by_paths"] = pathFiltered
	}
	if eventFiltered > 0 {
		resp["filtered_by_event"] = eventFiltered
	}
	if driftOutcome.Attempted {
		resp["drift"] = map[string]any{
			"applied":  driftOutcome.Applied,
			"error":    driftOutcome.Error,
			"revision": driftOutcome.Revision,
		}
	}

	// Trigger a run per matched material. Fan-out failures on
	// individual pipelines are logged but don't fail the whole
	// delivery — partial success is still actionable. If EVERY
	// matched pipeline errored, surface a 500 so the provider
	// retries (matching the pre-fan-out semantics for a single
	// pipeline error).
	allErrored := len(outcomes) > 0
	for _, oc := range outcomes {
		if oc.Err == nil {
			allErrored = false
			if oc.RunID != uuid.Nil {
				h.log.Info("github webhook: run queued",
					"delivery", delivery, "pipeline_id", oc.PipelineID,
					"run_id", oc.RunID, "counter", oc.RunCounter)
				h.reporter.ReportRunCreated(r.Context(), oc.RunID)
			}
		}
	}
	if allErrored {
		rec.status = store.WebhookStatusError
		rec.errText = fmt.Sprintf("fan-out: all %d pipelines errored", len(outcomes))
		h.log.Error("github webhook: every pipeline fan-out failed",
			"delivery", delivery, "materials", len(materials))
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if len(runs) == 0 {
		// Every modification deduped (provider retry of an
		// already-processed push). Nothing new to run — log once
		// at info level so an operator inspecting deliveries
		// understands why the 202 body has runs: [].
		h.log.Info("github webhook: all modifications deduped, no runs queued",
			"delivery", delivery, "materials", len(materials))
	}

	rec.status = store.WebhookStatusAccepted
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		// Already wrote 202 — nothing we can do, just log.
		h.log.Warn("github webhook: encode response failed", "err", fmt.Sprint(err))
	}
}
