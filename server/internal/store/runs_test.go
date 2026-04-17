package store_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/domain"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// seedPipeline runs ApplyProject for a simple 2-stage pipeline and returns the
// project/pipeline/material UUIDs plus the matching fingerprint — everything
// the run-creation tests need to feed CreateRunFromModification.
func seedPipeline(t *testing.T, pool *pgxpool.Pool, withMatrix bool) (pipelineID, materialID uuid.UUID, fp string) {
	t.Helper()
	s := store.New(pool)

	url, branch := "https://github.com/org/demo", "main"
	fp = store.FingerprintFor(url, branch)

	p := &domain.Pipeline{
		Name:   "build",
		Stages: []string{"build", "test"},
		Materials: []domain.Material{{
			Type: domain.MaterialGit, Fingerprint: fp, AutoUpdate: true,
			Git: &domain.GitMaterial{URL: url, Branch: branch, Events: []string{"push"}},
		}},
		Jobs: []domain.Job{
			{Name: "compile", Stage: "build", Tasks: []domain.Task{{Script: "make"}}},
			{Name: "unit", Stage: "test", Tasks: []domain.Task{{Script: "go test"}}, Needs: []string{"compile"}},
		},
	}
	if withMatrix {
		p.Jobs = append(p.Jobs, domain.Job{
			Name: "integration", Stage: "test",
			Tasks:  []domain.Task{{Script: "go test -tags=int"}},
			Matrix: map[string][]string{"OS": {"linux", "darwin"}, "ARCH": {"amd64"}},
		})
	}

	ctx := context.Background()
	res, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: "demo", Name: "Demo", Pipelines: []*domain.Pipeline{p},
	})
	if err != nil {
		t.Fatalf("seed apply: %v", err)
	}
	pipelineID = res.Pipelines[0].PipelineID

	var mid uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM materials WHERE fingerprint = $1`, fp).Scan(&mid); err != nil {
		t.Fatalf("seed material lookup: %v", err)
	}
	materialID = mid
	return
}

func baseTriggerInput(pipelineID, materialID uuid.UUID, modID int64) store.CreateRunFromModificationInput {
	return store.CreateRunFromModificationInput{
		PipelineID:     pipelineID,
		MaterialID:     materialID,
		ModificationID: modID,
		Revision:       "d8f8c1eab2a2c0a4e6c4b5e8a1d0e9f7b6c3d2e1",
		Branch:         "main",
		Provider:       "github",
		Delivery:       "test-delivery",
		TriggeredBy:    "system:webhook",
	}
}

func TestCreateRunFromModification_CreatesRunStagesJobs(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	pipelineID, materialID, _ := seedPipeline(t, pool, false)

	got, err := s.CreateRunFromModification(ctx, baseTriggerInput(pipelineID, materialID, 0))
	if err != nil {
		t.Fatalf("CreateRunFromModification: %v", err)
	}
	if got.Counter != 1 {
		t.Fatalf("counter = %d, want 1", got.Counter)
	}
	if len(got.StageRuns) != 2 {
		t.Fatalf("stages = %d, want 2", len(got.StageRuns))
	}
	if got.StageRuns[0].Name != "build" || got.StageRuns[0].Ordinal != 0 {
		t.Fatalf("stage[0] = %+v", got.StageRuns[0])
	}
	if got.StageRuns[1].Name != "test" || got.StageRuns[1].Ordinal != 1 {
		t.Fatalf("stage[1] = %+v", got.StageRuns[1])
	}
	if len(got.JobRuns) != 2 {
		t.Fatalf("jobs = %d, want 2", len(got.JobRuns))
	}

	var status, cause string
	var revisions []byte
	if err := pool.QueryRow(ctx,
		`SELECT status, cause, revisions FROM runs WHERE id = $1`, got.RunID,
	).Scan(&status, &cause, &revisions); err != nil {
		t.Fatalf("run row: %v", err)
	}
	if status != "queued" || cause != "webhook" {
		t.Fatalf("run status=%s cause=%s", status, cause)
	}
	var revMap map[string]any
	_ = json.Unmarshal(revisions, &revMap)
	if _, ok := revMap[materialID.String()]; !ok {
		t.Fatalf("revisions snapshot missing triggered material: %s", revisions)
	}
}

func TestCreateRunFromModification_CounterIncrementsPerPipeline(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	pipelineID, materialID, _ := seedPipeline(t, pool, false)

	var counters []int64
	for i := 0; i < 3; i++ {
		in := baseTriggerInput(pipelineID, materialID, int64(i))
		in.Revision = string(rune('a'+i)) + "0000000000000000000000000000000000000000"[1:]
		got, err := s.CreateRunFromModification(ctx, in)
		if err != nil {
			t.Fatalf("iter %d: %v", i, err)
		}
		counters = append(counters, got.Counter)
	}
	for i, c := range counters {
		if int64(i+1) != c {
			t.Fatalf("counters = %v, want [1 2 3]", counters)
		}
	}
}

func TestCreateRunFromModification_MatrixExpandsJobRuns(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	pipelineID, materialID, _ := seedPipeline(t, pool, true)

	got, err := s.CreateRunFromModification(ctx, baseTriggerInput(pipelineID, materialID, 0))
	if err != nil {
		t.Fatalf("CreateRunFromModification: %v", err)
	}

	integrationKeys := map[string]bool{}
	plainCompile := 0
	for _, j := range got.JobRuns {
		switch j.Name {
		case "integration":
			integrationKeys[j.MatrixKey] = true
		case "compile":
			plainCompile++
			if j.MatrixKey != "" {
				t.Fatalf("non-matrix job has matrix_key=%q", j.MatrixKey)
			}
		}
	}
	if plainCompile != 1 {
		t.Fatalf("compile count = %d, want 1", plainCompile)
	}
	wantKeys := []string{"ARCH=amd64,OS=darwin", "ARCH=amd64,OS=linux"}
	for _, k := range wantKeys {
		if !integrationKeys[k] {
			t.Fatalf("missing matrix key %q in %v", k, integrationKeys)
		}
	}
	if len(integrationKeys) != 2 {
		t.Fatalf("integration matrix count = %d, want 2", len(integrationKeys))
	}
}

func TestCreateRunFromModification_EmitsNotify(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	pipelineID, materialID, _ := seedPipeline(t, pool, false)

	listener, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer listener.Release()
	if _, err := listener.Exec(ctx, "LISTEN run_queued"); err != nil {
		t.Fatalf("LISTEN: %v", err)
	}

	got, err := s.CreateRunFromModification(ctx, baseTriggerInput(pipelineID, materialID, 0))
	if err != nil {
		t.Fatalf("CreateRunFromModification: %v", err)
	}

	waitCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	n, err := listener.Conn().WaitForNotification(waitCtx)
	if err != nil {
		t.Fatalf("WaitForNotification: %v", err)
	}
	if n.Channel != "run_queued" {
		t.Fatalf("channel = %q", n.Channel)
	}
	if n.Payload != got.RunID.String() {
		t.Fatalf("payload = %q, want %q", n.Payload, got.RunID.String())
	}
}

func TestCreateRunFromModification_JobRunsLinkCorrectStage(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	pipelineID, materialID, _ := seedPipeline(t, pool, false)

	got, err := s.CreateRunFromModification(ctx, baseTriggerInput(pipelineID, materialID, 0))
	if err != nil {
		t.Fatalf("CreateRunFromModification: %v", err)
	}

	stageByID := map[uuid.UUID]string{}
	for _, st := range got.StageRuns {
		stageByID[st.ID] = st.Name
	}
	for _, j := range got.JobRuns {
		stageName := stageByID[j.StageRunID]
		switch j.Name {
		case "compile":
			if stageName != "build" {
				t.Fatalf("compile linked to stage %q, want build", stageName)
			}
		case "unit":
			if stageName != "test" {
				t.Fatalf("unit linked to stage %q, want test", stageName)
			}
		}
	}
}
