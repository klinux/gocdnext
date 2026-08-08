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

// A DISRUPTED result (task pod preempted) must be routed to the
// requeue-or-terminal engine BEFORE mapStatus / artifact / output
// validation. Two things prove the bypass at once:
//   - The job is REQUEUED (queued, attempt+1). The normal path can't do
//     that: mapStatus(DISRUPTED) returns "" → "unsupported status" → the
//     result is dropped and the job stays 'running'. A requeue is only
//     reachable via the early DISRUPTED branch.
//   - A bogus/missing artifact on the result never trips the integrity
//     downgrade (gocdnext_job_result_validation_failed_total stays flat) —
//     confirmArtifacts was never called.
func TestHandleJobResult_DisruptedBypassesValidationAndRequeues(t *testing.T) {
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

	fp := store.FingerprintFor("https://github.com/org/dis", "main")
	applied, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: "proj-dis", Name: "DisruptTest",
		Pipelines: []*domain.Pipeline{{
			Name: "p1", Stages: []string{"build"},
			Materials: []domain.Material{{
				Type: domain.MaterialGit, Fingerprint: fp, AutoUpdate: true,
				Git: &domain.GitMaterial{URL: "https://github.com/org/dis", Branch: "main", Events: []string{"push"}},
			}},
			Jobs: []domain.Job{
				{Name: "a", Stage: "build", Image: "alpine", Tasks: []domain.Task{{Script: "true"}}},
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
	jobA := res.JobRuns[0].ID

	var agentID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO agents (name, token_hash) VALUES ('dis-agent','h') RETURNING id`).Scan(&agentID); err != nil {
		t.Fatalf("agent: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE job_runs SET status='running', agent_id=$1, started_at=NOW() WHERE id=$2`, agentID, jobA); err != nil {
		t.Fatalf("flip running: %v", err)
	}
	sess := sessions.CreateSession(agentID, nil, 2, 0)
	sess.RecordAssignment(jobA, 0)

	valCounter := metrics.JobResultValidationFailed.WithLabelValues("artifacts")
	disruptedRequeued := metrics.JobsDisrupted.WithLabelValues("requeued")
	beforeVal := testutil.ToFloat64(valCounter)
	beforeDis := testutil.ToFloat64(disruptedRequeued)

	// DISRUPTED + a missing artifact: the artifact must be IGNORED (bypassed),
	// and the job requeued.
	svc.HandleJobResultForTest(ctx, sess, &gocdnextv1.JobResult{
		JobId:     jobA.String(),
		Status:    gocdnextv1.RunStatus_RUN_STATUS_DISRUPTED,
		ExitCode:  143,
		Error:     "job pod terminated externally — node preemption",
		Artifacts: []*gocdnextv1.ArtifactRef{{Path: "dist", StorageKey: "does-not-exist", Size: 1}},
	})

	var status string
	var attempt int32
	if err := pool.QueryRow(ctx, `SELECT status, attempt FROM job_runs WHERE id=$1`, jobA).Scan(&status, &attempt); err != nil {
		t.Fatalf("read job: %v", err)
	}
	if status != "queued" || attempt != 1 {
		t.Fatalf("status=%q attempt=%d, want queued/1 (only the early DISRUPTED branch requeues; the normal path rejects DISRUPTED at mapStatus)", status, attempt)
	}
	if got := testutil.ToFloat64(valCounter) - beforeVal; got != 0 {
		t.Errorf("artifact validation counter moved by %v; DISRUPTED must bypass confirmArtifacts", got)
	}
	if got := testutil.ToFloat64(disruptedRequeued) - beforeDis; got != 1 {
		t.Errorf("jobs_disrupted{requeued} delta = %v, want 1", got)
	}

	// The session's assignment + running counter were freed (care: cleared
	// before the requeue notify).
	if _, ok := sess.LookupAssignment(jobA); ok {
		t.Errorf("assignment still present; disrupted requeue must ClearAssignment")
	}
}
