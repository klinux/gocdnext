package projects

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/gocdnext/gocdnext/server/internal/api/authapi"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// deployWatchDTO is one in-flight native deploy for the live-status endpoint. The
// live-state fields are viewer-readable; the config fields (application/cluster/
// sync_mode) are MAINTAINER-only — the same fields protected on /deploy-targets — and
// omitted for viewers via omitempty.
type deployWatchDTO struct {
	Environment      string     `json:"environment"`
	Version          string     `json:"version"`
	ExpectedRevision string     `json:"expected_revision"`
	WatchStartedAt   time.Time  `json:"watch_started_at"`
	SyncRequestedAt  *time.Time `json:"sync_requested_at,omitempty"`
	DeadlineAt       time.Time  `json:"deadline_at"`
	DegradedSince    *time.Time `json:"degraded_since,omitempty"`

	// Maintainer-only config.
	Application string `json:"application,omitempty"`
	Cluster     string `json:"cluster,omitempty"`
	SyncMode    string `json:"sync_mode,omitempty"`
}

// ListDeployWatches returns the project's in-flight native deploys for the UI to poll.
// Viewer-readable (route is in the authenticated group), but the response is
// role-sanitised: only maintainers get the config fields, so this endpoint can't leak
// what /deploy-targets protects.
func (h *Handler) ListDeployWatches(w http.ResponseWriter, r *http.Request) {
	projectID, ok := h.resolveProjectID(w, r, chi.URLParam(r, "slug"))
	if !ok {
		return
	}
	// showConfig defaults true so an auth-disabled deployment (no user in context)
	// sees everything; with auth on, RequireAuth guarantees a user and the role gates it.
	showConfig := true
	if u, ok := authapi.UserFromContext(r.Context()); ok {
		showConfig = store.RoleSatisfies(u.Role, store.RoleMaintainer)
	}

	watches, err := h.store.ListDeployWatchesForProject(r.Context(), projectID)
	if err != nil {
		h.log.Error("deploy watches: list", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	dtos := make([]deployWatchDTO, 0, len(watches))
	for _, wch := range watches {
		d := deployWatchDTO{
			Environment:      wch.Environment,
			Version:          wch.Version,
			ExpectedRevision: wch.ExpectedRevision,
			WatchStartedAt:   wch.WatchStartedAt,
			SyncRequestedAt:  wch.SyncRequestedAt,
			DeadlineAt:       wch.DeadlineAt,
			DegradedSince:    wch.DegradedSince,
		}
		if showConfig {
			d.Application = wch.Application
			d.Cluster = wch.Cluster
			d.SyncMode = wch.SyncMode
		}
		dtos = append(dtos, d)
	}
	writeJSON(w, http.StatusOK, map[string]any{"deploy_watches": dtos})
}
