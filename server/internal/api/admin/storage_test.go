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

func newStorageHandler(t *testing.T, withCipher bool) (*store.Store, http.Handler) {
	t.Helper()
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	sweeper := retention.New(s, nil, quietLogger())
	h := adminapi.NewHandler(s, sweeper, nil, adminapi.WiringState{}, quietLogger())
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
	h.SetArtifactsEnv(adminapi.ArtifactsEnvSnapshot{
		Backend:  "s3",
		S3Bucket: "env-bucket",
		S3Region: "us-east-1",
	})
	return s, mount(h)
}

func TestStorage_GetEnvFallback(t *testing.T) {
	_, srv := newStorageHandler(t, true)

	rr := request(srv, http.MethodGet, "/api/v1/admin/storage", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["source"] != "env" {
		t.Fatalf("source = %v, want env", got["source"])
	}
	if got["backend"] != "s3" {
		t.Fatalf("backend = %v, want s3", got["backend"])
	}
	value, _ := got["value"].(map[string]any)
	if value["bucket"] != "env-bucket" {
		t.Fatalf("env bucket = %v", value["bucket"])
	}
}

func TestStorage_PutValidatesBucket(t *testing.T) {
	_, srv := newStorageHandler(t, true)

	// Missing bucket on s3 → 400.
	rr := request(srv, http.MethodPut, "/api/v1/admin/storage",
		bytes.NewBufferString(`{"backend":"s3","value":{}}`))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "bucket") {
		t.Fatalf("error should mention bucket: %s", rr.Body.String())
	}

	// Unsupported backend → 400.
	rr = request(srv, http.MethodPut, "/api/v1/admin/storage",
		bytes.NewBufferString(`{"backend":"floppy"}`))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("unsupported status = %d, want 400", rr.Code)
	}
}

func TestStorage_PutWithCredentialsRequiresCipher(t *testing.T) {
	_, srv := newStorageHandler(t, false)

	body := bytes.NewBufferString(`{
		"backend":"s3",
		"value":{"bucket":"override-bucket","region":"eu-west-1"},
		"credentials":{"access_key":"AKIA","secret_key":"shh"}
	}`)
	rr := request(srv, http.MethodPut, "/api/v1/admin/storage", body)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503, body=%s", rr.Code, rr.Body.String())
	}
}

func TestStorage_PutThenGetSwitchesToDB(t *testing.T) {
	_, srv := newStorageHandler(t, true)

	body := bytes.NewBufferString(`{
		"backend":"s3",
		"value":{"bucket":"override-bucket","region":"eu-west-1","use_path_style":true},
		"credentials":{"access_key":"AKIA","secret_key":"shh"}
	}`)
	rr := request(srv, http.MethodPut, "/api/v1/admin/storage", body)
	if rr.Code != http.StatusOK {
		t.Fatalf("put status = %d, body=%s", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("X-Gocdnext-Restart-Required") != "true" {
		t.Fatalf("restart-required header = %q", rr.Header().Get("X-Gocdnext-Restart-Required"))
	}
	var saved struct {
		Backend        string         `json:"backend"`
		Value          map[string]any `json:"value"`
		CredentialKeys []string       `json:"credential_keys"`
		Source         string         `json:"source"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &saved); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if saved.Source != "db" || saved.Backend != "s3" {
		t.Fatalf("saved = %+v", saved)
	}
	if saved.Value["bucket"] != "override-bucket" {
		t.Fatalf("value = %+v", saved.Value)
	}
	// Backend mustn't appear inside `value` — top-level field carries it.
	if _, ok := saved.Value["backend"]; ok {
		t.Fatalf("backend leaked into value: %+v", saved.Value)
	}
	wantKeys := map[string]bool{"access_key": true, "secret_key": true}
	for _, k := range saved.CredentialKeys {
		delete(wantKeys, k)
	}
	if len(wantKeys) != 0 {
		t.Fatalf("credential_keys = %v, missing %v", saved.CredentialKeys, wantKeys)
	}

	// Now GET — should return source=db with no env leakage.
	rr = request(srv, http.MethodGet, "/api/v1/admin/storage", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("get status = %d", rr.Code)
	}
	var fetched map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &fetched); err != nil {
		t.Fatalf("decode get: %v", err)
	}
	if fetched["source"] != "db" {
		t.Fatalf("get source = %v, want db", fetched["source"])
	}
	value, _ := fetched["value"].(map[string]any)
	if value["bucket"] != "override-bucket" {
		t.Fatalf("get bucket = %v", value["bucket"])
	}
	// credKeysFromBlob returns the ["configured"] sentinel — names live
	// in audit metadata, not the row, so the GET response is the
	// shorthand: "creds present, ask audit for names".
	keys, _ := fetched["credential_keys"].([]any)
	if len(keys) != 1 || keys[0] != "configured" {
		t.Fatalf("credential_keys on read = %v", keys)
	}
}

func TestStorage_DeleteDropsOverride(t *testing.T) {
	_, srv := newStorageHandler(t, true)

	// Seed an override.
	put := bytes.NewBufferString(`{
		"backend":"s3",
		"value":{"bucket":"override-bucket"}
	}`)
	if rr := request(srv, http.MethodPut, "/api/v1/admin/storage", put); rr.Code != http.StatusOK {
		t.Fatalf("seed put: %d %s", rr.Code, rr.Body.String())
	}

	// Delete.
	rr := request(srv, http.MethodDelete, "/api/v1/admin/storage", nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, body=%s", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("X-Gocdnext-Restart-Required") != "true" {
		t.Fatalf("restart-required missing on delete")
	}

	// GET should now fall back to env snapshot.
	rr = request(srv, http.MethodGet, "/api/v1/admin/storage", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("get status = %d", rr.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["source"] != "env" {
		t.Fatalf("source after delete = %v, want env", got["source"])
	}
}

func TestStorage_FilesystemAcceptsEmptyValue(t *testing.T) {
	_, srv := newStorageHandler(t, true)

	rr := request(srv, http.MethodPut, "/api/v1/admin/storage",
		bytes.NewBufferString(`{"backend":"filesystem"}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	var saved map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &saved)
	if saved["backend"] != "filesystem" {
		t.Fatalf("backend = %v", saved["backend"])
	}
}

func TestStorage_MethodNotAllowed(t *testing.T) {
	_, srv := newStorageHandler(t, true)
	rr := request(srv, http.MethodPost, "/api/v1/admin/storage",
		bytes.NewBufferString(`{}`))
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d, want 405", rr.Code)
	}
}
