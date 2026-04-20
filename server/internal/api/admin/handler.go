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

	"github.com/gocdnext/gocdnext/server/internal/retention"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

const (
	defaultWebhooksLimit int32 = 50
	maxWebhooksLimit     int32 = 200
)

// IntegrationState is what main.go reports about the GitHub integration
// that's already wired at boot. Handed to the handler so the admin UI
// can show "GitHub App configured: yes/no" without poking at env vars.
type IntegrationState struct {
	GitHubAppConfigured bool
	WebhookTokenSet     bool
	PublicBaseSet       bool
	ChecksReporterOn    bool
	AutoRegisterOn      bool
}

// Handler owns the /api/v1/admin/* routes.
type Handler struct {
	store        *store.Store
	sweeper      *retention.Sweeper
	integrations IntegrationState
	log          *slog.Logger
}

// NewHandler wires the admin handler. sweeper may be nil — retention
// endpoint will then report disabled. integrations is a value snapshot
// captured at server boot.
func NewHandler(s *store.Store, sweeper *retention.Sweeper, integrations IntegrationState, log *slog.Logger) *Handler {
	if log == nil {
		log = slog.Default()
	}
	return &Handler{store: s, sweeper: sweeper, integrations: integrations, log: log}
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

// IntegrationGitHub handles GET /api/v1/admin/integrations/github.
// Reports only booleans (the config values themselves can be secret).
func (h *Handler) IntegrationGitHub(w http.ResponseWriter, r *http.Request) {
	if !methodGET(w, r) {
		return
	}
	writeJSON(w, map[string]any{
		"github_app_configured": h.integrations.GitHubAppConfigured,
		"webhook_token_set":     h.integrations.WebhookTokenSet,
		"public_base_set":       h.integrations.PublicBaseSet,
		"checks_reporter_on":    h.integrations.ChecksReporterOn,
		"auto_register_on":      h.integrations.AutoRegisterOn,
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
