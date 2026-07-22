package admin

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/api/authapi"
	"github.com/gocdnext/gocdnext/server/internal/audit"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// clusterDTO is the wire shape /api/v1/admin/clusters returns. The
// registry surface is write-only for the SECRET: the sealed credential
// (kubeconfig/token) NEVER crosses the wire on a read. The CA cert,
// however, is a PUBLIC certificate (no private key) and IS echoed back
// as CACert — the UI prefills it on edit so a metadata-only update
// can't silently drop the CA and degrade a token cluster to insecure
// TLS. HasCA stays as a cheap boolean for list rendering.
// AllowedProjects is always a JSON array (nil → []) so the UI can map
// over it without a null guard.
type clusterDTO struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	Description     string   `json:"description"`
	AuthType        string   `json:"auth_type"`
	APIServer       string   `json:"api_server"`
	HasCA           bool     `json:"has_ca"`
	CACert          string   `json:"ca_cert"`
	AllowedProjects []string `json:"allowed_projects"`
	// Always emitted (not omitempty): the admin form round-trips this on a full PUT,
	// and an omitted false would be indistinguishable from "field absent" — which is
	// exactly the ambiguity the *bool on the request side exists to avoid.
	AllowDeclarativeTargets bool   `json:"allow_declarative_targets"`
	CreatedBy               string `json:"created_by"`
	CreatedAt               string `json:"created_at"`
	UpdatedAt               string `json:"updated_at"`
}

// clusterWriteRequest is the create/update payload.
//
// CACert is a PEM string on the wire (decoded to []byte before it
// reaches the store). Credential is the kubeconfig (auth_type=
// kubeconfig) or bearer token (auth_type=token); empty for
// in_cluster. On Update, Credential may be store.SecretPreserveSentinel
// to keep the existing sealed credential so a metadata-only edit
// doesn't force re-entering the kubeconfig.
type clusterWriteRequest struct {
	Name            string   `json:"name"`
	Description     string   `json:"description"`
	AuthType        string   `json:"auth_type"`
	APIServer       string   `json:"api_server"`
	CACert          string   `json:"ca_cert"`
	Credential      string   `json:"credential"`
	AllowedProjects []string `json:"allowed_projects"`
	// AllowDeclarativeTargets is a POINTER because this is a full-PUT payload: a plain
	// bool would let a client that predates the field send an implicit false and
	// silently revoke the opt-in on every unrelated edit. Absent => preserve.
	AllowDeclarativeTargets *bool `json:"allow_declarative_targets,omitempty"`
}

// maxClusterBytes bounds the create/update body. A kubeconfig with an
// embedded client cert plus a CA PEM comfortably fits under 256 KiB;
// anything larger is almost certainly an upload mistake or an attack.
const maxClusterBytes = 256 << 10

// supportedClusterAuthTypes is the allow-list checked at write time.
// Mirrors the store's validateClusterInput switch so a typo surfaces
// as a readable 400 before any DB work (the store revalidates too —
// defence in depth, nobody bypasses by forgetting the validator).
var supportedClusterAuthTypes = map[string]struct{}{
	store.ClusterAuthKubeconfig: {},
	store.ClusterAuthToken:      {},
	store.ClusterAuthInCluster:  {},
}

// Clusters handles GET /api/v1/admin/clusters.
func (h *Handler) Clusters(w http.ResponseWriter, r *http.Request) {
	if !methodGET(w, r) {
		return
	}
	rows, err := h.store.ListClusters(r.Context())
	if err != nil {
		h.log.Error("admin clusters: list", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// Bare JSON array (not an envelope) — the UI query reads
	// `AdminCluster[]` directly. make(...0) → `[]`, never null.
	out := make([]clusterDTO, 0, len(rows))
	for _, c := range rows {
		out = append(out, toClusterDTO(c))
	}
	writeJSON(w, out)
}

// CreateCluster handles POST /api/v1/admin/clusters.
func (h *Handler) CreateCluster(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	req, ok := decodeClusterWrite(w, r)
	if !ok {
		return
	}
	// kubeconfig/token need the cipher to seal the credential. Mirror
	// the runner-profiles nil-cipher guard: 503 so operators see the
	// same "feature unavailable" signal regardless of scope. in_cluster
	// carries no credential, so it stays usable with the cipher off.
	if req.AuthType != store.ClusterAuthInCluster && h.cipher == nil {
		http.Error(w, "cluster credentials unavailable: server cipher not configured", http.StatusServiceUnavailable)
		return
	}
	in := clusterInputFromReq(req)
	if u, ok := authapi.UserFromContext(r.Context()); ok {
		in.CreatedBy = u.Email
	}
	c, err := h.store.InsertCluster(r.Context(), h.cipher, in)
	if err != nil {
		if status, msg, handled := mapClusterWriteErr(err); handled {
			http.Error(w, msg, status)
			return
		}
		h.log.Error("admin clusters: create", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// Audit captures metadata only — NEVER the credential. Operators
	// tracing "who wired the prod cluster" need name + auth_type +
	// allowed projects + actor + time; the kubeconfig belongs in the
	// encrypted column, period.
	audit.Emit(r.Context(), h.log, h.store,
		store.AuditActionClusterCreate, "cluster", c.ID.String(),
		map[string]any{
			"name":                      c.Name,
			"auth_type":                 c.AuthType,
			"allowed_projects":          sortedStrings(c.AllowedProjects),
			"allow_declarative_targets": c.AllowDeclarativeTargets,
		})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(toClusterDTO(c))
}

// UpdateCluster handles PUT /api/v1/admin/clusters/{id}.
func (h *Handler) UpdateCluster(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		w.Header().Set("Allow", "PUT")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id, ok := parseClusterID(w, r)
	if !ok {
		return
	}
	req, ok := decodeClusterWrite(w, r)
	if !ok {
		return
	}
	// A new (non-preserve) credential for kubeconfig/token needs the
	// cipher. The preserve sentinel re-seals existing ciphertext and
	// in_cluster carries none, so both stay usable with the cipher off.
	needsCipher := req.AuthType != store.ClusterAuthInCluster && req.Credential != store.SecretPreserveSentinel
	if needsCipher && h.cipher == nil {
		http.Error(w, "cluster credentials unavailable: server cipher not configured", http.StatusServiceUnavailable)
		return
	}
	// Name is the dispatch-time identity of every `cluster:` reference,
	// so it's immutable (the store ignores it on update too — defence in
	// depth). Reject a rename loudly instead of silently no-op'ing it, so
	// the operator isn't misled into thinking the new name took effect.
	existing, err := h.store.GetCluster(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrClusterNotFound) {
			http.Error(w, "cluster not found", http.StatusNotFound)
			return
		}
		h.log.Error("admin clusters: lookup before update", "id", id, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if req.Name != existing.Name {
		http.Error(w, "cluster name is immutable — delete and recreate to rename (the delete-guard will surface any live dependents)", http.StatusUnprocessableEntity)
		return
	}
	in := clusterInputFromReq(req)
	// Preserve-when-absent: this is a full-PUT payload, so a client that predates the
	// field omits it — which must not silently revoke the opt-in on an unrelated edit.
	if req.AllowDeclarativeTargets == nil {
		in.AllowDeclarativeTargets = existing.AllowDeclarativeTargets
	}
	if err := h.store.UpdateCluster(r.Context(), h.cipher, id, in); err != nil {
		if errors.Is(err, store.ErrClusterNotFound) {
			http.Error(w, "cluster not found", http.StatusNotFound)
			return
		}
		if status, msg, handled := mapClusterWriteErr(err); handled {
			http.Error(w, msg, status)
			return
		}
		h.log.Error("admin clusters: update", "id", id, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	audit.Emit(r.Context(), h.log, h.store,
		store.AuditActionClusterUpdate, "cluster", id.String(),
		map[string]any{
			"name":                      req.Name,
			"auth_type":                 req.AuthType,
			"allowed_projects":          sortedStrings(req.AllowedProjects),
			"allow_declarative_targets": in.AllowDeclarativeTargets,
		})
	w.WriteHeader(http.StatusNoContent)
}

// TestCluster handles POST /api/v1/admin/clusters/{id}/test — an
// on-demand connectivity probe against the registered deploy target.
// Returns the probe result ({status, message}); the credential is
// resolved inside the store and never crosses this boundary.
func (h *Handler) TestCluster(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id, ok := parseClusterID(w, r)
	if !ok {
		return
	}
	res, err := h.store.ProbeCluster(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrClusterNotFound) {
			http.Error(w, "cluster not found", http.StatusNotFound)
			return
		}
		h.log.Error("admin clusters: probe", "id", id, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, res)
}

// DeleteCluster handles DELETE /api/v1/admin/clusters/{id}. Refuses
// to delete a cluster any pipeline still names or any queued/running
// run is bound to — the dispatcher would fail to resolve it
// otherwise. The admin must rewire the pipelines / drain the runs
// first. Usage is counted by NAME, so the row is looked up first.
func (h *Handler) DeleteCluster(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		w.Header().Set("Allow", "DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id, ok := parseClusterID(w, r)
	if !ok {
		return
	}
	existing, err := h.store.GetCluster(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrClusterNotFound) {
			http.Error(w, "cluster not found", http.StatusNotFound)
			return
		}
		h.log.Error("admin clusters: lookup before delete", "id", id, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	usage, err := h.store.CountClusterUsage(r.Context(), existing.Name)
	if err != nil {
		h.log.Error("admin clusters: count usage", "name", existing.Name, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if usage.Pipelines > 0 || usage.ActiveRuns > 0 {
		http.Error(w, formatClusterUsageError(usage), http.StatusConflict)
		return
	}
	// A cluster referenced by a deploy_target is FK-RESTRICTed at the DB — surface a
	// friendly 409 here rather than letting the DELETE fail as a raw 500.
	targets, err := h.store.CountDeployTargetsForCluster(r.Context(), existing.Name)
	if err != nil {
		h.log.Error("admin clusters: count deploy targets", "name", existing.Name, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if targets > 0 {
		http.Error(w, fmt.Sprintf(
			"cluster is referenced by %d deploy target(s) — remove them before deleting", targets),
			http.StatusConflict)
		return
	}
	// An in-flight deploy watch also FK-RESTRICTs the cluster; block with a friendly
	// 409 (a live deploy is watching this cluster) rather than a raw FK 500.
	watches, err := h.store.CountActiveWatchesForCluster(r.Context(), existing.Name)
	if err != nil {
		h.log.Error("admin clusters: count active watches", "name", existing.Name, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if watches > 0 {
		http.Error(w, fmt.Sprintf(
			"cluster has %d in-flight deploy(s) — wait for them to finish before deleting", watches),
			http.StatusConflict)
		return
	}
	if err := h.store.DeleteCluster(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrClusterNotFound) {
			http.Error(w, "cluster not found", http.StatusNotFound)
			return
		}
		// Race backstop: a deploy target or an in-flight deploy watch created after the
		// pre-checks trips the FK — cover both generically rather than naming just one.
		if errors.Is(err, store.ErrClusterInUse) {
			http.Error(w, "cluster is still in use (deploy target or in-flight deploy) — remove/wait before deleting", http.StatusConflict)
			return
		}
		h.log.Error("admin clusters: delete", "id", id, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	audit.Emit(r.Context(), h.log, h.store,
		store.AuditActionClusterDelete, "cluster", id.String(),
		map[string]any{
			"name":             existing.Name,
			"auth_type":        existing.AuthType,
			"allowed_projects": sortedStrings(existing.AllowedProjects),
		})
	w.WriteHeader(http.StatusNoContent)
}

// formatClusterUsageError builds the 409 message the admin sees when
// a delete is blocked. Always names both axes (pipelines + active
// runs) so the operator knows whether the fix is "rewire pipelines"
// (static), "wait for runs to drain" (dynamic), or both.
func formatClusterUsageError(u store.ClusterUsage) string {
	switch {
	case u.Pipelines > 0 && u.ActiveRuns > 0:
		return fmt.Sprintf(
			"cluster is referenced by %d pipeline(s) with %d active run(s) — rewire the pipelines and wait for the runs to drain before deleting",
			u.Pipelines, u.ActiveRuns)
	case u.Pipelines > 0:
		return fmt.Sprintf(
			"cluster is referenced by %d pipeline(s) — remove the references before deleting",
			u.Pipelines)
	case u.ActiveRuns > 0:
		return fmt.Sprintf(
			"cluster is still bound to %d active run(s) (queued or running) — wait for them to finish or cancel them before deleting",
			u.ActiveRuns)
	}
	return "cluster is in use"
}

// --- helpers ---

func parseClusterID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid cluster id", http.StatusBadRequest)
		return uuid.Nil, false
	}
	return id, true
}

func decodeClusterWrite(w http.ResponseWriter, r *http.Request) (clusterWriteRequest, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxClusterBytes)
	var req clusterWriteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return req, false
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return req, false
	}
	req.AuthType = strings.TrimSpace(req.AuthType)
	if req.AuthType == "" {
		http.Error(w, "auth_type is required", http.StatusBadRequest)
		return req, false
	}
	if _, ok := supportedClusterAuthTypes[req.AuthType]; !ok {
		http.Error(w, "unsupported auth_type (allowed: kubeconfig, token, in_cluster)", http.StatusBadRequest)
		return req, false
	}
	// api_server is only meaningful for token auth — that's the one
	// path that synthesizes a kubeconfig from server+CA+token. A
	// kubeconfig blob already embeds its own server, and in_cluster
	// uses the pod's in-cluster endpoint; requiring api_server for
	// either was a false barrier.
	if req.AuthType == store.ClusterAuthToken && strings.TrimSpace(req.APIServer) == "" {
		http.Error(w, "api_server is required for token auth", http.StatusBadRequest)
		return req, false
	}
	return req, true
}

// clusterInputFromReq decodes the PEM CACert string to bytes and maps
// the wire shape onto the store input. The credential is passed
// through verbatim — including store.SecretPreserveSentinel, which
// the store interprets as "keep the existing sealed value" on update.
func clusterInputFromReq(req clusterWriteRequest) store.ClusterInput {
	var ca []byte
	if s := strings.TrimSpace(req.CACert); s != "" {
		ca = []byte(s)
	}
	return store.ClusterInput{
		Name:            req.Name,
		Description:     req.Description,
		AuthType:        req.AuthType,
		APIServer:       req.APIServer,
		CACert:          ca,
		Credential:      req.Credential,
		AllowedProjects: req.AllowedProjects,
		// Caller resolves the preserve-when-absent semantics (it needs the existing
		// row); this helper only carries what the request stated.
		AllowDeclarativeTargets: req.AllowDeclarativeTargets != nil && *req.AllowDeclarativeTargets,
	}
}

func toClusterDTO(c store.Cluster) clusterDTO {
	allowed := c.AllowedProjects
	if allowed == nil {
		// JSON-serialised empty array, not null — the UI maps over it.
		allowed = []string{}
	}
	return clusterDTO{
		ID:                      c.ID.String(),
		Name:                    c.Name,
		Description:             c.Description,
		AuthType:                c.AuthType,
		APIServer:               c.APIServer,
		HasCA:                   len(c.CACert) > 0,
		CACert:                  string(c.CACert),
		AllowedProjects:         allowed,
		AllowDeclarativeTargets: c.AllowDeclarativeTargets,
		CreatedBy:               c.CreatedBy,
		CreatedAt:               formatClusterTime(c.CreatedAt),
		UpdatedAt:               formatClusterTime(c.UpdatedAt),
	}
}

// formatClusterTime renders a nullable timestamp. Cluster carries
// *time.Time (set by the DB DEFAULT NOW()); a nil here means the row
// was built in-memory and never persisted — emit "" rather than panic.
func formatClusterTime(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.Format("2006-01-02T15:04:05Z07:00")
}

// mapClusterWriteErr translates store-side write errors into HTTP
// status + message. Validation failures (bad name, missing credential,
// unknown auth_type) are the caller's fault → 422; a unique-name
// collision → 409. Returns handled=false for anything else so the
// caller falls through to a 500.
func mapClusterWriteErr(err error) (status int, msg string, handled bool) {
	s := err.Error()
	switch {
	case strings.Contains(s, "clusters_name_key"):
		return http.StatusConflict, "cluster name already exists", true
	case strings.Contains(s, "name "), strings.Contains(s, "needs a credential"),
		strings.Contains(s, "takes no credential"), strings.Contains(s, "unknown auth_type"),
		strings.Contains(s, "requires a ca_cert"), strings.Contains(s, "preserve sentinel"),
		strings.Contains(s, "api_server"), strings.Contains(s, "exec-based"),
		strings.Contains(s, "no cipher configured"):
		// store: prefixes leak the package name; strip it for the wire.
		return http.StatusUnprocessableEntity, strings.TrimPrefix(s, "store: "), true
	}
	return 0, "", false
}

// sortedStrings returns a lexically-sorted copy of in so two writes
// with the same set emit a stable audit serialization (easier to grep
// through history). nil → [] so the metadata always carries an array.
func sortedStrings(in []string) []string {
	if len(in) == 0 {
		return []string{}
	}
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}
