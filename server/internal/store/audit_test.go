package store_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

func TestEmitAuditEvent_RoundTripsFields(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	actor := uuid.New()
	ev, err := s.EmitAuditEvent(ctx, store.AuditEmit{
		ActorID:    actor,
		ActorEmail: "alice@example.com",
		Action:     store.AuditActionApprovalApprove,
		TargetType: "job_run",
		TargetID:   "11111111-1111-1111-1111-111111111111",
		Metadata: map[string]any{
			"job_name": "deploy-prod",
			"run_id":   "22222222-2222-2222-2222-222222222222",
		},
	})
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	if ev.ActorID == nil || *ev.ActorID != actor {
		t.Errorf("actor round-trip: %+v", ev.ActorID)
	}
	if ev.Action != store.AuditActionApprovalApprove {
		t.Errorf("action = %q", ev.Action)
	}
	// Metadata must round-trip with values intact — the admin UI
	// renders this key/value.
	var meta map[string]any
	if err := json.Unmarshal(ev.Metadata, &meta); err != nil {
		t.Fatalf("unmarshal metadata: %v", err)
	}
	if meta["job_name"] != "deploy-prod" {
		t.Errorf("metadata.job_name = %v", meta["job_name"])
	}
	if time.Since(ev.At) > time.Minute {
		t.Errorf("at stamp drifted: %v", ev.At)
	}
}

func TestEmitAuditEvent_SystemEventHasNoActor(t *testing.T) {
	// Webhook-driven events (no authenticated user) should record
	// cleanly with NULL actor_id. The listing query handles this
	// by not filtering on actor_id when the filter is unset.
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	ev, err := s.EmitAuditEvent(ctx, store.AuditEmit{
		Action:     store.AuditActionRunTrigger,
		TargetType: "pipeline",
		TargetID:   "pipeline-id",
	})
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	if ev.ActorID != nil {
		t.Errorf("actor_id = %+v, want nil for system event", ev.ActorID)
	}
	if ev.ActorEmail != "" {
		t.Errorf("actor_email = %q, want empty", ev.ActorEmail)
	}
}

func TestEmitAuditEvent_RejectsEmptyActionOrTarget(t *testing.T) {
	// Defense-in-depth: handlers should never pass empty action
	// or target_type but a defensive guard means a regression
	// surfaces here instead of as an unparseable log entry.
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	for _, bad := range []store.AuditEmit{
		{Action: "", TargetType: "x"},
		{Action: "x", TargetType: ""},
	} {
		if _, err := s.EmitAuditEvent(ctx, bad); err == nil {
			t.Errorf("expected err on %+v", bad)
		}
	}
}

func TestListAuditEvents_NewestFirst(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	// Insert three events with staggered timestamps by sleeping —
	// the DB's NOW() returns microsecond-resolution so two inserts
	// inside the same microsecond tie, and the ORDER BY tiebreaker
	// is id DESC (monotonic UUIDs aren't a guarantee) — a tiny
	// sleep removes the flake.
	for _, action := range []string{
		store.AuditActionProjectApply,
		store.AuditActionSecretSet,
		store.AuditActionCachePurge,
	} {
		if _, err := s.EmitAuditEvent(ctx, store.AuditEmit{
			Action: action, TargetType: "t",
		}); err != nil {
			t.Fatalf("emit %s: %v", action, err)
		}
		time.Sleep(time.Millisecond)
	}

	page, err := s.ListAuditEvents(ctx, store.ListAuditEventsFilter{Limit: 10})
	got := page.Events
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d events, want 3", len(got))
	}
	// Newest first: cache.purge → secret.set → project.apply
	if got[0].Action != store.AuditActionCachePurge ||
		got[1].Action != store.AuditActionSecretSet ||
		got[2].Action != store.AuditActionProjectApply {
		t.Errorf("order wrong: %s %s %s",
			got[0].Action, got[1].Action, got[2].Action)
	}
}

func TestListAuditEvents_FiltersByAction(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	for _, a := range []string{
		store.AuditActionProjectApply,
		store.AuditActionProjectApply,
		store.AuditActionSecretSet,
	} {
		_, _ = s.EmitAuditEvent(ctx, store.AuditEmit{Action: a, TargetType: "t"})
	}

	page, err := s.ListAuditEvents(ctx, store.ListAuditEventsFilter{
		Action: store.AuditActionProjectApply, Limit: 10,
	})
	got := page.Events
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("filtered list = %d, want 2", len(got))
	}
	for _, e := range got {
		if e.Action != store.AuditActionProjectApply {
			t.Errorf("leaked non-matching action: %q", e.Action)
		}
	}
}

func TestListAuditEvents_FiltersByActor(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	alice := uuid.New()
	bob := uuid.New()
	for _, a := range []uuid.UUID{alice, alice, bob} {
		_, _ = s.EmitAuditEvent(ctx, store.AuditEmit{
			ActorID: a, Action: "x", TargetType: "t",
		})
	}

	page, err := s.ListAuditEvents(ctx, store.ListAuditEventsFilter{
		ActorID: alice, Limit: 10,
	})
	got := page.Events
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("alice's events = %d, want 2", len(got))
	}
	for _, e := range got {
		if e.ActorID == nil || *e.ActorID != alice {
			t.Errorf("leaked other actor: %+v", e.ActorID)
		}
	}
}

func TestListAuditEvents_Limit(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		_, _ = s.EmitAuditEvent(ctx, store.AuditEmit{Action: "x", TargetType: "t"})
	}
	page, err := s.ListAuditEvents(ctx, store.ListAuditEventsFilter{Limit: 3})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(page.Events) != 3 {
		t.Errorf("limit honored = %d, want 3", len(page.Events))
	}
	if page.Total != 5 {
		t.Errorf("total = %d, want 5", page.Total)
	}
}

func TestListAuditEvents_Offset(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	// Emit 5 events — later offset query pulls a window from the
	// middle and confirms the total still reflects all 5.
	for i := 0; i < 5; i++ {
		_, _ = s.EmitAuditEvent(ctx, store.AuditEmit{Action: "x", TargetType: "t"})
		time.Sleep(time.Millisecond)
	}
	page, err := s.ListAuditEvents(ctx, store.ListAuditEventsFilter{
		Limit: 2, Offset: 2,
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(page.Events) != 2 {
		t.Errorf("window size = %d, want 2", len(page.Events))
	}
	if page.Total != 5 {
		t.Errorf("total = %d, want 5 (offset/limit shouldn't narrow it)", page.Total)
	}
	if page.Offset != 2 {
		t.Errorf("offset echoed = %d, want 2", page.Offset)
	}
}
