// Package projects exposes the HTTP endpoints that manage project/pipeline
// definitions. The apply endpoint accepts the contents of a `.gocdnext/`
// folder and synchronizes the DB to match (upsert project+pipelines+materials,
// remove stale rows).
package projects

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/gocdnext/gocdnext/server/internal/crypto"
	"github.com/gocdnext/gocdnext/server/internal/domain"
	"github.com/gocdnext/gocdnext/server/internal/parser"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

const maxApplyBodyBytes = 5 << 20 // 5 MiB — room for large mono-repo configs.

type Handler struct {
	store        *store.Store
	log          *slog.Logger
	cipher       *crypto.Cipher
	autoRegister *AutoRegisterConfig
}

func NewHandler(s *store.Store, log *slog.Logger) *Handler {
	if log == nil {
		log = slog.Default()
	}
	return &Handler{store: s, log: log}
}

type ApplyFile struct {
	Name    string `json:"name"`
	Content string `json:"content"`
}

type ApplyRequest struct {
	Slug        string           `json:"slug"`
	Name        string           `json:"name"`
	Description string           `json:"description,omitempty"`
	ConfigRepo  string           `json:"config_repo,omitempty"`
	Files       []ApplyFile      `json:"files"`
	SCMSource   *ApplySCMSource  `json:"scm_source,omitempty"`
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

	result, err := h.store.ApplyProject(r.Context(), store.ApplyProjectInput{
		Slug:        req.Slug,
		Name:        req.Name,
		Description: req.Description,
		ConfigRepo:  req.ConfigRepo,
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
			ID:            result.SCMSource.ID.String(),
			Provider:      result.SCMSource.Provider,
			URL:           result.SCMSource.URL,
			DefaultBranch: result.SCMSource.DefaultBranch,
			Created:       result.SCMSource.Created,
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

	// Best-effort webhook registration. Always runs after the apply
	// succeeded so even a 100% failure here leaves the project
	// definition in place; operator can retry later.
	resp.Webhooks = h.reconcilePipelines(r.Context(), pipelines)

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
