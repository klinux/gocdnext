package projects

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/api/authapi"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// freezeRequest is the PUT body. `reason` is REQUIRED: an unexplained freeze on
// production is an outage nobody can triage when the person who set it is
// off-shift, and it is what the UI shows every operator who hits the block.
type freezeRequest struct {
	Reason string `json:"reason"`
}

// freezeBodyLimit caps the decoded body. `reason` is bounded at 500 characters
// by both the store and the DB CHECK; 4 KiB leaves room for multi-byte text
// while making an oversized POST cost nothing to reject. Mirrors the limit on
// RollbackEnvironment.
const freezeBodyLimit = 4 << 10

// FreezeEnvironment handles
// PUT /api/v1/projects/{slug}/environment-freezes/{name} with body {reason}.
//
// Keyed by NAME, not by environments.id, and that is the whole point:
// `environments` rows are created lazily at the first deploy, so an id-keyed
// endpoint could not freeze `production` before anything had ever shipped there
// — precisely the deploy a freeze most needs to stop. Maintainer-gated at the
// router.
//
// Idempotent: freezing an already-frozen environment returns 200 and does NOT
// reset who froze it, when, or why.
func (h *Handler) FreezeEnvironment(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	// Normalise ONCE, before anything else uses it. The store trims internally,
	// so passing the raw path segment on would freeze `production` while every
	// downstream use (the response, the logs) still said ` production `.
	name, err := store.NormalizeEnvironmentName(chi.URLParam(r, "name"))
	if err != nil {
		http.Error(w, "invalid environment name", http.StatusBadRequest)
		return
	}

	var req freezeRequest
	// MaxBytesReader BEFORE the decoder, so an oversized body is refused while
	// streaming rather than after being buffered.
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, freezeBodyLimit)).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	projectID, ok := h.resolveProjectID(w, r, slug)
	if !ok {
		return
	}

	froze, err := h.store.FreezeEnvironment(r.Context(), projectID, name, h.freezeActor(r), req.Reason)
	if err != nil {
		h.writeFreezeError(w, r, "freeze environment", slug, name, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"environment": name,
		"frozen":      true,
		// changed=false means "it was already frozen" — the client can tell a
		// real state change from a no-op without a follow-up read.
		"changed": froze,
	})
}

// UnfreezeEnvironment handles
// DELETE /api/v1/projects/{slug}/environment-freezes/{name}.
//
// After the freeze is lifted, every run currently held by it is woken with the
// same run_queued NOTIFY a fresh run fires, so deploys resume immediately rather
// than after up to a full scheduler tick. That wake is BEST-EFFORT by
// construction — it keys on the `frozen-deploy:<name>` stamp, so a run that was
// never stamped (frozen in the window between the pre-scan and the admission
// re-check) is not in the list. The periodic drain tick is the backstop, and it
// is why a failure here is logged rather than surfaced: the unfreeze itself has
// already committed and is not in doubt.
//
// Idempotent: unfreezing an environment that isn't frozen returns 200.
func (h *Handler) UnfreezeEnvironment(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	// Normalised BEFORE the wake lookup, which composes the exact
	// `frozen-deploy:<name>` stamp the scheduler wrote. A raw ` production `
	// here unfreezes correctly but matches no held run, so the immediate resume
	// silently degrades to "wait for the next tick".
	name, err := store.NormalizeEnvironmentName(chi.URLParam(r, "name"))
	if err != nil {
		http.Error(w, "invalid environment name", http.StatusBadRequest)
		return
	}

	projectID, ok := h.resolveProjectID(w, r, slug)
	if !ok {
		return
	}

	thawed, err := h.store.UnfreezeEnvironment(r.Context(), projectID, name, h.freezeActor(r))
	if err != nil {
		h.writeFreezeError(w, r, "unfreeze environment", slug, name, err)
		return
	}

	woken := 0
	if thawed {
		woken = h.wakeRunsHeldBy(r, projectID, name)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"environment": name,
		"frozen":      false,
		"changed":     thawed,
		"woken_runs":  woken,
	})
}

// freezeActor builds the CANONICAL actor from the request context. It is never
// assembled from client input: frozen_by is quoted back to every operator who
// hits the block, and the audit row derives its actor from the same struct.
//
// With auth disabled RequireMinRole passes with no user in context, so a
// documented sentinel is recorded rather than an empty actor.
func (h *Handler) freezeActor(r *http.Request) store.FreezeActor {
	if u, ok := authapi.UserFromContext(r.Context()); ok {
		return store.FreezeActorFromUser(u)
	}
	return store.SystemFreezeActor()
}

// wakeRunsHeldBy fires run_queued for the runs the lifted freeze was holding and
// returns how many were woken.
//
// ONE batched statement, not one per run: a freeze on a busy environment can
// hold dozens of runs, and a per-run round-trip would make lifting it slower the
// better it worked.
//
// Errors are logged, never surfaced: the unfreeze has already committed, and the
// scheduler's periodic tick is the backstop that recovers anything this misses
// (including runs that were never stamped, which this query cannot see).
func (h *Handler) wakeRunsHeldBy(r *http.Request, projectID uuid.UUID, name string) int {
	held, err := h.store.ListRunsHeldByEnvironment(r.Context(), projectID, name)
	if err != nil {
		h.log.Warn("unfreeze: list held runs", "name", name, "err", err)
		return 0
	}
	if len(held) == 0 {
		return 0
	}
	if nerr := h.store.NotifyRunsQueued(r.Context(), held); nerr != nil {
		h.log.Warn("unfreeze: notify run_queued", "name", name, "runs", len(held), "err", nerr)
		return 0
	}
	return len(held)
}

// writeFreezeError maps the store's typed validation errors to 400 and anything
// else to 500. The 400 bodies echo the store's message, which names the offending
// FIELD and its bound — never the value of anything else.
func (h *Handler) writeFreezeError(w http.ResponseWriter, r *http.Request, op, slug, name string, err error) {
	switch {
	case errors.Is(err, store.ErrFreezeNameInvalid):
		http.Error(w, "invalid environment name", http.StatusBadRequest)
	case errors.Is(err, store.ErrFreezeReasonInvalid):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, store.ErrFreezeActorUnusable):
		// The authenticated identity has no usable email/name/id. A 422 rather
		// than a 500: the server is fine, the IdP data is not.
		http.Error(w, "cannot record who is freezing this environment — the account has no usable email, name or id",
			http.StatusUnprocessableEntity)
	default:
		h.log.Error(op, "slug", slug, "environment", name, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}
