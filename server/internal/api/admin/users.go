package admin

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/api/authapi"
	"github.com/gocdnext/gocdnext/server/internal/audit"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// Users handles GET /api/v1/admin/users. Lists every user with
// role + last-login so the admin UI can render the promote/
// demote grid. Admin-only; the router enforces role=admin before
// the handler runs.
func (h *Handler) Users(w http.ResponseWriter, r *http.Request) {
	if !methodGET(w, r) {
		return
	}
	users, err := h.store.ListAllUsers(r.Context())
	if err != nil {
		h.log.Error("admin: list users", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"users": users})
}

type setUserRoleRequest struct {
	Role string `json:"role"`
}

// SetUserRole handles PUT /api/v1/admin/users/{id}/role.
// Validates the role at the store layer (typos → 400) and
// refuses a self-demotion so an admin can't accidentally lock
// themselves out. Emits an audit event on success with the
// before/after role captured in metadata.
//
// Responses:
//
//	200 → updated row
//	400 → malformed id or role
//	403 → admin tried to demote themselves
//	404 → unknown user id
func (h *Handler) SetUserRole(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		w.Header().Set("Allow", "PUT")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "malformed id", http.StatusBadRequest)
		return
	}
	var req setUserRoleRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&req); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Role == "" {
		http.Error(w, "role is required", http.StatusBadRequest)
		return
	}

	// Refuse self-demotion. An admin calling PUT on their own id
	// with a non-admin role is almost always a misclick or a
	// script bug; the blast radius (last admin locking
	// themselves out and needing SQL to recover) is big enough
	// that guarding here is cheap insurance. The admin UI can
	// still show the current user in the list — just greys out
	// the role dropdown.
	if u, ok := authapi.UserFromContext(r.Context()); ok {
		if u.ID == id && req.Role != store.RoleAdmin {
			http.Error(w, "cannot demote yourself", http.StatusForbidden)
			return
		}
	}

	// Read the old role so audit metadata can record the
	// before/after. A missing user returns 404; a missing role
	// here implies a race between list + update on a deleted
	// user — same 404 treatment.
	before, err := h.store.GetUser(r.Context(), id)
	if err != nil {
		http.Error(w, "user not found", http.StatusNotFound)
		return
	}

	updated, err := h.store.UpdateUserRole(r.Context(), id, req.Role)
	if errors.Is(err, store.ErrInvalidRole) {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err != nil {
		h.log.Error("admin: update user role", "id", id, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	audit.Emit(r.Context(), h.log, h.store,
		store.AuditActionUserRoleChange, "user", id.String(),
		map[string]any{
			"email": updated.Email,
			"from":  before.Role,
			"to":    updated.Role,
		})

	writeJSON(w, updated)
}
