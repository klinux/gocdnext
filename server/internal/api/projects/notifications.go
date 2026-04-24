package projects

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/gocdnext/gocdnext/server/internal/audit"
	"github.com/gocdnext/gocdnext/server/internal/domain"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// Max body for a notifications PUT. Comfortably fits a few
// dozen entries with substantial `with:` maps; anything much
// larger is a config-as-code smell the user should split.
const maxNotificationsBytes = 128 << 10 // 128 KiB

// notificationDTO is the wire shape for one entry. Flat mirror
// of domain.Notification with JSON tags so the field names read
// the same as in the YAML (`on`, `uses`, `with`, `secrets`).
type notificationDTO struct {
	On      string            `json:"on"`
	Uses    string            `json:"uses"`
	With    map[string]string `json:"with,omitempty"`
	Secrets []string          `json:"secrets,omitempty"`
}

type notificationsResponse struct {
	Notifications []notificationDTO `json:"notifications"`
}

type notificationsRequest struct {
	Notifications []notificationDTO `json:"notifications"`
}

// ListNotifications handles GET /api/v1/projects/{slug}/notifications.
// Returns the project-level list — empty list when the project
// has none. Pipelines inherit this at run-create time when they
// don't declare their own `notifications:` block.
func (h *Handler) ListNotifications(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	if slug == "" {
		http.Error(w, "slug is required", http.StatusBadRequest)
		return
	}

	detail, err := h.store.GetProjectDetail(r.Context(), slug, 1)
	if err != nil {
		if errors.Is(err, store.ErrProjectNotFound) {
			http.Error(w, "project not found", http.StatusNotFound)
			return
		}
		h.log.Error("list notifications: load project", "slug", slug, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	ns, err := h.store.GetProjectNotifications(r.Context(), detail.Project.ID)
	if err != nil {
		h.log.Error("list notifications", "slug", slug, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	out := make([]notificationDTO, 0, len(ns))
	for _, n := range ns {
		out = append(out, notificationDTO{
			On:      string(n.On),
			Uses:    n.Uses,
			With:    n.With,
			Secrets: n.Secrets,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(notificationsResponse{Notifications: out})
}

// SetNotifications handles PUT /api/v1/projects/{slug}/notifications.
// Body: { "notifications": [...] } — replaces the entire list.
// Passing an empty array clears the project-level entries.
func (h *Handler) SetNotifications(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	if slug == "" {
		http.Error(w, "slug is required", http.StatusBadRequest)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxNotificationsBytes)
	var req notificationsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}

	parsed, err := parseNotificationDTOs(req.Notifications)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Validate each entry against the plugin catalog the same way
	// ApplyProject validates pipeline-level notifications, so a
	// typo in `with:` fails here instead of hiding until the next
	// run spawns a synth job with a ghost PLUGIN_* env.
	if h.pluginCatalog != nil {
		for i, n := range parsed {
			if err := h.pluginCatalog.Validate(n.Uses, n.With); err != nil {
				http.Error(w,
					fmt.Sprintf("notifications[%d]: %s", i, err.Error()),
					http.StatusBadRequest)
				return
			}
		}
	}

	detail, err := h.store.GetProjectDetail(r.Context(), slug, 1)
	if err != nil {
		if errors.Is(err, store.ErrProjectNotFound) {
			http.Error(w, "project not found", http.StatusNotFound)
			return
		}
		h.log.Error("set notifications: load project", "slug", slug, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	if err := h.store.SetProjectNotifications(r.Context(), detail.Project.ID, parsed); err != nil {
		h.log.Error("set notifications", "slug", slug, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	h.log.Info("project notifications updated", "slug", slug, "count", len(parsed))
	audit.Emit(r.Context(), h.log, h.store,
		store.AuditActionProjectNotifsSet, "project", slug,
		map[string]any{"slug": slug, "count": len(parsed)})

	w.WriteHeader(http.StatusNoContent)
}

// parseNotificationDTOs runs the same validation the YAML parser
// applies (known trigger, non-empty uses) so the API layer can
// reject garbage before it hits the store. Returns a clean
// domain.Notification slice with normalized triggers.
func parseNotificationDTOs(in []notificationDTO) ([]domain.Notification, error) {
	out := make([]domain.Notification, 0, len(in))
	allowed := map[string]domain.NotificationTrigger{
		"failure":  domain.NotifyOnFailure,
		"success":  domain.NotifyOnSuccess,
		"always":   domain.NotifyOnAlways,
		"canceled": domain.NotifyOnCanceled,
	}
	for i, n := range in {
		trig, ok := allowed[n.On]
		if !ok {
			return nil, fmt.Errorf(
				"notifications[%d]: unknown on %q (allowed: failure, success, always, canceled)",
				i, n.On)
		}
		if n.Uses == "" {
			return nil, fmt.Errorf("notifications[%d]: `uses:` is required", i)
		}
		out = append(out, domain.Notification{
			On:      trig,
			Uses:    n.Uses,
			With:    n.With,
			Secrets: append([]string(nil), n.Secrets...),
		})
	}
	return out, nil
}
