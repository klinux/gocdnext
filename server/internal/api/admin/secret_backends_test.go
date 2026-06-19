package admin_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	adminapi "github.com/gocdnext/gocdnext/server/internal/api/admin"
	"github.com/gocdnext/gocdnext/server/internal/crypto"
	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/retention"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

func newSecretBackendsHandler(t *testing.T, withCipher bool) (*store.Store, http.Handler) {
	t.Helper()
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	h := adminapi.NewHandler(s, retention.New(s, nil, quietLogger()), nil, adminapi.WiringState{}, quietLogger())
	if withCipher {
		key := make([]byte, 32)
		for i := range key {
			key[i] = byte(i)
		}
		c, err := crypto.NewCipher(key)
		if err != nil {
			t.Fatalf("cipher: %v", err)
		}
		h.SetCipher(c)
	}
	h.SetSecretBackendsEnv(adminapi.SecretBackendsEnvSnapshot{
		Vault: adminapi.SecretBackendEnv{Enabled: true, Value: map[string]any{"addr": "http://env-vault", "auth": "token"}, HasCreds: true},
	})
	return s, mount(h)
}

func TestSecretBackends_GetEnvFallback(t *testing.T) {
	_, srv := newSecretBackendsHandler(t, true)
	rr := request(srv, http.MethodGet, "/api/v1/admin/secret-backends", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Backends []map[string]any `json:"backends"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Backends) != 3 {
		t.Fatalf("want 3 backends, got %d", len(resp.Backends))
	}
	var vault map[string]any
	for _, b := range resp.Backends {
		if b["source"] == "vault" {
			vault = b
		}
	}
	if vault == nil || vault["source_origin"] != "env" || vault["enabled"] != true {
		t.Fatalf("vault env snapshot = %+v", vault)
	}
	keys, _ := vault["credential_keys"].([]any)
	if len(keys) != 1 || keys[0] != "configured" {
		t.Fatalf("vault credential_keys = %v, want [configured]", keys)
	}
}

func TestSecretBackends_PutValidates(t *testing.T) {
	_, srv := newSecretBackendsHandler(t, true)
	// vault enabled without addr → 400
	rr := request(srv, http.MethodPut, "/api/v1/admin/secret-backends/vault",
		bytes.NewBufferString(`{"enabled":true,"value":{}}`))
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "addr") {
		t.Fatalf("vault no-addr = %d %s", rr.Code, rr.Body.String())
	}
	// unknown source → 400
	rr = request(srv, http.MethodPut, "/api/v1/admin/secret-backends/azure",
		bytes.NewBufferString(`{"enabled":true}`))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("unknown source = %d", rr.Code)
	}
	// gcp enabled without project → 400
	rr = request(srv, http.MethodPut, "/api/v1/admin/secret-backends/gcp",
		bytes.NewBufferString(`{"enabled":true,"value":{}}`))
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "project") {
		t.Fatalf("gcp no-project = %d %s", rr.Code, rr.Body.String())
	}
}

func TestSecretBackends_PutCredsRequiresCipher(t *testing.T) {
	_, srv := newSecretBackendsHandler(t, false) // no cipher
	rr := request(srv, http.MethodPut, "/api/v1/admin/secret-backends/vault",
		bytes.NewBufferString(`{"enabled":true,"value":{"addr":"http://v","auth":"approle","role_id":"r"},"credentials":{"secret_id":"s"}}`))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503, body=%s", rr.Code, rr.Body.String())
	}
}

func TestSecretBackends_PutThenGetSwitchesToDB(t *testing.T) {
	_, srv := newSecretBackendsHandler(t, true)
	put := `{"enabled":true,"value":{"addr":"http://db-vault","auth":"approle","role_id":"rid"},"credentials":{"secret_id":"shh"}}`
	rr := request(srv, http.MethodPut, "/api/v1/admin/secret-backends/vault", bytes.NewBufferString(put))
	if rr.Code != http.StatusOK {
		t.Fatalf("put = %d %s", rr.Code, rr.Body.String())
	}
	// No restart-required header (hot-reload).
	if rr.Header().Get("X-Gocdnext-Restart-Required") != "" {
		t.Fatalf("hot-reload backend must not set restart-required header")
	}
	var dto map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &dto)
	if dto["source_origin"] != "db" || dto["enabled"] != true {
		t.Fatalf("dto = %+v", dto)
	}
	value, _ := dto["value"].(map[string]any)
	if value["addr"] != "http://db-vault" {
		t.Fatalf("value = %+v", value)
	}
	// The secret_id must NOT round-trip; enabled must not be duplicated in value.
	if _, leaked := value["secret_id"]; leaked {
		t.Fatalf("secret_id leaked into value: %+v", value)
	}
	if _, dup := value["enabled"]; dup {
		t.Fatalf("enabled duplicated in value: %+v", value)
	}
	if !strings.Contains(rr.Body.String(), "configured") {
		t.Fatalf("expected credential_keys=[configured]: %s", rr.Body.String())
	}

	// GET reflects db origin.
	rr = request(srv, http.MethodGet, "/api/v1/admin/secret-backends", nil)
	var resp struct {
		Backends []map[string]any `json:"backends"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	for _, b := range resp.Backends {
		if b["source"] == "vault" && b["source_origin"] != "db" {
			t.Fatalf("vault origin after PUT = %v, want db", b["source_origin"])
		}
	}
}

func TestSecretBackends_EnvOriginApproleRequiresCredential(t *testing.T) {
	_, srv := newSecretBackendsHandler(t, true)
	// Vault is enabled from ENV (with a secret_id). Saving a DB override
	// without re-typing the credential must NOT create an override missing
	// the secret_id (env credential can't be copied) — 400, citing secret_id.
	rr := request(srv, http.MethodPut, "/api/v1/admin/secret-backends/vault",
		bytes.NewBufferString(`{"enabled":true,"value":{"addr":"http://v","auth":"approle","role_id":"rid"}}`))
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "secret_id") {
		t.Fatalf("env-origin approle save without cred = %d %s (want 400 citing secret_id)", rr.Code, rr.Body.String())
	}
}

func TestSecretBackends_PreserveWithoutStoredRejected(t *testing.T) {
	_, srv := newSecretBackendsHandler(t, true)
	// preserve with no stored DB credential for the selected auth → 400.
	rr := request(srv, http.MethodPut, "/api/v1/admin/secret-backends/vault",
		bytes.NewBufferString(`{"enabled":true,"value":{"addr":"http://v","auth":"approle","role_id":"rid"},"preserve_credentials":true}`))
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "secret_id") {
		t.Fatalf("preserve-without-stored = %d %s (want 400 citing secret_id)", rr.Code, rr.Body.String())
	}
}

func TestSecretBackends_PreserveAfterStoreSucceeds(t *testing.T) {
	_, srv := newSecretBackendsHandler(t, true)
	// Seed a DB override WITH a credential.
	if rr := request(srv, http.MethodPut, "/api/v1/admin/secret-backends/vault",
		bytes.NewBufferString(`{"enabled":true,"value":{"addr":"http://v1","auth":"approle","role_id":"rid"},"credentials":{"secret_id":"s"}}`)); rr.Code != http.StatusOK {
		t.Fatalf("seed = %d %s", rr.Code, rr.Body.String())
	}
	// Now a metadata-only edit (new addr) preserving the stored credential → OK.
	rr := request(srv, http.MethodPut, "/api/v1/admin/secret-backends/vault",
		bytes.NewBufferString(`{"enabled":true,"value":{"addr":"http://v2","auth":"approle","role_id":"rid"},"preserve_credentials":true}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("preserve-after-store = %d %s (want 200)", rr.Code, rr.Body.String())
	}
}

func seedVault(t *testing.T, srv http.Handler, body string) {
	t.Helper()
	if rr := request(srv, http.MethodPut, "/api/v1/admin/secret-backends/vault", bytes.NewBufferString(body)); rr.Code != http.StatusOK {
		t.Fatalf("seed vault = %d %s", rr.Code, rr.Body.String())
	}
}

func TestSecretBackends_AuthSwitchPreserveMismatchRejected(t *testing.T) {
	// approle stored (secret_id) → switch to token, preserve, no token → 400.
	_, srv := newSecretBackendsHandler(t, true)
	seedVault(t, srv, `{"enabled":true,"value":{"addr":"http://v","auth":"approle","role_id":"r"},"credentials":{"secret_id":"s"}}`)
	rr := request(srv, http.MethodPut, "/api/v1/admin/secret-backends/vault",
		bytes.NewBufferString(`{"enabled":true,"value":{"addr":"http://v","auth":"token"},"preserve_credentials":true}`))
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "token") {
		t.Fatalf("approle→token preserve = %d %s (want 400 citing token)", rr.Code, rr.Body.String())
	}

	// token stored → switch to approle, preserve, no secret_id → 400.
	_, srv2 := newSecretBackendsHandler(t, true)
	seedVault(t, srv2, `{"enabled":true,"value":{"addr":"http://v","auth":"token"},"credentials":{"token":"t"}}`)
	rr = request(srv2, http.MethodPut, "/api/v1/admin/secret-backends/vault",
		bytes.NewBufferString(`{"enabled":true,"value":{"addr":"http://v","auth":"approle","role_id":"r"},"preserve_credentials":true}`))
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "secret_id") {
		t.Fatalf("token→approle preserve = %d %s (want 400 citing secret_id)", rr.Code, rr.Body.String())
	}
}

func TestSecretBackends_SwitchToKubernetesClearsCredential(t *testing.T) {
	_, srv := newSecretBackendsHandler(t, true)
	seedVault(t, srv, `{"enabled":true,"value":{"addr":"http://v","auth":"approle","role_id":"r"},"credentials":{"secret_id":"s"}}`)
	// Switch to kubernetes (no credential), preserve=true as the UI would send.
	rr := request(srv, http.MethodPut, "/api/v1/admin/secret-backends/vault",
		bytes.NewBufferString(`{"enabled":true,"value":{"addr":"http://v","auth":"kubernetes","role":"k8s-role"},"preserve_credentials":true}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("switch to kubernetes = %d %s", rr.Code, rr.Body.String())
	}
	// The now-unused secret_id blob must be cleared — no "•••• stored".
	var dto map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &dto)
	keys, _ := dto["credential_keys"].([]any)
	if len(keys) != 0 {
		t.Fatalf("kubernetes switch left a stale credential: %v", keys)
	}
}

func TestSecretBackends_DeleteDropsToEnv(t *testing.T) {
	_, srv := newSecretBackendsHandler(t, true)
	// Override gcp via DB.
	if rr := request(srv, http.MethodPut, "/api/v1/admin/secret-backends/gcp",
		bytes.NewBufferString(`{"enabled":true,"value":{"project":"p"}}`)); rr.Code != http.StatusOK {
		t.Fatalf("seed put: %d %s", rr.Code, rr.Body.String())
	}
	if rr := request(srv, http.MethodDelete, "/api/v1/admin/secret-backends/gcp", nil); rr.Code != http.StatusNoContent {
		t.Fatalf("delete = %d %s", rr.Code, rr.Body.String())
	}
	// gcp now falls back to env (disabled, since env has no gcp).
	rr := request(srv, http.MethodGet, "/api/v1/admin/secret-backends", nil)
	var resp struct {
		Backends []map[string]any `json:"backends"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	for _, b := range resp.Backends {
		if b["source"] == "gcp" && b["source_origin"] != "env" {
			t.Fatalf("gcp origin after delete = %v, want env", b["source_origin"])
		}
	}
}

func TestSecretBackends_TestConnection_Unreachable(t *testing.T) {
	_, srv := newSecretBackendsHandler(t, true)
	// token auth builds without dialing; HealthCheck then fails to reach the
	// closed port → a non-ok probe status (never panics, never 500s).
	rr := request(srv, http.MethodPost, "/api/v1/admin/secret-backends/vault/test",
		bytes.NewBufferString(`{"value":{"addr":"http://127.0.0.1:1","auth":"token"},"credentials":{"token":"t"}}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("probe status = %d, body=%s", rr.Code, rr.Body.String())
	}
	var res struct {
		Status string `json:"status"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &res)
	if res.Status == "" || res.Status == "ok" {
		t.Fatalf("probe to closed port should not be ok, got %q", res.Status)
	}
}
