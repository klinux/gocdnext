package projects

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/gocdnext/gocdnext/server/internal/api/authapi"
	"github.com/gocdnext/gocdnext/server/internal/audit"
	"github.com/gocdnext/gocdnext/server/internal/checks"
	ghscm "github.com/gocdnext/gocdnext/server/internal/scm/github"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// errUnsupportedProvider marks a required-checks request against a non-GitHub
// project — v1 writes GitHub rulesets only.
var errUnsupportedProvider = errors.New("projects: required checks are GitHub-only")

type requiredChecksSync struct {
	Status     string     `json:"status"` // not_configured|pending|synced|failed|skipped
	RulesetID  *int64     `json:"ruleset_id,omitempty"`
	SyncedAt   *time.Time `json:"synced_at,omitempty"`
	Error      string     `json:"error,omitempty"`
	NeedsAdmin bool       `json:"needs_admin,omitempty"`
}

type requiredChecksResponse struct {
	Pipelines      []string           `json:"pipelines"`           // required-for-merge
	Available      []string           `json:"available_pipelines"` // PR-firing → selectable
	Provider       string             `json:"provider"`
	StatusContexts []string           `json:"status_contexts"` // the exact contexts required
	Sync           requiredChecksSync `json:"sync"`
}

type requiredChecksRequest struct {
	Pipelines []string `json:"pipelines"`
}

// GetRequiredChecks handles GET /api/v1/projects/{slug}/required-checks — the
// current required pipelines, the selectable (PR-firing) set, and the last-sync
// state, for the settings UI.
func (h *Handler) GetRequiredChecks(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	if slug == "" {
		http.Error(w, "slug is required", http.StatusBadRequest)
		return
	}
	resp, err := h.buildRequiredChecksResponse(r.Context(), slug)
	if err != nil {
		h.writeRequiredChecksErr(w, slug, "get required-checks", err)
		return
	}
	writeRequiredChecksJSON(w, resp)
}

// SetRequiredChecks handles PUT /api/v1/projects/{slug}/required-checks.
// Body: {"pipelines": ["build","e2e"]}. ADMIN ONLY (in-handler, matching the
// pr-head-config convention). Validates the pipelines against the project, then
// reconciles the dedicated GitHub ruleset. A sync failure keeps the config and
// surfaces an actionable state rather than a 500.
func (h *Handler) SetRequiredChecks(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	if slug == "" {
		http.Error(w, "slug is required", http.StatusBadRequest)
		return
	}
	if u, ok := authapi.UserFromContext(r.Context()); ok && !store.RoleSatisfies(u.Role, store.RoleAdmin) {
		http.Error(w, "changing required checks requires admin", http.StatusForbidden)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var req requiredChecksRequest
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if dec.More() {
		http.Error(w, "unexpected trailing content after the JSON object", http.StatusBadRequest)
		return
	}
	// Shape validation (bounds, duplicates, empty names) BEFORE any external
	// effect — a duplicate must never reach the GitHub payload only to fail at
	// the DB write afterwards.
	if err := (&store.RequiredChecksConfig{Pipelines: req.Pipelines}).Validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// Reject anything that would deadlock a merge (unknown / non-PR-firing
	// pipeline, or a project that emits no commit statuses).
	if err := h.store.ValidateRequiredPipelines(r.Context(), slug, req.Pipelines); err != nil {
		switch {
		case errors.Is(err, store.ErrProjectNotFound):
			http.Error(w, "project not found", http.StatusNotFound)
		case errors.Is(err, store.ErrRequiredCheckUnreportable):
			http.Error(w, err.Error(), http.StatusBadRequest)
		default:
			h.log.Error("required-checks validate", "slug", slug, "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
		}
		return
	}
	if err := h.reconcileRequiredChecks(r.Context(), slug, req.Pipelines); err != nil {
		h.writeRequiredChecksErr(w, slug, "reconcile required-checks", err)
		return
	}
	h.log.Info("project required-checks updated", "slug", slug, "count", len(req.Pipelines))
	audit.Emit(r.Context(), h.log, h.store,
		store.AuditActionProjectRequiredChecksSet, "project", slug,
		map[string]any{"slug": slug, "pipelines": req.Pipelines})

	resp, err := h.buildRequiredChecksResponse(r.Context(), slug)
	if err != nil {
		h.writeRequiredChecksErr(w, slug, "get required-checks", err)
		return
	}
	writeRequiredChecksJSON(w, resp)
}

// reconcileRequiredChecks persists the requested pipelines AND writes the
// dedicated GitHub ruleset to match — create/update by the stored ruleset id,
// tear it down when the list is empty. A GitHub error (notably a missing
// Administration:write, 403) is recorded as sync state (config preserved), not
// bubbled as a 500.
func (h *Handler) reconcileRequiredChecks(ctx context.Context, slug string, pipelines []string) error {
	existing, err := h.store.GetProjectRequiredChecks(ctx, slug)
	if err != nil {
		return err
	}
	var existingRuleset *int64
	if existing != nil {
		existingRuleset = existing.RulesetID
	}

	src, err := h.store.FindSCMSourceByProjectSlug(ctx, slug)
	if err != nil {
		return err // ErrSCMSourceNotFound → 400 in the error mapper
	}
	if src.Provider != "github" {
		return errUnsupportedProvider
	}

	now := time.Now().UTC()
	cfg := &store.RequiredChecksConfig{Pipelines: pipelines, RulesetID: existingRuleset, SyncedAt: &now}

	app := h.autoRegister.currentApp() // nil-safe on a nil receiver
	owner, repo, perr := ghscm.ParseRepoURL(src.URL)
	if app == nil || perr != nil {
		cfg.SyncStatus = store.RequiredChecksSkipped
		if app == nil {
			cfg.SyncError = "no GitHub App configured on this server"
		} else {
			cfg.SyncError = "repo URL is not a GitHub repo: " + src.URL
		}
		return h.store.SaveProjectRequiredChecks(ctx, slug, cfg)
	}
	installID, ierr := app.InstallationID(ctx, owner, repo)
	if errors.Is(ierr, ghscm.ErrNoInstallation) {
		cfg.SyncStatus = store.RequiredChecksSkipped
		cfg.SyncError = "gocdnext App is not installed on " + owner + "/" + repo
		return h.store.SaveProjectRequiredChecks(ctx, slug, cfg)
	}
	if ierr != nil {
		return h.saveSyncFailure(ctx, slug, cfg, ierr)
	}

	// No required pipelines: tear the ruleset down and clear the feature.
	if len(pipelines) == 0 {
		if existingRuleset != nil {
			if derr := app.DeleteRuleset(ctx, installID, owner, repo, *existingRuleset); derr != nil {
				return h.saveSyncFailure(ctx, slug, cfg, derr)
			}
		}
		return h.store.ClearProjectRequiredChecks(ctx, slug)
	}

	contexts := make([]string, 0, len(pipelines))
	for _, p := range pipelines {
		contexts = append(contexts, checks.StatusContext(slug, p))
	}
	id, uerr := app.UpsertRequiredChecksRuleset(ctx, installID, ghscm.RulesetInput{
		Owner: owner, Repo: repo, Contexts: contexts,
		Name: ghscm.RequiredChecksRulesetName(slug),
	}, existingRuleset)
	if uerr != nil {
		return h.saveSyncFailure(ctx, slug, cfg, uerr)
	}
	cfg.RulesetID = &id
	cfg.SyncStatus = store.RequiredChecksSynced
	return h.store.SaveProjectRequiredChecks(ctx, slug, cfg)
}

// ReconcileRequiredChecksAfterApply re-syncs the required-checks ruleset after a
// project apply that may have changed the pipeline set (rename/remove, an events
// or branch change, a newly-added path filter). It DROPS any required pipeline
// no longer eligible and rewrites the ruleset, so GitHub never keeps requiring a
// context that can no longer be posted. Best-effort: it never fails the apply —
// errors are logged and recorded as sync state.
func (h *Handler) ReconcileRequiredChecksAfterApply(ctx context.Context, slug string) {
	cfg, err := h.store.GetProjectRequiredChecks(ctx, slug)
	if err != nil || cfg == nil || len(cfg.Pipelines) == 0 {
		return
	}
	eligible, err := h.store.ListPRFiringPipelineNames(ctx, slug)
	if err != nil {
		h.log.Warn("required-checks post-apply: list eligible", "slug", slug, "err", err)
		return
	}
	set := make(map[string]struct{}, len(eligible))
	for _, n := range eligible {
		set[n] = struct{}{}
	}
	surviving := make([]string, 0, len(cfg.Pipelines))
	for _, n := range cfg.Pipelines {
		if _, ok := set[n]; ok {
			surviving = append(surviving, n)
		}
	}
	if len(surviving) == len(cfg.Pipelines) {
		return // nothing dropped — the ruleset already matches
	}
	h.log.Info("required-checks post-apply prune",
		"slug", slug, "before", len(cfg.Pipelines), "after", len(surviving))
	if err := h.reconcileRequiredChecks(ctx, slug, surviving); err != nil {
		h.log.Warn("required-checks post-apply reconcile failed", "slug", slug, "err", err)
	}
}

// saveSyncFailure records a reconcile error as sync state, flagging the
// missing-admin case so the UI can show the re-approve hint.
func (h *Handler) saveSyncFailure(ctx context.Context, slug string, cfg *store.RequiredChecksConfig, cause error) error {
	cfg.SyncStatus = store.RequiredChecksFailed
	cfg.SyncError = cause.Error()
	cfg.NeedsAdmin = errors.Is(cause, ghscm.ErrAppLacksAdmin)
	h.log.Warn("required-checks sync failed", "slug", slug, "err", cause)
	return h.store.SaveProjectRequiredChecks(ctx, slug, cfg)
}

func (h *Handler) buildRequiredChecksResponse(ctx context.Context, slug string) (requiredChecksResponse, error) {
	cfg, err := h.store.GetProjectRequiredChecks(ctx, slug)
	if err != nil {
		return requiredChecksResponse{}, err
	}
	available, err := h.store.ListPRFiringPipelineNames(ctx, slug)
	if err != nil {
		return requiredChecksResponse{}, err
	}
	provider := ""
	if src, serr := h.store.FindSCMSourceByProjectSlug(ctx, slug); serr == nil {
		provider = src.Provider
	}
	resp := requiredChecksResponse{
		Pipelines:      []string{},
		Available:      available,
		Provider:       provider,
		StatusContexts: []string{},
		Sync:           requiredChecksSync{Status: "not_configured"},
	}
	if resp.Available == nil {
		resp.Available = []string{}
	}
	if cfg != nil {
		if cfg.Pipelines != nil {
			resp.Pipelines = cfg.Pipelines
		}
		for _, p := range resp.Pipelines {
			resp.StatusContexts = append(resp.StatusContexts, checks.StatusContext(slug, p))
		}
		status := cfg.SyncStatus
		if status == "" {
			status = "not_configured"
		}
		resp.Sync = requiredChecksSync{
			Status:     status,
			RulesetID:  cfg.RulesetID,
			SyncedAt:   cfg.SyncedAt,
			Error:      cfg.SyncError,
			NeedsAdmin: cfg.NeedsAdmin,
		}
	}
	return resp, nil
}

func (h *Handler) writeRequiredChecksErr(w http.ResponseWriter, slug, op string, err error) {
	switch {
	case errors.Is(err, store.ErrProjectNotFound):
		http.Error(w, "project not found", http.StatusNotFound)
	case errors.Is(err, store.ErrSCMSourceNotFound):
		http.Error(w, "project has no SCM source; required checks need a GitHub repo binding", http.StatusBadRequest)
	case errors.Is(err, errUnsupportedProvider):
		http.Error(w, "required checks are GitHub-only in this version", http.StatusBadRequest)
	default:
		h.log.Error(op, "slug", slug, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

func writeRequiredChecksJSON(w http.ResponseWriter, resp requiredChecksResponse) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}
