package projects

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/audit"
	"github.com/gocdnext/gocdnext/server/internal/deploy"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// rolloutActuator drives a direct Promote/Abort on an Argo Rollouts canary
// (*deploy.ArgoProvider satisfies it). Behind an interface so the handler test injects a
// fake that records the call without a live cluster; nil => the endpoints answer 501.
type rolloutActuator interface {
	Promote(ctx context.Context, target deploy.DeploymentTarget) error
	Abort(ctx context.Context, target deploy.DeploymentTarget) error
}

// rolloutGateLookup returns the project's armed, undecided rollout gates on a cluster
// (*store.Store satisfies it). It backs BOTH the read correlation and the write guard;
// a test seam injects a fake so the promote/abort 409 path can be exercised without
// seeding a full gated-deploy lifecycle.
type rolloutGateLookup interface {
	ListArmedRolloutGatesForCluster(ctx context.Context, projectID uuid.UUID, cluster string) ([]store.ArmedRolloutGate, error)
}

// WithRolloutActuator wires the direct Promote/Abort transport (ADR-0001 PR-C), mirroring
// WithRolloutLister. The same *deploy.ArgoProvider backs the deploy watcher's gated path.
func (h *Handler) WithRolloutActuator(a rolloutActuator) *Handler {
	h.rolloutActuator = a
	return h
}

// WithRolloutGateLookup overrides the armed-gate source (default: the store). Intended for
// tests — production leaves it nil so armedGates falls through to the store.
func (h *Handler) WithRolloutGateLookup(l rolloutGateLookup) *Handler {
	h.gateLookup = l
	return h
}

// armedGates resolves the armed-gate source (the injected seam, else the store) and lists
// the project's armed, undecided gates on the cluster.
func (h *Handler) armedGates(ctx context.Context, projectID uuid.UUID, cluster string) ([]store.ArmedRolloutGate, error) {
	lookup := rolloutGateLookup(h.store)
	if h.gateLookup != nil {
		lookup = h.gateLookup
	}
	return lookup.ListArmedRolloutGatesForCluster(ctx, projectID, cluster)
}

// PromoteRollout handles POST /api/v1/projects/{slug}/rollouts/{cluster}/{namespace}/{name}/promote —
// advances a paused canary one step. Maintainer-gated by route placement. AbortRollout is
// the same for abort (traffic → stable, NOT a Git revert). Both refuse a GATED Rollout
// with 409: a gated decision must flow through the audited vote path (Approve/Reject),
// never a direct bypass.
func (h *Handler) PromoteRollout(w http.ResponseWriter, r *http.Request) {
	h.actuateRollout(w, r, "promote")
}

// AbortRollout handles POST .../{name}/abort — reverts the canary's TRAFFIC to stable.
func (h *Handler) AbortRollout(w http.ResponseWriter, r *http.Request) {
	h.actuateRollout(w, r, "abort")
}

func (h *Handler) actuateRollout(w http.ResponseWriter, r *http.Request, verb string) {
	slug := chi.URLParam(r, "slug")
	if h.rolloutActuator == nil {
		http.Error(w, "rollout actuation is not configured on this server", http.StatusNotImplemented)
		return
	}
	cluster := strings.TrimSpace(chi.URLParam(r, "cluster"))
	namespace := strings.TrimSpace(chi.URLParam(r, "namespace"))
	name := strings.TrimSpace(chi.URLParam(r, "name"))
	if cluster == "" || namespace == "" || name == "" {
		http.Error(w, "cluster, namespace and name path segments are required", http.StatusBadRequest)
		return
	}
	projectID, ok := h.resolveProjectID(w, r, slug)
	if !ok {
		return
	}

	// Fail-closed gate guard: a gated Rollout's decision must go through the vote/audit
	// path, never a direct actuation. If the armed-gate lookup itself fails we CANNOT tell
	// whether this Rollout is gated, so we refuse (500) rather than risk a bypass — the
	// read side may fail open, but the write side never does.
	gates, err := h.armedGates(r.Context(), projectID, cluster)
	if err != nil {
		h.log.Error("rollout actuation: armed-gate guard", "slug", slug, "cluster", cluster, "verb", verb, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	for _, g := range gates {
		if g.Namespace == namespace && g.Name == name {
			http.Error(w, "this rollout is gated — use the gate's Approve/Reject, not a direct promote/abort", http.StatusConflict)
			return
		}
	}

	// The URL identity IS the pinned actuation target (RolloutCluster/Namespace/Name);
	// the actuator patches /status directly, never re-discovering. A non-existent Rollout
	// 404s at the patch and surfaces as 404 below.
	target := deploy.DeploymentTarget{
		ProjectID:        projectID,
		RolloutCluster:   cluster,
		RolloutNamespace: namespace,
		RolloutName:      name,
	}
	var actErr error
	if verb == "promote" {
		actErr = h.rolloutActuator.Promote(r.Context(), target)
	} else {
		actErr = h.rolloutActuator.Abort(r.Context(), target)
	}
	if actErr != nil {
		h.writeRolloutActuationError(w, slug, cluster, namespace, name, verb, actErr)
		return
	}

	action := store.AuditActionRolloutPromote
	if verb == "abort" {
		action = store.AuditActionRolloutAbort
	}
	audit.Emit(r.Context(), h.log, h.store, action, "rollout", namespace+"/"+name,
		map[string]any{"slug": slug, "cluster": cluster, "namespace": namespace, "name": name})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// writeRolloutActuationError maps an actuation failure to an HTTP status by KIND, never by
// message (an error may carry the internal API-server URL): an unresolved/unauthorized
// cluster collapses to 404 (see store.ClusterUnavailableMessage), a Rollout the /status
// patch reports missing surfaces as 404, and everything else is a logged generic 500.
func (h *Handler) writeRolloutActuationError(w http.ResponseWriter, slug, cluster, namespace, name, verb string, err error) {
	if store.IsClusterUnavailable(err) {
		http.Error(w, store.ClusterUnavailableMessage, http.StatusNotFound)
		return
	}
	var apiErr *store.ClusterAPIStatusError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
		http.Error(w, "rollout not found on the cluster", http.StatusNotFound)
		return
	}
	h.log.Error("rollout actuation failed",
		"slug", slug, "cluster", cluster, "namespace", namespace, "name", name, "verb", verb, "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}
