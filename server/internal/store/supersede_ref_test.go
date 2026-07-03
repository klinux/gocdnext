package store_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

// refFixture applies a one-git-material pipeline and returns the pieces the ref
// tests need. slug distinguishes the repo/lane per test.
type refFixture struct {
	s          *store.Store
	pool       *pgxpool.Pool
	ctx        context.Context
	pipelineID uuid.UUID
	materialID uuid.UUID
	url        string
}

func newRefFixture(t *testing.T, slug string) refFixture {
	t.Helper()
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()
	url := "https://github.com/acme/" + slug
	fp := store.FingerprintFor(url, "main")
	applied, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: slug, Name: slug,
		Pipelines: []*domain.Pipeline{{
			Name: "p1", Stages: []string{"build"},
			Materials: []domain.Material{{
				Type: domain.MaterialGit, Fingerprint: fp, AutoUpdate: true,
				Git: &domain.GitMaterial{URL: url, Branch: "main", Events: []string{"push"}},
			}},
			Jobs: []domain.Job{{Name: "compile", Stage: "build", Image: "alpine", Tasks: []domain.Task{{Script: "true"}}}},
		}},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	var materialID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM materials WHERE fingerprint = $1`, fp).Scan(&materialID); err != nil {
		t.Fatalf("material: %v", err)
	}
	return refFixture{s, pool, ctx, applied.Pipelines[0].PipelineID, materialID, url}
}

func (f refFixture) refOf(t *testing.T, runID uuid.UUID) string {
	t.Helper()
	var ref string
	if err := f.pool.QueryRow(f.ctx, `SELECT ref FROM runs WHERE id = $1`, runID).Scan(&ref); err != nil {
		t.Fatalf("read ref: %v", err)
	}
	return ref
}

func (f refFixture) createFromBranch(t *testing.T, branch string) store.RunCreated {
	t.Helper()
	run, err := f.s.CreateRunFromModification(f.ctx, store.CreateRunFromModificationInput{
		PipelineID: f.pipelineID, MaterialID: f.materialID, ModificationID: 1,
		Revision: "abc", Branch: branch, Provider: "github", Delivery: "d", TriggeredBy: "system:test",
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	return run
}

func TestRunRefStampedFromBranch(t *testing.T) {
	f := newRefFixture(t, "refbranch")
	if got := f.refOf(t, f.createFromBranch(t, "feature-x").RunID); got != "feature-x" {
		t.Fatalf("runs.ref = %q, want feature-x", got)
	}
}

func TestRunRefCappedUTF8Safe(t *testing.T) {
	f := newRefFixture(t, "refcap")
	// 254 ASCII + a 2-byte rune → 256 bytes; a naive ref[:255] would split the
	// rune and produce invalid UTF-8 (Postgres would reject the insert).
	got := f.refOf(t, f.createFromBranch(t, strings.Repeat("a", 254)+"é").RunID)
	if !utf8.ValidString(got) {
		t.Fatalf("capped ref is not valid UTF-8: %q", got)
	}
	if len(got) > 255 {
		t.Fatalf("capped ref length = %d bytes, want <= 255", len(got))
	}
}

func TestRunRefPRLaneByNumber(t *testing.T) {
	f := newRefFixture(t, "refpr")
	// Two PRs sharing the head-ref "fix/foo" must land in DIFFERENT lanes.
	prDetail := func(n int) json.RawMessage {
		b, _ := json.Marshal(map[string]any{"pr_number": n})
		return b
	}
	run5, err := f.s.CreateRunFromModification(f.ctx, store.CreateRunFromModificationInput{
		PipelineID: f.pipelineID, MaterialID: f.materialID, ModificationID: 1,
		Revision: "a", Branch: "fix/foo", Provider: "github", Delivery: "d1",
		TriggeredBy: "sys", Cause: string(domain.CausePullRequest), CauseDetail: prDetail(5),
	})
	if err != nil {
		t.Fatalf("create PR run: %v", err)
	}
	if got := f.refOf(t, run5.RunID); got != "pr:5" {
		t.Fatalf("PR run ref = %q, want pr:5 (not the shared head-ref)", got)
	}
}

func TestGetRunForDispatchReturnsRef(t *testing.T) {
	f := newRefFixture(t, "refdispatch")
	run := f.createFromBranch(t, "release-1")
	rfd, err := f.s.GetRunForDispatch(f.ctx, run.RunID)
	if err != nil {
		t.Fatalf("GetRunForDispatch: %v", err)
	}
	if rfd.Ref != "release-1" {
		t.Fatalf("RunForDispatch.Ref = %q, want release-1", rfd.Ref)
	}
}

func TestCreateRunFromUpstreamPropagatesRef(t *testing.T) {
	f := newRefFixture(t, "refupstream")
	// Fix for fanout.go's hardcoded "branch": "" — the downstream run must
	// inherit the upstream lane so prod deploys from `main` share one lane.
	down, created, err := f.s.CreateRunFromUpstream(f.ctx, store.CreateRunFromUpstreamInput{
		DownstreamPipelineID: f.pipelineID,
		DownstreamMaterialID: f.materialID,
		UpstreamRunID:        uuid.New(),
		UpstreamRunCounter:   1,
		UpstreamPipelineName: "up",
		UpstreamStageName:    "deploy",
		UpstreamRef:          "main",
	})
	if err != nil || !created {
		t.Fatalf("CreateRunFromUpstream: created=%v err=%v", created, err)
	}
	if got := f.refOf(t, down.RunID); got != "main" {
		t.Fatalf("downstream ref = %q, want main (propagated from upstream)", got)
	}
}
