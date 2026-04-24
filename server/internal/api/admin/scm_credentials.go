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

// scmCredentialDTO is the wire shape. AuthRefPreview is the
// first + last few chars of the plaintext so the UI can render
// "glpat-…abc1" without flashing the full token; the full
// plaintext never leaves the server.
type scmCredentialDTO struct {
	ID              string `json:"id"`
	Provider        string `json:"provider"`
	Host            string `json:"host"`
	APIBase         string `json:"api_base,omitempty"`
	DisplayName     string `json:"display_name,omitempty"`
	AuthRefPreview  string `json:"auth_ref_preview,omitempty"`
	CreatedAt       string `json:"created_at"`
	UpdatedAt       string `json:"updated_at"`
}

type scmCredentialsResponse struct {
	Credentials []scmCredentialDTO `json:"credentials"`
}

type scmCredentialWriteRequest struct {
	Provider    string `json:"provider"`
	Host        string `json:"host"`
	APIBase     string `json:"api_base,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
	AuthRef     string `json:"auth_ref"`
}

// SCMCredentials handles GET /api/v1/admin/scm-credentials.
func (h *Handler) SCMCredentials(w http.ResponseWriter, r *http.Request) {
	if !methodGET(w, r) {
		return
	}
	rows, err := h.store.ListSCMCredentials(r.Context())
	if err != nil {
		if errors.Is(err, store.ErrAuthProviderCipherUnset) {
			http.Error(w, "GOCDNEXT_SECRET_KEY must be set to manage credentials", http.StatusServiceUnavailable)
			return
		}
		h.log.Error("admin scm_credentials: list", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	out := make([]scmCredentialDTO, 0, len(rows))
	for _, c := range rows {
		out = append(out, toSCMCredentialDTO(c))
	}
	writeJSON(w, scmCredentialsResponse{Credentials: out})
}

// UpsertSCMCredential handles POST /api/v1/admin/scm-credentials.
// Upsert-on-(provider, host) — PUT-by-id wasn't worth a separate
// route since the conflict key is the natural identifier.
func (h *Handler) UpsertSCMCredential(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	var req scmCredentialWriteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}
	req.Provider = strings.TrimSpace(req.Provider)
	req.Host = strings.TrimSpace(req.Host)
	req.AuthRef = strings.TrimSpace(req.AuthRef)
	if req.Provider != "gitlab" && req.Provider != "bitbucket" {
		http.Error(w, "provider must be gitlab or bitbucket", http.StatusBadRequest)
		return
	}
	if req.Host == "" {
		http.Error(w, "host is required", http.StatusBadRequest)
		return
	}
	if req.AuthRef == "" {
		http.Error(w, "auth_ref is required", http.StatusBadRequest)
		return
	}

	var createdBy *uuid.UUID
	if u, ok := authapi.UserFromContext(r.Context()); ok {
		id := u.ID
		createdBy = &id
	}
	cred, err := h.store.UpsertSCMCredential(r.Context(), store.SCMCredentialInput{
		Provider:    req.Provider,
		Host:        req.Host,
		APIBase:     req.APIBase,
		DisplayName: req.DisplayName,
		AuthRef:     req.AuthRef,
		CreatedBy:   createdBy,
	})
	if err != nil {
		if errors.Is(err, store.ErrAuthProviderCipherUnset) {
			http.Error(w, "GOCDNEXT_SECRET_KEY must be set", http.StatusServiceUnavailable)
			return
		}
		h.log.Error("admin scm_credentials: upsert", "err", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	audit.Emit(r.Context(), h.log, h.store,
		store.AuditActionSCMCredentialSet, "scm_credential", cred.ID.String(),
		map[string]any{"provider": cred.Provider, "host": cred.Host})

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	// Plaintext never rides back — return the row without AuthRef.
	_ = json.NewEncoder(w).Encode(toSCMCredentialDTO(cred))
}

// DeleteSCMCredential handles DELETE /api/v1/admin/scm-credentials/{id}.
func (h *Handler) DeleteSCMCredential(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		w.Header().Set("Allow", "DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid credential id", http.StatusBadRequest)
		return
	}
	if err := h.store.DeleteSCMCredential(r.Context(), id); err != nil {
		h.log.Error("admin scm_credentials: delete", "id", id, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	audit.Emit(r.Context(), h.log, h.store,
		store.AuditActionSCMCredentialDelete, "scm_credential", id.String(), nil)
	w.WriteHeader(http.StatusNoContent)
}

func toSCMCredentialDTO(c store.SCMCredential) scmCredentialDTO {
	return scmCredentialDTO{
		ID:             c.ID.String(),
		Provider:       c.Provider,
		Host:           c.Host,
		APIBase:        c.APIBase,
		DisplayName:    c.DisplayName,
		AuthRefPreview: previewToken(c.AuthRef),
		CreatedAt:      c.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
		UpdatedAt:      c.UpdatedAt.Format("2006-01-02T15:04:05Z07:00"),
	}
}

// previewToken renders a masked token hint like "glpat-…abc1" —
// enough for the admin to confirm which credential they're
// looking at without leaking usable bits.
func previewToken(raw string) string {
	if raw == "" {
		return ""
	}
	if len(raw) <= 8 {
		return strings.Repeat("•", len(raw))
	}
	return raw[:4] + "…" + raw[len(raw)-4:]
}
