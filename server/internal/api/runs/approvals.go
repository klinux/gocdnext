package runs

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/api/authapi"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// Approve handles POST /api/v1/job_runs/{id}/approve — transitions
// an `awaiting_approval` gate to success and cascades the stage +
// run. The authenticated user lands in decided_by; anonymous
// requests are accepted only for gates with an empty approvers
// list (the parser's permissive default). Responses:
//
//	202 Accepted → { job_run_id, run_id, stage_completed, stage_status, run_completed, run_status }
//	400 → malformed id
//	403 → user not in approvers allow-list
//	404 → unknown gate, or the job isn't an approval row at all
//	409 → gate already decided (race, duplicate click)
//	503 → gate exists but store returned an unexpected error
func (h *Handler) Approve(w http.ResponseWriter, r *http.Request) {
	h.decideGate(w, r, true)
}

// Reject handles POST /api/v1/job_runs/{id}/reject — transitions
// the gate to failed and fails-fast the rest of the run (cascade
// via the shared completion helper). Same response contract as
// Approve.
func (h *Handler) Reject(w http.ResponseWriter, r *http.Request) {
	h.decideGate(w, r, false)
}

func (h *Handler) decideGate(w http.ResponseWriter, r *http.Request, approve bool) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	jobRunID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid job_run id", http.StatusBadRequest)
		return
	}

	decision := store.ApprovalDecision{JobRunID: jobRunID}
	// Pull the authenticated user when present. A deployment
	// without auth middleware wired will pass through as
	// anonymous — the store enforces allow-list membership, so
	// a gate with `approvers:` populated still rejects.
	if u, ok := authapi.UserFromContext(r.Context()); ok {
		decision.User = u.Name
		if decision.User == "" {
			decision.User = u.Email
		}
	}

	var (
		res store.ApprovalResult
	)
	if approve {
		res, err = h.store.ApproveGate(r.Context(), decision)
	} else {
		res, err = h.store.RejectGate(r.Context(), decision)
	}

	switch {
	case err == nil:
		// If the stage advanced (or the whole run completed),
		// wake the scheduler so the next stage doesn't wait
		// for the periodic tick. Safe no-op when nothing
		// actually advanced — NOTIFY with no listeners costs
		// only a round-trip.
		if res.StageCompleted && !res.RunCompleted {
			if nerr := h.store.NotifyRunQueued(r.Context(), res.RunID); nerr != nil {
				h.log.Warn("approve: notify run_queued failed",
					"run_id", res.RunID, "err", nerr)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"job_run_id":      jobRunID.String(),
			"run_id":          res.RunID.String(),
			"stage_completed": res.StageCompleted,
			"stage_status":    res.StageStatus,
			"run_completed":   res.RunCompleted,
			"run_status":      res.RunStatus,
		})
	case errors.Is(err, store.ErrApprovalGateNotFound):
		http.Error(w, "approval gate not found", http.StatusNotFound)
	case errors.Is(err, store.ErrApprovalNotPending):
		http.Error(w, "gate already decided", http.StatusConflict)
	case errors.Is(err, store.ErrApproverNotAllowed):
		http.Error(w, "user not in approvers list", http.StatusForbidden)
	default:
		verb := "approve"
		if !approve {
			verb = "reject"
		}
		h.log.Error("approval decision failed",
			"op", verb, "job_run_id", jobRunID, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}
