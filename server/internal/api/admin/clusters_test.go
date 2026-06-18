package admin_test

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	adminapi "github.com/gocdnext/gocdnext/server/internal/api/admin"
	"github.com/gocdnext/gocdnext/server/internal/crypto"
	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/retention"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

// RBAC NOTE: the /api/v1/admin/clusters routes are gated by
// authMiddleware.RequireMinRole(store.RoleAdmin) in main.go, applied at
// the chi.Router group level — NOT inside the handler. The test harness
// (mount) wires the handlers without that middleware, so a 403-for-
// non-admin assertion isn't reachable here. RBAC is exercised by the
// router-level middleware tests; these tests cover handler behaviour.

func newClusterHandler(t *testing.T) (*store.Store, *pgxpool.Pool, http.Handler) {
	t.Helper()
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	sweeper := retention.New(s, nil, quietLogger())
	h := adminapi.NewHandler(s, sweeper, nil, adminapi.WiringState{}, quietLogger())
	// Deterministic cipher so credential-bearing tests round-trip
	// without flakiness from random key material.
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	c, err := crypto.NewCipher(key)
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	h.SetCipher(c)
	s.SetAuthCipher(c) // ProbeCluster decrypts the credential via the store cipher
	return s, pool, mount(h)
}

func TestClusters_CreateListUpdateDelete(t *testing.T) {
	s, _, srv := newClusterHandler(t)
	ctx := context.Background()

	// Create (token auth, with a CA PEM + a bearer credential).
	body := bytes.NewBufferString(`{
        "name": "prod-gke",
        "description": "production cluster",
        "auth_type": "token",
        "api_server": "https://k8s.example.com:6443",
        "ca_cert": "-----BEGIN CERTIFICATE-----\nMIIBkTCB+w==\n-----END CERTIFICATE-----",
        "credential": "super-secret-bearer-token",
        "allowed_projects": ["proj-b", "proj-a"]
    }`)
	rr := request(srv, http.MethodPost, "/api/v1/admin/clusters", body)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body=%s", rr.Code, rr.Body.String())
	}
	// Credential never crosses the wire — not even on the create echo.
	if strings.Contains(rr.Body.String(), "super-secret-bearer-token") {
		t.Fatalf("credential leaked in create response: %s", rr.Body.String())
	}
	var created struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		AuthType  string `json:"auth_type"`
		APIServer string `json:"api_server"`
		HasCA     bool   `json:"has_ca"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created: %v", err)
	}
	if created.Name != "prod-gke" || created.AuthType != "token" || !created.HasCA {
		t.Fatalf("created = %+v", created)
	}

	// Audit row landed for the create — metadata carries name/auth_type/
	// allowed_projects but NEVER the credential.
	page, err := s.ListAuditEvents(ctx, store.ListAuditEventsFilter{
		Action: store.AuditActionClusterCreate,
	})
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	if !auditHasTarget(page.Events, created.ID) {
		t.Fatalf("no %q audit event for cluster %s; got %+v", store.AuditActionClusterCreate, created.ID, page.Events)
	}
	for _, e := range page.Events {
		if strings.Contains(string(e.Metadata), "super-secret-bearer-token") {
			t.Fatalf("credential leaked into audit metadata: %s", e.Metadata)
		}
	}

	// List — a BARE JSON array (no envelope). Credential absent; ca_cert
	// PRESENT (it's a public cert, echoed so the UI prefills it on edit).
	rr = request(srv, http.MethodGet, "/api/v1/admin/clusters", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("list status = %d", rr.Code)
	}
	if strings.Contains(rr.Body.String(), "super-secret-bearer-token") {
		t.Fatalf("credential leaked in list response: %s", rr.Body.String())
	}
	var listed []map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &listed); err != nil {
		t.Fatalf("list is not a bare array: %v (%s)", err, rr.Body.String())
	}
	if len(listed) != 1 {
		t.Fatalf("list len = %d", len(listed))
	}
	got := listed[0]
	if got["name"] != "prod-gke" {
		t.Errorf("name = %v", got["name"])
	}
	if _, present := got["credential"]; present {
		t.Errorf("credential field present in list DTO: %+v", got)
	}
	if ca, _ := got["ca_cert"].(string); ca == "" {
		t.Errorf("ca_cert absent/empty in list DTO — UI can't prefill on edit: %+v", got)
	}
	if got["has_ca"] != true {
		t.Errorf("has_ca = %v, want true", got["has_ca"])
	}

	// Update with the preserve sentinel — metadata-only edit keeps the
	// sealed credential, must not 503 or wipe it.
	upd := bytes.NewBufferString(`{
        "name": "prod-gke",
        "description": "now documented",
        "auth_type": "token",
        "api_server": "https://k8s.example.com:6443",
        "ca_cert": "-----BEGIN CERTIFICATE-----\nMIIBkTCB+w==\n-----END CERTIFICATE-----",
        "credential": "` + store.SecretPreserveSentinel + `",
        "allowed_projects": ["proj-a"]
    }`)
	rr = request(srv, http.MethodPut, "/api/v1/admin/clusters/"+created.ID, upd)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("update status = %d, body=%s", rr.Code, rr.Body.String())
	}

	// Delete — no usage, so 204.
	rr = request(srv, http.MethodDelete, "/api/v1/admin/clusters/"+created.ID, nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, body=%s", rr.Code, rr.Body.String())
	}
}

func TestClusters_CreateInCluster_NoCredential(t *testing.T) {
	// in_cluster carries no credential and needs no cipher; the api_server
	// is optional for it. Confirms the nil-cipher guard doesn't fire.
	_, _, srv := newClusterHandler(t)
	body := bytes.NewBufferString(`{
        "name": "local",
        "auth_type": "in_cluster"
    }`)
	rr := request(srv, http.MethodPost, "/api/v1/admin/clusters", body)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body=%s", rr.Code, rr.Body.String())
	}
}

func TestClusters_RejectsBadAuthType(t *testing.T) {
	_, _, srv := newClusterHandler(t)
	body := bytes.NewBufferString(`{"name":"x","auth_type":"oauth"}`)
	rr := request(srv, http.MethodPost, "/api/v1/admin/clusters", body)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "unsupported auth_type") {
		t.Errorf("body missing hint: %s", rr.Body.String())
	}
}

func TestClusters_RejectsMissingAPIServer(t *testing.T) {
	_, _, srv := newClusterHandler(t)
	body := bytes.NewBufferString(`{"name":"x","auth_type":"token","credential":"tok"}`)
	rr := request(srv, http.MethodPost, "/api/v1/admin/clusters", body)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "api_server") {
		t.Errorf("body missing hint: %s", rr.Body.String())
	}
}

func TestClusters_DuplicateNameConflict(t *testing.T) {
	_, _, srv := newClusterHandler(t)
	body := func() *bytes.Buffer {
		return bytes.NewBufferString(`{"name":"twice","auth_type":"in_cluster"}`)
	}
	if rr := request(srv, http.MethodPost, "/api/v1/admin/clusters", body()); rr.Code != http.StatusCreated {
		t.Fatalf("first create = %d", rr.Code)
	}
	rr := request(srv, http.MethodPost, "/api/v1/admin/clusters", body())
	if rr.Code != http.StatusConflict {
		t.Fatalf("second create status = %d, want 409, body=%s", rr.Code, rr.Body.String())
	}
}

func TestClusters_UpdateNotFound(t *testing.T) {
	// The handler looks the row up first (to enforce the immutable-name
	// rule), so ANY update to a missing id → 404, regardless of auth_type
	// or the preserve sentinel.
	_, _, srv := newClusterHandler(t)
	upd := bytes.NewBufferString(`{
        "name": "ghost",
        "auth_type": "token",
        "api_server": "https://k8s.example.com:6443",
        "credential": "` + store.SecretPreserveSentinel + `"
    }`)
	rr := request(srv, http.MethodPut, "/api/v1/admin/clusters/00000000-0000-0000-0000-000000000000", upd)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body=%s", rr.Code, rr.Body.String())
	}
}

// TestClusters_KubeconfigNoAPIServer_OK pins HIGH 2: a kubeconfig blob
// embeds its own server, so api_server is NOT required for it (the UI
// sends "" — that must not 400).
func TestClusters_KubeconfigNoAPIServer_OK(t *testing.T) {
	_, _, srv := newClusterHandler(t)
	body := bytes.NewBufferString(`{
        "name": "kc-cluster",
        "auth_type": "kubeconfig",
        "api_server": "",
        "credential": "apiVersion: v1\nkind: Config\nclusters: []\n"
    }`)
	rr := request(srv, http.MethodPost, "/api/v1/admin/clusters", body)
	if rr.Code != http.StatusCreated {
		t.Fatalf("kubeconfig create status = %d, want 201, body=%s", rr.Code, rr.Body.String())
	}
}

// TestClusters_TokenWithoutCA_Rejected pins HIGH 3 at the API edge: a
// token cluster with no ca_cert is a 422, never a silent insecure-TLS
// cluster.
func TestClusters_TokenWithoutCA_Rejected(t *testing.T) {
	_, _, srv := newClusterHandler(t)
	body := bytes.NewBufferString(`{
        "name": "no-ca",
        "auth_type": "token",
        "api_server": "https://k8s.example.com:6443",
        "credential": "tok"
    }`)
	rr := request(srv, http.MethodPost, "/api/v1/admin/clusters", body)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("token-without-ca status = %d, want 422, body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "ca_cert") {
		t.Errorf("body missing ca_cert hint: %s", rr.Body.String())
	}
}

// TestClusters_TokenHTTPAPIServer_Rejected pins the api_server
// hardening at the API edge: a non-https api_server (a typo that would
// ship the bearer token in cleartext) is a 422, not a cluster that only
// breaks at deploy.
func TestClusters_TokenHTTPAPIServer_Rejected(t *testing.T) {
	_, _, srv := newClusterHandler(t)
	body := bytes.NewBufferString(`{
        "name": "http-srv",
        "auth_type": "token",
        "api_server": "http://k8s.example.com:6443",
        "ca_cert": "-----BEGIN CERTIFICATE-----\nMIIBkTCB+w==\n-----END CERTIFICATE-----",
        "credential": "sa-token"
    }`)
	rr := request(srv, http.MethodPost, "/api/v1/admin/clusters", body)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("http api_server status = %d, want 422, body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "api_server") {
		t.Errorf("body missing api_server hint: %s", rr.Body.String())
	}
}

// TestClusters_RejectsRename pins MED 4: the name is a cluster's
// dispatch-time identity, so an update that changes it is a 422.
func TestClusters_RejectsRename(t *testing.T) {
	s, _, srv := newClusterHandler(t)
	ctx := context.Background()
	created, err := s.InsertCluster(ctx, nil, store.ClusterInput{
		Name: "original", AuthType: store.ClusterAuthInCluster,
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	upd := bytes.NewBufferString(`{"name":"renamed","auth_type":"in_cluster"}`)
	rr := request(srv, http.MethodPut, "/api/v1/admin/clusters/"+created.ID.String(), upd)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("rename status = %d, want 422, body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "immutable") {
		t.Errorf("body missing immutable hint: %s", rr.Body.String())
	}
}

func TestClusters_DeleteNotFound(t *testing.T) {
	_, _, srv := newClusterHandler(t)
	rr := request(srv, http.MethodDelete, "/api/v1/admin/clusters/00000000-0000-0000-0000-000000000000", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body=%s", rr.Code, rr.Body.String())
	}
}

// TestClusters_TestConnection registers a token cluster pointing at a
// stand-in TLS API server, then exercises POST /{id}/test → the probe
// reports ok with the version, and the token never crosses the wire.
func TestClusters_TestConnection(t *testing.T) {
	_, _, srv := newClusterHandler(t)

	const tok = "probe-token-xyz"
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/version" && r.Header.Get("Authorization") == "Bearer "+tok {
			_, _ = w.Write([]byte(`{"gitVersion":"v1.29.0"}`))
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer ts.Close()
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ts.Certificate().Raw})

	// Create via the API so the credential is sealed with the handler's
	// cipher (the same one ProbeCluster decrypts with).
	body, _ := json.Marshal(map[string]string{
		"name": "probe-me", "auth_type": "token",
		"api_server": ts.URL, "ca_cert": string(ca), "credential": tok,
	})
	rr := request(srv, http.MethodPost, "/api/v1/admin/clusters", bytes.NewBuffer(body))
	if rr.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body=%s", rr.Code, rr.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created: %v", err)
	}

	rr = request(srv, http.MethodPost, "/api/v1/admin/clusters/"+created.ID+"/test", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("test status = %d, body=%s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), tok) {
		t.Fatalf("token leaked into test response: %s", rr.Body.String())
	}
	var res struct{ Status, Message string }
	if err := json.Unmarshal(rr.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode test result: %v", err)
	}
	if res.Status != "ok" || !strings.Contains(res.Message, "v1.29.0") {
		t.Fatalf("probe result = %+v, want ok + version", res)
	}

	// A missing id → 404.
	rr = request(srv, http.MethodPost, "/api/v1/admin/clusters/00000000-0000-0000-0000-000000000000/test", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("test missing status = %d, want 404", rr.Code)
	}
}

func TestClusters_DeleteBlockedByUsage(t *testing.T) {
	// A pipeline references the cluster by name AND a queued run is bound
	// to it. The delete-guard must 409 on either axis; here both fire.
	s, pool, srv := newClusterHandler(t)
	ctx := context.Background()

	created, err := s.InsertCluster(ctx, nil, store.ClusterInput{
		Name:     "in-use",
		AuthType: store.ClusterAuthInCluster,
	})
	if err != nil {
		t.Fatalf("seed cluster: %v", err)
	}
	apply, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: "demo",
		Pipelines: []*domain.Pipeline{{
			Name:      "p1",
			Stages:    []string{"deploy"},
			Materials: []domain.Material{{Type: domain.MaterialManual, Fingerprint: "manual-1"}},
			Jobs: []domain.Job{{
				Name: "deploy", Stage: "deploy", Cluster: "in-use",
			}},
		}},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	pipelineID := apply.Pipelines[0].PipelineID

	// Seed a queued run so the active-runs axis fires too.
	if _, err := pool.Exec(ctx, `
        INSERT INTO runs (id, pipeline_id, counter, status, cause, revisions, started_at)
        VALUES (gen_random_uuid(), $1, 1, 'queued', 'manual', '{}'::jsonb, NOW())
    `, pipelineID); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	rr := request(srv, http.MethodDelete, "/api/v1/admin/clusters/"+created.ID.String(), nil)
	if rr.Code != http.StatusConflict {
		t.Fatalf("delete status = %d, want 409, body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "active run") &&
		!strings.Contains(rr.Body.String(), "1 pipeline") {
		t.Fatalf("error message did not mention active runs or pipelines: %q", rr.Body.String())
	}
}

func auditHasTarget(events []store.AuditEvent, targetID string) bool {
	for _, e := range events {
		if e.TargetID == targetID {
			return true
		}
	}
	return false
}
