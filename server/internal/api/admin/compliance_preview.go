package admin

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

// effectivePipelineDTO is one pipeline's raw (pre-policy) and effective
// (post-merge) definition for the preview panel. SystemManaged flags the
// server-owned synthetic `_compliance` pipeline.
type effectivePipelineDTO struct {
	Name          string          `json:"name"`
	SystemManaged bool            `json:"system_managed"`
	Raw           domain.Pipeline `json:"raw"`
	Effective     domain.Pipeline `json:"effective"`
}

func toEffectivePipelineDTO(v store.EffectivePipelineView) effectivePipelineDTO {
	return effectivePipelineDTO{
		Name: v.Name, SystemManaged: v.SystemManaged, Raw: v.Raw, Effective: v.Effective,
	}
}

// EffectivePipelinePreview handles
// GET /api/v1/admin/projects/{slug}/effective-pipeline.
//
// Read-only. Without a `frameworks` query param it returns the stored effective
// definition every run already uses (a plain read). With `?frameworks=a,b` it
// returns a what-if recompute under that hypothetical framework set — nothing is
// persisted. `?frameworks=` (present but empty) previews "no frameworks".
func (h *Handler) EffectivePipelinePreview(w http.ResponseWriter, r *http.Request) {
	if !methodGET(w, r) {
		return
	}
	slug := chi.URLParam(r, "slug")

	var whatIf *[]string
	if q := r.URL.Query(); q.Has("frameworks") {
		ids := splitFrameworksParam(q.Get("frameworks"))
		// Reject malformed ids with a clean 400 rather than letting the store
		// surface an opaque 500.
		for _, id := range ids {
			if _, err := uuid.Parse(id); err != nil {
				http.Error(w, "invalid framework id", http.StatusBadRequest)
				return
			}
		}
		whatIf = &ids
	}

	views, err := h.store.PreviewEffectivePipelines(r.Context(), slug, whatIf)
	if err != nil {
		h.writeComplianceErr(w, "effective pipeline preview", err)
		return
	}
	out := make([]effectivePipelineDTO, 0, len(views))
	for _, v := range views {
		out = append(out, toEffectivePipelineDTO(v))
	}
	writeJSON(w, out)
}

// splitFrameworksParam parses a comma-separated framework-id list, trimming
// whitespace and dropping empty entries (so "a,,b " yields ["a","b"]).
func splitFrameworksParam(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
