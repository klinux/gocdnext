package grpcsrv

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/artifacts"
	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

const sarifCritical = `{"runs":[{"tool":{"driver":{"name":"Trivy"}},"results":[
  {"ruleId":"CVE-X","level":"error","message":{"text":"vuln in openssl"},
   "locations":[{"physicalLocation":{"artifactLocation":{"uri":"go.sum"},"region":{"startLine":1}}}],
   "properties":{"security-severity":"9.5"}}]}]}`

// TestReconcileSecurityFindings exercises the ingestion reconcile: a ready SARIF
// artifact is parsed + stored; a malformed SARIF on a rerun keeps prior
// findings; removing the SARIF on a rerun clears them.
func TestReconcileSecurityFindings(t *testing.T) {
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
	svc := NewAgentService(s, NewSessionStore(), slog.New(slog.NewTextHandler(io.Discard, nil)), 30).
		WithArtifactStore(fs, 5*time.Minute, 5*time.Minute, 24*time.Hour)

	// Seed project → pipeline → run → job_run via the public store API.
	fp := store.FingerprintFor("https://github.com/org/sec", "main")
	applied, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: "proj-sec", Name: "SecTest",
		Pipelines: []*domain.Pipeline{{
			Name:   "p1",
			Stages: []string{"build"},
			Materials: []domain.Material{{
				Type: domain.MaterialGit, Fingerprint: fp, AutoUpdate: true,
				Git: &domain.GitMaterial{URL: "https://github.com/org/sec", Branch: "main", Events: []string{"push"}},
			}},
			Jobs: []domain.Job{{Name: "scan", Stage: "build", Image: "alpine", Tasks: []domain.Task{{Script: "true"}}}},
		}},
	})
	if err != nil {
		t.Fatalf("apply project: %v", err)
	}
	projectID := applied.ProjectID
	pipelineID := applied.Pipelines[0].PipelineID
	var materialID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM materials WHERE fingerprint = $1`, fp).Scan(&materialID); err != nil {
		t.Fatalf("material lookup: %v", err)
	}
	run, err := s.CreateRunFromModification(ctx, store.CreateRunFromModificationInput{
		PipelineID: pipelineID, MaterialID: materialID, ModificationID: 1,
		Revision: "abc123", Branch: "main", Provider: "github", Delivery: "t", TriggeredBy: "system:test",
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	jobID := run.JobRuns[0].ID

	// A ready SARIF artifact for the job.
	key := "sec/" + jobID.String() + "/findings.sarif"
	putBlob(t, fs, ctx, key, sarifCritical)
	seedReadyArtifact(t, s, ctx, run.RunID, jobID, pipelineID, projectID, "findings.sarif", key)

	// 1) Parse + store.
	svc.reconcileSecurityFindings(jobID, 0)
	page, err := s.FindingsForProject(ctx, projectID, store.FindingsFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if page.Total != 1 || page.Findings[0].Severity != "critical" || page.Findings[0].Tool != "Trivy" {
		t.Fatalf("after ingest: %+v", page.Findings)
	}

	// 2) Malformed SARIF on a rerun → keep prior findings (don't clear on error).
	putBlob(t, fs, ctx, key, "}{ not json")
	svc.reconcileSecurityFindings(jobID, 1)
	page, _ = s.FindingsForProject(ctx, projectID, store.FindingsFilter{})
	if page.Total != 1 {
		t.Fatalf("malformed SARIF must KEEP prior findings, got total=%d", page.Total)
	}

	// 3) No SARIF on a rerun → clear.
	if err := s.RetireArtifactsByJobRun(ctx, jobID); err != nil {
		t.Fatalf("retire: %v", err)
	}
	svc.reconcileSecurityFindings(jobID, 1)
	page, _ = s.FindingsForProject(ctx, projectID, store.FindingsFilter{})
	if page.Total != 0 {
		t.Fatalf("dropped SARIF on rerun must clear, got total=%d", page.Total)
	}
}

func putBlob(t *testing.T, fs *artifacts.FilesystemStore, ctx context.Context, key, body string) {
	t.Helper()
	if _, err := fs.Put(ctx, key, strings.NewReader(body)); err != nil {
		t.Fatalf("put %s: %v", key, err)
	}
}

func seedReadyArtifact(t *testing.T, s *store.Store, ctx context.Context, runID, jobID, pipelineID, projectID uuid.UUID, path, key string) {
	t.Helper()
	if _, err := s.InsertPendingArtifact(ctx, store.InsertPendingArtifact{
		RunID: runID, JobRunID: jobID, PipelineID: pipelineID, ProjectID: projectID,
		Path: path, StorageKey: key,
	}); err != nil {
		t.Fatalf("insert artifact: %v", err)
	}
	if _, err := s.MarkArtifactReady(ctx, key, 100, ""); err != nil {
		t.Fatalf("mark ready: %v", err)
	}
}
