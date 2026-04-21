package projects

import (
	"context"
	"errors"

	ghscm "github.com/gocdnext/gocdnext/server/internal/scm/github"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/internal/vcs"
)

// AutoRegisterConfig wires the handler into the GitHub App flow so
// that binding an scm_source auto-installs a webhook on the repo
// with the project's sealed secret as the HMAC key. Requires
// PublicBase (for the hook URL) and a VCS registry carrying an
// active GitHub App client.
type AutoRegisterConfig struct {
	VCS        *vcs.Registry
	PublicBase string
}

// WithAutoRegister enables post-apply webhook registration for
// scm_source bindings. Safe to call with an unconfigured registry;
// reconcileSCMSourceWebhook checks for a live App at call time and
// downgrades to skipped_no_install when absent.
func (h *Handler) WithAutoRegister(cfg AutoRegisterConfig) *Handler {
	h.autoRegister = &cfg
	return h
}

// currentApp returns the active GitHub App client or nil. The
// reconcile path guards on this — "no app right now" becomes a
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
	SCMSourceURL string `json:"scm_source_url"`
	Status       string `json:"status"` // registered | already_exists | skipped_no_install | skipped_not_github | failed
	HookID       int64  `json:"hook_id,omitempty"`
	Error        string `json:"error,omitempty"`
}

// reconcileSCMSourceWebhook ensures a gocdnext-owned webhook is
// installed on the repo referenced by applied (post-ApplyProject).
// Idempotent: an existing hook whose config.url matches our public
// base is reported as already_exists, not recreated. Best-effort —
// any failure lands in the response body so the operator can
// decide to install manually, but never aborts the apply.
//
// plaintextSecret takes precedence over DB lookup: when the apply
// just generated the secret, passing it in avoids re-decrypting
// what's already in memory. When empty (re-apply that preserved
// existing ciphertext), the store cipher is used to recover the
// plaintext for GitHub's HMAC config.
func (h *Handler) reconcileSCMSourceWebhook(
	ctx context.Context,
	applied *store.SCMSourceApplied,
	plaintextSecret string,
) *HookRegistration {
	if h.autoRegister == nil || applied == nil {
		return nil
	}
	cfg := h.autoRegister
	res := &HookRegistration{SCMSourceURL: applied.URL}

	if applied.Provider != "github" {
		res.Status = "skipped_not_github"
		return res
	}

	app := cfg.currentApp()
	if app == nil {
		res.Status = "skipped_no_install"
		res.Error = "no github app configured"
		return res
	}

	owner, repo, err := ghscm.ParseRepoURL(applied.URL)
	if err != nil {
		res.Status = "skipped_not_github"
		res.Error = "url does not look like a GitHub repo"
		return res
	}

	installationID, err := app.InstallationID(ctx, owner, repo)
	if errors.Is(err, ghscm.ErrNoInstallation) {
		res.Status = "skipped_no_install"
		res.Error = "gocdnext App is not installed on " + owner + "/" + repo
		h.log.Info("autoregister: no installation",
			"repo", owner+"/"+repo)
		return res
	}
	if err != nil {
		res.Status = "failed"
		res.Error = err.Error()
		h.log.Warn("autoregister: installation lookup failed",
			"repo", owner+"/"+repo, "err", err)
		return res
	}

	hookURL := trimTrailingSlash(cfg.PublicBase) + "/api/webhooks/github"
	existing, err := app.ListRepoHooks(ctx, installationID, owner, repo)
	if err != nil {
		res.Status = "failed"
		res.Error = err.Error()
		h.log.Warn("autoregister: list hooks failed",
			"repo", owner+"/"+repo, "err", err)
		return res
	}
	if hook, ok := ghscm.FindHookForURL(existing, hookURL); ok {
		res.Status = "already_exists"
		res.HookID = hook.ID
		return res
	}

	// Secret resolution: prefer the plaintext from the just-completed
	// apply (free — already in memory), fall back to decrypting the
	// stored ciphertext (re-applies that preserved the existing
	// secret). If both paths leave us empty, the hook would be
	// registered unsigned — refuse so the operator doesn't end up
	// with a 401-ing webhook that looks installed.
	secret := plaintextSecret
	if secret == "" {
		auth, err := h.store.FindSCMSourceWebhookSecret(ctx, applied.URL)
		if err != nil {
			res.Status = "failed"
			res.Error = "secret lookup: " + err.Error()
			return res
		}
		secret = auth.Secret
	}
	if secret == "" {
		res.Status = "failed"
		res.Error = "no webhook secret available (rotate the secret and retry)"
		return res
	}

	created, err := app.CreateRepoHook(ctx, installationID, ghscm.CreateHookInput{
		Owner:  owner,
		Repo:   repo,
		URL:    hookURL,
		Secret: secret,
	})
	if err != nil {
		res.Status = "failed"
		res.Error = err.Error()
		h.log.Warn("autoregister: create hook failed",
			"repo", owner+"/"+repo, "err", err)
		return res
	}
	res.Status = "registered"
	res.HookID = created.ID
	h.log.Info("autoregister: hook created",
		"repo", owner+"/"+repo, "hook_id", created.ID)
	return res
}

func trimTrailingSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}
