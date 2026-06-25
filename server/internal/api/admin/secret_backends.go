package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/api/authapi"
	"github.com/gocdnext/gocdnext/server/internal/audit"
	"github.com/gocdnext/gocdnext/server/internal/secrets"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// secretBackendDTO is the GET wire shape. Credentials never round-trip — only
// credential_keys (a "configured" sentinel) so the UI can show a "•••• stored"
// badge. source_origin distinguishes a DB override from the env baseline.
type secretBackendDTO struct {
	Source         string         `json:"source"`
	Enabled        bool           `json:"enabled"`
	Value          map[string]any `json:"value"`
	CredentialKeys []string       `json:"credential_keys"`
	SourceOrigin   string         `json:"source_origin"` // "db" | "env"
	UpdatedAt      string         `json:"updated_at,omitempty"`
}

type secretBackendsListResponse struct {
	Backends []secretBackendDTO `json:"backends"`
}

// secretBackendWriteRequest is the PUT shape. Credentials are PLAINTEXT on the
// wire (sealed by the store). PreserveCredentials keeps the stored blob on a
// metadata-only edit (the UI sets it when the credential field is left blank).
type secretBackendWriteRequest struct {
	Enabled             bool              `json:"enabled"`
	Value               map[string]any    `json:"value"`
	Credentials         map[string]string `json:"credentials"`
	PreserveCredentials bool              `json:"preserve_credentials"`
}

// secretBackendProbeResult is the Test-connection payload. No credential ever
// appears here — only a coarse status + a sanitised message.
type secretBackendProbeResult struct {
	Status  string `json:"status"` // ok | unauthorized | unreachable | error
	Message string `json:"message,omitempty"`
}

// SecretBackends handles GET /api/v1/admin/secret-backends — all three
// backends, DB override or env snapshot, never a credential value.
func (h *Handler) SecretBackends(w http.ResponseWriter, r *http.Request) {
	if !methodGET(w, r) {
		return
	}
	out := make([]secretBackendDTO, 0, 3)
	for _, src := range []string{store.SecretSourceVault, store.SecretSourceGCP, store.SecretSourceAWS} {
		dto, err := h.secretBackendDTO(r.Context(), src)
		if err != nil {
			h.log.Error("admin secret-backends: get", "source", src, "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		out = append(out, dto)
	}
	writeJSON(w, secretBackendsListResponse{Backends: out})
}

func (h *Handler) secretBackendDTO(ctx context.Context, source string) (secretBackendDTO, error) {
	row, err := h.store.GetSecretBackend(ctx, source)
	if errors.Is(err, store.ErrPlatformSettingNotFound) {
		return h.envSecretBackendDTO(source), nil
	}
	if err != nil {
		return secretBackendDTO{}, err
	}
	enabled, _ := row.Value["enabled"].(bool)
	return secretBackendDTO{
		Source:         source,
		Enabled:        enabled,
		Value:          stripKey(row.Value, "enabled"),
		CredentialKeys: credKeysFromBlob(row.CredentialsEnc),
		SourceOrigin:   "db",
		UpdatedAt:      row.UpdatedAt.Format("2006-01-02T15:04:05Z07:00"),
	}, nil
}

func (h *Handler) envSecretBackendDTO(source string) secretBackendDTO {
	var e SecretBackendEnv
	switch source {
	case store.SecretSourceVault:
		e = h.secretBackendsEnv.Vault
	case store.SecretSourceGCP:
		e = h.secretBackendsEnv.GCP
	case store.SecretSourceAWS:
		e = h.secretBackendsEnv.AWS
	}
	credKeys := []string{}
	if e.HasCreds {
		credKeys = []string{"configured"}
	}
	value := e.Value
	if value == nil {
		value = map[string]any{}
	}
	return secretBackendDTO{
		Source:         source,
		Enabled:        e.Enabled,
		Value:          value,
		CredentialKeys: credKeys,
		SourceOrigin:   "env",
	}
}

// SetSecretBackend handles PUT /api/v1/admin/secret-backends/{source}.
// Validates per source, seals credentials, and persists — the write fires a
// commit-gated NOTIFY so every replica hot-reloads (no restart).
func (h *Handler) SetSecretBackend(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		w.Header().Set("Allow", "PUT")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	source := chi.URLParam(r, "source")
	if !isExternalSource(source) {
		http.Error(w, "unknown backend source (want vault | gcp | aws)", http.StatusBadRequest)
		return
	}
	var req secretBackendWriteRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := validateSecretBackendWrite(source, req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	value := req.Value
	if value == nil {
		value = map[string]any{}
	}
	value["enabled"] = req.Enabled

	creds := req.Credentials
	if req.PreserveCredentials {
		creds = nil
	}
	preserve := req.PreserveCredentials
	if req.Enabled {
		// Auth-aware credential resolution: ensures the persisted credential
		// matches the SELECTED auth (not just "some blob exists"), and clears a
		// now-unused credential when switching to kubernetes auth.
		var verr error
		creds, preserve, verr = h.resolveBackendCredential(r.Context(), source, value, creds, preserve)
		if verr != nil {
			http.Error(w, verr.Error(), http.StatusBadRequest)
			return
		}
	}
	if !preserve && len(creds) > 0 && h.cipher == nil {
		http.Error(w, "credentials cannot be saved: server cipher not configured (set GOCDNEXT_SECRET_KEY)", http.StatusServiceUnavailable)
		return
	}

	actorID := uuid.Nil
	if u, ok := authapi.UserFromContext(r.Context()); ok {
		actorID = u.ID
	}
	if err := h.store.SetSecretBackend(r.Context(), h.cipher, store.SecretBackendInput{
		Source:        source,
		Value:         value,
		Credentials:   creds,
		PreserveCreds: preserve,
		UpdatedBy:     actorID,
	}); err != nil {
		h.log.Error("admin secret-backends: set", "source", source, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if h.secretBackendInvalidator != nil {
		h.secretBackendInvalidator(source) // converge this replica now; NOTIFY handles the rest
	}
	meta := map[string]any{"source": source, "enabled": req.Enabled, "credential_keys": sortedMapKeys(req.Credentials)}
	if source == store.SecretSourceVault {
		// TLS posture is security-relevant — record it in the audit trail.
		// Booleans only; the CA PEM itself is never written to the audit.
		skip, _ := value["insecure_skip_verify"].(bool)
		ca, _ := value["ca_cert"].(string)
		meta["insecure_skip_verify"] = skip
		meta["has_ca_cert"] = strings.TrimSpace(ca) != ""
	}
	audit.Emit(r.Context(), h.log, h.store,
		store.AuditActionPlatformSettingSet, "secret_backend", source, meta)

	dto, err := h.secretBackendDTO(r.Context(), source)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, dto)
}

// DeleteSecretBackend handles DELETE /api/v1/admin/secret-backends/{source} —
// drops the DB override; the registry falls back to the env baseline.
func (h *Handler) DeleteSecretBackend(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		w.Header().Set("Allow", "DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	source := chi.URLParam(r, "source")
	if !isExternalSource(source) {
		http.Error(w, "unknown backend source (want vault | gcp | aws)", http.StatusBadRequest)
		return
	}
	if err := h.store.DeleteSecretBackend(r.Context(), source); err != nil {
		h.log.Error("admin secret-backends: delete", "source", source, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if h.secretBackendInvalidator != nil {
		h.secretBackendInvalidator(source)
	}
	audit.Emit(r.Context(), h.log, h.store,
		store.AuditActionPlatformSettingDel, "secret_backend", source,
		map[string]any{"source": source})
	w.WriteHeader(http.StatusNoContent)
}

// TestSecretBackend handles POST /api/v1/admin/secret-backends/{source}/test.
// Builds a transient client from the submitted config (or stored credentials
// when preserving) and health-checks it. The credential never appears in the
// result. Always 200 — the probe outcome is the payload.
func (h *Handler) TestSecretBackend(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	source := chi.URLParam(r, "source")
	if !isExternalSource(source) {
		http.Error(w, "unknown backend source (want vault | gcp | aws)", http.StatusBadRequest)
		return
	}
	var req secretBackendWriteRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}
	creds := req.Credentials
	if req.PreserveCredentials {
		// Probe with the stored credential (the form left it blank).
		row, err := h.store.GetSecretBackend(r.Context(), source)
		if err == nil {
			if c, derr := store.DecryptPlatformCredentials(h.cipher, row.CredentialsEnc); derr == nil {
				creds = c
			}
		}
	}
	value := req.Value
	if value == nil {
		value = map[string]any{}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	err := secrets.TestSecretBackend(ctx, source, value, creds)
	writeJSON(w, classifyProbe(err))
}

// resolveBackendCredential applies auth-aware credential rules for an ENABLED
// backend and returns the effective (creds, preserve) to persist, or an error.
//   - gcp/aws: ambient ADC/IRSA — no stored credential (drop any).
//   - vault kubernetes: SA-JWT auth, no credential — DROP any stored blob so a
//     switch away from approle/token doesn't leave a stale secret behind.
//   - vault approle/token: the key the SELECTED auth needs (secret_id|token)
//     must be present — either freshly provided, or in the stored blob being
//     preserved. Preserving a blob that lacks the key for the CURRENT auth
//     (e.g. approle's secret_id after switching to token) is rejected; without
//     this, the save would 200 then break at dispatch with "needs a token".
func (h *Handler) resolveBackendCredential(ctx context.Context, source string, value map[string]any, creds map[string]string, preserve bool) (map[string]string, bool, error) {
	if source != store.SecretSourceVault {
		return nil, false, nil
	}
	auth, _ := value["auth"].(string)
	if auth == "" {
		auth = "approle"
	}
	if auth == "kubernetes" {
		return nil, false, nil // clear any unused credential
	}
	key := "secret_id"
	if auth == "token" {
		key = "token"
	}
	if !preserve && creds[key] != "" {
		return creds, false, nil // freshly provided for this auth
	}
	if preserve && h.storedCredentialHasKey(ctx, source, key) {
		return nil, true, nil // preserve a blob that carries the right key
	}
	return nil, false, fmt.Errorf("vault %s auth requires %s — re-enter it to save", auth, key)
}

// storedCredentialHasKey reports whether the persisted credential blob for a
// source contains a non-empty value at key (decrypted in-process; never logged).
func (h *Handler) storedCredentialHasKey(ctx context.Context, source, key string) bool {
	row, err := h.store.GetSecretBackend(ctx, source)
	if err != nil || len(row.CredentialsEnc) == 0 {
		return false
	}
	stored, derr := store.DecryptPlatformCredentials(h.cipher, row.CredentialsEnc)
	return derr == nil && stored[key] != ""
}

// validateSecretBackendWrite enforces the per-source non-secret fields up front
// (credential completeness is validated at Test-connection / dispatch since a
// preserved credential isn't on the wire). Disabled backends skip checks.
func validateSecretBackendWrite(source string, req secretBackendWriteRequest) error {
	if !req.Enabled {
		return nil
	}
	get := func(k string) string {
		s, _ := req.Value[k].(string)
		return strings.TrimSpace(s)
	}
	switch source {
	case store.SecretSourceVault:
		if get("addr") == "" {
			return errors.New("vault backend requires value.addr")
		}
		switch get("auth") {
		case "approle", "":
			if get("role_id") == "" {
				return errors.New("vault approle auth requires value.role_id")
			}
		case "kubernetes":
			if get("role") == "" {
				return errors.New("vault kubernetes auth requires value.role")
			}
		case "token":
			// token is a credential — checked at Test connection / dispatch.
		default:
			return fmt.Errorf("unknown vault auth %q (want approle | kubernetes | token)", get("auth"))
		}
	case store.SecretSourceGCP:
		if get("project") == "" {
			return errors.New("gcp backend requires value.project")
		}
	case store.SecretSourceAWS:
		if get("region") == "" {
			return errors.New("aws backend requires value.region")
		}
	}
	return nil
}

// classifyProbe maps a health-check error to a coarse status for the UI. The
// message comes from our HealthCheck wrappers (backend addr at worst — never a
// credential).
func classifyProbe(err error) secretBackendProbeResult {
	if err == nil {
		return secretBackendProbeResult{Status: "ok"}
	}
	msg := err.Error()
	low := strings.ToLower(msg)
	switch {
	case strings.Contains(low, "403"), strings.Contains(low, "401"),
		strings.Contains(low, "permission denied"), strings.Contains(low, "unauthorized"),
		strings.Contains(low, "access denied"), strings.Contains(low, "invalid"):
		return secretBackendProbeResult{Status: "unauthorized", Message: msg}
	case strings.Contains(low, "connection refused"), strings.Contains(low, "no such host"),
		strings.Contains(low, "timeout"), strings.Contains(low, "deadline exceeded"),
		strings.Contains(low, "dial tcp"), strings.Contains(low, "i/o timeout"):
		return secretBackendProbeResult{Status: "unreachable", Message: msg}
	default:
		return secretBackendProbeResult{Status: "error", Message: msg}
	}
}

func isExternalSource(s string) bool {
	switch s {
	case store.SecretSourceVault, store.SecretSourceGCP, store.SecretSourceAWS:
		return true
	}
	return false
}

// stripKey returns a copy of m without one key (top-level fields the DTO
// carries separately would otherwise be duplicated inside `value`).
func stripKey(m map[string]any, key string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		if k == key {
			continue
		}
		out[k] = v
	}
	return out
}
