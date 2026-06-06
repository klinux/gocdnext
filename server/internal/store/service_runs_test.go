package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// seedRunForServices reuses the approval pipeline seeder to land a
// real `runs` row we can FK to. Service runs cascade-delete on the
// run, so plumbing through ApplyProject + CreateRunFromModification
// keeps the test honest about the FK direction.
func seedRunForServices(t *testing.T) (s *store.Store, runID uuid.UUID) {
	t.Helper()
	pool := dbtest.SetupPool(t)
	s = store.New(pool)
	pipelineID, materialID := seedApprovalPipeline(t, pool, "svc-pipe", []string{"a@example.com"})
	runID, _ = triggerApprovalRun(t, pool, pipelineID, materialID)
	return s, runID
}

func TestUpsertServiceRun_StartingThenReadyThenStopped(t *testing.T) {
	s, runID := seedRunForServices(t)
	ctx := context.Background()

	t0 := time.Date(2026, 6, 5, 10, 0, 0, 0, time.UTC)
	t1 := t0.Add(2 * time.Second)
	t2 := t0.Add(10 * time.Second)

	// starting → started_at stamped, ready_at/stopped_at still null
	row, err := s.UpsertServiceRun(ctx, store.ServiceRunInput{
		RunID:   runID,
		Name:    "postgres",
		Image:   "postgres:16",
		PodName: "gocdnext-svc-abc-postgres",
		Status:  "starting",
		At:      t0,
	})
	if err != nil {
		t.Fatalf("upsert starting: %v", err)
	}
	if row.Status != "starting" {
		t.Errorf("status: got %q", row.Status)
	}
	if !row.StartedAt.Valid || !row.StartedAt.Time.Equal(t0) {
		t.Errorf("started_at: got %+v want %v", row.StartedAt, t0)
	}
	if row.ReadyAt.Valid {
		t.Errorf("ready_at should be null on starting; got %v", row.ReadyAt.Time)
	}

	// ready → ready_at stamped, started_at preserved (COALESCE in SQL)
	row, err = s.UpsertServiceRun(ctx, store.ServiceRunInput{
		RunID:   runID,
		Name:    "postgres",
		Image:   "postgres:16",
		PodName: "gocdnext-svc-abc-postgres",
		Status:  "ready",
		At:      t1,
	})
	if err != nil {
		t.Fatalf("upsert ready: %v", err)
	}
	if row.Status != "ready" {
		t.Errorf("status: got %q", row.Status)
	}
	if !row.StartedAt.Time.Equal(t0) {
		t.Errorf("started_at must persist across upserts; got %v", row.StartedAt.Time)
	}
	if !row.ReadyAt.Valid || !row.ReadyAt.Time.Equal(t1) {
		t.Errorf("ready_at: got %+v want %v", row.ReadyAt, t1)
	}

	// stopped → stopped_at stamped, prior columns preserved
	row, err = s.UpsertServiceRun(ctx, store.ServiceRunInput{
		RunID:   runID,
		Name:    "postgres",
		Image:   "postgres:16",
		PodName: "gocdnext-svc-abc-postgres",
		Status:  "stopped",
		At:      t2,
	})
	if err != nil {
		t.Fatalf("upsert stopped: %v", err)
	}
	if row.Status != "stopped" {
		t.Errorf("status: got %q", row.Status)
	}
	if !row.StartedAt.Time.Equal(t0) || !row.ReadyAt.Time.Equal(t1) {
		t.Errorf("prior timestamps must persist; got start=%v ready=%v",
			row.StartedAt.Time, row.ReadyAt.Time)
	}
	if !row.StoppedAt.Valid || !row.StoppedAt.Time.Equal(t2) {
		t.Errorf("stopped_at: got %+v want %v", row.StoppedAt, t2)
	}
}

func TestUpsertServiceRun_IdempotentSameStatus(t *testing.T) {
	// A re-issued ready (after a stream reconnect on the agent)
	// must NOT reset the first-observed ready_at.
	s, runID := seedRunForServices(t)
	ctx := context.Background()

	first := time.Date(2026, 6, 5, 10, 0, 0, 0, time.UTC)
	second := first.Add(5 * time.Second)

	if _, err := s.UpsertServiceRun(ctx, store.ServiceRunInput{
		RunID:  runID,
		Name:   "redis",
		Image:  "redis:7",
		Status: "ready",
		At:     first,
	}); err != nil {
		t.Fatal(err)
	}
	row, err := s.UpsertServiceRun(ctx, store.ServiceRunInput{
		RunID:  runID,
		Name:   "redis",
		Image:  "redis:7",
		Status: "ready",
		At:     second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !row.ReadyAt.Time.Equal(first) {
		t.Errorf("idempotent ready must preserve first timestamp; got %v want %v",
			row.ReadyAt.Time, first)
	}
}

func TestUpsertServiceRun_FailedCarriesError(t *testing.T) {
	s, runID := seedRunForServices(t)
	ctx := context.Background()

	row, err := s.UpsertServiceRun(ctx, store.ServiceRunInput{
		RunID:  runID,
		Name:   "postgres",
		Image:  "postgres:16",
		Status: "failed",
		At:     time.Date(2026, 6, 5, 10, 0, 0, 0, time.UTC),
		Error:  "ImagePullBackOff: back-off pulling image",
	})
	if err != nil {
		t.Fatalf("upsert failed: %v", err)
	}
	if row.Status != "failed" {
		t.Errorf("status: got %q", row.Status)
	}
	if row.Error == "" {
		t.Errorf("error should round-trip; got empty")
	}
	if !row.StoppedAt.Valid {
		t.Errorf("failed must stamp stopped_at as the terminal marker")
	}
}

func TestListServiceRunsByRunID_AlphabeticalAndScoped(t *testing.T) {
	s, runID := seedRunForServices(t)
	ctx := context.Background()
	at := time.Date(2026, 6, 5, 10, 0, 0, 0, time.UTC)

	for _, name := range []string{"redis", "postgres", "kafka"} {
		if _, err := s.UpsertServiceRun(ctx, store.ServiceRunInput{
			RunID: runID, Name: name, Image: name + ":latest",
			Status: "ready", At: at,
		}); err != nil {
			t.Fatal(err)
		}
	}

	rows, err := s.ListServiceRunsByRunID(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(rows), 3; got != want {
		t.Fatalf("len: got %d want %d", got, want)
	}
	for i, name := range []string{"kafka", "postgres", "redis"} {
		if rows[i].Name != name {
			t.Errorf("alphabetical order broken at %d: got %q want %q",
				i, rows[i].Name, name)
		}
	}
}
