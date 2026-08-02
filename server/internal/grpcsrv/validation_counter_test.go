package grpcsrv_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/testutil"

	gocdnextv1 "github.com/gocdnext/gocdnext/proto/gen/go/gocdnext/v1"
	"github.com/gocdnext/gocdnext/server/internal/artifacts"
	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/grpcsrv"
	"github.com/gocdnext/gocdnext/server/internal/metrics"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

// #207: gocdnext_job_result_validation_failed_total{kind} increments ONLY when a
// reported SUCCESS is downgraded by a server-side integrity check — never when the
// agent already reported failure (that is not a downgrade).
func TestHandleJobResult_ValidationCounterOnlyOnSuccessDowngrade(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	signer, err := artifacts.NewSigner([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	fs, err := artifacts.NewFilesystemStore(t.TempDir(), "http://unit-test", signer)
	if err != nil {
		t.Fatalf("fs store: %v", err)
	}
	sessions := grpcsrv.NewSessionStore()
	svc := grpcsrv.NewAgentService(s, sessions,
		slog.New(slog.NewTextHandler(io.Discard, nil)), 30).
		WithArtifactStore(fs, 5*time.Minute, 5*time.Minute, 24*time.Hour)

	// Two independent running jobs (same stage) so we can exercise both branches.
	fp := store.FingerprintFor("https://github.com/org/cnt", "main")
	applied, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: "proj-cnt", Name: "CounterTest",
		Pipelines: []*domain.Pipeline{{
			Name: "p1", Stages: []string{"build"},
			Materials: []domain.Material{{
				Type: domain.MaterialGit, Fingerprint: fp, AutoUpdate: true,
				Git: &domain.GitMaterial{URL: "https://github.com/org/cnt", Branch: "main", Events: []string{"push"}},
			}},
			Jobs: []domain.Job{
				{Name: "a", Stage: "build", Image: "alpine", Tasks: []domain.Task{{Script: "true"}}},
				{Name: "b", Stage: "build", Image: "alpine", Tasks: []domain.Task{{Script: "true"}}},
			},
		}},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	var materialID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM materials WHERE fingerprint=$1`, fp).Scan(&materialID); err != nil {
		t.Fatalf("material: %v", err)
	}
	res, err := s.CreateRunFromModification(ctx, store.CreateRunFromModificationInput{
		PipelineID: applied.Pipelines[0].PipelineID, MaterialID: materialID, ModificationID: 1,
		Revision: "deadbeef", Branch: "main", Provider: "github", Delivery: "t", TriggeredBy: "system:test",
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	var jobA, jobB uuid.UUID
	for _, j := range res.JobRuns {
		switch j.Name {
		case "a":
			jobA = j.ID
		case "b":
			jobB = j.ID
		}
	}

	var agentID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO agents (name, token_hash) VALUES ('cnt-agent','h') RETURNING id`).Scan(&agentID); err != nil {
		t.Fatalf("agent: %v", err)
	}
	sess := sessions.CreateSession(agentID, nil, 2, 0)
	for _, j := range []uuid.UUID{jobA, jobB} {
		if _, err := pool.Exec(ctx,
			`UPDATE job_runs SET status='running', agent_id=$1, started_at=NOW() WHERE id=$2`, agentID, j); err != nil {
			t.Fatalf("flip running: %v", err)
		}
		var attempt int32
		if err := pool.QueryRow(ctx, `SELECT attempt FROM job_runs WHERE id=$1`, j).Scan(&attempt); err != nil {
			t.Fatalf("attempt: %v", err)
		}
		sess.RecordAssignment(j, attempt)
	}

	missing := []*gocdnextv1.ArtifactRef{{Path: "dist", StorageKey: "does-not-exist", Size: 1}}
	counter := metrics.JobResultValidationFailed.WithLabelValues("artifacts")

	// (1) reported SUCCESS + a missing artifact → REAL downgrade → counter +1.
	before := testutil.ToFloat64(counter)
	svc.HandleJobResultForTest(ctx, sess, &gocdnextv1.JobResult{
		JobId: jobA.String(), Status: gocdnextv1.RunStatus_RUN_STATUS_SUCCESS, Artifacts: missing,
	})
	if got := testutil.ToFloat64(counter) - before; got != 1 {
		t.Fatalf("counter delta after success-downgrade = %v, want 1", got)
	}
	var stA string
	_ = pool.QueryRow(ctx, `SELECT status FROM job_runs WHERE id=$1`, jobA).Scan(&stA)
	if stA != "failed" {
		t.Errorf("jobA status = %q, want failed (downgraded)", stA)
	}

	// (2) reported FAILED + a missing artifact → NOT a downgrade → counter unchanged.
	mid := testutil.ToFloat64(counter)
	svc.HandleJobResultForTest(ctx, sess, &gocdnextv1.JobResult{
		JobId: jobB.String(), Status: gocdnextv1.RunStatus_RUN_STATUS_FAILED, Artifacts: missing,
	})
	if got := testutil.ToFloat64(counter) - mid; got != 0 {
		t.Fatalf("counter delta after reported-failure = %v, want 0 (no downgrade)", got)
	}
}
