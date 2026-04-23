// Package projects exposes the HTTP endpoints that manage project/pipeline
// definitions. The apply endpoint accepts the contents of a `.gocdnext/`
// folder and synchronizes the DB to match (upsert project+pipelines+materials,
// remove stale rows).
package projects

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/gocdnext/gocdnext/server/internal/artifacts"
	"github.com/gocdnext/gocdnext/server/internal/configsync"
	"github.com/gocdnext/gocdnext/server/internal/crypto"
	"github.com/gocdnext/gocdnext/server/internal/domain"
	"github.com/gocdnext/gocdnext/server/internal/parser"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

const maxApplyBodyBytes = 5 << 20 // 5 MiB — room for large mono-repo configs.

type Handler struct {
	store         *store.Store
	log           *slog.Logger
	cipher        *crypto.Cipher
	autoRegister  *AutoRegisterConfig
	fetcher       configsync.Fetcher
	artifactStore artifacts.Store
}

// WithArtifactStore enables the cache purge endpoint. Without it
// the endpoint returns 503 — a manual purge would delete the row
// but leave the blob orphaned, which is worse than just refusing.
func (h *Handler) WithArtifactStore(st artifacts.Store) *Handler {
	h.artifactStore = st
	return h
}

func NewHandler(s *store.Store, log *slog.Logger) *Handler {
	if log == nil {
		log = slog.Default()
	}
	return &Handler{store: s, log: log}
}

// WithConfigFetcher opts the apply handler into initial-sync
// behavior: when the request binds an scm_source and doesn't
// supply local pipeline files, the handler reads .gocdnext/ from
// the repo at the default branch and uses those files as the
// pipeline set. Leaving the fetcher nil keeps the legacy behavior
// (bind without pipelines; wait for a push to populate them).
func (h *Handler) WithConfigFetcher(f configsync.Fetcher) *Handler {
	h.fetcher = f
	return h
}

type ApplyFile struct {
	Name    string `json:"name"`
	Content string `json:"content"`
}

type ApplyRequest struct {
	Slug        string          `json:"slug"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	ConfigRepo  string          `json:"config_repo,omitempty"`
	// ConfigPath overrides the default ".gocdnext" folder where
	// pipelines live inside the repo. Leave empty to keep the
	// current value (or default on insert). Validated in the
	// handler against validConfigPath.
	ConfigPath string          `json:"config_path,omitempty"`
	Files      []ApplyFile     `json:"files"`
	SCMSource  *ApplySCMSource `json:"scm_source,omitempty"`
}

type ApplySCMSource struct {
	Provider      string `json:"provider"`
	URL           string `json:"url"`
	DefaultBranch string `json:"default_branch,omitempty"`
	WebhookSecret string `json:"webhook_secret,omitempty"`
	AuthRef       string `json:"auth_ref,omitempty"`
}

type ApplyPipeline struct {
	Name             string `json:"name"`
	PipelineID       string `json:"pipeline_id"`
	Created          bool   `json:"created"`
	MaterialsAdded   int    `json:"materials_added"`
	MaterialsRemoved int    `json:"materials_removed"`
}

type ApplySCMSourceResult struct {
	ID            string `json:"id"`
	Provider      string `json:"provider"`
	URL           string `json:"url"`
	DefaultBranch string `json:"default_branch"`
	Created       bool   `json:"created"`
	// GeneratedWebhookSecret is present ONLY when this apply
	// minted a fresh secret (new scm_source or an explicit
	// plaintext caller-supplied value). Returned plaintext
	// exactly once — the UI shows it with a copy button and
	// a "won't be shown again" banner. Subsequent reads never
	// see it; rotation goes through a dedicated endpoint.
	GeneratedWebhookSecret string `json:"generated_webhook_secret,omitempty"`
}

type ApplyResponse struct {
	ProjectID        string                `json:"project_id"`
	ProjectCreated   bool                  `json:"project_created"`
	Pipelines        []ApplyPipeline       `json:"pipelines"`
	PipelinesRemoved []string              `json:"pipelines_removed"`
	SCMSource        *ApplySCMSourceResult `json:"scm_source,omitempty"`
	// Webhooks is present only when the server has a GitHub App
	// configured AND the request contained git materials with
	// auto_register_webhook=true. Each entry is best-effort:
	// "skipped_no_install" / "failed" don't abort the apply.
	Webhooks []HookRegistration `json:"webhooks,omitempty"`
	// Warnings collects non-fatal issues the caller should see
	// but that don't warrant failing the apply: e.g. "scm_source
	// bound but the .gocdnext/ folder doesn't exist yet at
	// default_branch HEAD, so no pipelines were synced". The CLI
	// prints these as yellow lines; the UI surfaces them in the
	// create-result toast.
	Warnings []string `json:"warnings,omitempty"`
}

func (h *Handler) Apply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxApplyBodyBytes)
	var req ApplyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Slug == "" {
		http.Error(w, "slug is required", http.StatusBadRequest)
		return
	}
	if req.ConfigPath != "" && !validConfigPath(req.ConfigPath) {
		http.Error(w, "invalid config_path: allow letters, digits, . _ - / (no .., no leading /)", http.StatusBadRequest)
		return
	}
	// files is optional: the web "New project" dialog supports an
	// Empty + Connect-repo flow that just registers metadata (and
	// optionally an scm_source). ApplyProject with zero pipelines
	// is a valid no-op on the pipeline side. The CLI always sends
	// files via `gocdnext apply` so that path is unaffected.
	pipelines, err := parseFiles(req.Files)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	var scm *store.SCMSourceInput
	if req.SCMSource != nil {
		if req.SCMSource.URL == "" || req.SCMSource.Provider == "" {
			http.Error(w, "scm_source.url and scm_source.provider are required", http.StatusBadRequest)
			return
		}
		scm = &store.SCMSourceInput{
			Provider:      req.SCMSource.Provider,
			URL:           req.SCMSource.URL,
			DefaultBranch: req.SCMSource.DefaultBranch,
			WebhookSecret: req.SCMSource.WebhookSecret,
			AuthRef:       req.SCMSource.AuthRef,
		}
	}

	// Initial sync from the repo: when the caller binds an
	// scm_source and doesn't ship local pipeline files, read the
	// configured folder from the default branch and use those as
	// the pipeline set. This makes pipelines appear at bind time
	// instead of waiting for the first push. Reachable-but-empty
	// folders come back as a warning (valid state); network/auth
	// errors hard-fail so we don't create a half-bound project
	// whose webhook will 404 later too.
	var warnings []string
	if scm != nil && len(req.Files) == 0 && h.fetcher != nil {
		fetchRef := scm.DefaultBranch
		if fetchRef == "" {
			fetchRef = "main"
		}
		remote := store.SCMSource{
			Provider:      scm.Provider,
			URL:           scm.URL,
			DefaultBranch: fetchRef,
			AuthRef:       scm.AuthRef,
		}
		fetchPath := req.ConfigPath // empty → fetcher defaults to ".gocdnext"
		files, ferr := h.fetcher.Fetch(r.Context(), remote, fetchRef, fetchPath)
		switch {
		case errors.Is(ferr, configsync.ErrFolderNotFound):
			warnings = append(warnings, fmt.Sprintf(
				"config folder %q not found at %s@%s — project bound, pipelines will sync on first push",
				displayConfigPath(req.ConfigPath), scm.URL, fetchRef))
		case ferr != nil:
			h.log.Warn("apply: initial sync fetch failed",
				"slug", req.Slug, "url", scm.URL, "err", ferr)
			http.Error(w, "fetch .gocdnext/ from repo: "+ferr.Error(), http.StatusBadGateway)
			return
		default:
			parsed, perr := configsync.ParseFiles(files)
			if perr != nil {
				http.Error(w, "parse remote .gocdnext/: "+perr.Error(), http.StatusUnprocessableEntity)
				return
			}
			if len(parsed) == 0 {
				warnings = append(warnings, fmt.Sprintf(
					"no YAML files in %q at %s@%s — pipelines will sync on first push with files",
					displayConfigPath(req.ConfigPath), scm.URL, fetchRef))
			}
			pipelines = parsed
		}
	}

	// Project repo is implicit: when the project has (or is about to
	// have) an scm_source and pipelines don't declare a git material
	// for it, synthesize one so operators don't restate the project's
	// own repo in every YAML. For re-applies without an scm_source in
	// the request we fall back to the existing DB binding — otherwise
	// a CLI apply that trims explicit git from YAML would strip the
	// material entirely on apply.
	effectiveSCM := scm
	if effectiveSCM == nil {
		if existing, ferr := h.store.FindSCMSourceByProjectSlug(r.Context(), req.Slug); ferr == nil {
			effectiveSCM = &store.SCMSourceInput{
				Provider:      existing.Provider,
				URL:           existing.URL,
				DefaultBranch: existing.DefaultBranch,
				AuthRef:       existing.AuthRef,
			}
		}
	}
	injectImplicitProjectMaterial(pipelines, effectiveSCM)

	result, err := h.store.ApplyProject(r.Context(), store.ApplyProjectInput{
		Slug:        req.Slug,
		Name:        req.Name,
		Description: req.Description,
		ConfigRepo:  req.ConfigRepo,
		ConfigPath:  req.ConfigPath,
		Pipelines:   pipelines,
		SCMSource:   scm,
	})
	if err != nil {
		h.log.Error("apply project", "slug", req.Slug, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	resp := ApplyResponse{
		ProjectID:        result.ProjectID.String(),
		ProjectCreated:   result.ProjectCreated,
		PipelinesRemoved: result.PipelinesRemoved,
	}
	if result.SCMSource != nil {
		resp.SCMSource = &ApplySCMSourceResult{
			ID:                     result.SCMSource.ID.String(),
			Provider:               result.SCMSource.Provider,
			URL:                    result.SCMSource.URL,
			DefaultBranch:          result.SCMSource.DefaultBranch,
			Created:                result.SCMSource.Created,
			GeneratedWebhookSecret: result.SCMSource.GeneratedWebhookSecret,
		}
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

	resp.Warnings = warnings

	// Best-effort webhook registration on the scm_source that the
	// apply just bound. A 100% failure here leaves the project
	// definition intact — the operator can install the webhook
	// manually or re-apply later.
	if result.SCMSource != nil {
		if hr := h.reconcileSCMSourceWebhook(
			r.Context(),
			result.SCMSource,
			result.SCMSource.GeneratedWebhookSecret,
		); hr != nil {
			resp.Webhooks = []HookRegistration{*hr}
		}
	}

	h.log.Info("apply project",
		"slug", req.Slug,
		"project_created", result.ProjectCreated,
		"pipelines", len(result.Pipelines),
		"pipelines_removed", len(result.PipelinesRemoved))

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func parseFiles(files []ApplyFile) ([]*domain.Pipeline, error) {
	seen := map[string]string{}
	out := make([]*domain.Pipeline, 0, len(files))
	for _, f := range files {
		if f.Name == "" {
			return nil, fmt.Errorf("file entry missing name")
		}
		fallback := strings.TrimSuffix(f.Name, filepath.Ext(f.Name))
		p, err := parser.ParseNamed(strings.NewReader(f.Content), "", fallback)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f.Name, err)
		}
		if prev, dup := seen[p.Name]; dup {
			return nil, fmt.Errorf("pipeline %q defined twice: %s and %s", p.Name, prev, f.Name)
		}
		seen[p.Name] = f.Name
		out = append(out, p)
	}
	return out, nil
}

// displayConfigPath renders the config path as it appears to the
// user: empty → ".gocdnext" (the default the fetcher would use).
// Only used inside warning strings so operators see the folder
// they can push files into.
func displayConfigPath(s string) string {
	if s == "" {
		return ".gocdnext"
	}
	return s
}

// configPathRE restricts project-level config paths to a safe
// shape: letters, digits, dots, dashes, underscores and forward
// slashes. No leading slash (would look absolute), no "..", no
// whitespace. Matches what you'd reasonably find at a repo root
// (".gocdnext", ".woodpecker", "apps/api/.gocdnext").
var configPathRE = regexp.MustCompile(`^[a-zA-Z0-9._-]+(/[a-zA-Z0-9._-]+)*$`)

// validConfigPath returns true when s is a safe, repo-relative
// folder name the server can hand to GitHub's contents API /
// filepath.Join without ambiguity. Max length bounds the URL
// size; "." and ".." segments are rejected explicitly on top of
// the regex because those slip through the charset check.
func validConfigPath(s string) bool {
	if s == "" || len(s) > 512 {
		return false
	}
	if !configPathRE.MatchString(s) {
		return false
	}
	for _, seg := range strings.Split(s, "/") {
		if seg == "." || seg == ".." {
			return false
		}
	}
	return true
}
