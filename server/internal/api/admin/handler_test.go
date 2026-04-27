package admin_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/go-chi/chi/v5"

	adminapi "github.com/gocdnext/gocdnext/server/internal/api/admin"
	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/retention"
	ghscm "github.com/gocdnext/gocdnext/server/internal/scm/github"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/internal/vcs"
)

func testAppClient(t *testing.T) *ghscm.AppClient {
	t.Helper()
	c, err := ghscm.NewAppClient(ghscm.AppConfig{
		AppID:         1,
		PrivateKeyPEM: []byte(realPEM(t)),
	})
	if err != nil {
		t.Fatalf("app client: %v", err)
	}
	return c
}

func TestAdminHandler_Retention(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)

	sweeper := retention.New(s, nil, quietLogger())
	h := adminapi.NewHandler(s, sweeper, nil, adminapi.WiringState{}, quietLogger())
	srv := mount(h)

	resp := httpGet(t, srv, "/api/v1/admin/retention")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["enabled"] != false {
		t.Fatalf("enabled = %v, want false", got["enabled"])
	}
}

func TestAdminHandler_WebhooksList(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	cases := []struct {
		provider string
		event    string
		status   string
		httpCode int32
	}{
		{"github", "push", store.WebhookStatusAccepted, 202},
		{"github", "push", store.WebhookStatusIgnored, 204},
		{"github", "push", store.WebhookStatusRejected, 401},
		{"gitlab", "push", store.WebhookStatusAccepted, 202},
	}
	for _, c := range cases {
		if _, _, err := s.InsertWebhookDelivery(ctx, store.InsertWebhookDeliveryInput{
			Provider:   c.provider,
			Event:      c.event,
			Status:     c.status,
			HTTPStatus: c.httpCode,
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	h := adminapi.NewHandler(s, nil, nil, adminapi.WiringState{}, quietLogger())
	srv := mount(h)

	// No filters → all 4.
	resp := httpGet(t, srv, "/api/v1/admin/webhooks")
	defer resp.Body.Close()
	var listed struct {
		Deliveries []map[string]any `json:"deliveries"`
		Total      int64            `json:"total"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&listed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if listed.Total != 4 || len(listed.Deliveries) != 4 {
		t.Fatalf("unfiltered total=%d len=%d, want 4/4", listed.Total, len(listed.Deliveries))
	}

	// Provider filter → 3.
	resp = httpGet(t, srv, "/api/v1/admin/webhooks?provider=github")
	defer resp.Body.Close()
	_ = json.NewDecoder(resp.Body).Decode(&listed)
	if listed.Total != 3 {
		t.Fatalf("github-only total = %d, want 3", listed.Total)
	}

	// Provider + status → 1.
	resp = httpGet(t, srv, "/api/v1/admin/webhooks?provider=github&status=accepted")
	defer resp.Body.Close()
	_ = json.NewDecoder(resp.Body).Decode(&listed)
	if listed.Total != 1 {
		t.Fatalf("github+accepted total = %d, want 1", listed.Total)
	}
}

func TestAdminHandler_WebhookDetail(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	id, _, err := s.InsertWebhookDelivery(ctx, store.InsertWebhookDeliveryInput{
		Provider:   "github",
		Event:      "push",
		Status:     store.WebhookStatusAccepted,
		HTTPStatus: 202,
		Headers:    json.RawMessage(`{"X-GitHub-Event":"push"}`),
		Payload:    json.RawMessage(`{"ref":"refs/heads/main"}`),
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	h := adminapi.NewHandler(s, nil, nil, adminapi.WiringState{}, quietLogger())
	srv := mount(h)

	resp := httpGet(t, srv, "/api/v1/admin/webhooks/"+strconv.FormatInt(id, 10))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got struct {
		Headers map[string]string `json:"headers"`
		Payload map[string]any    `json:"payload"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Headers["X-GitHub-Event"] != "push" {
		t.Fatalf("headers = %+v", got.Headers)
	}
	if got.Payload["ref"] != "refs/heads/main" {
		t.Fatalf("payload = %+v", got.Payload)
	}

	// Missing id → 404.
	resp = httpGet(t, srv, "/api/v1/admin/webhooks/999999")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing status = %d, want 404", resp.StatusCode)
	}
}

func TestAdminHandler_IntegrationGitHub(t *testing.T) {
	// Registry carries an active app via a direct Replace — this
	// exercises the live-lookup path that the bug fix introduced.
	reg := vcs.New()
	reg.Replace(testAppClient(t), []vcs.Integration{{
		Name: "env", Kind: "github_app", Enabled: true, Source: vcs.SourceEnv,
	}})
	h := adminapi.NewHandler(nil, nil, reg, adminapi.WiringState{
		PublicBaseSet:    false,
		ChecksReporterOn: true,
	}, quietLogger())
	srv := mount(h)

	resp := httpGet(t, srv, "/api/v1/admin/integrations/github")
	defer resp.Body.Close()
	var got map[string]bool
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["github_app_configured"] != true || got["public_base_set"] != false {
		t.Fatalf("booleans = %+v", got)
	}
}

// --- helpers ---

func mount(h *adminapi.Handler) http.Handler {
	r := chi.NewRouter()
	r.Get("/api/v1/admin/retention", h.Retention)
	r.Get("/api/v1/admin/webhooks", h.Webhooks)
	r.Get("/api/v1/admin/webhooks/{id}", h.WebhookDetail)
	r.Get("/api/v1/admin/health", h.Health)
	r.Get("/api/v1/admin/integrations/github", h.IntegrationGitHub)
	r.Get("/api/v1/admin/users", h.Users)
	r.Put("/api/v1/admin/users/{id}/role", h.SetUserRole)
	r.Get("/api/v1/admin/audit", h.Audit)
	r.Get("/api/v1/admin/runner-profiles", h.RunnerProfiles)
	r.Post("/api/v1/admin/runner-profiles", h.CreateRunnerProfile)
	r.Put("/api/v1/admin/runner-profiles/{id}", h.UpdateRunnerProfile)
	r.Delete("/api/v1/admin/runner-profiles/{id}", h.DeleteRunnerProfile)
	return r
}

func httpGet(t *testing.T, srv http.Handler, path string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	return rr.Result()
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
