package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

// seedGovernedMigrationPipeline applies a pipeline whose approval gate governs a
// NON-deploy migration (a job declaring `environment:` with no `deploy:` marker,
// #206). GovernedEnvs sees nothing here — only GovernedFreezeEnvs does — so this
// exercises the lockApprovalEnvs split + the no-early-return fix.
func seedGovernedMigrationPipeline(t *testing.T, pool *pgxpool.Pool, slug, env string) (gateJobID, projectID uuid.UUID) {
	t.Helper()
	s := store.New(pool)
	ctx := context.Background()
	url, branch := "https://github.com/org/"+slug, "main"
	fp := store.FingerprintFor(url, branch)

	res, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: slug, Name: slug,
		Pipelines: []*domain.Pipeline{{
			Name: "release", Supersede: domain.SupersedeOff, Stages: []string{"approve", "migration"},
			Materials: []domain.Material{{
				Type: domain.MaterialGit, Fingerprint: fp, AutoUpdate: true,
				Git: &domain.GitMaterial{URL: url, Branch: branch, Events: []string{"push"}},
			}},
			Jobs: []domain.Job{
				{Name: "gate", Stage: "approve", Approval: &domain.ApprovalSpec{Description: "Run the migration?"}},
				{
					Name: "migrate", Stage: "migration", Image: "goose",
					Tasks:       []domain.Task{{Script: "goose up"}},
					Environment: env,      // acts on the env, but is NOT a deploy
					Needs:       []string{"gate"}, // governed by the gate
				},
			},
		}},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	projectID = res.ProjectID
	var materialID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM materials WHERE fingerprint=$1`, fp).Scan(&materialID); err != nil {
		t.Fatalf("material: %v", err)
	}
	run, err := s.CreateRunFromModification(ctx, store.CreateRunFromModificationInput{
		PipelineID: res.Pipelines[0].PipelineID, MaterialID: materialID, ModificationID: 1,
		Revision: "deadbeef", Branch: branch, Provider: "github",
		Delivery: slug, TriggeredBy: "system:webhook",
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, j := range run.JobRuns {
		if j.Name == "gate" {
			gateJobID = j.ID
		}
	}
	if gateJobID == uuid.Nil {
		t.Fatal("no gate job in the seeded run")
	}
	return gateJobID, projectID
}

// A gate that governs ONLY a migration (no deploy) is un-approvable while the
// migration's environment is frozen — the whole point of #206 on the approval
// side. Without GovernedFreezeEnvs + the no-early-return fix, GovernedEnvs would
// be empty and the approval would slip through.
func TestApproveGate_RefusedWhileGovernedMigrationEnvIsFrozen(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	gateID, projectID := seedGovernedMigrationPipeline(t, pool, "appr-freeze-mig", "prod")
	if _, err := s.FreezeEnvironment(ctx, projectID, "prod", testActor(), "close"); err != nil {
		t.Fatalf("freeze: %v", err)
	}

	// One decision, reused: the refused (frozen) attempt records no vote, so the
	// same voter can approve after the thaw. Re-seeding a second voter would
	// collide on the derived user key.
	decision := approvalDecision(t, pool, gateID)
	if _, err := s.ApproveGate(ctx, decision); !errors.Is(err, store.ErrEnvironmentFrozen) {
		t.Fatalf("err = %v, want ErrEnvironmentFrozen (a migration-governing gate must be held)", err)
	}

	// Unfreeze and the same gate approves — proving the freeze, not the shape, is
	// what refused it.
	if _, err := s.UnfreezeEnvironment(ctx, projectID, "prod", testActor()); err != nil {
		t.Fatalf("unfreeze: %v", err)
	}
	if _, err := s.ApproveGate(ctx, decision); err != nil {
		t.Fatalf("approve after unfreeze: %v", err)
	}
}
