package projects

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/api/authapi"
	"github.com/gocdnext/gocdnext/server/internal/audit"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

var validFindingStates = map[string]bool{
	"open": true, "dismissed": true, "false_positive": true, "accepted": true,
}

const maxFindingStateReason = 1000

type setFindingStateRequest struct {
	State  string `json:"state"`
	Reason string `json:"reason"`
}

// SetFindingState handles PUT /api/v1/projects/{slug}/finding-states/{id}/state —
// set a finding identity's triage state (open|dismissed|false_positive|accepted).
// Maintainer+ (the route group enforces the role). {id} is a
// security_finding_states identity id; the mutation is project-scoped so a
// maintainer of one project can't touch another's findings (404 on mismatch).
func (h *Handler) SetFindingState(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	if slug == "" {
		http.Error(w, "slug is required", http.StatusBadRequest)
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "invalid finding state id", http.StatusBadRequest)
		return
	}

	var req setFindingStateRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	req.State = strings.TrimSpace(req.State)
	if !validFindingStates[req.State] {
		http.Error(w, "invalid state (open|dismissed|false_positive|accepted)", http.StatusBadRequest)
		return
	}
	reason := strings.TrimSpace(req.Reason)
	if len(reason) > maxFindingStateReason {
		http.Error(w, "reason too long (max 1000)", http.StatusBadRequest)
		return
	}

	detail, err := h.store.GetProjectDetail(r.Context(), slug, 1)
	if err != nil {
		if errors.Is(err, store.ErrProjectNotFound) {
			http.Error(w, "project not found", http.StatusNotFound)
			return
		}
		h.log.Error("set finding state: load project", "slug", slug, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Actor from the authenticated session — stored on the identity + audited.
	var actorID *uuid.UUID
	actorEmail := ""
	if u, ok := authapi.UserFromContext(r.Context()); ok {
		uid := u.ID
		actorID = &uid
		actorEmail = u.Email
	}

	if err := h.store.SetFindingState(r.Context(), detail.Project.ID, id, req.State, reason, actorID, actorEmail); err != nil {
		if errors.Is(err, store.ErrFindingStateNotFound) {
			http.Error(w, "finding not found", http.StatusNotFound)
			return
		}
		h.log.Error("set finding state", "slug", slug, "id", id, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	audit.Emit(r.Context(), h.log, h.store,
		store.AuditActionFindingState, "security_finding_state", strconv.FormatInt(id, 10),
		map[string]any{"slug": slug, "state": req.State, "reason": reason})

	w.WriteHeader(http.StatusNoContent)
}
