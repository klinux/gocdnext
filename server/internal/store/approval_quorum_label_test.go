package store_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
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

func TestGetRunDetail_QuorumByLabel_SerialisesWhenOverrideFires(t *testing.T) {
	// API response: gate carrying an override exposes both
	// approval_required (effective) AND approval_quorum_label
	// (the disparadora) so the UI can render
	// "label: hotfix → quorum 1/2 default" without a second query.
	pool := dbtest.SetupPool(t)
	pipelineID, materialID := seedQuorumByLabelPipeline(t, pool, "quorum-detail-fire",
		2, map[string]int{"hotfix": 1})
	gateID := triggerPRRun(t, pool, pipelineID, materialID, []string{"hotfix"})

	s := store.New(pool)
	// fetch the run id from the gate row so we can call GetRunDetail.
	var runID uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`SELECT run_id FROM job_runs WHERE id = $1`, gateID).Scan(&runID); err != nil {
		t.Fatalf("look up run: %v", err)
	}
	detail, err := s.GetRunDetail(context.Background(), runID, 0, nil)
	if err != nil {
		t.Fatalf("GetRunDetail: %v", err)
	}

	var jd *store.JobDetail
	for i := range detail.Stages {
		for j := range detail.Stages[i].Jobs {
			if detail.Stages[i].Jobs[j].ID == gateID {
				jd = &detail.Stages[i].Jobs[j]
			}
		}
	}
	if jd == nil {
		t.Fatal("gate JobDetail missing from RunDetail")
	}
	if jd.ApprovalRequired != 1 {
		t.Errorf("approval_required = %d, want 1 (override)", jd.ApprovalRequired)
	}
	if jd.ApprovalQuorumLabel != "hotfix" {
		t.Errorf("approval_quorum_label = %q, want \"hotfix\"", jd.ApprovalQuorumLabel)
	}

	// JSON round-trip — confirm `approval_quorum_label` lands on
	// the wire when the override fired (handlers send JobDetail
	// directly).
	buf, err := json.Marshal(jd)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var roundtrip map[string]any
	if err := json.Unmarshal(buf, &roundtrip); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := roundtrip["approval_quorum_label"]; got != "hotfix" {
		t.Errorf("JSON approval_quorum_label = %v, want \"hotfix\"", got)
	}
	if got := roundtrip["approval_required"]; got != float64(1) {
		t.Errorf("JSON approval_required = %v, want 1", got)
	}
}

func TestGetRunDetail_QuorumByLabel_OmitsWhenNoOverride(t *testing.T) {
	// `omitempty` on ApprovalQuorumLabel keeps regular gates +
	// non-PR runs clean — operator shouldn't see a phantom
	// `"approval_quorum_label": ""` field on every gate that
	// wasn't a label-driven override. JSON should not carry the
	// key at all.
	pool := dbtest.SetupPool(t)
	pipelineID, materialID := seedQuorumByLabelPipeline(t, pool, "quorum-detail-quiet",
		2, map[string]int{"hotfix": 1})
	gateID := triggerPRRun(t, pool, pipelineID, materialID, []string{"backend"})

	s := store.New(pool)
	var runID uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`SELECT run_id FROM job_runs WHERE id = $1`, gateID).Scan(&runID); err != nil {
		t.Fatalf("look up run: %v", err)
	}
	detail, err := s.GetRunDetail(context.Background(), runID, 0, nil)
	if err != nil {
		t.Fatalf("GetRunDetail: %v", err)
	}
	var jd *store.JobDetail
	for i := range detail.Stages {
		for j := range detail.Stages[i].Jobs {
			if detail.Stages[i].Jobs[j].ID == gateID {
				jd = &detail.Stages[i].Jobs[j]
			}
		}
	}
	if jd == nil {
		t.Fatal("gate JobDetail missing")
	}
	if jd.ApprovalQuorumLabel != "" {
		t.Errorf("ApprovalQuorumLabel = %q, want \"\"", jd.ApprovalQuorumLabel)
	}

	buf, _ := json.Marshal(jd)
	var roundtrip map[string]any
	_ = json.Unmarshal(buf, &roundtrip)
	if _, present := roundtrip["approval_quorum_label"]; present {
		t.Errorf("approval_quorum_label key should be omitted when empty, JSON = %s", buf)
	}
	// Baseline required (2) should still be visible — operators
	// need it to render "1 of 2" even on the default-quorum case.
	if got := roundtrip["approval_required"]; got != float64(2) {
		t.Errorf("approval_required = %v, want 2 (baseline visible)", got)
	}
}

func TestCreateRun_QuorumByLabel_EmitsAuditEventOnOverride(t *testing.T) {
	// When an override fires at materialisation, an audit event
	// with action=approval.quorum_overridden lands AFTER the run
	// tx commits. Metadata carries the explanation that the UI
	// + admin audit log surface verbatim.
	pool := dbtest.SetupPool(t)
	pipelineID, materialID := seedQuorumByLabelPipeline(t, pool, "quorum-audit-fire",
		2, map[string]int{"hotfix": 1, "risky": 3})
	gateID := triggerPRRun(t, pool, pipelineID, materialID, []string{"hotfix", "risky"})

	var (
		count    int
		metaRaw  []byte
		targetID string
	)
	if err := pool.QueryRow(context.Background(), `
		SELECT COUNT(*), MAX(metadata::text), MAX(target_id)
		FROM audit_events
		WHERE action = $1 AND target_id = $2
	`, store.AuditActionApprovalQuorumOverride, gateID.String()).Scan(&count, &metaRaw, &targetID); err != nil {
		t.Fatalf("query audit: %v", err)
	}
	if count != 1 {
		t.Fatalf("audit rows = %d, want 1", count)
	}
	var meta map[string]any
	if err := json.Unmarshal(metaRaw, &meta); err != nil {
		t.Fatalf("decode metadata: %v", err)
	}
	if meta["label"] != "risky" {
		t.Errorf("audit label = %v, want \"risky\"", meta["label"])
	}
	if meta["base_required"] != float64(2) {
		t.Errorf("audit base_required = %v, want 2", meta["base_required"])
	}
	if meta["effective_required"] != float64(3) {
		t.Errorf("audit effective_required = %v, want 3 (max wins)", meta["effective_required"])
	}
	if meta["cause"] != "pull_request" {
		t.Errorf("audit cause = %v, want pull_request", meta["cause"])
	}
}

func TestCreateRun_QuorumByLabel_NoAuditEventWithoutOverride(t *testing.T) {
	// PR has labels but none match → no override fires → no audit
	// row. Quiet on the default path keeps the audit log signal-
	// to-noise ratio high for actual policy events.
	pool := dbtest.SetupPool(t)
	pipelineID, materialID := seedQuorumByLabelPipeline(t, pool, "quorum-audit-quiet",
		2, map[string]int{"hotfix": 1})
	gateID := triggerPRRun(t, pool, pipelineID, materialID, []string{"backend", "chore"})

	var count int
	if err := pool.QueryRow(context.Background(), `
		SELECT COUNT(*) FROM audit_events
		WHERE action = $1 AND target_id = $2
	`, store.AuditActionApprovalQuorumOverride, gateID.String()).Scan(&count); err != nil {
		t.Fatalf("query audit: %v", err)
	}
	if count != 0 {
		t.Errorf("audit rows = %d, want 0 (no override fired)", count)
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
