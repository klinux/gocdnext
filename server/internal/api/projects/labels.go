package projects

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/gocdnext/gocdnext/server/internal/audit"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// Bounds on a labels PUT — labels are a grouping primitive, not config; a
// handful per project is the norm. Reject oversized payloads loudly.
const (
	maxLabelsBytes = 16 << 10 // 16 KiB
	maxLabels      = 50
	maxLabelLen    = 100
)

type labelDTO struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type labelsBody struct {
	Labels []labelDTO `json:"labels"`
}

// ListLabels handles GET /api/v1/projects/{slug}/labels.
func (h *Handler) ListLabels(w http.ResponseWriter, r *http.Request) {
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
		h.log.Error("list labels: load project", "slug", slug, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	out := make([]labelDTO, 0, len(detail.Project.Labels))
	for _, l := range detail.Project.Labels {
		out = append(out, labelDTO{Key: l.Key, Value: l.Value})
	}
	writeJSON(w, labelsBody{Labels: out})
}

// SetLabels handles PUT /api/v1/projects/{slug}/labels — replaces the full set.
func (h *Handler) SetLabels(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	if slug == "" {
		http.Error(w, "slug is required", http.StatusBadRequest)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxLabelsBytes)
	var body labelsBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}

	labels, err := parseLabels(body.Labels)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	detail, err := h.store.GetProjectDetail(r.Context(), slug, 1)
	if err != nil {
		if errors.Is(err, store.ErrProjectNotFound) {
			http.Error(w, "project not found", http.StatusNotFound)
			return
		}
		h.log.Error("set labels: load project", "slug", slug, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	if err := h.store.ReplaceProjectLabels(r.Context(), detail.Project.ID, labels); err != nil {
		h.log.Error("set labels", "slug", slug, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	h.log.Info("project labels updated", "slug", slug, "count", len(labels))
	audit.Emit(r.Context(), h.log, h.store,
		store.AuditActionProjectLabelsSet, "project", slug,
		map[string]any{"slug": slug, "count": len(labels)})

	w.WriteHeader(http.StatusNoContent)
}

// parseLabels trims + validates the wire labels. Key is required; value may be
// empty (a flat tag). Bounds count + length so a project can't carry an
// unreasonable set. Trimming happens server-side (never on the project name).
func parseLabels(in []labelDTO) ([]store.ProjectLabel, error) {
	if len(in) > maxLabels {
		return nil, errors.New("too many labels")
	}
	out := make([]store.ProjectLabel, 0, len(in))
	for _, l := range in {
		key := strings.TrimSpace(l.Key)
		value := strings.TrimSpace(l.Value)
		if key == "" {
			return nil, errors.New("label key is required")
		}
		if len(key) > maxLabelLen || len(value) > maxLabelLen {
			return nil, errors.New("label key/value too long")
		}
		out = append(out, store.ProjectLabel{Key: key, Value: value})
	}
	return out, nil
}
