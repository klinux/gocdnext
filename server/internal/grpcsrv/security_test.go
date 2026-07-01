package grpcsrv

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
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

	// A ready SARIF artifact for the job — stored as a gzipped tar, exactly as
	// the agent's uploader (runner.TarGzPath) writes it. Seeding raw bytes here
	// (as this test used to) hid a prod bug: the ingestion fed the still-gzipped
	// object to the JSON decoder ('\x1f' magic) and every real scan failed to
	// ingest. This step now reproduces + locks the real storage format.
	key := "sec/" + jobID.String() + "/findings.sarif"
	putTarGzBlob(t, fs, ctx, key, map[string]string{"findings.sarif": sarifCritical})
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

	// A rerun bumps the job's attempt; the reconciler is fenced on it.
	if _, err := pool.Exec(ctx, `UPDATE job_runs SET attempt = 1 WHERE id = $1`, jobID); err != nil {
		t.Fatalf("bump attempt: %v", err)
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

// putTarGzBlob stores files as a gzipped tar, mirroring the agent uploader's
// runner.TarGzPath output — the real on-store shape of every artifact.
func putTarGzBlob(t *testing.T, fs *artifacts.FilesystemStore, ctx context.Context, key string, files map[string]string) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatalf("tar header %s: %v", name, err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatalf("tar write %s: %v", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gz close: %v", err)
	}
	if _, err := fs.Put(ctx, key, &buf); err != nil {
		t.Fatalf("put %s: %v", key, err)
	}
}

// TestForEachSarifStream locks the artifact-decoding helper against both real
// (gzipped-tar) and raw inputs — no DB needed.
func TestForEachSarifStream(t *testing.T) {
	collect := func(r io.Reader) ([]string, error) {
		var got []string
		err := forEachSarifStream(r, func(sr io.Reader) error {
			b, e := io.ReadAll(sr)
			if e != nil {
				return e
			}
			got = append(got, string(b))
			return nil
		})
		return got, err
	}

	tgz := func(files map[string]string) io.Reader {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		for name, body := range files {
			_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
			_, _ = tw.Write([]byte(body))
		}
		_ = tw.Close()
		_ = gz.Close()
		return &buf
	}

	t.Run("gzipped tar single sarif", func(t *testing.T) {
		got, err := collect(tgz(map[string]string{"findings.sarif": `{"runs":[]}`}))
		if err != nil || len(got) != 1 || got[0] != `{"runs":[]}` {
			t.Fatalf("got=%v err=%v", got, err)
		}
	})

	t.Run("gzipped tar skips non-sarif entries", func(t *testing.T) {
		got, err := collect(tgz(map[string]string{"a.sarif": "A", "readme.txt": "x", "b.sarif": "B"}))
		if err != nil || len(got) != 2 {
			t.Fatalf("want 2 sarif entries, got=%v err=%v", got, err)
		}
	})

	t.Run("gzipped tar with no sarif entry errors", func(t *testing.T) {
		if _, err := collect(tgz(map[string]string{"coverage.xml": "x"})); err == nil {
			t.Fatal("expected error when tarball carries no .sarif entry")
		}
	})

	t.Run("raw sarif (non-gzip) passes through", func(t *testing.T) {
		got, err := collect(strings.NewReader(`{"runs":[]}`))
		if err != nil || len(got) != 1 || got[0] != `{"runs":[]}` {
			t.Fatalf("raw: got=%v err=%v", got, err)
		}
	})
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
