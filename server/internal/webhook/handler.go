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

	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/internal/webhook/github"
)

const maxBodyBytes = 5 << 20 // 5 MiB — GitHub payloads are usually <1 MiB.

// Handler serves the webhook endpoints. Register HandleGitHub on the router
// of your choice; the method signature is compatible with http.HandlerFunc.
type Handler struct {
	secret string
	store  *store.Store
	log    *slog.Logger
}

// NewHandler builds the webhook handler. secret is the HMAC shared secret for
// GitHub; an empty string disables the endpoint (returns 503).
func NewHandler(secret string, s *store.Store, log *slog.Logger) *Handler {
	if log == nil {
		log = slog.Default()
	}
	return &Handler{secret: secret, store: s, log: log}
}

// HandleGitHub verifies the HMAC signature, parses a push event and persists a
// modification when the payload matches a known material. Non-push events and
// pushes with no matching material are answered with 204 No Content so GitHub
// does not retry.
func (h *Handler) HandleGitHub(w http.ResponseWriter, r *http.Request) {
	if h.secret == "" {
		h.log.Error("github webhook: no secret configured")
		http.Error(w, "github webhook not configured", http.StatusServiceUnavailable)
		return
	}

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

	if err := github.VerifySignature(h.secret, body, signature); err != nil {
		h.log.Warn("github webhook: signature rejected", "event", event, "delivery", delivery, "err", err)
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}

	// Only push events land modifications. ping / others are ack'd with 204.
	if event != "push" {
		h.log.Info("github webhook: ignored event", "event", event, "delivery", delivery)
		w.WriteHeader(http.StatusNoContent)
		return
	}

	ev, err := github.ParsePushEvent(body)
	if err != nil {
		h.log.Warn("github webhook: parse failed", "delivery", delivery, "err", err)
		http.Error(w, "invalid push payload: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Branch deletions carry no head commit — nothing to persist.
	if ev.Deleted || ev.HeadCommit == nil {
		h.log.Info("github webhook: skipping delete/no-head-commit", "delivery", delivery, "ref", ev.Ref)
		w.WriteHeader(http.StatusNoContent)
		return
	}

	branch := ev.Branch
	fp := store.FingerprintFor(ev.Repository.CloneURL, branch)

	material, err := h.store.FindMaterialByFingerprint(r.Context(), fp)
	if errors.Is(err, store.ErrMaterialNotFound) {
		h.log.Info("github webhook: no matching material (dropped)",
			"delivery", delivery, "repo", ev.Repository.FullName, "ref", ev.Ref, "fingerprint", fp)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err != nil {
		h.log.Error("github webhook: material lookup failed", "delivery", delivery, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

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
		h.log.Error("github webhook: insert modification failed", "delivery", delivery, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	resp := map[string]any{
		"modification_id": res.ID,
		"created":         res.Created,
		"material_id":     material.ID.String(),
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
	} else {
		h.log.Info("github webhook: modification already present, no run queued",
			"delivery", delivery, "modification_id", res.ID)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		// Already wrote 202 — nothing we can do, just log.
		h.log.Warn("github webhook: encode response failed", "err", fmt.Sprint(err))
	}
}
