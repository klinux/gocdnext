package admin

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/api/authapi"
	"github.com/gocdnext/gocdnext/server/internal/audit"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// platformSettingArtifactStorage mirrors the const in main.go —
// duplicated here so the handler doesn't depend on the cmd package.
// Keep in sync.
const platformSettingArtifactStorage = "artifacts.storage"

// storageDTO is the wire shape for GET. Credentials never round-
// trip — only `credential_keys` (sorted names) so the UI can show
// "•••• stored" badges. The non-secret half of the JSONB value is
// returned verbatim.
type storageDTO struct {
	Backend        string         `json:"backend"`
	Value          map[string]any `json:"value"`
	CredentialKeys []string       `json:"credential_keys"`
	UpdatedAt      string         `json:"updated_at"`
	UpdatedBy      string         `json:"updated_by,omitempty"`
	// Source is "db" when the row exists, "env" when the boot
	// path will fall back to environment configuration.
	Source string `json:"source"`
}

// storageWriteRequest is the shape PUT accepts. Credentials are
// PLAINTEXT on the wire — sealed by the store before persisting.
// An empty/missing credentials map for a backend that needs them
// (s3, gcs) is allowed: the boot path then falls through to env
// (IRSA / Workload Identity / mounted SA file).
type storageWriteRequest struct {
	Backend     string            `json:"backend"`
	Value       map[string]any    `json:"value"`
	Credentials map[string]string `json:"credentials"`
}

// Storage handles GET /api/v1/admin/storage. Returns the active
// override (DB row), or an env-derived snapshot when no row exists
// so the UI can pre-populate the form with what the server is
// actually using.
func (h *Handler) Storage(w http.ResponseWriter, r *http.Request) {
	if !methodGET(w, r) {
		return
	}
	row, err := h.store.GetPlatformSetting(r.Context(), platformSettingArtifactStorage)
	if errors.Is(err, store.ErrPlatformSettingNotFound) {
		// No DB override — surface the env snapshot the boot path
		// is using so the form isn't blank on first open.
		writeJSON(w, h.envStorageSnapshot())
		return
	}
	if err != nil {
		h.log.Error("admin storage: get", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	backend, _ := row.Value["backend"].(string)
	out := storageDTO{
		Backend:        backend,
		Value:          stripBackendKey(row.Value),
		CredentialKeys: credKeysFromBlob(row.CredentialsEnc),
		UpdatedAt:      row.UpdatedAt.Format("2006-01-02T15:04:05Z07:00"),
		Source:         "db",
	}
	if row.UpdatedBy != nil {
		out.UpdatedBy = row.UpdatedBy.String()
	}
	writeJSON(w, out)
}

// SetStorage handles PUT /api/v1/admin/storage. Validates backend +
// required fields per backend, encrypts the credentials, and
// upserts. Server restart required for the change to take effect
// (documented in the response header).
func (h *Handler) SetStorage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		w.Header().Set("Allow", "PUT")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req storageWriteRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := validateStorageWrite(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if len(req.Credentials) > 0 && h.cipher == nil {
		http.Error(w, "credentials cannot be saved: server cipher not configured (set GOCDNEXT_SECRET_KEY)", http.StatusServiceUnavailable)
		return
	}
	// Stash backend inside the value blob so a future reader has a
	// single place to check the kind.
	if req.Value == nil {
		req.Value = map[string]any{}
	}
	req.Value["backend"] = req.Backend

	// Pull actor from session for audit trail; uuid.Nil when there's
	// no session in context (e.g. server-internal call path).
	var actorID = uuid.Nil
	if u, ok := authapi.UserFromContext(r.Context()); ok {
		actorID = u.ID
	}

	row, err := h.store.UpsertPlatformSetting(r.Context(), h.cipher, store.PlatformSettingInput{
		Key:         platformSettingArtifactStorage,
		Value:       req.Value,
		Credentials: req.Credentials,
	}, actorID)
	if err != nil {
		h.log.Error("admin storage: upsert", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	audit.Emit(r.Context(), h.log, h.store,
		store.AuditActionPlatformSettingSet, "platform_setting", row.Key,
		map[string]any{
			"backend":         req.Backend,
			"credential_keys": sortedMapKeys(req.Credentials),
		})

	// Heads-up header so the UI can show a "restart server pod to
	// apply" banner. Hot-reload is on the roadmap; today only the
	// boot path consumes the override.
	w.Header().Set("X-Gocdnext-Restart-Required", "true")
	writeJSON(w, storageDTO{
		Backend:        req.Backend,
		Value:          stripBackendKey(req.Value),
		CredentialKeys: sortedMapKeys(req.Credentials),
		UpdatedAt:      row.UpdatedAt.Format("2006-01-02T15:04:05Z07:00"),
		Source:         "db",
	})
}

// DeleteStorage handles DELETE /api/v1/admin/storage. Drops the
// DB override; boot path then falls back to env config. Same
// restart-required semantics as the PUT.
func (h *Handler) DeleteStorage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		w.Header().Set("Allow", "DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := h.store.DeletePlatformSetting(r.Context(), platformSettingArtifactStorage); err != nil {
		h.log.Error("admin storage: delete", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	audit.Emit(r.Context(), h.log, h.store,
		store.AuditActionPlatformSettingDel, "platform_setting",
		platformSettingArtifactStorage, map[string]any{})
	w.Header().Set("X-Gocdnext-Restart-Required", "true")
	w.WriteHeader(http.StatusNoContent)
}

// envStorageSnapshot returns what boot would use today when no DB
// row exists. Reads the env-derived snapshot the operator wired
// via SetArtifactsEnv at startup.
//
// Doesn't surface secret values; only credential_keys flagged as
// "configured via env" so the operator knows the env path is wired
// without exposing the key itself.
func (h *Handler) envStorageSnapshot() storageDTO {
	a := h.artifacts
	value := map[string]any{}
	credKeys := []string{}
	switch a.Backend {
	case "s3":
		value["bucket"] = a.S3Bucket
		value["region"] = a.S3Region
		value["endpoint"] = a.S3Endpoint
		value["use_path_style"] = a.S3UsePathStyle
		value["ensure_bucket"] = a.S3EnsureBucket
		if a.S3AccessKeyConfigured {
			credKeys = append(credKeys, "access_key")
		}
		if a.S3SecretKeyConfigured {
			credKeys = append(credKeys, "secret_key")
		}
	case "gcs":
		value["bucket"] = a.GCSBucket
		value["project_id"] = a.GCSProjectID
		value["ensure_bucket"] = a.GCSEnsureBucket
		if a.GCSCredsPresent {
			credKeys = append(credKeys, "service_account_json")
		}
	}
	sort.Strings(credKeys)
	backend := a.Backend
	if backend == "" {
		backend = "filesystem"
	}
	return storageDTO{
		Backend:        backend,
		Value:          value,
		CredentialKeys: credKeys,
		Source:         "env",
	}
}

// validateStorageWrite enforces the per-backend contract upfront so
// the dispatch path doesn't have to defensively check missing
// fields. Backends that need a bucket name simply must pass one.
func validateStorageWrite(req *storageWriteRequest) error {
	switch req.Backend {
	case "filesystem":
		// No required fields; filesystem path comes from env
		// (ArtifactsFSRoot) since changing it at runtime would
		// orphan existing files.
		return nil
	case "s3":
		bucket, _ := req.Value["bucket"].(string)
		if strings.TrimSpace(bucket) == "" {
			return errors.New("s3 backend requires `value.bucket`")
		}
		return nil
	case "gcs":
		bucket, _ := req.Value["bucket"].(string)
		if strings.TrimSpace(bucket) == "" {
			return errors.New("gcs backend requires `value.bucket`")
		}
		return nil
	case "":
		return errors.New("backend is required (filesystem | s3 | gcs)")
	default:
		return fmt.Errorf("unsupported backend %q (expected: filesystem | s3 | gcs)", req.Backend)
	}
}

// stripBackendKey returns a copy of the value blob without the
// `backend` key — the DTO already carries that as a top-level
// field so duplicating it inside `value` would just be noise.
func stripBackendKey(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		if k == "backend" {
			continue
		}
		out[k] = v
	}
	return out
}

// credKeysFromBlob parses the encrypted blob's structure WITHOUT
// decrypting — we only need the key NAMES for the UI. The blob is
// `nonce || ciphertext+tag` and we don't have the cipher here, so
// instead we surface the credential names via a side-channel: the
// audit metadata that recorded them at write time. Callers that
// want fresh names re-fetch via authenticated UI flow.
//
// Pragmatic shortcut: when the blob is non-empty we return a
// generic ["configured"] sentinel, signalling "credentials present"
// without exposing names. UI shows "•••• stored" either way.
func credKeysFromBlob(blob []byte) []string {
	if len(blob) == 0 {
		return []string{}
	}
	return []string{"configured"}
}
