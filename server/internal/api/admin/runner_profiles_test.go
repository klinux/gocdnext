package admin_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	adminapi "github.com/gocdnext/gocdnext/server/internal/api/admin"
	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/domain"
	"github.com/gocdnext/gocdnext/server/internal/retention"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

func newRunnerProfileHandler(t *testing.T) (*store.Store, http.Handler) {
	t.Helper()
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	sweeper := retention.New(s, nil, quietLogger())
	h := adminapi.NewHandler(s, sweeper, nil, adminapi.WiringState{}, quietLogger())
	return s, mount(h)
}

func TestRunnerProfiles_CreateListUpdateDelete(t *testing.T) {
	_, srv := newRunnerProfileHandler(t)

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
	_, srv := newRunnerProfileHandler(t)
	body := bytes.NewBufferString(`{"name":"foo","engine":"docker"}`)
	rr := request(srv, http.MethodPost, "/api/v1/admin/runner-profiles", body)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

func TestRunnerProfiles_DuplicateNameConflict(t *testing.T) {
	_, srv := newRunnerProfileHandler(t)
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

func TestRunnerProfiles_DeleteBlockedWhenInUse(t *testing.T) {
	s, srv := newRunnerProfileHandler(t)
	ctx := context.Background()

	created, err := s.InsertRunnerProfile(ctx, store.RunnerProfileInput{
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
