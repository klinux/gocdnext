package projects

import (
	"context"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/deploy"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// rolloutLister lists Argo Rollouts through the cluster registry's credentialed
// transport (deploy.RolloutLister satisfies it). Behind an interface so the handler
// test can inject a fake without a live cluster; nil => the endpoint answers 501.
type rolloutLister interface {
	ListRollouts(ctx context.Context, cluster string, projectID uuid.UUID, namespace string) ([]deploy.RolloutView, error)
}

// WithRolloutLister wires the Argo Rollouts read transport (ADR-0001), mirroring
// WithDeployRegistrar.
func (h *Handler) WithRolloutLister(l rolloutLister) *Handler {
	h.rolloutLister = l
	return h
}

// rolloutStepDTO mirrors deploy.RolloutViewStep. weight is a pointer (null for a
// non-setWeight step); pause_duration=="" is an indefinite pause:{} (the human gate).
type rolloutStepDTO struct {
	Kind          string `json:"kind"`
	Weight        *int   `json:"weight"`
	PauseDuration string `json:"pause_duration"`
}

type rolloutAnalysisDTO struct {
	Name    string `json:"name"`
	Phase   string `json:"phase"`
	Message string `json:"message"`
}

// rolloutGateDTO is the armed-gate correlation attached to the Rollout it governs
// (ADR-0001, PR-C). Present only while a step's gate is armed AND undecided; the UI
// echoes gate_id + revision_id on Approve/Reject and renders approvals_now/required as
// the quorum. `null` on the Rollout when no gate is armed.
type rolloutGateDTO struct {
	GateID       string `json:"gate_id"`
	RevisionID   string `json:"revision_id"`
	ApprovalsNow int    `json:"approvals_now"`
	Required     int    `json:"required"`
}

// rolloutDTO is the snake_case JSON view of one Rollout (mirrors deploy.RolloutView).
type rolloutDTO struct {
	Namespace        string              `json:"namespace"`
	Name             string              `json:"name"`
	Strategy         string              `json:"strategy"`
	Phase            string              `json:"phase"`
	Message          string              `json:"message"`
	Aborted          bool                `json:"aborted"`
	CurrentStepIndex int                 `json:"current_step_index"`
	CurrentStepKnown bool                `json:"current_step_known"`
	Steps            []rolloutStepDTO    `json:"steps"`
	CanaryWeight     int                 `json:"canary_weight"`
	StableHash       string              `json:"stable_hash"`
	PodHash          string              `json:"pod_hash"`
	Image            string              `json:"image"`
	Analysis         *rolloutAnalysisDTO `json:"analysis"`
	// Gate is the armed, undecided approval gate governing this Rollout, correlated by
	// pinned (namespace, name). nil (JSON null) when none is armed.
	Gate *rolloutGateDTO `json:"gate"`
}

type rolloutsListResponse struct {
	Rollouts []rolloutDTO `json:"rollouts"`
}

func toRolloutDTO(v deploy.RolloutView) rolloutDTO {
	// Always a non-nil array so the TS contract is a stable [] (never null) even for a
	// step-less blue-green Rollout.
	steps := make([]rolloutStepDTO, 0, len(v.Steps))
	for _, s := range v.Steps {
		steps = append(steps, rolloutStepDTO{Kind: s.Kind, Weight: s.Weight, PauseDuration: s.PauseDuration})
	}
	dto := rolloutDTO{
		Namespace:        v.Namespace,
		Name:             v.Name,
		Strategy:         v.Strategy,
		Phase:            string(v.Phase),
		Message:          v.Message,
		Aborted:          v.Aborted,
		CurrentStepIndex: v.CurrentStepIndex,
		CurrentStepKnown: v.CurrentStepKnown,
		Steps:            steps,
		CanaryWeight:     v.CanaryWeight,
		StableHash:       v.StableHash,
		PodHash:          v.PodHash,
		Image:            v.Image,
	}
	if v.Analysis != nil {
		dto.Analysis = &rolloutAnalysisDTO{
			Name: v.Analysis.Name, Phase: string(v.Analysis.Phase), Message: v.Analysis.Message,
		}
	}
	return dto
}

// ListRollouts handles GET /api/v1/projects/{slug}/rollouts?cluster=<name>&namespace=<ns>.
// Maintainer-gated by route placement (the cluster/namespace + Rollout topology is
// operator config, like deploy targets). Both query params are REQUIRED — a cluster-wide
// or namespace-less list is a deliberate follow-up.
func (h *Handler) ListRollouts(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	if h.rolloutLister == nil {
		http.Error(w, "rollouts are not configured on this server", http.StatusNotImplemented)
		return
	}
	cluster := strings.TrimSpace(r.URL.Query().Get("cluster"))
	if cluster == "" {
		http.Error(w, "cluster query parameter is required", http.StatusBadRequest)
		return
	}
	namespace := strings.TrimSpace(r.URL.Query().Get("namespace"))
	if namespace == "" {
		http.Error(w, "namespace query parameter is required", http.StatusBadRequest)
		return
	}
	projectID, ok := h.resolveProjectID(w, r, slug)
	if !ok {
		return
	}

	views, err := h.rolloutLister.ListRollouts(r.Context(), cluster, projectID, namespace)
	if err != nil {
		// Collapse the cluster missing-vs-unauthorized oracle to a single 404 (see
		// store.ClusterUnavailableMessage); log the specific reason for operators.
		// Everything else is a generic 500 — a transport error may carry the internal
		// API-server URL, which must not reach the caller.
		if store.IsClusterUnavailable(err) {
			http.Error(w, store.ClusterUnavailableMessage, http.StatusNotFound)
			return
		}
		h.log.Error("list rollouts", "slug", slug, "cluster", cluster, "namespace", namespace, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	out := make([]rolloutDTO, 0, len(views))
	for _, v := range views {
		out = append(out, toRolloutDTO(v))
	}
	// Correlate armed gates onto the matching Rollouts by pinned (namespace, name). This
	// enrichment is best-effort: the direct Promote/Abort endpoint does its OWN armed-gate
	// lookup and fails closed (409) on a gated Rollout, so a transient correlation error
	// must not blank the whole dashboard — log it and serve the Rollouts without gates.
	if gates, gerr := h.armedGates(r.Context(), projectID, cluster); gerr != nil {
		h.log.Error("rollouts: correlate armed gates", "slug", slug, "cluster", cluster, "err", gerr)
	} else {
		attachArmedGates(out, gates)
	}
	writeJSON(w, http.StatusOK, rolloutsListResponse{Rollouts: out})
}

// gateKey identifies a Rollout by its pinned namespace/name. The NUL separator can't
// appear in a k8s name/namespace, so distinct pairs never collide on the joined key.
func gateKey(namespace, name string) string { return namespace + "\x00" + name }

// attachArmedGates hangs each armed gate on the Rollout DTO it governs, matched by
// (namespace, name). A gate with no matching Rollout in the list is dropped (the Rollout
// may have finished the step between the gate read and the list read) — the write
// endpoint re-checks, so nothing is actionable off a stale correlation.
func attachArmedGates(out []rolloutDTO, gates []store.ArmedRolloutGate) {
	if len(gates) == 0 {
		return
	}
	byKey := make(map[string]store.ArmedRolloutGate, len(gates))
	for _, g := range gates {
		byKey[gateKey(g.Namespace, g.Name)] = g
	}
	for i := range out {
		if g, ok := byKey[gateKey(out[i].Namespace, out[i].Name)]; ok {
			out[i].Gate = &rolloutGateDTO{
				GateID:       g.GateID.String(),
				RevisionID:   g.RevisionID.String(),
				ApprovalsNow: g.ApprovalsNow,
				Required:     g.Required,
			}
		}
	}
}
