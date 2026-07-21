package projects

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/api/authapi"
	"github.com/gocdnext/gocdnext/server/internal/audit"
	"github.com/gocdnext/gocdnext/server/internal/deploy"
	"github.com/gocdnext/gocdnext/server/internal/deploysvc"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// maxDeployTargetBytes caps the register-target request body.
const maxDeployTargetBytes = 4 << 10

// deployTargetDTO is the JSON shape of one registered target.
type deployTargetDTO struct {
	Environment string `json:"environment"`
	Provider    string `json:"provider"`
	Cluster     string `json:"cluster"`
	Application string `json:"application"`
	Namespace   string `json:"namespace"`
	SyncMode    string `json:"sync_mode"`

	RolloutAware     bool   `json:"rollout_aware"`
	RolloutCluster   string `json:"rollout_cluster,omitempty"`
	RolloutNamespace string `json:"rollout_namespace,omitempty"`
	RolloutName      string `json:"rollout_name,omitempty"`

	// GoverningGate is the approval-gate config (control mode). Present => gated. Shown
	// to maintainers (route is maintainer-gated) so the UI can round-trip it on an edit;
	// CHANGING it is admin-only, enforced server-side.
	GoverningGate *store.GoverningGate `json:"governing_gate,omitempty"`
}

// WithDeployRegistrar wires the native deploy-target registrar (ADR-0001).
func (h *Handler) WithDeployRegistrar(r *deploysvc.Registrar) *Handler {
	h.deployRegistrar = r
	return h
}

// SetDeployTarget registers (or updates — it's an upsert) the deploy target for a
// project environment. Maintainer-gated by route placement. On success returns the
// registered target (single upsert status: 200).
func (h *Handler) SetDeployTarget(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	if h.deployRegistrar == nil {
		http.Error(w, "deploy targets are not configured on this server", http.StatusNotImplemented)
		return
	}
	var req struct {
		Environment string `json:"environment"`
		Cluster     string `json:"cluster"`
		Application string `json:"application"`
		Namespace   string `json:"namespace"`
		SyncMode    string `json:"sync_mode"`
		// Rollout observation (Phase 2). rollout_cluster/namespace/name optional
		// (defaults: App's cluster / auto-discover). Ignored when rollout_aware=false.
		RolloutAware     bool   `json:"rollout_aware"`
		RolloutCluster   string `json:"rollout_cluster"`
		RolloutNamespace string `json:"rollout_namespace"`
		RolloutName      string `json:"rollout_name"`
		// GoverningGate — control mode. Setting/changing it (and rerouting a gated
		// target) is admin-only, enforced in the registrar via CallerIsAdmin below.
		GoverningGate *store.GoverningGate `json:"governing_gate"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxDeployTargetBytes)).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	projectID, ok := h.resolveProjectID(w, r, slug)
	if !ok {
		return
	}

	// The route is maintainer-gated; the registrar additionally enforces that
	// gate/routing edits on a gated target are admin-only. Auth-disabled (no user) =>
	// full access, matching the rest of the deploy surface.
	var createdBy string
	callerIsAdmin := true
	if u, ok := authapi.UserFromContext(r.Context()); ok {
		createdBy = u.ID.String()
		callerIsAdmin = store.RoleSatisfies(u.Role, store.RoleAdmin)
	}

	tgt, err := h.deployRegistrar.Register(r.Context(), deploysvc.RegisterInput{
		ProjectID:        projectID,
		Environment:      req.Environment,
		Provider:         "argocd",
		Cluster:          req.Cluster,
		Application:      req.Application,
		Namespace:        req.Namespace,
		SyncMode:         req.SyncMode,
		CreatedBy:        createdBy,
		RolloutAware:     req.RolloutAware,
		RolloutCluster:   req.RolloutCluster,
		RolloutNamespace: req.RolloutNamespace,
		RolloutName:      req.RolloutName,
		GoverningGate:    req.GoverningGate,
		CallerIsAdmin:    callerIsAdmin,
	})
	if err != nil {
		// Enrich every fault log for this request with the request context so a
		// collapsed cluster-oracle rejection (whose caller-facing body is generic)
		// still tells an operator which cluster / project / environment was probed —
		// structured fields, not dependent on the error text carrying the name.
		writeFault(w, h.log.With(
			"slug", slug, "project_id", projectID,
			"cluster", req.Cluster, "environment", req.Environment), err)
		return
	}

	// Register returns the canonical (normalized) target — no read-back needed, so
	// the 200 body is always the promised DTO.
	audit.Emit(r.Context(), h.log, h.store,
		store.AuditActionDeployTargetSet, "environment", tgt.Environment,
		map[string]any{"slug": slug, "cluster": tgt.Cluster, "application": tgt.Application, "sync_mode": tgt.SyncMode})
	writeJSON(w, http.StatusOK, deployTargetDTO{
		Environment: tgt.Environment, Provider: tgt.Provider, Cluster: tgt.Cluster,
		Application: tgt.Application, Namespace: tgt.Namespace, SyncMode: tgt.SyncMode,
		RolloutAware: tgt.RolloutAware, RolloutCluster: tgt.RolloutCluster,
		RolloutNamespace: tgt.RolloutNamespace, RolloutName: tgt.RolloutName,
		GoverningGate: tgt.GoverningGate,
	})
}

// ListDeployTargets returns a project's registered targets.
func (h *Handler) ListDeployTargets(w http.ResponseWriter, r *http.Request) {
	projectID, ok := h.resolveProjectID(w, r, chi.URLParam(r, "slug"))
	if !ok {
		return
	}
	items, err := h.store.ListDeployTargets(r.Context(), projectID)
	if err != nil {
		h.log.Error("deploy targets: list", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	dtos := make([]deployTargetDTO, 0, len(items))
	for _, it := range items {
		dtos = append(dtos, deployTargetDTO{
			Environment: it.Environment, Provider: it.Provider, Cluster: it.Cluster,
			Application: it.Application, Namespace: it.Namespace, SyncMode: it.SyncMode,
			RolloutAware: it.RolloutAware, RolloutCluster: it.RolloutCluster,
			RolloutNamespace: it.RolloutNamespace, RolloutName: it.RolloutName,
			GoverningGate: it.GoverningGate,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"deploy_targets": dtos})
}

// DeleteDeployTarget removes a project environment's deploy target.
func (h *Handler) DeleteDeployTarget(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	env := chi.URLParam(r, "env")
	projectID, ok := h.resolveProjectID(w, r, slug)
	if !ok {
		return
	}
	// SoD: deleting a GATED target is admin-only — otherwise a maintainer could delete
	// it and re-create it without the gate (bypassing the admin-only gate edit). A
	// non-gated target stays maintainer-deletable. Auth-disabled (no user) => admin.
	callerIsAdmin := true
	if u, ok := authapi.UserFromContext(r.Context()); ok {
		callerIsAdmin = store.RoleSatisfies(u.Role, store.RoleAdmin)
	}
	if !callerIsAdmin {
		// Atomic: delete only if still ungated, reporting the outcome in one snapshot so
		// there's no TOCTOU window (a gate added between a read and the delete can't be
		// bypassed — the delete itself is conditional on governing_gate IS NULL).
		outcome, err := h.store.DeleteUngatedDeployTargetByEnvironment(r.Context(), projectID, env)
		if err != nil {
			h.log.Error("deploy targets: guarded delete", "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		switch outcome {
		case store.DeleteTargetGated:
			http.Error(w, "removing a gated deploy target requires admin", http.StatusForbidden)
			return
		case store.DeleteTargetAbsent:
			http.Error(w, "deploy target not found", http.StatusNotFound)
			return
		}
		audit.Emit(r.Context(), h.log, h.store,
			store.AuditActionDeployTargetDelete, "environment", env, map[string]any{"slug": slug})
		w.WriteHeader(http.StatusNoContent)
		return
	}
	deleted, err := h.store.DeleteDeployTargetByEnvironment(r.Context(), projectID, env)
	if err != nil {
		h.log.Error("deploy targets: delete", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !deleted {
		http.Error(w, "deploy target not found", http.StatusNotFound)
		return
	}
	audit.Emit(r.Context(), h.log, h.store,
		store.AuditActionDeployTargetDelete, "environment", env, map[string]any{"slug": slug})
	w.WriteHeader(http.StatusNoContent)
}

// resolveProjectID looks up a project by slug, writing 404/500 and returning
// ok=false on failure.
func (h *Handler) resolveProjectID(w http.ResponseWriter, r *http.Request, slug string) (uuid.UUID, bool) {
	detail, err := h.store.GetProjectDetail(r.Context(), slug, 1)
	if err != nil {
		if errors.Is(err, store.ErrProjectNotFound) {
			http.Error(w, "project not found", http.StatusNotFound)
			return uuid.UUID{}, false
		}
		h.log.Error("deploy targets: project lookup", "slug", slug, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return uuid.UUID{}, false
	}
	return detail.Project.ID, true
}

// writeFault maps a deploysvc.Fault to an HTTP status by kind (never by message).
// Internal faults are logged and return a generic 500 (no internal detail leaked).
func writeFault(w http.ResponseWriter, log *slog.Logger, err error) {
	var f *deploysvc.Fault
	if !errors.As(err, &f) {
		log.Error("deploy targets: unclassified register error", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// A fault carrying a Public message has a caller-safe string distinct from its
	// internal Err — emit the public one and log the detail. This is how the cluster
	// missing-vs-unauthorized oracle stays collapsed for the caller while operators
	// still see which cluster and why in the logs.
	if f.Public != "" {
		log.Warn("deploy targets: register rejected (detail withheld from caller)",
			"status", faultStatus(f.Kind), "err", f.Err)
		http.Error(w, f.Public, faultStatus(f.Kind))
		return
	}
	switch f.Kind {
	case deploysvc.FaultInvalid:
		http.Error(w, f.Error(), http.StatusBadRequest)
	case deploysvc.FaultNotFound:
		http.Error(w, f.Error(), http.StatusNotFound)
	case deploysvc.FaultForbidden:
		http.Error(w, f.Error(), http.StatusForbidden)
	case deploysvc.FaultConflict:
		http.Error(w, f.Error(), http.StatusConflict)
	case deploysvc.FaultUnprocessable:
		// A multi-source rejection is safe to echo; a transport failure may carry the
		// cluster's internal API-server URL, so return a short public message and log
		// the full error.
		if errors.Is(err, deploy.ErrMultiSource) {
			http.Error(w, f.Error(), http.StatusUnprocessableEntity)
		} else {
			log.Error("deploy targets: application could not be validated", "err", f.Err)
			http.Error(w, "the deploy target could not be validated — check the cluster is reachable and the application exists", http.StatusUnprocessableEntity)
		}
	default: // FaultInternal
		log.Error("deploy targets: register failed", "err", f.Err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// faultStatus maps a fault kind to its HTTP status — the single source used by the
// Public-message path (the switch above inlines the same mapping for the
// echo-the-error cases).
func faultStatus(kind deploysvc.FaultKind) int {
	switch kind {
	case deploysvc.FaultInvalid:
		return http.StatusBadRequest
	case deploysvc.FaultNotFound:
		return http.StatusNotFound
	case deploysvc.FaultForbidden:
		return http.StatusForbidden
	case deploysvc.FaultUnprocessable:
		return http.StatusUnprocessableEntity
	case deploysvc.FaultConflict:
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
