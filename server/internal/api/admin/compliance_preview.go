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
	Name          string         `json:"name"`
	SystemManaged bool           `json:"system_managed"`
	Raw           pipelineDefDTO `json:"raw"`
	Effective     pipelineDefDTO `json:"effective"`
}

// pipelineDefDTO is the minimal pipeline shape the preview renders: the stage
// order plus each job's name + stage. Deliberately NOT domain.Pipeline — the
// admin API shouldn't leak the internal Go struct's capitalised field names or
// its full surface, and the preview only needs these fields. Empty collections
// serialise as `[]`, not `null`, so clients need no null handling.
type pipelineDefDTO struct {
	Stages []string         `json:"stages"`
	Jobs   []pipelineJobDTO `json:"jobs"`
}

type pipelineJobDTO struct {
	Name  string `json:"name"`
	Stage string `json:"stage"`
}

func toEffectivePipelineDTO(v store.EffectivePipelineView) effectivePipelineDTO {
	return effectivePipelineDTO{
		Name:          v.Name,
		SystemManaged: v.SystemManaged,
		Raw:           toPipelineDefDTO(v.Raw),
		Effective:     toPipelineDefDTO(v.Effective),
	}
}

func toPipelineDefDTO(p domain.Pipeline) pipelineDefDTO {
	stages := make([]string, 0, len(p.Stages))
	stages = append(stages, p.Stages...)
	jobs := make([]pipelineJobDTO, 0, len(p.Jobs))
	for _, j := range p.Jobs {
		jobs = append(jobs, pipelineJobDTO{Name: j.Name, Stage: j.Stage})
	}
	return pipelineDefDTO{Stages: stages, Jobs: jobs}
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
