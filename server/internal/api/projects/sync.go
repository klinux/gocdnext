package projects

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/gocdnext/gocdnext/server/internal/configsync"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// Sync handles POST /api/v1/projects/{slug}/sync. It re-reads
// `.gocdnext/` from the project's already-bound scm_source at the
// default branch HEAD and runs the same ApplyProject flow the CLI
// drives — catching up the server state with the repo without
// waiting for a push webhook.
//
// Auth-gated (same middleware as Apply). Gated on:
//   - project must exist                            (404)
//   - project must have an scm_source bound         (409)
//   - server must have a configsync.Fetcher wired   (503)
//
// Response mirrors Apply's — pipelines applied/removed, warnings
// for reachable-but-empty folders. Intentionally does NOT rotate
// or even touch the webhook secret; SCM source stays as-is.
func (h *Handler) Sync(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	slug := chi.URLParam(r, "slug")
	if slug == "" {
		http.Error(w, "slug is required", http.StatusBadRequest)
		return
	}
	if h.fetcher == nil {
		http.Error(w, "server has no config fetcher wired", http.StatusServiceUnavailable)
		return
	}

	detail, err := h.store.GetProjectDetail(r.Context(), slug, 0)
	if err != nil {
		if errors.Is(err, store.ErrProjectNotFound) {
			http.Error(w, "project not found", http.StatusNotFound)
			return
		}
		h.log.Error("sync: load project", "slug", slug, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if detail.SCMSource == nil {
		http.Error(w, "project has no scm_source bound — connect a repo first", http.StatusConflict)
		return
	}

	ref := detail.SCMSource.DefaultBranch
	if ref == "" {
		ref = "main"
	}
	remote := store.SCMSource{
		Provider:      detail.SCMSource.Provider,
		URL:           detail.SCMSource.URL,
		DefaultBranch: ref,
		AuthRef:       detail.SCMSource.AuthRef,
	}
	configPath := detail.Project.ConfigPath

	var warnings []string
	files, ferr := h.fetcher.Fetch(r.Context(), remote, ref, configPath)
	switch {
	case errors.Is(ferr, configsync.ErrFolderNotFound):
		warnings = append(warnings, fmt.Sprintf(
			"config folder %q not found at %s@%s — nothing to sync",
			displayConfigPath(configPath), remote.URL, ref))
		// Fall through to ApplyProject with zero pipelines: removes
		// any stale pipelines that were added by a previous push.
	case ferr != nil:
		h.log.Warn("sync: fetch failed",
			"slug", slug, "url", remote.URL, "err", ferr)
		http.Error(w, "fetch from repo: "+ferr.Error(), http.StatusBadGateway)
		return
	}

	parsed, perr := configsync.ParseFiles(files)
	if perr != nil {
		http.Error(w, "parse remote .gocdnext/: "+perr.Error(), http.StatusUnprocessableEntity)
		return
	}
	if len(parsed) == 0 && len(warnings) == 0 {
		warnings = append(warnings, fmt.Sprintf(
			"no YAML files in %q at %s@%s",
			displayConfigPath(configPath), remote.URL, ref))
	}

	// Same implicit-material synthesis the Apply path does: the
	// project is already bound (detail.SCMSource is non-nil or we'd
	// have 409'd above), so synthesize a git material for every
	// pipeline that doesn't already declare one for this repo.
	injectImplicitProjectMaterial(parsed, &store.SCMSourceInput{
		Provider:      detail.SCMSource.Provider,
		URL:           detail.SCMSource.URL,
		DefaultBranch: detail.SCMSource.DefaultBranch,
		AuthRef:       detail.SCMSource.AuthRef,
	})

	result, err := h.store.ApplyProject(r.Context(), store.ApplyProjectInput{
		Slug:        slug,
		Name:        detail.Project.Name,
		Description: detail.Project.Description,
		ConfigPath:  configPath,
		Pipelines:   parsed,
		// Intentionally no SCMSource — sync preserves the binding
		// without risking a webhook rotation.
	})
	if err != nil {
		h.log.Error("sync: apply project", "slug", slug, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	resp := ApplyResponse{
		ProjectID:        result.ProjectID.String(),
		ProjectCreated:   false, // sync never creates a project
		PipelinesRemoved: result.PipelinesRemoved,
		Warnings:         warnings,
	}
	for _, p := range result.Pipelines {
		resp.Pipelines = append(resp.Pipelines, ApplyPipeline{
			Name:             p.Name,
			PipelineID:       p.PipelineID.String(),
			Created:          p.Created,
			MaterialsAdded:   p.MaterialsAdded,
			MaterialsRemoved: p.MaterialsRemoved,
		})
	}

	h.log.Info("sync project",
		"slug", slug,
		"pipelines", len(result.Pipelines),
		"pipelines_removed", len(result.PipelinesRemoved),
		"revision", ref)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}
