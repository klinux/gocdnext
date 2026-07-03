package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

// newStage0GateFixture applies a GATE-FIRST pipeline
//
//	approve-staging(gate) → deploy-staging(staging) → approve-prod(gate) → deploy-prod(prod)
//
// so the staging gate sits at stage 0 and is READY the instant a run is created —
// the exact shape the creation supersede fire targets. `mode` is the supersede
// setting (off | branch | pipeline).
func newStage0GateFixture(t *testing.T, slug, mode string) gateFixture {
	t.Helper()
	pool := dbtest.SetupPool(t)
	s := New(pool)
	ctx := context.Background()
	url := "https://github.com/acme/" + slug
	fp := domain.GitFingerprint(url, "main")
	def := domain.Pipeline{
		Name:      "p1",
		Supersede: mode,
		Stages:    []string{"approve-staging", "deploy-staging", "approve-prod", "deploy-prod"},
		Materials: []domain.Material{{
			Type: domain.MaterialGit, Fingerprint: fp, AutoUpdate: true,
			Git: &domain.GitMaterial{URL: url, Branch: "main", Events: []string{"push"}},
		}},
		Jobs: []domain.Job{
			{Name: "approve-staging", Stage: "approve-staging", Approval: &domain.ApprovalSpec{Required: 1}},
			{Name: "dep-staging", Stage: "deploy-staging", Image: "alpine",
				Tasks: []domain.Task{{Script: "true"}}, Deploy: &domain.DeploySpec{Environment: "staging"}},
			{Name: "approve-prod", Stage: "approve-prod", Approval: &domain.ApprovalSpec{Required: 1}},
			{Name: "dep-prod", Stage: "deploy-prod", Image: "alpine",
				Tasks: []domain.Task{{Script: "true"}}, Deploy: &domain.DeploySpec{Environment: "prod"}},
		},
	}
	applied, err := s.ApplyProject(ctx, ApplyProjectInput{Slug: slug, Name: slug, Pipelines: []*domain.Pipeline{&def}})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	var materialID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM materials WHERE fingerprint = $1`, fp).Scan(&materialID); err != nil {
		t.Fatalf("material: %v", err)
	}
	return gateFixture{s, pool, ctx, applied.Pipelines[0].PipelineID, materialID, def}
}

// Creating a newer run in a lane clears the older run pending at the same ready
// stage-0 gate, in the SAME create tx — the caller gets the victims on RunCreated.
func TestCreationFire_SupersedesOlderStage0Gate(t *testing.T) {
	f := newStage0GateFixture(t, "firebranch", domain.SupersedeBranch)
	older := f.createRun(t, "main")
	newer := f.createRun(t, "main")

	if len(newer.Superseded) != 1 || newer.Superseded[0].RunID != older.RunID {
		t.Fatalf("newer.Superseded = %+v, want [older #%d]", newer.Superseded, older.Counter)
	}
	if st := f.stateOf(t, older.RunID); st.status != "canceled" || st.supersededBy == nil || *st.supersededBy != newer.RunID {
		t.Fatalf("older run not superseded: %+v", st)
	}
	if st := f.stateOf(t, newer.RunID); st.status != "queued" {
		t.Fatalf("newer run status = %q, want queued", st.status)
	}

	// Audit: a system-actor run.superseded targeting the older run, counters only.
	var action string
	var actorID *uuid.UUID
	var metaRaw []byte
	if err := f.pool.QueryRow(f.ctx,
		`SELECT action, actor_id, metadata FROM audit_events WHERE target_id=$1 AND action='run.superseded'`,
		older.RunID.String()).Scan(&action, &actorID, &metaRaw); err != nil {
		t.Fatalf("read audit: %v", err)
	}
	if actorID != nil {
		t.Fatalf("run.superseded actor_id = %v, want NULL (system)", actorID)
	}
	var meta map[string]any
	if err := json.Unmarshal(metaRaw, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if meta["by_counter"] == nil || meta["superseded_counter"] == nil {
		t.Fatalf("audit metadata missing counters: %v", meta)
	}
	if s, _ := json.Marshal(meta); strings.Contains(string(s), "main") {
		t.Fatalf("audit metadata leaks the ref: %s", s)
	}
}

// supersede: off is a no-op at creation — the older run stays pending.
func TestCreationFire_OffIsNoop(t *testing.T) {
	f := newStage0GateFixture(t, "fireoff", domain.SupersedeOff)
	older := f.createRun(t, "main")
	newer := f.createRun(t, "main")

	if len(newer.Superseded) != 0 {
		t.Fatalf("supersede off still canceled %d victims", len(newer.Superseded))
	}
	if st := f.stateOf(t, older.RunID); st.status != "queued" || st.supersededBy != nil {
		t.Fatalf("older run touched with supersede off: %+v", st)
	}
}

// Different branch = different lane: a run on feature-x must not supersede one on main.
func TestCreationFire_DifferentLaneUntouched(t *testing.T) {
	f := newStage0GateFixture(t, "firelane", domain.SupersedeBranch)
	onMain := f.createRun(t, "main")
	onFeature := f.createRun(t, "feature-x")

	if len(onFeature.Superseded) != 0 {
		t.Fatalf("feature-x run superseded %d main runs — lanes not isolated", len(onFeature.Superseded))
	}
	if st := f.stateOf(t, onMain.RunID); st.status != "queued" || st.supersededBy != nil {
		t.Fatalf("main run canceled by a different-branch run: %+v", st)
	}
}

// A newer run whose ready stage-0 gate governs STAGING must not cancel an older run
// that already passed staging and is pending only PROD.
func TestCreationFire_StagingDoesNotCancelProdPending(t *testing.T) {
	f := newStage0GateFixture(t, "fireenv", domain.SupersedeBranch)
	older := f.createRun(t, "main")
	f.approveGate(t, older.RunID, "approve-staging") // advance older past staging → pending only prod

	newer := f.createRun(t, "main")
	if len(newer.Superseded) != 0 {
		t.Fatalf("staging-ready newer canceled a prod-pending older: %+v", newer.Superseded)
	}
	if st := f.stateOf(t, older.RunID); st.status != "queued" || st.supersededBy != nil {
		t.Fatalf("prod-pending older was superseded by a staging run: %+v", st)
	}
}
