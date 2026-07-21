package projects

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/api/authapi"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// deployWatchDTO is one in-flight native deploy for the live-status endpoint. The
// live-state fields are viewer-readable; the config fields (application/cluster/
// sync_mode) are MAINTAINER-only — the same fields protected on /deploy-targets — and
// omitted for viewers via omitempty.
type deployWatchDTO struct {
	DeploymentRevisionID string     `json:"deployment_revision_id"`
	Environment          string     `json:"environment"`
	Version              string     `json:"version"`
	ExpectedRevision     string     `json:"expected_revision"`
	WatchStartedAt       time.Time  `json:"watch_started_at"`
	SyncRequestedAt      *time.Time `json:"sync_requested_at,omitempty"`
	DeadlineAt           time.Time  `json:"deadline_at"`
	DegradedSince        *time.Time `json:"degraded_since,omitempty"`

	// Rollout live state (viewer-readable — progress, not config). Absent when the
	// deploy isn't rollout-aware or hasn't been observed. RolloutCurrentStep is a
	// pointer so an unknown controller step index renders distinctly from step 0.
	RolloutAware       bool       `json:"rollout_aware,omitempty"`
	RolloutPhase       string     `json:"rollout_phase,omitempty"`
	RolloutMessage     string     `json:"rollout_message,omitempty"`
	RolloutPauseReason string     `json:"rollout_pause_reason,omitempty"`
	RolloutCurrentStep *int       `json:"rollout_current_step,omitempty"`
	RolloutStepCount   int        `json:"rollout_step_count,omitempty"`
	RolloutAborted     bool       `json:"rollout_aborted,omitempty"`
	RolloutError       string     `json:"rollout_error,omitempty"`
	RolloutObservedAt  *time.Time `json:"rollout_observed_at,omitempty"`

	// Gate live-state (viewer-readable). GateID is the armed token the UI echoes back on
	// approve/reject (empty when no step is armed). GatePausedStep is a pointer (absent
	// vs step 0). GateApprovalsNow / GateRequired render "awaiting approval (1/2)".
	GateID           string `json:"gate_id,omitempty"`
	GatePausedStep   *int   `json:"gate_paused_step,omitempty"`
	GateRequired     int    `json:"gate_required,omitempty"`
	GateDecision     string `json:"gate_decision,omitempty"`
	GateApprovalsNow int    `json:"gate_approvals_now,omitempty"`

	// Maintainer-only config.
	Application string `json:"application,omitempty"`
	Cluster     string `json:"cluster,omitempty"`
	SyncMode    string `json:"sync_mode,omitempty"`
}

// ListDeployWatches returns the project's in-flight native deploys for the UI to poll.
// Viewer-readable (route is in the authenticated group), but the response is
// role-sanitised: only maintainers get the config fields, so this endpoint can't leak
// what /deploy-targets protects.
func (h *Handler) ListDeployWatches(w http.ResponseWriter, r *http.Request) {
	projectID, ok := h.resolveProjectID(w, r, chi.URLParam(r, "slug"))
	if !ok {
		return
	}
	// showConfig defaults true so an auth-disabled deployment (no user in context)
	// sees everything; with auth on, RequireAuth guarantees a user and the role gates it.
	showConfig := true
	if u, ok := authapi.UserFromContext(r.Context()); ok {
		showConfig = store.RoleSatisfies(u.Role, store.RoleMaintainer)
	}

	watches, err := h.store.ListDeployWatchesForProject(r.Context(), projectID)
	if err != nil {
		h.log.Error("deploy watches: list", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	dtos := make([]deployWatchDTO, 0, len(watches))
	for _, wch := range watches {
		d := deployWatchDTO{
			DeploymentRevisionID: wch.DeploymentRevisionID.String(),
			Environment:          wch.Environment,
			Version:              wch.Version,
			ExpectedRevision:     wch.ExpectedRevision,
			WatchStartedAt:       wch.WatchStartedAt,
			SyncRequestedAt:      wch.SyncRequestedAt,
			DeadlineAt:           wch.DeadlineAt,
			DegradedSince:        wch.DegradedSince,
			RolloutAware:         wch.RolloutAware,
			RolloutPhase:         wch.RolloutPhase,
			RolloutMessage:       wch.RolloutMessage,
			RolloutPauseReason:   wch.RolloutPauseReason,
			RolloutStepCount:     wch.RolloutStepCount,
			RolloutAborted:       wch.RolloutAborted,
			RolloutError:         wch.RolloutError,
			RolloutObservedAt:    wch.RolloutObservedAt,
		}
		if wch.RolloutStepKnown {
			step := wch.RolloutCurrentStep
			d.RolloutCurrentStep = &step
		}
		// Gate live-state (viewer-readable). Only a currently-armed gate exposes a token.
		if wch.GateID != uuid.Nil {
			d.GateID = wch.GateID.String()
			d.GateApprovalsNow = wch.GateApprovalsNow
			if wch.GatePausedStepKnown {
				step := wch.GatePausedStep
				d.GatePausedStep = &step
			}
			if wch.GateRequiredKnown {
				d.GateRequired = wch.GateRequired
			}
			d.GateDecision = wch.GateDecision
		}
		if showConfig {
			d.Application = wch.Application
			d.Cluster = wch.Cluster
			d.SyncMode = wch.SyncMode
		}
		dtos = append(dtos, d)
	}
	writeJSON(w, http.StatusOK, map[string]any{"deploy_watches": dtos})
}
