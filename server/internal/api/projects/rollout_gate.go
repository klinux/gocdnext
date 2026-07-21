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

// maxRolloutGateBytes caps the approve/reject request body.
const maxRolloutGateBytes = 2 << 10

// ApproveRolloutGate handles POST /api/v1/projects/{slug}/deploy-watches/{revID}/approve —
// a vote to promote the paused canary one step (once quorum is met, the watcher promotes
// next tick). RejectRolloutGate is the same for reject (→ abort). Both carry the armed
// gate_id so a stale UI can't decide a superseded step.
func (h *Handler) ApproveRolloutGate(w http.ResponseWriter, r *http.Request) {
	h.decideRolloutGate(w, r, "approved")
}

// RejectRolloutGate handles POST .../deploy-watches/{revID}/reject — a vote to ABORT the
// rollout (traffic back to stable; NOT a Git revert).
func (h *Handler) RejectRolloutGate(w http.ResponseWriter, r *http.Request) {
	h.decideRolloutGate(w, r, "rejected")
}

func (h *Handler) decideRolloutGate(w http.ResponseWriter, r *http.Request, decision string) {
	slug := chi.URLParam(r, "slug")
	revID, err := uuid.Parse(chi.URLParam(r, "revID"))
	if err != nil {
		http.Error(w, "invalid deployment revision id", http.StatusBadRequest)
		return
	}
	projectID, ok := h.resolveProjectID(w, r, slug)
	if !ok {
		return
	}
	// A rollout gate vote must come from an authenticated approver (votes key on a real
	// user; the allow-list is enforced by the store). Auth-disabled dev mode can't decide.
	u, ok := authapi.UserFromContext(r.Context())
	if !ok {
		http.Error(w, "authentication required to decide a rollout gate", http.StatusUnauthorized)
		return
	}

	var body struct {
		GateID  string `json:"gate_id"`
		Comment string `json:"comment,omitempty"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRolloutGateBytes)).Decode(&body); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	gateID, err := uuid.Parse(body.GateID)
	if err != nil {
		http.Error(w, "a gate_id is required", http.StatusBadRequest)
		return
	}

	name := u.Name
	if name == "" {
		name = u.Email
	}
	res, err := h.store.DecideRolloutGate(r.Context(), store.RolloutGateDecisionInput{
		RevisionID: revID,
		ProjectID:  projectID,
		GateID:     gateID,
		Decision:   decision,
		UserID:     u.ID,
		User:       name,
		UserEmail:  u.Email,
		Comment:    body.Comment,
	})
	switch {
	case err == nil:
		// The store already emitted the durable audit event in-tx. On a terminal decision
		// nudge the watcher isn't needed (its tick actuates); just report the outcome.
		writeJSON(w, http.StatusOK, map[string]any{
			"decided":            res.Decided,
			"decision":           res.Decision,
			"pending_quorum":     res.PendingQuorum,
			"approvals_now":      res.ApprovalsNow,
			"approvals_required": res.ApprovalsRequired,
		})
	case errors.Is(err, store.ErrGateStale):
		// Not armed / token mismatch / already decided — the UI must re-fetch.
		http.Error(w, "the approval gate has changed — reload the deploy and try again", http.StatusConflict)
	case errors.Is(err, store.ErrAlreadyVoted):
		http.Error(w, "you have already voted on this step", http.StatusConflict)
	case errors.Is(err, store.ErrApproverNotAllowed):
		http.Error(w, "you are not on this gate's approvers list", http.StatusForbidden)
	default:
		h.log.Error("rollout gate decision failed",
			"decision", decision, "slug", slug, "revision_id", revID, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}
