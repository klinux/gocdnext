package projects

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/gocdnext/gocdnext/server/internal/store"
)

var validFindingSeverities = map[string]bool{
	"critical": true, "high": true, "medium": true, "low": true,
}

const (
	defaultFindingsLimit = 50
	maxFindingsLimit     = 200
	maxFindingFilterLen  = 200
)

// ListFindings handles GET /api/v1/projects/{slug}/findings — the security
// findings from the latest run per pipeline, filterable by severity/tool/rule,
// paginated (#71).
func (h *Handler) ListFindings(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	if slug == "" {
		http.Error(w, "slug is required", http.StatusBadRequest)
		return
	}

	q := r.URL.Query()
	severity := strings.TrimSpace(q.Get("severity"))
	if severity != "" && !validFindingSeverities[severity] {
		http.Error(w, "invalid severity (critical|high|medium|low)", http.StatusBadRequest)
		return
	}
	tool := strings.TrimSpace(q.Get("tool"))
	rule := strings.TrimSpace(q.Get("rule"))
	if len(tool) > maxFindingFilterLen || len(rule) > maxFindingFilterLen {
		http.Error(w, "filter too long", http.StatusBadRequest)
		return
	}

	limit := defaultFindingsLimit
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			http.Error(w, "invalid limit", http.StatusBadRequest)
			return
		}
		if n > maxFindingsLimit {
			n = maxFindingsLimit
		}
		limit = n
	}
	offset := 0
	if v := q.Get("offset"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			http.Error(w, "invalid offset", http.StatusBadRequest)
			return
		}
		offset = n
	}
	// include_resolved reveals dismissed + false_positive; default lists only
	// open + accepted. Any non-empty truthy value enables it.
	includeResolved := q.Get("include_resolved") == "1" || q.Get("include_resolved") == "true"

	detail, err := h.store.GetProjectDetail(r.Context(), slug, 1)
	if err != nil {
		if errors.Is(err, store.ErrProjectNotFound) {
			http.Error(w, "project not found", http.StatusNotFound)
			return
		}
		h.log.Error("list findings: load project", "slug", slug, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	page, err := h.store.FindingsForProject(r.Context(), detail.Project.ID, store.FindingsFilter{
		Severity:        severity,
		Tool:            tool,
		Rule:            rule,
		IncludeResolved: includeResolved,
		Limit:           int32(limit),
		Offset:          int32(offset),
	})
	if err != nil {
		h.log.Error("list findings", "slug", slug, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(page)
}
