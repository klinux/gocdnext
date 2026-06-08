package store_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/domain"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// seedQuorumByLabelPipeline applies a pipeline with an approval gate
// carrying both ApproverGroups (so the listSize check accepts
// generous quorums) and the given QuorumByLabel map.
func seedQuorumByLabelPipeline(
	t *testing.T,
	pool *pgxpool.Pool,
	slug string,
	required int,
	quorumByLabel map[string]int,
) (pipelineID, materialID uuid.UUID) {
	t.Helper()
	s := store.New(pool)
	url, branch := "https://github.com/org/"+slug, "main"
	fp := store.FingerprintFor(url, branch)
	p := &domain.Pipeline{
		Name:   "build",
		Stages: []string{"deploy"},
		Materials: []domain.Material{{
			Type: domain.MaterialGit, Fingerprint: fp, AutoUpdate: true,
			Git: &domain.GitMaterial{URL: url, Branch: branch, Events: []string{"push", "pull_request"}},
		}},
		Jobs: []domain.Job{{
			Name:  "gate",
			Stage: "deploy",
			Approval: &domain.ApprovalSpec{
				ApproverGroups: []string{"sre", "platform", "security"},
				Required:       required,
				QuorumByLabel:  quorumByLabel,
				Description:    "ship?",
			},
		}},
	}
	ctx := context.Background()
	res, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: slug, Name: slug, Pipelines: []*domain.Pipeline{p},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	pipelineID = res.Pipelines[0].PipelineID
	if err := pool.QueryRow(ctx, `SELECT id FROM materials WHERE fingerprint = $1`, fp).Scan(&materialID); err != nil {
		t.Fatalf("material lookup: %v", err)
	}
	return
}

// triggerPRRun creates a pull_request-cause run whose cause_detail
// carries the given pr_labels. Returns the gate's job_run_id so the
// test can inspect approval_required + approval_quorum_label.
func triggerPRRun(t *testing.T, pool *pgxpool.Pool, pipelineID, materialID uuid.UUID, prLabels []string) uuid.UUID {
	t.Helper()
	s := store.New(pool)
	detail, _ := json.Marshal(map[string]any{
		"pr_number": 42,
		"pr_labels": prLabels,
	})
	res, err := s.CreateRunFromModification(context.Background(), store.CreateRunFromModificationInput{
		PipelineID:     pipelineID,
		MaterialID:     materialID,
		ModificationID: 1,
		Revision:       "deadbeef",
		Branch:         "main",
		Provider:       "github",
		Delivery:       "t",
		TriggeredBy:    "system:webhook",
		Cause:          "pull_request",
		CauseDetail:    detail,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	for _, jr := range res.JobRuns {
		if jr.Name == "gate" {
			return jr.ID
		}
	}
	t.Fatal("gate job_run missing")
	return uuid.Nil
}

func readGateQuorum(t *testing.T, pool *pgxpool.Pool, gateID uuid.UUID) (required int, label *string) {
	t.Helper()
	err := pool.QueryRow(context.Background(), `
		SELECT approval_required, approval_quorum_label
		FROM job_runs WHERE id = $1
	`, gateID).Scan(&required, &label)
	if err != nil {
		t.Fatalf("query gate: %v", err)
	}
	return
}

func TestCreateRun_QuorumByLabel_SingleMatch(t *testing.T) {
	// PR label `hotfix` matches a key in quorum_by_label → the
	// gate is materialised with the override quorum (1) and the
	// label disparadora persisted in approval_quorum_label.
	pool := dbtest.SetupPool(t)
	pipelineID, materialID := seedQuorumByLabelPipeline(t, pool, "quorum-single",
		2, map[string]int{"hotfix": 1, "risky": 3})

	gateID := triggerPRRun(t, pool, pipelineID, materialID, []string{"hotfix"})
	got, label := readGateQuorum(t, pool, gateID)
	if got != 1 {
		t.Errorf("approval_required = %d, want 1 (override fired)", got)
	}
	if label == nil || *label != "hotfix" {
		t.Errorf("approval_quorum_label = %v, want \"hotfix\"", label)
	}
}

func TestCreateRun_QuorumByLabel_MultipleLabelsMaxWins(t *testing.T) {
	// PR carries both `hotfix` (1) and `risky` (3). MAX wins so
	// the persisted quorum is 3 and the persisted label is "risky".
	pool := dbtest.SetupPool(t)
	pipelineID, materialID := seedQuorumByLabelPipeline(t, pool, "quorum-multi",
		2, map[string]int{"hotfix": 1, "risky": 3})

	gateID := triggerPRRun(t, pool, pipelineID, materialID, []string{"hotfix", "risky", "chore"})
	got, label := readGateQuorum(t, pool, gateID)
	if got != 3 {
		t.Errorf("approval_required = %d, want 3 (max wins)", got)
	}
	if label == nil || *label != "risky" {
		t.Errorf("approval_quorum_label = %v, want \"risky\"", label)
	}
}

func TestCreateRun_QuorumByLabel_NoMatchKeepsBaselineWithNullLabel(t *testing.T) {
	// PR has labels but NONE intersect quorum_by_label → baseline
	// Required (2) persists, and approval_quorum_label stays NULL
	// so the UI + audit can show "default quorum" without
	// inferring it from a missing override.
	pool := dbtest.SetupPool(t)
	pipelineID, materialID := seedQuorumByLabelPipeline(t, pool, "quorum-nomatch",
		2, map[string]int{"hotfix": 1, "risky": 3})

	gateID := triggerPRRun(t, pool, pipelineID, materialID, []string{"backend", "chore"})
	got, label := readGateQuorum(t, pool, gateID)
	if got != 2 {
		t.Errorf("approval_required = %d, want 2 (baseline)", got)
	}
	if label != nil {
		t.Errorf("approval_quorum_label = %q, want NULL (no override fired)", *label)
	}
}

func TestCreateRun_QuorumByLabel_NonPRCauseUsesBaseline(t *testing.T) {
	// Push to base branch creates a run with cause="webhook"
	// (the default in CreateRunFromModification). quorum_by_label
	// is PR-only state, so the resolver short-circuits to
	// baseline + NULL label.
	pool := dbtest.SetupPool(t)
	pipelineID, materialID := seedQuorumByLabelPipeline(t, pool, "quorum-push",
		2, map[string]int{"hotfix": 1, "risky": 3})

	s := store.New(pool)
	res, err := s.CreateRunFromModification(context.Background(), store.CreateRunFromModificationInput{
		PipelineID:     pipelineID,
		MaterialID:     materialID,
		ModificationID: 1,
		Revision:       "feedface",
		Branch:         "main",
		Provider:       "github",
		Delivery:       "p",
		TriggeredBy:    "system:webhook",
		// no Cause / CauseDetail → defaults to "webhook"
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	var gateID uuid.UUID
	for _, jr := range res.JobRuns {
		if jr.Name == "gate" {
			gateID = jr.ID
		}
	}
	got, label := readGateQuorum(t, pool, gateID)
	if got != 2 {
		t.Errorf("approval_required = %d, want 2 (non-PR cause)", got)
	}
	if label != nil {
		t.Errorf("approval_quorum_label = %q, want NULL", *label)
	}
}
