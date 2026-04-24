package admin

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/api/authapi"
	"github.com/gocdnext/gocdnext/server/internal/audit"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// groupDTO is the wire shape for /api/v1/admin/groups responses.
// Timestamps serialise as RFC3339 strings (default JSON tag).
type groupDTO struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	MemberCount int    `json:"member_count"`
	CreatedBy   string `json:"created_by,omitempty"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

type groupsResponse struct {
	Groups []groupDTO `json:"groups"`
}

type groupWriteRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// Groups handles GET /api/v1/admin/groups — lists all groups.
func (h *Handler) Groups(w http.ResponseWriter, r *http.Request) {
	if !methodGET(w, r) {
		return
	}
	rows, err := h.store.ListGroups(r.Context())
	if err != nil {
		h.log.Error("admin groups: list", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	out := make([]groupDTO, 0, len(rows))
	for _, g := range rows {
		out = append(out, toGroupDTO(g))
	}
	writeJSON(w, groupsResponse{Groups: out})
}

// CreateGroup handles POST /api/v1/admin/groups.
func (h *Handler) CreateGroup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	req, ok := decodeGroupWrite(w, r)
	if !ok {
		return
	}
	var createdBy *uuid.UUID
	if u, ok := authapi.UserFromContext(r.Context()); ok {
		id := u.ID
		createdBy = &id
	}
	g, err := h.store.InsertGroup(r.Context(), store.GroupInput{
		Name:        req.Name,
		Description: req.Description,
		CreatedBy:   createdBy,
	})
	if err != nil {
		// Unique-name violation surfaces as a postgres code; the
		// store wraps it, so look for the name substring to keep
		// the error message useful in the UI without leaking a
		// raw SQL error.
		if strings.Contains(err.Error(), "groups_name_key") {
			http.Error(w, "group name already exists", http.StatusConflict)
			return
		}
		h.log.Error("admin groups: create", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	audit.Emit(r.Context(), h.log, h.store,
		store.AuditActionGroupCreate, "group", g.ID.String(),
		map[string]any{"name": g.Name})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(toGroupDTO(g))
}

// UpdateGroup handles PUT /api/v1/admin/groups/{id}.
func (h *Handler) UpdateGroup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		w.Header().Set("Allow", "PUT")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id, ok := parseGroupID(w, r)
	if !ok {
		return
	}
	req, ok := decodeGroupWrite(w, r)
	if !ok {
		return
	}
	if err := h.store.UpdateGroup(r.Context(), id, store.GroupInput{
		Name:        req.Name,
		Description: req.Description,
	}); err != nil {
		if strings.Contains(err.Error(), "groups_name_key") {
			http.Error(w, "group name already exists", http.StatusConflict)
			return
		}
		h.log.Error("admin groups: update", "id", id, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	audit.Emit(r.Context(), h.log, h.store,
		store.AuditActionGroupUpdate, "group", id.String(),
		map[string]any{"name": req.Name})
	w.WriteHeader(http.StatusNoContent)
}

// DeleteGroup handles DELETE /api/v1/admin/groups/{id}. Cascades
// group_members via FK; approval gates that reference the group
// by NAME keep their reference (dangling group_name becomes a
// no-op at decision time, same as deleting a user mid-gate).
func (h *Handler) DeleteGroup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		w.Header().Set("Allow", "DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id, ok := parseGroupID(w, r)
	if !ok {
		return
	}
	if err := h.store.DeleteGroup(r.Context(), id); err != nil {
		h.log.Error("admin groups: delete", "id", id, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	audit.Emit(r.Context(), h.log, h.store,
		store.AuditActionGroupDelete, "group", id.String(), nil)
	w.WriteHeader(http.StatusNoContent)
}

type groupMemberDTO struct {
	UserID  string `json:"user_id"`
	Email   string `json:"email"`
	Name    string `json:"name"`
	Role    string `json:"role"`
	AddedAt string `json:"added_at"`
}

type groupMembersResponse struct {
	Members []groupMemberDTO `json:"members"`
}

// GroupMembers handles GET /api/v1/admin/groups/{id}/members.
func (h *Handler) GroupMembers(w http.ResponseWriter, r *http.Request) {
	if !methodGET(w, r) {
		return
	}
	id, ok := parseGroupID(w, r)
	if !ok {
		return
	}
	members, err := h.store.ListGroupMembers(r.Context(), id)
	if err != nil {
		h.log.Error("admin groups: list members", "id", id, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	out := make([]groupMemberDTO, 0, len(members))
	for _, m := range members {
		out = append(out, groupMemberDTO{
			UserID:  m.UserID.String(),
			Email:   m.Email,
			Name:    m.Name,
			Role:    m.Role,
			AddedAt: m.AddedAt.Format("2006-01-02T15:04:05Z07:00"),
		})
	}
	writeJSON(w, groupMembersResponse{Members: out})
}

type addMemberRequest struct {
	UserID string `json:"user_id"`
}

// AddGroupMember handles POST /api/v1/admin/groups/{id}/members.
func (h *Handler) AddGroupMember(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	groupID, ok := parseGroupID(w, r)
	if !ok {
		return
	}
	var req addMemberRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	userID, err := uuid.Parse(req.UserID)
	if err != nil {
		http.Error(w, "invalid user_id", http.StatusBadRequest)
		return
	}
	var addedBy *uuid.UUID
	if u, ok := authapi.UserFromContext(r.Context()); ok {
		id := u.ID
		addedBy = &id
	}
	if err := h.store.AddGroupMember(r.Context(), groupID, userID, addedBy); err != nil {
		if errors.Is(err, store.ErrGroupNotFound) {
			http.Error(w, "group not found", http.StatusNotFound)
			return
		}
		h.log.Error("admin groups: add member", "group_id", groupID, "user_id", userID, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	audit.Emit(r.Context(), h.log, h.store,
		store.AuditActionGroupMemberAdd, "group", groupID.String(),
		map[string]any{"user_id": userID.String()})
	w.WriteHeader(http.StatusNoContent)
}

// RemoveGroupMember handles DELETE /api/v1/admin/groups/{id}/members/{user_id}.
func (h *Handler) RemoveGroupMember(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		w.Header().Set("Allow", "DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	groupID, ok := parseGroupID(w, r)
	if !ok {
		return
	}
	userID, err := uuid.Parse(chi.URLParam(r, "user_id"))
	if err != nil {
		http.Error(w, "invalid user_id", http.StatusBadRequest)
		return
	}
	if err := h.store.RemoveGroupMember(r.Context(), groupID, userID); err != nil {
		h.log.Error("admin groups: remove member", "group_id", groupID, "user_id", userID, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	audit.Emit(r.Context(), h.log, h.store,
		store.AuditActionGroupMemberRemove, "group", groupID.String(),
		map[string]any{"user_id": userID.String()})
	w.WriteHeader(http.StatusNoContent)
}

// --- helpers ---

func parseGroupID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid group id", http.StatusBadRequest)
		return uuid.Nil, false
	}
	return id, true
}

func decodeGroupWrite(w http.ResponseWriter, r *http.Request) (groupWriteRequest, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	var req groupWriteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return req, false
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return req, false
	}
	// Simple validity rule: name is used in YAML approval gates as
	// a string — match the identifier-ish constraints YAML users
	// expect. Allow dashes, underscores, dots; reject whitespace
	// and special chars that'd require quoting.
	for _, r := range req.Name {
		if !(r == '-' || r == '_' || r == '.' ||
			(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9')) {
			http.Error(w, "name may only contain letters, digits, dash, underscore, dot", http.StatusBadRequest)
			return req, false
		}
	}
	return req, true
}

func toGroupDTO(g store.Group) groupDTO {
	dto := groupDTO{
		ID:          g.ID.String(),
		Name:        g.Name,
		Description: g.Description,
		MemberCount: g.MemberCount,
		CreatedAt:   g.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
		UpdatedAt:   g.UpdatedAt.Format("2006-01-02T15:04:05Z07:00"),
	}
	if g.CreatedBy != nil {
		dto.CreatedBy = g.CreatedBy.String()
	}
	return dto
}
