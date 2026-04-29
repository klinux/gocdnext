package admin_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	adminapi "github.com/gocdnext/gocdnext/server/internal/api/admin"
	"github.com/gocdnext/gocdnext/server/internal/crypto"
	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/domain"
	"github.com/gocdnext/gocdnext/server/internal/retention"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

func newRunnerProfileHandler(t *testing.T) (*store.Store, *pgxpool.Pool, http.Handler) {
	t.Helper()
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	sweeper := retention.New(s, nil, quietLogger())
	h := adminapi.NewHandler(s, sweeper, nil, adminapi.WiringState{}, quietLogger())
	// Wire a deterministic cipher so secret-bearing tests can
	// round-trip without flakiness from random key material.
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	c, err := crypto.NewCipher(key)
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	h.SetCipher(c)
	return s, pool, mount(h)
}

func TestRunnerProfiles_CreateListUpdateDelete(t *testing.T) {
	_, _, srv := newRunnerProfileHandler(t)

	// Create
	body := bytes.NewBufferString(`{
        "name": "default",
        "description": "vanilla pool",
        "engine": "kubernetes",
        "default_image": "alpine:3.20",
        "max_cpu": "4",
        "max_mem": "8Gi",
        "tags": ["linux"]
    }`)
	rr := request(srv, http.MethodPost, "/api/v1/admin/runner-profiles", body)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body=%s", rr.Code, rr.Body.String())
	}
	var created struct{ ID, Name, Engine string }
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created: %v", err)
	}
	if created.Name != "default" || created.Engine != "kubernetes" {
		t.Fatalf("created = %+v", created)
	}

	// List
	rr = request(srv, http.MethodGet, "/api/v1/admin/runner-profiles", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("list status = %d", rr.Code)
	}
	var listed struct {
		Profiles []map[string]any `json:"profiles"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &listed)
	if len(listed.Profiles) != 1 || listed.Profiles[0]["name"] != "default" {
		t.Fatalf("list = %+v", listed)
	}

	// Update
	upd := bytes.NewBufferString(`{
        "name": "default",
        "description": "now with budget",
        "engine": "kubernetes",
        "max_cpu": "2",
        "max_mem": "4Gi",
        "tags": ["linux"]
    }`)
	rr = request(srv, http.MethodPut, "/api/v1/admin/runner-profiles/"+created.ID, upd)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("update status = %d, body=%s", rr.Code, rr.Body.String())
	}

	// Delete
	rr = request(srv, http.MethodDelete, "/api/v1/admin/runner-profiles/"+created.ID, nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, body=%s", rr.Code, rr.Body.String())
	}
}

func TestRunnerProfiles_RejectsBadEngine(t *testing.T) {
	_, _, srv := newRunnerProfileHandler(t)
	body := bytes.NewBufferString(`{"name":"foo","engine":"docker"}`)
	rr := request(srv, http.MethodPost, "/api/v1/admin/runner-profiles", body)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

func TestRunnerProfiles_DuplicateNameConflict(t *testing.T) {
	_, _, srv := newRunnerProfileHandler(t)
	body := func() *bytes.Buffer {
		return bytes.NewBufferString(`{"name":"twice","engine":"kubernetes"}`)
	}
	if rr := request(srv, http.MethodPost, "/api/v1/admin/runner-profiles", body()); rr.Code != http.StatusCreated {
		t.Fatalf("first create = %d", rr.Code)
	}
	rr := request(srv, http.MethodPost, "/api/v1/admin/runner-profiles", body())
	if rr.Code != http.StatusConflict {
		t.Fatalf("second create status = %d, want 409", rr.Code)
	}
}

func TestRunnerProfiles_DeleteBlockedByActiveRun(t *testing.T) {
	// Pipeline rewired to drop the profile reference, BUT a queued
	// run still exists against it. The legacy guard (pipelines-only)
	// would let the delete through and orphan the in-flight run;
	// the extended guard catches it.
	s, pool, srv := newRunnerProfileHandler(t)
	ctx := context.Background()

	created, err := s.InsertRunnerProfile(ctx, nil, store.RunnerProfileInput{
		Name: "still-running", Engine: "kubernetes",
	})
	if err != nil {
		t.Fatalf("seed profile: %v", err)
	}
	apply, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: "demo",
		Pipelines: []*domain.Pipeline{{
			Name:   "p1",
			Stages: []string{"build"},
			Materials: []domain.Material{{Type: domain.MaterialManual, Fingerprint: "manual-1"}},
			Jobs: []domain.Job{{
				Name: "build", Stage: "build", Profile: "still-running",
			}},
		}},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	pipelineID := apply.Pipelines[0].PipelineID

	// Insert a queued run row directly so we can drive the guard
	// without a full webhook flow.
	if _, err := pool.Exec(ctx, `
        INSERT INTO runs (id, pipeline_id, counter, status, cause, revisions, started_at)
        VALUES (gen_random_uuid(), $1, 1, 'queued', 'manual', '{}'::jsonb, NOW())
    `, pipelineID); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	rr := request(srv, http.MethodDelete, "/api/v1/admin/runner-profiles/"+created.ID.String(), nil)
	if rr.Code != http.StatusConflict {
		t.Fatalf("delete status = %d, want 409, body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "active run") &&
		!strings.Contains(rr.Body.String(), "1 pipeline") {
		t.Fatalf("error message did not mention active runs or pipelines: %q", rr.Body.String())
	}
}

func TestRunnerProfiles_DeleteBlockedWhenInUse(t *testing.T) {
	s, _, srv := newRunnerProfileHandler(t)
	ctx := context.Background()

	created, err := s.InsertRunnerProfile(ctx, nil, store.RunnerProfileInput{
		Name: "in-use", Engine: "kubernetes",
	})
	if err != nil {
		t.Fatalf("seed profile: %v", err)
	}
	// Persist a pipeline whose definition references the profile name.
	if _, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: "demo",
		Pipelines: []*domain.Pipeline{{
			Name:   "p1",
			Stages: []string{"build"},
			Materials: []domain.Material{{Type: domain.MaterialManual, Fingerprint: "manual-1"}},
			Jobs: []domain.Job{{
				Name: "build", Stage: "build", Profile: "in-use",
			}},
		}},
	}); err != nil {
		t.Fatalf("apply: %v", err)
	}

	rr := request(srv, http.MethodDelete, "/api/v1/admin/runner-profiles/"+created.ID.String(), nil)
	if rr.Code != http.StatusConflict {
		t.Fatalf("delete status = %d, want 409, body=%s", rr.Code, rr.Body.String())
	}
}

func TestRunnerProfiles_EnvAndSecrets_RoundTripMasksValues(t *testing.T) {
	_, _, srv := newRunnerProfileHandler(t)

	// Create with both env and secrets — the response must echo
	// env plainly and surface secret_keys (sorted) without values.
	body := bytes.NewBufferString(`{
        "name": "fast-builds",
        "engine": "kubernetes",
        "tags": ["linux"],
        "env": {
            "GOCDNEXT_LAYER_CACHE_BUCKET": "ci-cache",
            "GOCDNEXT_LAYER_CACHE_REGION": "us-east-1"
        },
        "secrets": {
            "AWS_ACCESS_KEY_ID":     "AKIA-FAKE",
            "AWS_SECRET_ACCESS_KEY": "supersecret"
        }
    }`)
	rr := request(srv, http.MethodPost, "/api/v1/admin/runner-profiles", body)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body=%s", rr.Code, rr.Body.String())
	}

	rr = request(srv, http.MethodGet, "/api/v1/admin/runner-profiles", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("list status = %d", rr.Code)
	}
	// Plaintext values must not appear in any GET response.
	if strings.Contains(rr.Body.String(), "supersecret") || strings.Contains(rr.Body.String(), "AKIA-FAKE") {
		t.Fatalf("secret values leaked in list response: %s", rr.Body.String())
	}
	var listed struct {
		Profiles []struct {
			Name       string            `json:"name"`
			Env        map[string]string `json:"env"`
			SecretKeys []string          `json:"secret_keys"`
		} `json:"profiles"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(listed.Profiles) != 1 {
		t.Fatalf("expected 1 profile, got %d", len(listed.Profiles))
	}
	got := listed.Profiles[0]
	if got.Env["GOCDNEXT_LAYER_CACHE_BUCKET"] != "ci-cache" {
		t.Errorf("env not echoed: %+v", got.Env)
	}
	wantKeys := []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"}
	if len(got.SecretKeys) != len(wantKeys) || got.SecretKeys[0] != wantKeys[0] || got.SecretKeys[1] != wantKeys[1] {
		t.Errorf("secret_keys = %+v, want %+v (sorted)", got.SecretKeys, wantKeys)
	}
}

func TestRunnerProfiles_RejectsInvalidEnvKey(t *testing.T) {
	_, _, srv := newRunnerProfileHandler(t)
	body := bytes.NewBufferString(`{
        "name": "bad",
        "engine": "kubernetes",
        "env": {"lower-case": "nope"}
    }`)
	rr := request(srv, http.MethodPost, "/api/v1/admin/runner-profiles", body)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "UPPER_SNAKE") {
		t.Errorf("error should hint at UPPER_SNAKE_CASE convention: %s", rr.Body.String())
	}
}

func request(srv http.Handler, method, path string, body *bytes.Buffer) *httptest.ResponseRecorder {
	var req *http.Request
	if body != nil {
		req = httptest.NewRequest(method, path, body)
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	return rr
}
