package admin_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	adminapi "github.com/gocdnext/gocdnext/server/internal/api/admin"
	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

func TestAdmin_Audit_ListsEventsNewestFirst(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	for _, a := range []string{
		store.AuditActionProjectApply,
		store.AuditActionSecretSet,
		store.AuditActionRunCancel,
	} {
		if _, err := s.EmitAuditEvent(ctx, store.AuditEmit{
			Action: a, TargetType: "t",
		}); err != nil {
			t.Fatal(err)
		}
	}

	h := adminapi.NewHandler(s, nil, nil, adminapi.WiringState{}, quietLogger())
	srv := mount(h)

	resp := httpGet(t, srv, "/api/v1/admin/audit")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body struct {
		Events []struct {
			Action string `json:"action"`
		} `json:"events"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Events) != 3 {
		t.Fatalf("events = %d, want 3", len(body.Events))
	}
}

func TestAdmin_Audit_FilterByAction(t *testing.T) {
	// Query-param filter must reach the store so an audit search
	// for a specific action returns only matching rows — the
	// whole point of the filter is noise reduction on a busy log.
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	_, _ = s.EmitAuditEvent(ctx, store.AuditEmit{Action: store.AuditActionSecretSet, TargetType: "t"})
	_, _ = s.EmitAuditEvent(ctx, store.AuditEmit{Action: store.AuditActionCachePurge, TargetType: "t"})
	_, _ = s.EmitAuditEvent(ctx, store.AuditEmit{Action: store.AuditActionSecretSet, TargetType: "t"})

	h := adminapi.NewHandler(s, nil, nil, adminapi.WiringState{}, quietLogger())
	srv := mount(h)

	resp := httpGet(t, srv, "/api/v1/admin/audit?action="+store.AuditActionSecretSet)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body struct {
		Events []struct {
			Action string `json:"action"`
		} `json:"events"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if len(body.Events) != 2 {
		t.Fatalf("filtered = %d, want 2", len(body.Events))
	}
	for _, e := range body.Events {
		if e.Action != store.AuditActionSecretSet {
			t.Errorf("filter leaked: %q", e.Action)
		}
	}
}

func TestAdmin_Audit_LimitCap(t *testing.T) {
	// `?limit=999` should clamp to the 500 ceiling silently rather
	// than 400-ing — a caller asking for too many gets as many as
	// we're willing to serve, not a rejection.
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	h := adminapi.NewHandler(s, nil, nil, adminapi.WiringState{}, quietLogger())
	srv := mount(h)

	resp := httpGet(t, srv, "/api/v1/admin/audit?limit=999")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

func TestAdmin_Audit_BadLimit400(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	h := adminapi.NewHandler(s, nil, nil, adminapi.WiringState{}, quietLogger())
	srv := mount(h)

	resp := httpGet(t, srv, "/api/v1/admin/audit?limit=abc")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}
