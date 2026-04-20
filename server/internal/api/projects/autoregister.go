package projects

import (
	"context"
	"errors"
	"strings"

	"github.com/gocdnext/gocdnext/server/internal/domain"
	ghscm "github.com/gocdnext/gocdnext/server/internal/scm/github"
	"github.com/gocdnext/gocdnext/server/internal/vcs"
)

// AutoRegisterConfig wires the handler into the GitHub App flow.
//
// DISABLED as of UI.10.a: auto-register used to rely on one
// global webhook secret (GOCDNEXT_WEBHOOK_TOKEN) that got
// embedded into every installed webhook. The per-repo-secret
// refactor killed that env var, and reviving auto-register
// cleanly requires making scm_sources multi-row-per-project so
// each material gets its own sealed secret. Until that schema
// change lands, WithAutoRegister is an explicit no-op and
// reconcilePipelines early-returns.
type AutoRegisterConfig struct {
	VCS        *vcs.Registry
	PublicBase string
}

// WithAutoRegister used to enable post-apply webhook registration.
// Currently a no-op — see AutoRegisterConfig's doc comment.
func (h *Handler) WithAutoRegister(cfg AutoRegisterConfig) *Handler {
	// Intentionally keeps the same signature so main.go wiring
	// stays one line; all callers become no-ops transparently.
	_ = cfg
	return h
}

// currentApp returns the active GitHub App client or nil. The
// reconcileOne path guards on this — "no app right now" becomes a
// skipped_no_install outcome, not a failure.
func (c *AutoRegisterConfig) currentApp() *ghscm.AppClient {
	if c == nil || c.VCS == nil {
		return nil
	}
	return c.VCS.GitHubApp()
}

// HookRegistration is one line in the apply response's webhooks
// list. The CLI / UI shows this so the operator knows whether they
// need to install the App manually.
type HookRegistration struct {
	Pipeline    string `json:"pipeline"`
	MaterialURL string `json:"material_url"`
	Status      string `json:"status"` // registered | already_exists | skipped_no_install | failed
	HookID      int64  `json:"hook_id,omitempty"`
	Error       string `json:"error,omitempty"`
}

// reconcilePipelines walks every git material with
// auto_register_webhook=true and ensures a gocdnext hook is
// installed on the repo. Idempotent: an existing hook whose
// config.url starts with our public base is treated as "already
// exists" and not duplicated. Best-effort — errors on one pipeline
// don't affect the rest, and the whole apply still succeeds.
func (h *Handler) reconcilePipelines(ctx context.Context, pipelines []*domain.Pipeline) []HookRegistration {
	if h.autoRegister == nil {
		return nil
	}
	cfg := h.autoRegister
	hookURL := strings.TrimRight(cfg.PublicBase, "/") + "/api/webhooks/github"

	var out []HookRegistration
	for _, p := range pipelines {
		for _, m := range p.Materials {
			if m.Type != domain.MaterialGit || m.Git == nil || !m.Git.AutoRegisterWebhook {
				continue
			}
			out = append(out, h.reconcileOne(ctx, p.Name, m.Git.URL, hookURL))
		}
	}
	return out
}

func (h *Handler) reconcileOne(ctx context.Context, pipeline, materialURL, hookURL string) HookRegistration {
	cfg := h.autoRegister
	res := HookRegistration{Pipeline: pipeline, MaterialURL: materialURL}

	app := cfg.currentApp()
	if app == nil {
		// Admin deleted the DB row (or env was the only source and
		// it's gone). We already gated reconcilePipelines on the
		// autoRegister struct being non-nil, but the underlying
		// AppClient can still disappear at runtime — treat as
		// skipped_no_install so the caller sees a clean outcome.
		res.Status = "skipped_no_install"
		res.Error = "no github app configured"
		return res
	}

	owner, repo, err := ghscm.ParseRepoURL(materialURL)
	if err != nil {
		// Not a GitHub-shaped URL (GitLab etc.); skip rather than error.
		res.Status = "skipped_no_install"
		res.Error = "url does not look like a GitHub repo"
		return res
	}

	installationID, err := app.InstallationID(ctx, owner, repo)
	if errors.Is(err, ghscm.ErrNoInstallation) {
		res.Status = "skipped_no_install"
		res.Error = "gocdnext App is not installed on " + owner + "/" + repo
		h.log.Info("autoregister: no installation",
			"pipeline", pipeline, "repo", owner+"/"+repo)
		return res
	}
	if err != nil {
		res.Status = "failed"
		res.Error = err.Error()
		h.log.Warn("autoregister: installation lookup failed",
			"pipeline", pipeline, "repo", owner+"/"+repo, "err", err)
		return res
	}

	existing, err := app.ListRepoHooks(ctx, installationID, owner, repo)
	if err != nil {
		res.Status = "failed"
		res.Error = err.Error()
		h.log.Warn("autoregister: list hooks failed",
			"pipeline", pipeline, "repo", owner+"/"+repo, "err", err)
		return res
	}
	if hook, ok := ghscm.FindHookForURL(existing, hookURL); ok {
		res.Status = "already_exists"
		res.HookID = hook.ID
		return res
	}

	// Dead code since WithAutoRegister is a no-op; kept for the
	// quick-revive path once per-repo scm_sources become plural.
	// Passes an empty secret — real flow will pull from a store
	// helper keyed on the material URL.
	_ = cfg
	created, err := app.CreateRepoHook(ctx, installationID, ghscm.CreateHookInput{
		Owner: owner,
		Repo:  repo,
		URL:   hookURL,
	})
	if err != nil {
		res.Status = "failed"
		res.Error = err.Error()
		h.log.Warn("autoregister: create hook failed",
			"pipeline", pipeline, "repo", owner+"/"+repo, "err", err)
		return res
	}
	res.Status = "registered"
	res.HookID = created.ID
	h.log.Info("autoregister: hook created",
		"pipeline", pipeline, "repo", owner+"/"+repo, "hook_id", created.ID)
	return res
}
