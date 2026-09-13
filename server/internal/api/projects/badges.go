package projects

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"

	"github.com/go-chi/chi/v5"

	"github.com/gocdnext/gocdnext/server/internal/audit"
	badgetoken "github.com/gocdnext/gocdnext/server/internal/badges"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

type badgeConfigResponse struct {
	Enabled bool `json:"enabled"`
}

type badgeRotateResponse struct {
	Enabled  bool   `json:"enabled"`
	Token    string `json:"token"`
	BadgeURL string `json:"badge_url"`
	Markdown string `json:"markdown"`
}

// GetBadgeConfig handles GET /api/v1/projects/{slug}/badge.
func (h *Handler) GetBadgeConfig(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	if slug == "" {
		http.Error(w, "slug is required", http.StatusBadRequest)
		return
	}
	enabled, err := h.store.GetProjectBadgeEnabledBySlug(r.Context(), slug)
	if errors.Is(err, store.ErrProjectNotFound) {
		http.Error(w, "project not found", http.StatusNotFound)
		return
	}
	if err != nil {
		h.log.Error("get badge config", "slug", slug, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(badgeConfigResponse{Enabled: enabled})
}

// RotateBadgeToken handles POST /api/v1/projects/{slug}/badge/token.
// The plaintext token is returned once and only its hash is stored.
func (h *Handler) RotateBadgeToken(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	if slug == "" {
		http.Error(w, "slug is required", http.StatusBadRequest)
		return
	}
	token, err := badgetoken.NewToken()
	if err != nil {
		h.log.Error("rotate badge token: random", "slug", slug, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := h.store.SetProjectBadgeTokenHashBySlug(r.Context(), slug, badgetoken.HashToken(token)); err != nil {
		if errors.Is(err, store.ErrProjectNotFound) {
			http.Error(w, "project not found", http.StatusNotFound)
			return
		}
		h.log.Error("rotate badge token", "slug", slug, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	badgeURL := h.badgeURL(slug, "", token)
	audit.Emit(r.Context(), h.log, h.store,
		store.AuditActionProjectBadgeRotate, "project", slug,
		map[string]any{"slug": slug})

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(badgeRotateResponse{
		Enabled:  true,
		Token:    token,
		BadgeURL: badgeURL,
		Markdown: "![build](" + badgeURL + ")",
	})
}

// DisableBadgeToken handles DELETE /api/v1/projects/{slug}/badge/token.
func (h *Handler) DisableBadgeToken(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	if slug == "" {
		http.Error(w, "slug is required", http.StatusBadRequest)
		return
	}
	if err := h.store.ClearProjectBadgeTokenBySlug(r.Context(), slug); err != nil {
		if errors.Is(err, store.ErrProjectNotFound) {
			http.Error(w, "project not found", http.StatusNotFound)
			return
		}
		h.log.Error("disable badge token", "slug", slug, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	audit.Emit(r.Context(), h.log, h.store,
		store.AuditActionProjectBadgeDisable, "project", slug,
		map[string]any{"slug": slug})

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(badgeConfigResponse{Enabled: false})
}

func (h *Handler) badgeURL(slug, pipeline, token string) string {
	path := "/api/v1/badge/" + url.PathEscape(slug)
	if pipeline != "" {
		path += "/" + url.PathEscape(pipeline)
	}
	path += ".svg"
	q := url.Values{}
	q.Set("token", token)
	raw := path + "?" + q.Encode()
	if h.publicBase == "" {
		return raw
	}
	return h.publicBase + raw
}
