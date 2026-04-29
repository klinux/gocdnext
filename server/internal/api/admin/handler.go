// Package admin serves the /api/v1/admin/* endpoints that back the
// /settings pages in the web UI. Read-only in this slice — mutations
// happen via existing Apply/Secret paths and (future) run actions.
package admin

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/gocdnext/gocdnext/server/internal/crypto"
	"github.com/gocdnext/gocdnext/server/internal/retention"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/internal/vcs"
)

const (
	defaultWebhooksLimit int32 = 50
	maxWebhooksLimit     int32 = 200
)

// WiringState captures the parts of the integration summary that
// never change at runtime (public base, checks reporter). The
// dynamic bits (GitHub App configured) are recomputed per
// request via the live vcs.Registry. Webhook auth moved to
// per-scm_source secrets in UI.10.a — no global token lives in
// env anymore.
type WiringState struct {
	// PublicBase carries the actual URL, not just the "set" bit,
	// so the UI can render copy-paste-ready webhook endpoints
	// per provider. Empty = "not configured".
	PublicBase       string
	PublicBaseSet    bool
	ChecksReporterOn bool
}

// Handler owns the /api/v1/admin/* routes.
type Handler struct {
	store   *store.Store
	sweeper *retention.Sweeper
	vcs     *vcs.Registry
	wiring  WiringState
	log     *slog.Logger
	// cipher is wired post-construction via SetCipher so the
	// existing test harness (which builds Handler with NewHandler
	// and never touches secrets) keeps compiling. Endpoints that
	// need to seal/open secrets (runner profile secrets today)
	// 503 when nil, mirroring how GlobalSecretsHandler behaves.
	cipher *crypto.Cipher
}

// NewHandler wires the admin handler. sweeper may be nil —
// retention endpoint will then report disabled. vcsRegistry is
// the shared registry; the handler reads it live so a CRUD write
// is visible to the wiring-summary page immediately.
func NewHandler(s *store.Store, sweeper *retention.Sweeper, vcsRegistry *vcs.Registry, wiring WiringState, log *slog.Logger) *Handler {
	if log == nil {
		log = slog.Default()
	}
	return &Handler{store: s, sweeper: sweeper, vcs: vcsRegistry, wiring: wiring, log: log}
}

// SetCipher attaches the AEAD used to encrypt runner profile
// secrets. Call once during boot after NewHandler. Nil-safe — a
// later call to a secret-bearing endpoint surfaces 503 when the
// cipher hasn't been configured (GOCDNEXT_SECRET_KEY unset).
func (h *Handler) SetCipher(c *crypto.Cipher) {
	h.cipher = c
}

// Retention handles GET /api/v1/admin/retention. Returns the sweeper
// snapshot (config + last tick stats) or a disabled envelope when no
// sweeper is wired.
func (h *Handler) Retention(w http.ResponseWriter, r *http.Request) {
	if !methodGET(w, r) {
		return
	}
	writeJSON(w, h.retentionSnapshot())
}

func (h *Handler) retentionSnapshot() any {
	if h.sweeper == nil {
		return map[string]any{"enabled": false}
	}
	return h.sweeper.Snapshot()
}

// Webhooks handles GET /api/v1/admin/webhooks with optional
// ?provider=, ?status=, ?limit=, ?offset= filters. Paginated so the
// admin page can scroll back through audit rows without dumping the
// whole table.
func (h *Handler) Webhooks(w http.ResponseWriter, r *http.Request) {
	if !methodGET(w, r) {
		return
	}
	limit := defaultWebhooksLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 32)
		if err != nil || parsed < 1 {
			http.Error(w, "invalid 'limit' query", http.StatusBadRequest)
			return
		}
		if int32(parsed) > maxWebhooksLimit {
			parsed = int64(maxWebhooksLimit)
		}
		limit = int32(parsed)
	}
	var offset int64
	if raw := r.URL.Query().Get("offset"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed < 0 {
			http.Error(w, "invalid 'offset' query", http.StatusBadRequest)
			return
		}
		offset = parsed
	}
	filter := store.WebhookDeliveryFilter{
		Provider: r.URL.Query().Get("provider"),
		Status:   r.URL.Query().Get("status"),
	}

	deliveries, err := h.store.ListWebhookDeliveries(r.Context(), limit, offset, filter)
	if err != nil {
		h.log.Error("admin webhooks list", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	total, err := h.store.CountWebhookDeliveries(r.Context(), filter)
	if err != nil {
		h.log.Error("admin webhooks count", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{
		"deliveries": deliveries,
		"total":      total,
		"limit":      limit,
		"offset":     offset,
	})
}

// WebhookDetail handles GET /api/v1/admin/webhooks/{id}. Returns the
// full headers + payload for the drawer view.
func (h *Handler) WebhookDetail(w http.ResponseWriter, r *http.Request) {
	if !methodGET(w, r) {
		return
	}
	raw := chi.URLParam(r, "id")
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id < 1 {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	d, err := h.store.GetWebhookDelivery(r.Context(), id)
	if errors.Is(err, store.ErrWebhookDeliveryNotFound) {
		http.Error(w, "webhook delivery not found", http.StatusNotFound)
		return
	}
	if err != nil {
		h.log.Error("admin webhook detail", "id", id, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, d)
}

// Health handles GET /api/v1/admin/health. Returns a small payload
// suitable for a traffic-light UI card. Computed from the same cheap
// aggregates the dashboard uses, so safe to hit every few seconds.
func (h *Handler) Health(w http.ResponseWriter, r *http.Request) {
	if !methodGET(w, r) {
		return
	}
	dbOK := true
	dbErr := ""
	metrics, err := h.store.GetDashboardMetrics(r.Context())
	if err != nil {
		dbOK = false
		dbErr = err.Error()
		h.log.Warn("admin health: metrics", "err", err)
	}
	agents, err := h.store.ListAgentsWithRunning(r.Context(), time.Now())
	if err != nil {
		h.log.Warn("admin health: agents", "err", err)
	}
	online, stale, offline := 0, 0, 0
	for _, a := range agents {
		switch a.HealthState {
		case "online", "idle":
			online++
		case "stale":
			stale++
		case "offline":
			offline++
		}
	}

	writeJSON(w, map[string]any{
		"db_ok":           dbOK,
		"db_error":        dbErr,
		"agents_online":   online,
		"agents_stale":    stale,
		"agents_offline":  offline,
		"queued_runs":     metrics.QueuedRuns,
		"pending_jobs":    metrics.PendingJobs,
		"success_rate_7d": metrics.SuccessRate7d,
		"checked_at":      time.Now().UTC(),
	})
}

// Integrations handles GET /api/v1/admin/integrations. Returns a
// poly-provider summary: per-provider readiness + the shared
// public base URL the UI needs to render copy-paste webhook
// endpoints. Superset of the older /integrations/github shape.
//
// GitHub readiness = App installed + public base set + checks
// reporter toggle. GitLab / Bitbucket readiness = public base
// set (auto-register needs a reachable callback; per-project
// PAT/App-Password comes from scm_source.auth_ref at bind time).
func (h *Handler) Integrations(w http.ResponseWriter, r *http.Request) {
	if !methodGET(w, r) {
		return
	}
	appActive := h.vcs != nil && h.vcs.GitHubApp() != nil
	publicBase := h.wiring.PublicBase
	// Auto-register needs both the provider credential AND a
	// reachable public base. GitHub's credential is the App
	// (global); GitLab/Bitbucket credentials are per-scm_source
	// so we can only say "ready at the wiring level" here.
	githubAutoOK := appActive && h.wiring.PublicBaseSet
	writeJSON(w, map[string]any{
		"public_base":     publicBase,
		"public_base_set": h.wiring.PublicBaseSet,
		"github": map[string]any{
			"app_configured":     appActive,
			"checks_reporter_on": h.wiring.ChecksReporterOn,
			"auto_register_on":   githubAutoOK,
			"webhook_endpoint":   webhookURL(publicBase, "github"),
		},
		"gitlab": map[string]any{
			"auto_register_on": h.wiring.PublicBaseSet,
			"webhook_endpoint": webhookURL(publicBase, "gitlab"),
			"required_scope":   "api",
		},
		"bitbucket": map[string]any{
			"auto_register_on": h.wiring.PublicBaseSet,
			"webhook_endpoint": webhookURL(publicBase, "bitbucket"),
			"required_scope":   "webhooks",
		},
	})
	return
}

// webhookURL returns the public endpoint a provider will POST
// deliveries to. Empty public_base → empty URL so the UI
// renders "not configured" instead of a broken link.
func webhookURL(publicBase, provider string) string {
	if publicBase == "" {
		return ""
	}
	trimmed := publicBase
	for len(trimmed) > 0 && trimmed[len(trimmed)-1] == '/' {
		trimmed = trimmed[:len(trimmed)-1]
	}
	return trimmed + "/api/webhooks/" + provider
}

// IntegrationGitHub is the legacy shape kept so in-flight UI
// callers don't 404 mid-refresh. New code reads /integrations.
// Drops after the UI migrates and we ship a release.
func (h *Handler) IntegrationGitHub(w http.ResponseWriter, r *http.Request) {
	if !methodGET(w, r) {
		return
	}
	appActive := h.vcs != nil && h.vcs.GitHubApp() != nil
	autoRegisterOn := appActive && h.wiring.PublicBaseSet
	writeJSON(w, map[string]any{
		"github_app_configured": appActive,
		"public_base_set":       h.wiring.PublicBaseSet,
		"checks_reporter_on":    h.wiring.ChecksReporterOn,
		"auto_register_on":      autoRegisterOn,
	})
}

// --- tiny helpers ---

func methodGET(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}
