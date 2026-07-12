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
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxDeployTargetBytes)).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	projectID, ok := h.resolveProjectID(w, r, slug)
	if !ok {
		return
	}

	var createdBy string
	if u, ok := authapi.UserFromContext(r.Context()); ok {
		createdBy = u.ID.String()
	}

	tgt, err := h.deployRegistrar.Register(r.Context(), deploysvc.RegisterInput{
		ProjectID:   projectID,
		Environment: req.Environment,
		Provider:    "argocd",
		Cluster:     req.Cluster,
		Application: req.Application,
		Namespace:   req.Namespace,
		SyncMode:    req.SyncMode,
		CreatedBy:   createdBy,
	})
	if err != nil {
		writeFault(w, h.log, err)
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
	switch f.Kind {
	case deploysvc.FaultInvalid:
		http.Error(w, f.Error(), http.StatusBadRequest)
	case deploysvc.FaultNotFound:
		http.Error(w, f.Error(), http.StatusNotFound)
	case deploysvc.FaultForbidden:
		http.Error(w, f.Error(), http.StatusForbidden)
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

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
