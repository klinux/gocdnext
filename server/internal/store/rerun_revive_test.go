package store_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// TestRerunJob_RevivesFailFastCanceledGate is the regression for the
// stuck-canceled approval gate: when a stage fails, the cascade
// fail-fast-cancels every downstream stage/job — including the
// awaiting approval gate (CancelQueuedJobsInRun). Re-running the
// failed upstream job must REVIVE that gate (back to
// awaiting_approval) and reopen its stage + the run. Without the
// revive, the rerun finalizes the run 'success' with the gate stuck
// 'canceled' and production silently skipped — exactly what happened
// to a live release whose deploy failed on a missing secret, was
// fixed, and rerun.
func TestRerunJob_RevivesFailFastCanceledGate(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	// seedApprovalPipeline: stages [test, deploy]; job `unit` in test,
	// approval gate `gate` in deploy. Failing `unit` fail-fast-cancels
	// the downstream gate — the minimal shape of the production bug.
	pipelineID, materialID := seedApprovalPipeline(t, pool, "rerun-revive", []string{"alice"})
	res, err := s.CreateRunFromModification(ctx, store.CreateRunFromModificationInput{
		PipelineID: pipelineID, MaterialID: materialID, ModificationID: 1,
		Revision: "deadbeef", Branch: "main", Provider: "github",
		Delivery: "t", TriggeredBy: "test",
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	var unitJobID, gateJobID uuid.UUID
	for _, jr := range res.JobRuns {
		switch jr.Name {
		case "unit":
			unitJobID = jr.ID
		case "gate":
			gateJobID = jr.ID
		}
	}
	if unitJobID == uuid.Nil || gateJobID == uuid.Nil {
		t.Fatalf("unit (%v) or gate (%v) missing from RunCreated", unitJobID, gateJobID)
	}

	// Fail the upstream at dispatch (mirrors failJobWithError on a
	// missing secret). This fail-fast-cancels the downstream gate.
	if _, ok, err := s.FailJobWithReason(ctx, unitJobID, "secrets: not set"); err != nil || !ok {
		t.Fatalf("FailJobWithReason: ok=%v err=%v", ok, err)
	}
	// Precondition: the cascade canceled the gate — this is the state
	// production was stuck in. If this assert fails the test is no
	// longer exercising the bug.
	if got := scalarStr(t, pool, `SELECT status FROM job_runs WHERE id=$1`, gateJobID); got != "canceled" {
		t.Fatalf("precondition: gate status = %q, want canceled (fail-fast)", got)
	}

	// Re-run the failed upstream job.
	if _, err := s.RerunJob(ctx, store.RerunJobInput{JobRunID: unitJobID, TriggeredBy: "user:test"}); err != nil {
		t.Fatalf("RerunJob: %v", err)
	}

	// Gate revived to awaiting_approval (NOT queued — the dispatch query
	// only sees queued rows and would try to run a task-less gate), with
	// awaiting_since re-stamped and the declared approvers intact.
	var (
		status    string
		gate      bool
		approvers []string
		awaiting  *string // TIMESTAMPTZ as text; nil = unset
	)
	if err := pool.QueryRow(ctx, `
		SELECT status, approval_gate, approvers, awaiting_since::text
		FROM job_runs WHERE id = $1`, gateJobID,
	).Scan(&status, &gate, &approvers, &awaiting); err != nil {
		t.Fatalf("query gate: %v", err)
	}
	if status != "awaiting_approval" {
		t.Errorf("gate status = %q, want awaiting_approval (revived)", status)
	}
	if !gate {
		t.Error("approval_gate flag lost on revive")
	}
	if len(approvers) != 1 || approvers[0] != "alice" {
		t.Errorf("approvers = %+v, want [alice] (preserved across revive)", approvers)
	}
	if awaiting == nil {
		t.Error("awaiting_since not re-stamped on revive")
	}

	// The gate's stage + the run reopened so the gate isn't a ghost in
	// a terminal 'success' run.
	if got := scalarStr(t, pool, `SELECT status FROM runs WHERE id=$1`, res.RunID); got != "running" {
		t.Errorf("run status = %q, want running", got)
	}
	stageStatus := scalarStr(t, pool, `
		SELECT s.status FROM stage_runs s
		JOIN job_runs j ON j.stage_run_id = s.id
		WHERE j.id = $1`, gateJobID)
	if stageStatus != "queued" && stageStatus != "running" {
		t.Errorf("gate stage status = %q, want queued/running", stageStatus)
	}
}

// TestRerunRun_PreservesTagCauseForCIVars is the regression for the
// vanishing CI_TAG_NAME: a tag-triggered release run rerun via RerunRun
// used to be demoted to cause="manual" with no tag_name, so
// addTagVars emitted nothing and a `deploy.version: ${CI_TAG_NAME}`
// (or any ${CI_*} shell ref) failed to resolve at dispatch ("CI var
// not present this run"). The rerun must carry the original cause +
// tag_name forward — with rerun_of stamped and fresh bookkeeping.
func TestRerunRun_PreservesTagCauseForCIVars(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	pipelineID, materialID, _ := seedPipeline(t, pool, false)
	mod, err := s.InsertModification(ctx, store.Modification{
		MaterialID:  materialID,
		Revision:    "d8f8c1eab2a2c0a4e6c4b5e8a1d0e9f7b6c3d2e1",
		Branch:      "v1.2.3",
		Author:      "tagger",
		Message:     "Release v1.2.3",
		Payload:     json.RawMessage(`{"ref":"refs/tags/v1.2.3"}`),
		CommittedAt: time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("InsertModification: %v", err)
	}

	// Original run: cause=tag with tag_name in cause_detail — exactly
	// what the tag-push webhook stamps.
	tagDetail, _ := json.Marshal(map[string]any{
		"tag_name": "v1.2.3", "tagger": "Kleber", "tag_message": "Release v1.2.3",
	})
	orig, err := s.CreateRunFromModification(ctx, store.CreateRunFromModificationInput{
		PipelineID: pipelineID, MaterialID: materialID, ModificationID: mod.ID,
		Revision: "d8f8c1eab2a2c0a4e6c4b5e8a1d0e9f7b6c3d2e1", Branch: "v1.2.3",
		Provider: "github", Delivery: "orig-delivery", TriggeredBy: "system:webhook",
		Cause: "tag", CauseDetail: tagDetail,
	})
	if err != nil {
		t.Fatalf("create original tag run: %v", err)
	}

	rerun, err := s.RerunRun(ctx, store.RerunRunInput{RunID: orig.RunID, TriggeredBy: "user:test"})
	if err != nil {
		t.Fatalf("RerunRun: %v", err)
	}

	var cause string
	var detailRaw []byte
	if err := pool.QueryRow(ctx,
		`SELECT cause, cause_detail FROM runs WHERE id=$1`, rerun.RunID,
	).Scan(&cause, &detailRaw); err != nil {
		t.Fatalf("query rerun: %v", err)
	}
	if cause != "tag" {
		t.Errorf("rerun cause = %q, want tag (preserved so CI_CAUSE/CI_TAG_* resolve)", cause)
	}
	var detail map[string]any
	if err := json.Unmarshal(detailRaw, &detail); err != nil {
		t.Fatalf("decode cause_detail: %v", err)
	}
	if detail["tag_name"] != "v1.2.3" {
		t.Errorf("rerun tag_name = %v, want v1.2.3 (preserved → CI_TAG_NAME resolves)", detail["tag_name"])
	}
	if detail["rerun_of"] != orig.RunID.String() {
		t.Errorf("rerun_of = %v, want %s", detail["rerun_of"], orig.RunID)
	}
	// Bookkeeping is the rerun's own, not the original run's — the
	// stripped keys are refilled by CreateRunFromModification's base.
	if detail["delivery"] != "rerun-"+orig.RunID.String() {
		t.Errorf("delivery = %v, want rerun-<orig> (fresh bookkeeping, not original)", detail["delivery"])
	}
}

// scalarStr runs a single-column, single-row query and returns the
// string result — small read helper for the asserts above.
func scalarStr(t *testing.T, pool *pgxpool.Pool, query string, args ...any) string {
	t.Helper()
	var out string
	if err := pool.QueryRow(context.Background(), query, args...).Scan(&out); err != nil {
		t.Fatalf("scalarStr %q: %v", query, err)
	}
	return out
}
