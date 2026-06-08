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

func TestRunnerProfiles_NodeSelectorAndTolerations_RoundTrip(t *testing.T) {
	// Admin POST → GET round-trip preserves node_selector +
	// tolerations exactly, including TolerationSeconds + the
	// normalisation of empty operator → "Equal".
	_, _, srv := newRunnerProfileHandler(t)

	body := bytes.NewBufferString(`{
        "name": "gradle-pool",
        "engine": "kubernetes",
        "node_selector": {
            "workload": "ci",
            "kubernetes.io/arch": "amd64"
        },
        "tolerations": [
            {"key": "ci-only", "operator": "Equal", "value": "true", "effect": "NoSchedule"},
            {"key": "spot", "operator": "Exists", "effect": "NoExecute", "toleration_seconds": 60},
            {"key": "implicit", "value": "yes", "effect": "NoSchedule"}
        ]
    }`)
	rr := request(srv, http.MethodPost, "/api/v1/admin/runner-profiles", body)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body=%s", rr.Code, rr.Body.String())
	}
	var created struct{ ID string }
	_ = json.Unmarshal(rr.Body.Bytes(), &created)

	rr = request(srv, http.MethodGet, "/api/v1/admin/runner-profiles", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("list status = %d", rr.Code)
	}
	var listed struct {
		Profiles []map[string]any `json:"profiles"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &listed)
	if len(listed.Profiles) != 1 {
		t.Fatalf("len = %d", len(listed.Profiles))
	}
	p := listed.Profiles[0]
	ns, _ := p["node_selector"].(map[string]any)
	if ns["workload"] != "ci" || ns["kubernetes.io/arch"] != "amd64" {
		t.Errorf("node_selector lost: %+v", ns)
	}
	tolerations, _ := p["tolerations"].([]any)
	if len(tolerations) != 3 {
		t.Fatalf("tolerations len = %d", len(tolerations))
	}
	// Third entry had no operator — normalised to "Equal".
	implicit, _ := tolerations[2].(map[string]any)
	if implicit["operator"] != "Equal" {
		t.Errorf("operator not normalised: %+v", implicit)
	}
	// Second entry's toleration_seconds round-trips as JSON number.
	spot, _ := tolerations[1].(map[string]any)
	if spot["toleration_seconds"] != float64(60) {
		t.Errorf("toleration_seconds = %v, want 60", spot["toleration_seconds"])
	}
}

func TestRunnerProfiles_RejectsInvalidScheduling(t *testing.T) {
	// Each row hits one of the validation invariants. Failure mode
	// is HTTP 400 with the offending field surfaced in the body.
	_, _, srv := newRunnerProfileHandler(t)

	cases := []struct {
		name     string
		body     string
		wantHint string
	}{
		{
			name: "node_selector key has bad charset",
			body: `{"name":"x","engine":"kubernetes","node_selector":{"bad key":"v"}}`,
			wantHint: "node_selector key",
		},
		{
			name: "node_selector prefix not DNS subdomain (uppercase)",
			body: `{"name":"x","engine":"kubernetes","node_selector":{"BAD/name":"v"}}`,
			wantHint: "node_selector key",
		},
		{
			// Catches the regex permissiveness gap the previous hand-rolled
			// validator had — `..` between dot-separated subdomain labels
			// is rejected by k8svalidation but slipped past the old
			// `^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$` shape.
			name: "node_selector prefix has consecutive dots",
			body: `{"name":"x","engine":"kubernetes","node_selector":{"a..b/name":"v"}}`,
			wantHint: "node_selector key",
		},
		{
			// `.-` between labels: same gap as `..`. apiserver rejects;
			// hand-rolled regex would have accepted.
			name: "node_selector prefix has dot-dash transition",
			body: `{"name":"x","engine":"kubernetes","node_selector":{"a.-b/name":"v"}}`,
			wantHint: "node_selector key",
		},
		{
			// `-.`: mirror case. Same gap.
			name: "node_selector prefix has dash-dot transition",
			body: `{"name":"x","engine":"kubernetes","node_selector":{"a-.b/name":"v"}}`,
			wantHint: "node_selector key",
		},
		{
			name: "node_selector value too long",
			body: `{"name":"x","engine":"kubernetes","node_selector":{"k":"` + strings.Repeat("a", 64) + `"}}`,
			wantHint: "node_selector[",
		},
		{
			name: "toleration operator unknown",
			body: `{"name":"x","engine":"kubernetes","tolerations":[{"key":"k","operator":"DoesNotEqual","effect":"NoSchedule"}]}`,
			wantHint: "operator",
		},
		{
			name: "toleration effect unknown",
			body: `{"name":"x","engine":"kubernetes","tolerations":[{"key":"k","operator":"Equal","effect":"Maybe"}]}`,
			wantHint: "effect",
		},
		{
			name: "toleration Exists with value",
			body: `{"name":"x","engine":"kubernetes","tolerations":[{"key":"k","operator":"Exists","value":"oops","effect":"NoSchedule"}]}`,
			wantHint: "Exists requires empty value",
		},
		{
			name: "toleration_seconds negative",
			body: `{"name":"x","engine":"kubernetes","tolerations":[{"key":"k","operator":"Equal","value":"v","effect":"NoExecute","toleration_seconds":-1}]}`,
			wantHint: "must be ≥ 0",
		},
		{
			name: "toleration_seconds with wrong effect",
			body: `{"name":"x","engine":"kubernetes","tolerations":[{"key":"k","operator":"Equal","value":"v","effect":"NoSchedule","toleration_seconds":10}]}`,
			wantHint: "only valid with effect=NoExecute",
		},
		{
			name: "toleration with empty key + Equal",
			body: `{"name":"x","engine":"kubernetes","tolerations":[{"operator":"Equal","value":"v","effect":"NoSchedule"}]}`,
			wantHint: "key required unless operator=Exists",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := request(srv, http.MethodPost, "/api/v1/admin/runner-profiles", bytes.NewBufferString(tc.body))
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
			}
			if !strings.Contains(rr.Body.String(), tc.wantHint) {
				t.Errorf("body %q missing hint %q", rr.Body.String(), tc.wantHint)
			}
		})
	}
}

func TestRunnerProfiles_TolerationExistsWithoutKey_Allowed(t *testing.T) {
	// `tolerations: [{operator: Exists}]` — the kubelet's "tolerate
	// everything" pattern — is legal: empty Key + Exists matches
	// every taint. Validator must allow this; only Equal+empty-Key
	// is meaningless and gets rejected.
	_, _, srv := newRunnerProfileHandler(t)

	body := bytes.NewBufferString(`{
        "name": "tolerate-all",
        "engine": "kubernetes",
        "tolerations": [{"operator": "Exists"}]
    }`)
	rr := request(srv, http.MethodPost, "/api/v1/admin/runner-profiles", body)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
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
			Name:      "p1",
			Stages:    []string{"build"},
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
			Name:      "p1",
			Stages:    []string{"build"},
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
