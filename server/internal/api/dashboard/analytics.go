package dashboard

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

const (
	maxAnalyticsLabelKeyLen = 100
	maxAnalyticsEnvLen      = 100
)

// LabelKeys handles GET /api/v1/analytics/label-keys — the distinct project
// label keys available as a "group by" dimension.
func (h *Handler) LabelKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := h.store.LabelKeys(r.Context())
	if err != nil {
		h.log.Error("analytics: label keys", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if keys == nil {
		keys = []string{}
	}
	writeAnalyticsJSON(w, map[string]any{"keys": keys})
}

// requireKey validates the shared `key` query param (required, bounded, no ':').
func requireKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	key := strings.TrimSpace(r.URL.Query().Get("key"))
	if key == "" {
		http.Error(w, "key is required", http.StatusBadRequest)
		return "", false
	}
	if len(key) > maxAnalyticsLabelKeyLen || strings.Contains(key, ":") {
		http.Error(w, "invalid key", http.StatusBadRequest)
		return "", false
	}
	return key, true
}

// Environments handles GET /api/v1/analytics/environments?key= — the deploy
// environments available as the dashboard's environment filter.
func (h *Handler) Environments(w http.ResponseWriter, r *http.Request) {
	key, ok := requireKey(w, r)
	if !ok {
		return
	}
	envs, err := h.store.Environments(r.Context(), key)
	if err != nil {
		h.log.Error("analytics: environments", "key", key, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if envs == nil {
		envs = []string{}
	}
	writeAnalyticsJSON(w, map[string]any{"environments": envs})
}

// parseEnvironment reads the optional `environment` filter; "" means all. A
// bounded string — the store query treats empty as no filter.
func parseEnvironment(w http.ResponseWriter, r *http.Request) (string, bool) {
	env := strings.TrimSpace(r.URL.Query().Get("environment"))
	if len(env) > maxAnalyticsEnvLen {
		http.Error(w, "invalid environment", http.StatusBadRequest)
		return "", false
	}
	return env, true
}

// parseDoraParams validates the shared `key` (required, bounded, no ':') and
// `window_days` (1..365, default 30) query params. On a bad request it writes
// the 400 and returns ok=false.
func parseDoraParams(w http.ResponseWriter, r *http.Request) (key string, windowDays int, ok bool) {
	key = strings.TrimSpace(r.URL.Query().Get("key"))
	if key == "" {
		http.Error(w, "key is required", http.StatusBadRequest)
		return "", 0, false
	}
	if len(key) > maxAnalyticsLabelKeyLen || strings.Contains(key, ":") {
		http.Error(w, "invalid key", http.StatusBadRequest)
		return "", 0, false
	}
	windowDays = 30
	if v := r.URL.Query().Get("window_days"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 365 {
			http.Error(w, "window_days must be 1..365", http.StatusBadRequest)
			return "", 0, false
		}
		windowDays = n
	}
	return key, windowDays, true
}

// DoraRollup handles GET /api/v1/analytics/dora?key=<labelKey>&window_days=<N>.
// Returns the four DORA metrics for each value of the label key over the
// trailing window. `key` is required.
func (h *Handler) DoraRollup(w http.ResponseWriter, r *http.Request) {
	key, windowDays, ok := parseDoraParams(w, r)
	if !ok {
		return
	}
	environment, ok := parseEnvironment(w, r)
	if !ok {
		return
	}

	groups, err := h.store.DoraRollup(r.Context(), key, windowDays, environment)
	if err != nil {
		h.log.Error("analytics: dora rollup", "key", key, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeAnalyticsJSON(w, map[string]any{
		"key":         key,
		"window_days": windowDays,
		"environment": environment,
		"groups":      groups,
	})
}

// Overview handles GET /api/v1/analytics/dora/overview?key=&window_days=. It is
// the single read behind the redesigned Analytics page: org rollup (current +
// prior window for deltas), the daily series for sparklines, and the per-team
// leaderboard.
func (h *Handler) Overview(w http.ResponseWriter, r *http.Request) {
	key, windowDays, ok := parseDoraParams(w, r)
	if !ok {
		return
	}
	environment, ok := parseEnvironment(w, r)
	if !ok {
		return
	}

	ov, err := h.store.AnalyticsOverview(r.Context(), key, windowDays, environment)
	if err != nil {
		h.log.Error("analytics: overview", "key", key, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeAnalyticsJSON(w, ov)
}

// Reliability handles GET /api/v1/analytics/reliability?key=&window_days=. It
// returns run-based throughput per label-value group plus the worst-failing
// pipelines among labelled projects. No environment filter — runs aren't
// environment-scoped (unlike deploys).
func (h *Handler) Reliability(w http.ResponseWriter, r *http.Request) {
	key, windowDays, ok := parseDoraParams(w, r)
	if !ok {
		return
	}
	rep, err := h.store.ReliabilityReport(r.Context(), key, windowDays)
	if err != nil {
		h.log.Error("analytics: reliability", "key", key, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeAnalyticsJSON(w, rep)
}

func writeAnalyticsJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}
