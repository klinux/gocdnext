package store_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

// seedRunForRef applies a git-backed pipeline and creates a run for `branch`,
// returning the stored runs.ref.
func seedRunForRef(t *testing.T, branch string) string {
	t.Helper()
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	url := "https://github.com/acme/refproj"
	fp := store.FingerprintFor(url, "main")
	applied, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: "refproj", Name: "refproj",
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
	run, err := s.CreateRunFromModification(ctx, store.CreateRunFromModificationInput{
		PipelineID: applied.Pipelines[0].PipelineID, MaterialID: materialID, ModificationID: 1,
		Revision: "abc", Branch: branch, Provider: "github", Delivery: "d", TriggeredBy: "system:test",
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	var ref string
	if err := pool.QueryRow(ctx, `SELECT ref FROM runs WHERE id = $1`, run.RunID).Scan(&ref); err != nil {
		t.Fatalf("read ref: %v", err)
	}
	return ref
}

func TestRunRefStampedFromBranch(t *testing.T) {
	if got := seedRunForRef(t, "feature-x"); got != "feature-x" {
		t.Fatalf("runs.ref = %q, want feature-x", got)
	}
}

func TestRunRefCappedAt255(t *testing.T) {
	long := strings.Repeat("a", 400)
	got := seedRunForRef(t, long)
	if len(got) != 255 {
		t.Fatalf("runs.ref length = %d, want 255 (capped)", len(got))
	}
	if got != long[:255] {
		t.Fatalf("runs.ref not the 255-char prefix of the branch")
	}
}
