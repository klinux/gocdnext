package admin_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	adminapi "github.com/gocdnext/gocdnext/server/internal/api/admin"
	"github.com/gocdnext/gocdnext/server/internal/api/authapi"
	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

func seedUser(t *testing.T, s *store.Store, email, extID, role string) store.User {
	t.Helper()
	u, err := s.UpsertUserByProvider(context.Background(), store.UpsertUserInput{
		Email:       email,
		Name:        email,
		Provider:    "github",
		ExternalID:  extID,
		InitialRole: role,
	})
	if err != nil {
		t.Fatalf("seed user %s: %v", email, err)
	}
	return u
}

func TestAdmin_Users_ListsAllWithRole(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	seedUser(t, s, "alice@example.com", "a", store.RoleAdmin)
	seedUser(t, s, "bob@example.com", "b", store.RoleMaintainer)
	seedUser(t, s, "carol@example.com", "c", store.RoleViewer)

	h := adminapi.NewHandler(s, nil, nil, adminapi.WiringState{}, quietLogger())
	srv := mount(h)

	resp := httpGet(t, srv, "/api/v1/admin/users")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body struct {
		Users []struct {
			Email string `json:"email"`
			Role  string `json:"role"`
		} `json:"users"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Users) != 3 {
		t.Fatalf("got %d users, want 3", len(body.Users))
	}
	roles := map[string]string{}
	for _, u := range body.Users {
		roles[u.Email] = u.Role
	}
	if roles["alice@example.com"] != store.RoleAdmin ||
		roles["bob@example.com"] != store.RoleMaintainer ||
		roles["carol@example.com"] != store.RoleViewer {
		t.Errorf("roles round-trip wrong: %+v", roles)
	}
}

func TestAdmin_SetUserRole_PromotesAndEmitsAudit(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	target := seedUser(t, s, "bob@example.com", "bob", store.RoleMaintainer)
	actor := seedUser(t, s, "admin@example.com", "admin", store.RoleAdmin)

	h := adminapi.NewHandler(s, nil, nil, adminapi.WiringState{}, quietLogger())
	srv := mount(h)

	body, _ := json.Marshal(map[string]string{"role": store.RoleAdmin})
	req := httptest.NewRequest(http.MethodPut,
		"/api/v1/admin/users/"+target.ID.String()+"/role",
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	// Inject the acting admin into context so self-demotion guard
	// has something to compare against and audit emit stamps the
	// right actor.
	req = req.WithContext(authapi.WithUser(req.Context(), actor))
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var updated struct {
		Role string `json:"role"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &updated)
	if updated.Role != store.RoleAdmin {
		t.Errorf("returned role = %q", updated.Role)
	}

	// Audit emit must fire with before/after captured.
	page, err := s.ListAuditEvents(context.Background(), store.ListAuditEventsFilter{
		Action: store.AuditActionUserRoleChange, Limit: 10,
	})
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	events := page.Events
	if len(events) != 1 {
		t.Fatalf("expected 1 audit event, got %d", len(events))
	}
	e := events[0]
	var meta map[string]any
	_ = json.Unmarshal(e.Metadata, &meta)
	if meta["from"] != store.RoleMaintainer || meta["to"] != store.RoleAdmin {
		t.Errorf("audit metadata = %+v", meta)
	}
	if e.ActorEmail != actor.Email {
		t.Errorf("audit actor_email = %q", e.ActorEmail)
	}
}

func TestAdmin_SetUserRole_BlocksSelfDemotion(t *testing.T) {
	// Last-admin-locked-out is the tripwire; an admin PUT-ing
	// their own id with a non-admin role is almost always a
	// misclick. 403 surfaces the refusal so the UI can grey out
	// the dropdown row for "self" instead of the user guessing.
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	me := seedUser(t, s, "me@example.com", "me", store.RoleAdmin)

	h := adminapi.NewHandler(s, nil, nil, adminapi.WiringState{}, quietLogger())
	srv := mount(h)

	body, _ := json.Marshal(map[string]string{"role": store.RoleViewer})
	req := httptest.NewRequest(http.MethodPut,
		"/api/v1/admin/users/"+me.ID.String()+"/role",
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(authapi.WithUser(req.Context(), me))
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rr.Code)
	}
	// The store must NOT have been touched.
	after, _ := s.GetUser(context.Background(), me.ID)
	if after.Role != store.RoleAdmin {
		t.Errorf("role drifted to %q despite 403", after.Role)
	}
}

func TestAdmin_SetUserRole_MalformedRole400(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	target := seedUser(t, s, "alice@example.com", "a", store.RoleMaintainer)
	actor := seedUser(t, s, "admin@example.com", "admin", store.RoleAdmin)

	h := adminapi.NewHandler(s, nil, nil, adminapi.WiringState{}, quietLogger())
	srv := mount(h)

	body, _ := json.Marshal(map[string]string{"role": "superadmin"})
	req := httptest.NewRequest(http.MethodPut,
		"/api/v1/admin/users/"+target.ID.String()+"/role",
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(authapi.WithUser(req.Context(), actor))
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
}

func TestAdmin_SetUserRole_UnknownID404(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	actor := seedUser(t, s, "admin@example.com", "admin", store.RoleAdmin)

	h := adminapi.NewHandler(s, nil, nil, adminapi.WiringState{}, quietLogger())
	srv := mount(h)

	body, _ := json.Marshal(map[string]string{"role": store.RoleAdmin})
	req := httptest.NewRequest(http.MethodPut,
		"/api/v1/admin/users/"+uuid.New().String()+"/role",
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(authapi.WithUser(req.Context(), actor))
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rr.Code)
	}
}
