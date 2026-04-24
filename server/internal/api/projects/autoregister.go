package projects

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/gocdnext/gocdnext/server/internal/scm/bitbucket"
	ghscm "github.com/gocdnext/gocdnext/server/internal/scm/github"
	"github.com/gocdnext/gocdnext/server/internal/scm/gitlab"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/internal/vcs"
)

// AutoRegisterConfig wires post-apply webhook registration. Works
// across all three providers — GitHub via the installed App (VCS
// registry), GitLab + Bitbucket via the PAT / App Password
// stored in scm_source.auth_ref.
//
// Required scopes per provider:
//   - GitHub App: webhooks (part of the gocdnext App manifest)
//   - GitLab PAT: api (not read_api — the former covers hook
//     writes; the latter is read-only)
//   - Bitbucket App Password: webhooks (read + write)
//     or OAuth token with scope webhook
//
// Missing scope surfaces as a 403 from the provider which we
// render into HookRegistration.Error verbatim so the operator
// knows exactly which credential to rotate.
type AutoRegisterConfig struct {
	VCS        *vcs.Registry
	PublicBase string
	// WebhookPublicURL overrides the base URL we hand to providers
	// so the server stays on localhost for the UI while the repo
	// sees a public tunnel (smee.io, ngrok). The path suffix is
	// picked per-provider inside hookURLFor.
	WebhookPublicURL string
}

// WithAutoRegister enables post-apply webhook registration.
func (h *Handler) WithAutoRegister(cfg AutoRegisterConfig) *Handler {
	h.autoRegister = &cfg
	return h
}

// HookRegistration is one line in the apply response's webhooks
// list. Status is one of:
//   - registered: new hook created
//   - updated: existing gocdnext hook PATCHed
//   - already_exists: hook with our URL found, no change required
//   - skipped_no_install: provider side isn't wired (GitHub App
//     not installed on repo; auth_ref missing on GitLab/Bitbucket)
//   - skipped_unsupported_provider: provider not one of the three
//     we auto-register for (e.g. manual)
//   - failed: call to provider returned an error; Error carries
//     the detail
type HookRegistration struct {
	SCMSourceURL string `json:"scm_source_url"`
	Provider     string `json:"provider,omitempty"`
	Status       string `json:"status"`
	HookID       string `json:"hook_id,omitempty"` // string so it holds GitHub's int64 AND Bitbucket's uuid
	Error        string `json:"error,omitempty"`
}

// reconcileSCMSourceWebhook dispatches by provider to the right
// auto-register helper. Preserves the old API (returns a single
// HookRegistration, nil when auto-register isn't configured) so
// Apply/RotateWebhookSecret don't branch on provider.
func (h *Handler) reconcileSCMSourceWebhook(
	ctx context.Context,
	applied *store.SCMSourceApplied,
	plaintextSecret string,
) *HookRegistration {
	if h.autoRegister == nil || applied == nil {
		return nil
	}
	res := &HookRegistration{
		SCMSourceURL: applied.URL,
		Provider:     applied.Provider,
	}
	switch applied.Provider {
	case "github":
		return h.reconcileGitHub(ctx, applied, plaintextSecret, res)
	case "gitlab":
		return h.reconcileGitLab(ctx, applied, plaintextSecret, res)
	case "bitbucket":
		return h.reconcileBitbucket(ctx, applied, plaintextSecret, res)
	default:
		res.Status = "skipped_unsupported_provider"
		return res
	}
}

// hookURLFor picks the public URL that the provider's delivery
// will hit. Each provider gets its own /api/webhooks/<provider>
// endpoint so the handler dispatch by URL rather than sniffing
// headers.
func (c *AutoRegisterConfig) hookURLFor(provider string) string {
	if c == nil {
		return ""
	}
	base := trimTrailingSlash(c.WebhookPublicURL)
	if base == "" {
		base = trimTrailingSlash(c.PublicBase)
	}
	if base == "" {
		return ""
	}
	// Historical: WebhookPublicURL used to be the full
	// /api/webhooks/github URL (localdev tunnels). Keep that
	// mode working for github calls that wire it, while new
	// provider routes derive from the base.
	if c.WebhookPublicURL != "" && provider == "github" {
		return c.WebhookPublicURL
	}
	return base + "/api/webhooks/" + provider
}

// resolveSecret pulls the plaintext webhook secret, preferring
// the in-memory value from the just-completed apply and falling
// back to the sealed DB ciphertext. Shared across providers.
func (h *Handler) resolveSecret(
	ctx context.Context, url, hint string,
) (string, error) {
	if hint != "" {
		return hint, nil
	}
	auth, err := h.store.FindSCMSourceWebhookSecret(ctx, url)
	if err != nil {
		return "", err
	}
	return auth.Secret, nil
}

// --- GitHub (App-based) ---

func (h *Handler) reconcileGitHub(
	ctx context.Context, applied *store.SCMSourceApplied,
	plaintextSecret string, res *HookRegistration,
) *HookRegistration {
	cfg := h.autoRegister
	app := cfg.currentApp()
	if app == nil {
		res.Status = "skipped_no_install"
		res.Error = "no github app configured"
		return res
	}
	owner, repo, err := ghscm.ParseRepoURL(applied.URL)
	if err != nil {
		res.Status = "skipped_unsupported_provider"
		res.Error = "url does not look like a GitHub repo"
		return res
	}
	installationID, err := app.InstallationID(ctx, owner, repo)
	if errors.Is(err, ghscm.ErrNoInstallation) {
		res.Status = "skipped_no_install"
		res.Error = "gocdnext App is not installed on " + owner + "/" + repo
		h.log.Info("autoregister: no installation", "repo", owner+"/"+repo)
		return res
	}
	if err != nil {
		res.Status = "failed"
		res.Error = err.Error()
		return res
	}

	hookURL := cfg.hookURLFor("github")
	existing, err := app.ListRepoHooks(ctx, installationID, owner, repo)
	if err != nil {
		res.Status = "failed"
		res.Error = err.Error()
		return res
	}
	secret, err := h.resolveSecret(ctx, applied.URL, plaintextSecret)
	if err != nil {
		res.Status = "failed"
		res.Error = "secret lookup: " + err.Error()
		return res
	}
	if secret == "" {
		res.Status = "failed"
		res.Error = "no webhook secret available (rotate the secret and retry)"
		return res
	}

	input := ghscm.CreateHookInput{Owner: owner, Repo: repo, URL: hookURL, Secret: secret}
	if hook, ok := ghscm.FindHookForURL(existing, hookURL); ok {
		updated, err := app.UpdateRepoHook(ctx, installationID, hook.ID, input)
		if err != nil {
			res.Status = "failed"
			res.Error = err.Error()
			return res
		}
		res.Status = "updated"
		res.HookID = fmtInt(updated.ID)
		return res
	}
	created, err := app.CreateRepoHook(ctx, installationID, input)
	if err != nil {
		res.Status = "failed"
		res.Error = err.Error()
		return res
	}
	res.Status = "registered"
	res.HookID = fmtInt(created.ID)
	return res
}

// --- GitLab (PAT-based) ---

func (h *Handler) reconcileGitLab(
	ctx context.Context, applied *store.SCMSourceApplied,
	plaintextSecret string, res *HookRegistration,
) *HookRegistration {
	cfg := h.autoRegister
	// Per-project auth_ref wins; fall back to the org-level
	// scm_credentials row matching the repo's host. Same
	// resolver the fetcher + poll worker consult — three
	// subsystems agree on which token speaks for the repo.
	authRef, apiBase := h.store.ResolveAuthRef(
		ctx, "gitlab", applied.URL, applied.AuthRef,
	)
	if authRef == "" {
		res.Status = "skipped_no_install"
		res.Error = "no credential available (bind a per-project PAT or add a GitLab credential in /settings/integrations)"
		return res
	}
	path, err := gitlab.ParseRepoURL(applied.URL)
	if err != nil {
		res.Status = "skipped_unsupported_provider"
		res.Error = "url does not look like a GitLab project"
		return res
	}
	glCfg := gitlab.Config{
		APIBase:     apiBase, // empty = gitlab.com default
		ProjectPath: path,
		Token:       authRef,
	}
	client := autoHTTPClient()

	existing, err := gitlab.ListProjectHooks(ctx, client, glCfg)
	if err != nil {
		res.Status = "failed"
		res.Error = err.Error()
		return res
	}
	secret, err := h.resolveSecret(ctx, applied.URL, plaintextSecret)
	if err != nil {
		res.Status = "failed"
		res.Error = "secret lookup: " + err.Error()
		return res
	}
	if secret == "" {
		res.Status = "failed"
		res.Error = "no webhook secret available (rotate the secret and retry)"
		return res
	}

	hookURL := cfg.hookURLFor("gitlab")
	input := gitlab.CreateHookInput{
		ProjectPath: path,
		URL:         hookURL,
		Secret:      secret,
	}
	if hook, ok := gitlab.FindHookForURL(existing, hookURL); ok {
		updated, err := gitlab.UpdateProjectHook(ctx, client, glCfg, hook.ID, input)
		if err != nil {
			res.Status = "failed"
			res.Error = err.Error()
			return res
		}
		res.Status = "updated"
		res.HookID = fmtInt(updated.ID)
		return res
	}
	created, err := gitlab.CreateProjectHook(ctx, client, glCfg, input)
	if err != nil {
		res.Status = "failed"
		res.Error = err.Error()
		return res
	}
	res.Status = "registered"
	res.HookID = fmtInt(created.ID)
	return res
}

// --- Bitbucket (App Password / OAuth) ---

func (h *Handler) reconcileBitbucket(
	ctx context.Context, applied *store.SCMSourceApplied,
	plaintextSecret string, res *HookRegistration,
) *HookRegistration {
	cfg := h.autoRegister
	authRef, apiBase := h.store.ResolveAuthRef(
		ctx, "bitbucket", applied.URL, applied.AuthRef,
	)
	if authRef == "" {
		res.Status = "skipped_no_install"
		res.Error = "no credential available (bind a per-project token or add a Bitbucket credential in /settings/integrations)"
		return res
	}
	ws, repo, err := bitbucket.ParseRepoURL(applied.URL)
	if err != nil {
		res.Status = "skipped_unsupported_provider"
		res.Error = "url does not look like a Bitbucket repo"
		return res
	}
	bbCfg := bitbucket.Config{
		APIBase:   apiBase,
		Workspace: ws,
		RepoSlug:  repo,
	}
	// auth_ref convention for Bitbucket: "user:app_password" →
	// Basic; anything else → Bearer. Mirrors how the Fetcher
	// splits it when scm_source is consulted for reads.
	if idx := splitBasic(authRef); idx > 0 {
		bbCfg.Username = authRef[:idx]
		bbCfg.AppPassword = authRef[idx+1:]
	} else {
		bbCfg.Token = authRef
	}
	client := autoHTTPClient()

	existing, err := bitbucket.ListRepoHooks(ctx, client, bbCfg)
	if err != nil {
		res.Status = "failed"
		res.Error = err.Error()
		return res
	}
	secret, err := h.resolveSecret(ctx, applied.URL, plaintextSecret)
	if err != nil {
		res.Status = "failed"
		res.Error = "secret lookup: " + err.Error()
		return res
	}
	if secret == "" {
		res.Status = "failed"
		res.Error = "no webhook secret available (rotate the secret and retry)"
		return res
	}

	hookURL := cfg.hookURLFor("bitbucket")
	input := bitbucket.CreateHookInput{
		Workspace: ws, RepoSlug: repo,
		URL:    hookURL,
		Secret: secret,
	}
	if hook, ok := bitbucket.FindHookForURL(existing, hookURL); ok {
		updated, err := bitbucket.UpdateRepoHook(ctx, client, bbCfg, hook.UID, input)
		if err != nil {
			res.Status = "failed"
			res.Error = err.Error()
			return res
		}
		res.Status = "updated"
		res.HookID = updated.UID
		return res
	}
	created, err := bitbucket.CreateRepoHook(ctx, client, bbCfg, input)
	if err != nil {
		res.Status = "failed"
		res.Error = err.Error()
		return res
	}
	res.Status = "registered"
	res.HookID = created.UID
	return res
}

// --- small helpers ---

// splitBasic returns the index of the `:` that separates a
// Bitbucket `user:app_password` auth_ref value, or -1 when not
// present. Anything without a colon is treated as a Bearer
// token.
func splitBasic(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] == ':' {
			return i
		}
	}
	return -1
}

// currentApp returns the active GitHub App client or nil.
func (c *AutoRegisterConfig) currentApp() *ghscm.AppClient {
	if c == nil || c.VCS == nil {
		return nil
	}
	return c.VCS.GitHubApp()
}

// autoHTTPClient returns a shared httpClient with a sane timeout
// for provider hook-CRUD calls. 30s matches the fetcher default.
func autoHTTPClient() *http.Client {
	return &http.Client{Timeout: 30 * time.Second}
}

func fmtInt(n int64) string {
	// 32 digits is overkill for int64 but keeps compile-time
	// constant space without importing strconv for one call.
	const digits = "0123456789"
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = digits[n%10]
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func trimTrailingSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}
