package scheduler_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/domain"
	"github.com/gocdnext/gocdnext/server/internal/grpcsrv"
	"github.com/gocdnext/gocdnext/server/internal/scheduler"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

const testDSN = ""

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// seed creates a project + 1 pipeline (stages build/test, jobs compile/unit)
// and queues one run against it. Returns the run id + the material id the
// revisions snapshot refers to (for JobAssignment assertions).
func seed(t *testing.T, pool *pgxpool.Pool) (runID, materialID uuid.UUID) {
	t.Helper()
	s := store.New(pool)
	ctx := context.Background()

	fp := domain.GitFingerprint("https://github.com/org/demo", "main")
	applyRes, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: "demo", Name: "Demo",
		Pipelines: []*domain.Pipeline{{
			Name:   "ci",
			Stages: []string{"build", "test"},
			Materials: []domain.Material{{
				Type: domain.MaterialGit, Fingerprint: fp, AutoUpdate: true,
				Git: &domain.GitMaterial{URL: "https://github.com/org/demo", Branch: "main", Events: []string{"push"}},
			}},
			Jobs: []domain.Job{
				{Name: "compile", Stage: "build", Image: "golang:1.23", Tasks: []domain.Task{{Script: "make"}}},
				{Name: "unit", Stage: "test", Image: "golang:1.23", Tasks: []domain.Task{{Script: "make test"}}, Needs: []string{"compile"}},
			},
		}},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	pipelineID := applyRes.Pipelines[0].PipelineID

	if err := pool.QueryRow(ctx, `SELECT id FROM materials WHERE fingerprint = $1`, fp).Scan(&materialID); err != nil {
		t.Fatalf("mat lookup: %v", err)
	}

	runRes, err := s.CreateRunFromModification(ctx, store.CreateRunFromModificationInput{
		PipelineID:  pipelineID,
		MaterialID:  materialID,
		Revision:    "abc0123456789abc0123456789abc0123456789a",
		Branch:      "main",
		Provider:    "github",
		Delivery:    "test",
		TriggeredBy: "system:webhook",
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	return runRes.RunID, materialID
}

// seedAgentRow creates an `agents` row so AssignJob's FK to agent_id holds.
// The SessionStore is in-memory, but the DB still needs the agent to exist.
func seedAgentRow(t *testing.T, pool *pgxpool.Pool, name string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := pool.QueryRow(context.Background(),
		`INSERT INTO agents (name, token_hash) VALUES ($1, 'hash') RETURNING id`, name,
	).Scan(&id)
	if err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	return id
}

func TestDispatchRun_JobWithoutProfileInheritsDefaultScheduling(t *testing.T) {
	// Hotfix v0.14.1: a job that declares no `agent.profile:` AND a
	// runner profile named `default` exists in the DB must inherit
	// the default's NodeSelector + Tolerations on the wire. Mirrors
	// the apply-time bounds fallback shipped in v0.13.1 so the
	// safety net is consistent across both apply (bounds) and
	// dispatch (scheduling). Missing `default` profile = no-op,
	// same as before this fix.
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	tolSeconds := int64(60)
	if _, err := s.InsertRunnerProfile(ctx, nil, store.RunnerProfileInput{
		Name:   "default",
		Engine: "kubernetes",
		NodeSelector: map[string]string{
			"cloud.google.com/gke-nodepool": "node-pool-spot-ci",
		},
		Tolerations: []store.Toleration{
			{Key: "cora", Operator: "Equal", Value: "spot-ci", Effect: "NoSchedule"},
			{Key: "spot", Operator: "Exists", Effect: "NoExecute", TolerationSeconds: &tolSeconds},
		},
	}); err != nil {
		t.Fatalf("seed default profile: %v", err)
	}

	sessions := grpcsrv.NewSessionStore()
	sched := scheduler.New(s, sessions, quietLogger(), testDSN)

	runID, _ := seed(t, pool)
	agentID := seedAgentRow(t, pool, "agent-default-inherit")
	sess := sessions.CreateSession(agentID, nil, 1, 0)

	sched.DispatchRun(ctx, runID)

	select {
	case msg := <-sess.Out():
		assign := msg.GetAssign()
		if assign == nil {
			t.Fatalf("message is not JobAssignment: %+v", msg)
		}
		if assign.NodeSelector["cloud.google.com/gke-nodepool"] != "node-pool-spot-ci" {
			t.Errorf("NodeSelector not inherited from default: %+v", assign.NodeSelector)
		}
		if len(assign.Tolerations) != 2 {
			t.Fatalf("Tolerations len = %d, want 2", len(assign.Tolerations))
		}
		if assign.Tolerations[1].GetTolerationSeconds() != 60 {
			t.Errorf("Tolerations[1].TolerationSeconds = %d, want 60",
				assign.Tolerations[1].GetTolerationSeconds())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no assignment after 2s")
	}
}

func TestDispatchRun_JobWithoutProfileNoDefaultIsNoop(t *testing.T) {
	// No `default` profile in the DB → fallback is a no-op,
	// JobAssignment carries no NodeSelector / Tolerations. Defends
	// against a regression where the fallback would attribute
	// scheduling fields when none should be set.
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()
	sessions := grpcsrv.NewSessionStore()
	sched := scheduler.New(s, sessions, quietLogger(), testDSN)

	runID, _ := seed(t, pool)
	agentID := seedAgentRow(t, pool, "agent-no-default")
	sess := sessions.CreateSession(agentID, nil, 1, 0)

	sched.DispatchRun(ctx, runID)

	select {
	case msg := <-sess.Out():
		assign := msg.GetAssign()
		if assign == nil {
			t.Fatalf("message is not JobAssignment: %+v", msg)
		}
		if len(assign.NodeSelector) != 0 {
			t.Errorf("NodeSelector should be empty without default profile: %+v", assign.NodeSelector)
		}
		if len(assign.Tolerations) != 0 {
			t.Errorf("Tolerations should be empty without default profile: %+v", assign.Tolerations)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no assignment after 2s")
	}
}

func TestDispatchRun_PushesAssignmentToIdleAgent(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	sessions := grpcsrv.NewSessionStore()
	sched := scheduler.New(s, sessions, quietLogger(), testDSN)
	ctx := context.Background()

	runID, materialID := seed(t, pool)
	agentID := seedAgentRow(t, pool, "agent-1")
	sess := sessions.CreateSession(agentID, nil, 1, 0)

	sched.DispatchRun(ctx, runID)

	select {
	case msg := <-sess.Out():
		assign := msg.GetAssign()
		if assign == nil {
			t.Fatalf("message is not JobAssignment: %+v", msg)
		}
		if assign.Name != "compile" {
			t.Fatalf("job name = %q, want compile", assign.Name)
		}
		if assign.RunId != runID.String() {
			t.Fatalf("run_id = %s, want %s", assign.RunId, runID)
		}
		if len(assign.Checkouts) != 1 {
			t.Fatalf("checkouts len = %d, want 1", len(assign.Checkouts))
		}
		co := assign.Checkouts[0]
		if co.MaterialId != materialID.String() || co.Revision == "" {
			t.Fatalf("checkout = %+v", co)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("no assignment delivered within 2s")
	}

	var status, runStatus string
	_ = pool.QueryRow(ctx, `SELECT status FROM job_runs WHERE name='compile'`).Scan(&status)
	_ = pool.QueryRow(ctx, `SELECT status FROM runs WHERE id=$1`, runID).Scan(&runStatus)
	if status != "running" || runStatus != "running" {
		t.Fatalf("job=%s run=%s, want running/running", status, runStatus)
	}
}

func TestDispatchRun_NoIdleAgentKeepsJobQueued(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	sessions := grpcsrv.NewSessionStore()
	sched := scheduler.New(s, sessions, quietLogger(), testDSN)
	ctx := context.Background()

	runID, _ := seed(t, pool)
	sched.DispatchRun(ctx, runID)

	var status string
	_ = pool.QueryRow(ctx, `SELECT status FROM job_runs WHERE name='compile'`).Scan(&status)
	if status != "queued" {
		t.Fatalf("status = %s, want queued", status)
	}
}

func TestDispatchRun_SkipsSecondStageJobs(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	sessions := grpcsrv.NewSessionStore()
	sched := scheduler.New(s, sessions, quietLogger(), testDSN)
	ctx := context.Background()

	runID, _ := seed(t, pool)
	agentID := seedAgentRow(t, pool, "agent-1")
	sess := sessions.CreateSession(agentID, nil, 4, 0)

	sched.DispatchRun(ctx, runID)

	// Drain whatever was sent; there must be exactly 1 assignment (compile)
	// — the unit job sits in stage 2 and is blocked by the active-stage gate.
	count := 0
drain:
	for {
		select {
		case msg, ok := <-sess.Out():
			if !ok {
				break drain
			}
			if msg.GetAssign() != nil {
				count++
			}
		case <-time.After(200 * time.Millisecond):
			break drain
		}
	}
	if count != 1 {
		t.Fatalf("dispatched %d assignments, want 1 (stage gate)", count)
	}
}

func TestDispatchRun_RoutesToAgentMatchingTags(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	sessions := grpcsrv.NewSessionStore()
	sched := scheduler.New(s, sessions, quietLogger(), testDSN)
	ctx := context.Background()

	// Pipeline with a single job that requires the `docker` tag.
	url, branch := "https://github.com/org/tagged", "main"
	fp := domain.GitFingerprint(url, branch)
	applyRes, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: "tagged", Name: "Tagged",
		Pipelines: []*domain.Pipeline{{
			Name:   "ci",
			Stages: []string{"build"},
			Materials: []domain.Material{{
				Type: domain.MaterialGit, Fingerprint: fp, AutoUpdate: true,
				Git: &domain.GitMaterial{URL: url, Branch: branch, Events: []string{"push"}},
			}},
			Jobs: []domain.Job{
				{Name: "build", Stage: "build", Tasks: []domain.Task{{Script: "docker build ."}}, Tags: []string{"docker"}},
			},
		}},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	var matID uuid.UUID
	_ = pool.QueryRow(ctx, `SELECT id FROM materials WHERE fingerprint = $1`, fp).Scan(&matID)

	run, err := s.CreateRunFromModification(ctx, store.CreateRunFromModificationInput{
		PipelineID: applyRes.Pipelines[0].PipelineID, MaterialID: matID,
		Revision: "abc0123456789abc0123456789abc0123456789a", Branch: "main",
		Provider: "github", Delivery: "t", TriggeredBy: "system:webhook",
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	// Two agents: plain (no docker) and dockerized.
	plainID := seedAgentRow(t, pool, "plain")
	dockerID := seedAgentRow(t, pool, "dockerized")
	plainSess := sessions.CreateSession(plainID, []string{"linux"}, 1, 0)
	dockerSess := sessions.CreateSession(dockerID, []string{"linux", "docker"}, 1, 0)

	sched.DispatchRun(ctx, run.RunID)

	// The plain agent must not have received anything.
	select {
	case msg := <-plainSess.Out():
		if msg.GetAssign() != nil {
			t.Fatalf("plain agent received an assignment despite lacking docker tag: %+v", msg.GetAssign())
		}
	case <-time.After(200 * time.Millisecond):
		// ok
	}

	// The docker agent must have it.
	select {
	case msg := <-dockerSess.Out():
		if msg.GetAssign() == nil {
			t.Fatalf("docker agent got non-assignment message: %+v", msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("docker agent never received the assignment")
	}
}

func TestDispatchRun_LeavesJobQueuedWhenNoAgentHasRequiredTags(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	sessions := grpcsrv.NewSessionStore()
	sched := scheduler.New(s, sessions, quietLogger(), testDSN)
	ctx := context.Background()

	url, branch := "https://github.com/org/gpuonly", "main"
	fp := domain.GitFingerprint(url, branch)
	applyRes, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: "gpuonly", Name: "GPU Only",
		Pipelines: []*domain.Pipeline{{
			Name:   "ci",
			Stages: []string{"train"},
			Materials: []domain.Material{{
				Type: domain.MaterialGit, Fingerprint: fp, AutoUpdate: true,
				Git: &domain.GitMaterial{URL: url, Branch: branch, Events: []string{"push"}},
			}},
			Jobs: []domain.Job{
				{Name: "train", Stage: "train", Tasks: []domain.Task{{Script: "train.sh"}}, Tags: []string{"gpu"}},
			},
		}},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	var matID uuid.UUID
	_ = pool.QueryRow(ctx, `SELECT id FROM materials WHERE fingerprint = $1`, fp).Scan(&matID)

	run, err := s.CreateRunFromModification(ctx, store.CreateRunFromModificationInput{
		PipelineID: applyRes.Pipelines[0].PipelineID, MaterialID: matID,
		Revision: "abc0123456789abc0123456789abc0123456789a", Branch: "main",
		Provider: "github", Delivery: "t", TriggeredBy: "system:webhook",
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	plainID := seedAgentRow(t, pool, "plain-only")
	sessions.CreateSession(plainID, []string{"linux"}, 1, 0)

	sched.DispatchRun(ctx, run.RunID)

	var status string
	_ = pool.QueryRow(ctx, `SELECT status FROM job_runs WHERE name='train'`).Scan(&status)
	if status != "queued" {
		t.Fatalf("status = %q, want queued (no gpu agent available)", status)
	}
}

func TestBuildAssignment_InjectsSecretsIntoEnvAndMasks(t *testing.T) {
	def := domain.Pipeline{
		Stages: []string{"deploy"},
		Jobs: []domain.Job{
			{
				Name: "push", Stage: "deploy",
				Tasks:   []domain.Task{{Script: "echo $GH_TOKEN"}},
				Secrets: []string{"GH_TOKEN", "REGISTRY_PASSWORD"},
			},
		},
	}
	defJSON, _ := json.Marshal(def)
	run := store.RunForDispatch{
		ID: uuid.New(), PipelineID: uuid.New(), Definition: defJSON,
		Revisions: json.RawMessage(`{}`),
	}
	job := store.DispatchableJob{ID: uuid.New(), Name: "push"}

	secrets := map[string]string{
		"GH_TOKEN":          "ghp_abc123",
		"REGISTRY_PASSWORD": "reg-pw-xyz",
	}
	got, err := scheduler.BuildAssignment(run, job, nil, secrets, nil, store.ResolvedProfile{}, nil, nil)
	if err != nil {
		t.Fatalf("BuildAssignment: %v", err)
	}
	if got.Env["GH_TOKEN"] != "ghp_abc123" {
		t.Fatalf("env[GH_TOKEN] = %q", got.Env["GH_TOKEN"])
	}
	if got.Env["REGISTRY_PASSWORD"] != "reg-pw-xyz" {
		t.Fatalf("env[REGISTRY_PASSWORD] = %q", got.Env["REGISTRY_PASSWORD"])
	}
	// LogMasks must carry the VALUES, not the names — the runner matches on
	// the raw string to redact it from stdout/stderr.
	masks := map[string]bool{}
	for _, m := range got.LogMasks {
		masks[m] = true
	}
	if !masks["ghp_abc123"] || !masks["reg-pw-xyz"] {
		t.Fatalf("log_masks missing values: %+v", got.LogMasks)
	}
}

// TestBuildAssignment_MasksOptInOutputsBypassesEightCharHeuristic
// — issue #22.
//
// Contract: `masked: true` makes the scheduler emit the resolved
// value into LogMasks regardless of the 8-char threshold the
// heuristic uses for unmarked outputs. The agent runner still
// imposes its own 4-char floor (runner.go::applyMasks) — that's
// out of scope here; operators needing to mask <4-char values
// should use `secrets:` instead.
//
// The test pins the scheduler boundary: a 6-char opt-in value
// is below the 8-char heuristic but well above the 4-char floor,
// so it's the clean case for verifying opt-in fires where the
// heuristic wouldn't.
func TestBuildAssignment_MasksOptInOutputsBypassesEightCharHeuristic(t *testing.T) {
	def := domain.Pipeline{
		Stages: []string{"prep", "deploy"},
		Jobs: []domain.Job{
			{
				Name:  "bump",
				Stage: "prep",
				// Two aliases — both 6 chars (under heuristic, over
				// agent floor): `secret` is masked-opt-in → must mask.
				// `pub` is not masked → must NOT appear (heuristic
				// skips it).
				Outputs: map[string]string{
					"secret": "NEXT_SECRET",
					"pub":    "NEXT_PUB",
				},
				OutputMasks: map[string]bool{
					"secret": true,
				},
			},
			{
				Name:  "deploy",
				Stage: "deploy",
				Needs: []string{"bump"},
				Tasks: []domain.Task{{Script: "echo done"}},
			},
		},
	}
	defJSON, _ := json.Marshal(def)
	run := store.RunForDispatch{
		ID: uuid.New(), PipelineID: uuid.New(), Definition: defJSON,
		Revisions: json.RawMessage(`{}`),
	}
	job := store.DispatchableJob{ID: uuid.New(), Name: "deploy", Needs: []string{"bump"}}

	needs := scheduler.NeedsOutputs{
		"bump": {
			"secret": "abc123", // 6 chars: < heuristic, > floor; opt-in MUST fire
			"pub":    "public", // 6 chars: < heuristic; no opt-in → heuristic skips
		},
	}
	got, err := scheduler.BuildAssignment(run, job, nil, nil, nil, store.ResolvedProfile{}, nil, needs)
	if err != nil {
		t.Fatalf("BuildAssignment: %v", err)
	}
	masks := map[string]bool{}
	for _, m := range got.LogMasks {
		masks[m] = true
	}
	if !masks["abc123"] {
		t.Errorf("log_masks missing %q — opt-in masked output MUST bypass the 8-char scheduler heuristic; got %+v", "abc123", got.LogMasks)
	}
	if masks["public"] {
		t.Errorf("log_masks contains %q — non-masked 6-char output should be skipped by the heuristic; got %+v", "public", got.LogMasks)
	}
}

func TestBuildAssignment_MergesProfileEnvAndMasks(t *testing.T) {
	def := domain.Pipeline{
		Stages: []string{"build"},
		Jobs: []domain.Job{{
			Name: "build", Stage: "build",
			// Job-level Variables collide with profile env on
			// GOCDNEXT_LAYER_CACHE_NAME — job override wins, the
			// other profile vars come through as defaults.
			Variables: map[string]string{
				"GOCDNEXT_LAYER_CACHE_NAME": "override",
				"FROM_JOB":                  "yes",
			},
			Tasks: []domain.Task{{Script: "echo hi"}},
		}},
	}
	defJSON, _ := json.Marshal(def)
	run := store.RunForDispatch{
		ID: uuid.New(), PipelineID: uuid.New(), Definition: defJSON,
		Revisions: json.RawMessage(`{}`),
	}
	job := store.DispatchableJob{ID: uuid.New(), Name: "build"}

	profileEnv := map[string]string{
		"GOCDNEXT_LAYER_CACHE_BUCKET": "ci-cache",
		"GOCDNEXT_LAYER_CACHE_NAME":   "default",
		"AWS_ACCESS_KEY_ID":           "AKIA",
	}
	profileMasks := []string{"AKIA"}

	got, err := scheduler.BuildAssignment(run, job, nil, nil, nil, store.ResolvedProfile{Env: profileEnv, SecretValues: profileMasks}, nil, nil)
	if err != nil {
		t.Fatalf("BuildAssignment: %v", err)
	}
	if got.Env["GOCDNEXT_LAYER_CACHE_BUCKET"] != "ci-cache" {
		t.Errorf("profile env not merged: %q", got.Env["GOCDNEXT_LAYER_CACHE_BUCKET"])
	}
	if got.Env["AWS_ACCESS_KEY_ID"] != "AKIA" {
		t.Errorf("profile secret not merged: %q", got.Env["AWS_ACCESS_KEY_ID"])
	}
	// Job vars must win on collision (more specific beats default).
	if got.Env["GOCDNEXT_LAYER_CACHE_NAME"] != "override" {
		t.Errorf("job var did not override profile env: %q", got.Env["GOCDNEXT_LAYER_CACHE_NAME"])
	}
	if got.Env["FROM_JOB"] != "yes" {
		t.Errorf("plain job var lost: %q", got.Env["FROM_JOB"])
	}
	// Profile secret VALUES go into LogMasks for log redaction.
	masks := map[string]bool{}
	for _, m := range got.LogMasks {
		masks[m] = true
	}
	if !masks["AKIA"] {
		t.Errorf("profile mask missing from LogMasks: %+v", got.LogMasks)
	}
}

func TestBuildAssignment_PropagatesProfileNodeSelectorAndTolerations(t *testing.T) {
	// Scheduling hints from the runner profile must land on the
	// JobAssignment wire fields the agent engine consumes. Without
	// this, the admin-edited profile gets the values but the agent
	// pod spec never sees them — silent drop.
	pipeline := &domain.Pipeline{
		Name:   "p", Stages: []string{"build"},
		Jobs: []domain.Job{{Name: "b", Stage: "build", Tasks: []domain.Task{{Script: "true"}}}},
	}
	pipelineJSON, _ := json.Marshal(pipeline)
	run := store.RunForDispatch{
		ID: uuid.New(), PipelineID: uuid.New(), Counter: 1,
		Definition: pipelineJSON,
	}
	job := store.DispatchableJob{ID: uuid.New(), Name: "b"}

	tolerSeconds := int64(60)
	resolved := store.ResolvedProfile{
		NodeSelector: map[string]string{
			"workload":           "ci",
			"kubernetes.io/arch": "amd64",
		},
		Tolerations: []store.Toleration{
			{Key: "ci-only", Operator: "Equal", Value: "true", Effect: "NoSchedule"},
			{Key: "spot", Operator: "Exists", Effect: "NoExecute", TolerationSeconds: &tolerSeconds},
		},
	}

	got, err := scheduler.BuildAssignment(run, job, nil, nil, nil, resolved, nil, nil)
	if err != nil {
		t.Fatalf("BuildAssignment: %v", err)
	}
	if len(got.NodeSelector) != 2 ||
		got.NodeSelector["workload"] != "ci" ||
		got.NodeSelector["kubernetes.io/arch"] != "amd64" {
		t.Errorf("NodeSelector not propagated: %+v", got.NodeSelector)
	}
	if len(got.Tolerations) != 2 {
		t.Fatalf("Tolerations len = %d", len(got.Tolerations))
	}
	if got.Tolerations[0].GetKey() != "ci-only" || got.Tolerations[0].GetEffect() != "NoSchedule" {
		t.Errorf("Tolerations[0] = %+v", got.Tolerations[0])
	}
	if got.Tolerations[1].GetTolerationSeconds() != 60 {
		t.Errorf("Tolerations[1].TolerationSeconds = %d, want 60",
			got.Tolerations[1].GetTolerationSeconds())
	}

	// Aliasing guard: mutating the input slice's TolerationSeconds
	// pointer after BuildAssignment returned MUST NOT change the
	// wire object. tolerationsToProto copies the value into a fresh
	// *int64 — without that, a future caller cache that reuses the
	// slice across dispatches could mutate already-shipped
	// assignments.
	tolerSeconds = 999
	if got.Tolerations[1].GetTolerationSeconds() != 60 {
		t.Errorf("Tolerations[1] aliased the input pointer; mutation leaked: got %d",
			got.Tolerations[1].GetTolerationSeconds())
	}
}

func TestBuildAssignment_EmptyProfileLeavesSchedulingFieldsNil(t *testing.T) {
	// Job without a profile (the common case) ships an empty
	// NodeSelector + nil Tolerations on the wire. Defending against
	// a refactor that accidentally emits empty maps/slices: nil
	// keeps the proto bytes minimal AND lets the engine treat
	// absent + nil identically.
	pipeline := &domain.Pipeline{
		Name:   "p", Stages: []string{"build"},
		Jobs: []domain.Job{{Name: "b", Stage: "build", Tasks: []domain.Task{{Script: "true"}}}},
	}
	pipelineJSON, _ := json.Marshal(pipeline)
	run := store.RunForDispatch{
		ID: uuid.New(), PipelineID: uuid.New(), Counter: 1,
		Definition: pipelineJSON,
	}
	job := store.DispatchableJob{ID: uuid.New(), Name: "b"}

	got, err := scheduler.BuildAssignment(run, job, nil, nil, nil, store.ResolvedProfile{}, nil, nil)
	if err != nil {
		t.Fatalf("BuildAssignment: %v", err)
	}
	if got.NodeSelector != nil {
		t.Errorf("NodeSelector = %+v, want nil", got.NodeSelector)
	}
	if got.Tolerations != nil {
		t.Errorf("Tolerations = %+v, want nil", got.Tolerations)
	}
}

func TestBuildAssignment_PropagatesProfileAndResources(t *testing.T) {
	def := domain.Pipeline{
		Stages: []string{"build"},
		Jobs: []domain.Job{{
			Name: "build", Stage: "build",
			Profile: "gpu",
			Resources: domain.ResourceSpec{
				Requests: domain.ResourceQuantities{CPU: "500m", Memory: "512Mi"},
				Limits:   domain.ResourceQuantities{CPU: "2", Memory: "2Gi"},
			},
			Tasks: []domain.Task{{Script: "make"}},
		}},
	}
	defJSON, _ := json.Marshal(def)
	run := store.RunForDispatch{
		ID: uuid.New(), PipelineID: uuid.New(), Definition: defJSON,
		Revisions: json.RawMessage(`{}`),
	}
	job := store.DispatchableJob{ID: uuid.New(), Name: "build"}

	got, err := scheduler.BuildAssignment(run, job, nil, nil, nil, store.ResolvedProfile{}, nil, nil)
	if err != nil {
		t.Fatalf("BuildAssignment: %v", err)
	}
	if got.GetProfile() != "gpu" {
		t.Fatalf("profile = %q, want gpu", got.GetProfile())
	}
	r := got.GetResources()
	if r == nil {
		t.Fatal("expected resources, got nil")
	}
	if r.GetCpuRequest() != "500m" || r.GetMemoryRequest() != "512Mi" {
		t.Fatalf("requests = %+v", r)
	}
	if r.GetCpuLimit() != "2" || r.GetMemoryLimit() != "2Gi" {
		t.Fatalf("limits = %+v", r)
	}
}

func TestBuildAssignment_NoResourcesLeavesProtoNil(t *testing.T) {
	def := domain.Pipeline{
		Stages: []string{"x"},
		Jobs:   []domain.Job{{Name: "j", Stage: "x", Tasks: []domain.Task{{Script: "echo"}}}},
	}
	defJSON, _ := json.Marshal(def)
	run := store.RunForDispatch{
		ID: uuid.New(), PipelineID: uuid.New(), Definition: defJSON,
		Revisions: json.RawMessage(`{}`),
	}
	job := store.DispatchableJob{ID: uuid.New(), Name: "j"}

	got, err := scheduler.BuildAssignment(run, job, nil, nil, nil, store.ResolvedProfile{}, nil, nil)
	if err != nil {
		t.Fatalf("BuildAssignment: %v", err)
	}
	if got.GetResources() != nil {
		t.Fatalf("expected nil resources, got %+v", got.GetResources())
	}
	if got.GetProfile() != "" {
		t.Fatalf("expected empty profile, got %q", got.GetProfile())
	}
}

func TestBuildAssignment_MissingSecretIsError(t *testing.T) {
	def := domain.Pipeline{
		Stages: []string{"x"},
		Jobs:   []domain.Job{{Name: "j", Stage: "x", Secrets: []string{"ABSENT"}}},
	}
	defJSON, _ := json.Marshal(def)
	run := store.RunForDispatch{
		ID: uuid.New(), PipelineID: uuid.New(), Definition: defJSON,
		Revisions: json.RawMessage(`{}`),
	}
	job := store.DispatchableJob{ID: uuid.New(), Name: "j"}

	if _, err := scheduler.BuildAssignment(run, job, nil, map[string]string{}, nil, store.ResolvedProfile{}, nil, nil); err == nil {
		t.Fatalf("expected error when declared secret is unresolved")
	}
}

func TestJobSecretsFromDefinition_Extracts(t *testing.T) {
	def := domain.Pipeline{
		Stages: []string{"s"},
		Jobs: []domain.Job{
			{Name: "a", Stage: "s", Secrets: []string{"X"}},
			{Name: "b", Stage: "s"},
		},
	}
	defJSON, _ := json.Marshal(def)

	got, err := scheduler.JobSecretsFromDefinition(defJSON, "a")
	if err != nil || len(got) != 1 || got[0] != "X" {
		t.Fatalf("secrets for a = %v err=%v", got, err)
	}
	got, err = scheduler.JobSecretsFromDefinition(defJSON, "b")
	if err != nil || len(got) != 0 {
		t.Fatalf("secrets for b = %v err=%v", got, err)
	}
	if _, err := scheduler.JobSecretsFromDefinition(defJSON, "nope"); err == nil {
		t.Fatalf("expected error for unknown job name")
	}
}

func TestBuildAssignment_MapsTasksAndCheckouts(t *testing.T) {
	def := domain.Pipeline{
		Stages: []string{"build"},
		Jobs: []domain.Job{
			{Name: "compile", Stage: "build", Tasks: []domain.Task{{Script: "echo hi"}}},
		},
		Variables: map[string]string{"FOO": "bar"},
	}
	defJSON, _ := json.Marshal(def)
	materialID := uuid.New()

	run := store.RunForDispatch{
		ID:         uuid.New(),
		PipelineID: uuid.New(),
		Definition: defJSON,
		Revisions: json.RawMessage(`{"` + materialID.String() +
			`":{"revision":"deadbeef","branch":"main"}}`),
	}
	job := store.DispatchableJob{ID: uuid.New(), Name: "compile", Image: "golang:1.23"}
	gitCfg, _ := json.Marshal(domain.GitMaterial{URL: "https://github.com/x/y", Branch: "main"})
	materials := []store.Material{{
		ID: materialID, Type: string(domain.MaterialGit), Config: gitCfg,
	}}

	got, err := scheduler.BuildAssignment(run, job, materials, nil, nil, store.ResolvedProfile{}, nil, nil)
	if err != nil {
		t.Fatalf("BuildAssignment: %v", err)
	}
	if got.Name != "compile" || got.Image != "golang:1.23" {
		t.Fatalf("%+v", got)
	}
	if len(got.Tasks) != 1 || got.Tasks[0].GetScript() != "echo hi" {
		t.Fatalf("tasks = %+v", got.Tasks)
	}
	if got.Env["FOO"] != "bar" {
		t.Fatalf("env = %+v", got.Env)
	}
	if len(got.Checkouts) != 1 || got.Checkouts[0].Revision != "deadbeef" {
		t.Fatalf("checkouts = %+v", got.Checkouts)
	}
}

// TestBuildAssignment_DedupesArtifactPathsCanonical guards the
// dispatch-side defence: the parser already dedupes at apply time,
// but `run.Definition` is the persisted snapshot — pipelines
// applied BEFORE the parser fix shipped still carry the raw
// (potentially-duplicated) shape. BuildAssignment must clean
// them, otherwise the runner's required+optional two-RPC pattern
// would land an optional `dist/` insert that the storage layer
// canonicalizes to `dist`, collides with the required `dist` row
// on the partial unique index (00035), and rolls back the optional
// batch — dropping any other optional artifacts as collateral.
func TestBuildAssignment_DedupesArtifactPathsCanonical(t *testing.T) {
	def := domain.Pipeline{
		Stages: []string{"build"},
		Jobs: []domain.Job{{
			Name: "compile", Stage: "build",
			Tasks: []domain.Task{{Script: "make"}},
			// Pre-upgrade pipeline shape: duplicates in `paths`,
			// canonical-overlap across paths/optional, AND an
			// optional that's unique. Post-dedupe wire shape:
			//   ArtifactPaths         = [dist]
			//   OptionalArtifactPaths = [screenshots]
			ArtifactPaths:         []string{"dist", "dist/"},
			OptionalArtifactPaths: []string{"dist/", "screenshots"},
		}},
	}
	defJSON, _ := json.Marshal(def)
	materialID := uuid.New()
	run := store.RunForDispatch{
		ID:         uuid.New(),
		PipelineID: uuid.New(),
		Definition: defJSON,
		Revisions: json.RawMessage(`{"` + materialID.String() +
			`":{"revision":"deadbeef","branch":"main"}}`),
	}
	job := store.DispatchableJob{ID: uuid.New(), Name: "compile", Image: "alpine:3.19"}
	gitCfg, _ := json.Marshal(domain.GitMaterial{URL: "https://github.com/x/y", Branch: "main"})
	materials := []store.Material{{
		ID: materialID, Type: string(domain.MaterialGit), Config: gitCfg,
	}}

	got, err := scheduler.BuildAssignment(run, job, materials, nil, nil, store.ResolvedProfile{}, nil, nil)
	if err != nil {
		t.Fatalf("BuildAssignment: %v", err)
	}
	if want := []string{"dist"}; !slicesEqual(got.ArtifactPaths, want) {
		t.Fatalf("ArtifactPaths = %v, want %v", got.ArtifactPaths, want)
	}
	if want := []string{"screenshots"}; !slicesEqual(got.OptionalArtifactPaths, want) {
		t.Fatalf("OptionalArtifactPaths = %v, want %v (dist/ canonical-deduped against required)", got.OptionalArtifactPaths, want)
	}
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestBuildAssignment_SubstitutesPluginSettings is the e2e cover for
// the v0.4.8 fix: a `${{ NAME }}` token inside a plugin `with:` value
// must be replaced with the declared secret's value before the
// assignment hits the gRPC wire. Pre-fix, the literal token reached
// the agent (e.g. `docker login --username "${{ DOCKER_USERNAME }}"`).
func TestBuildAssignment_SubstitutesPluginSettings(t *testing.T) {
	def := domain.Pipeline{
		Stages:    []string{"publish"},
		Variables: map[string]string{"REGISTRY": "registry.example.com"},
		Jobs: []domain.Job{{
			Name:    "buildx",
			Stage:   "publish",
			Secrets: []string{"DOCKER_USERNAME", "DOCKER_PASSWORD"},
			Tasks: []domain.Task{{
				Plugin: &domain.PluginStep{
					Image: "ghcr.io/klinux/gocdnext-plugin-buildx:v1",
					Settings: map[string]string{
						"image":    "${{ REGISTRY }}/monorepo-app",
						"username": "${{ DOCKER_USERNAME }}",
						"password": "${{ DOCKER_PASSWORD }}",
					},
				},
			}},
		}},
	}
	defJSON, _ := json.Marshal(def)
	run := store.RunForDispatch{ID: uuid.New(), PipelineID: uuid.New(), Definition: defJSON}
	job := store.DispatchableJob{ID: uuid.New(), Name: "buildx"}
	secrets := map[string]string{
		"DOCKER_USERNAME": "deploybot",
		"DOCKER_PASSWORD": "hunter2",
	}

	got, err := scheduler.BuildAssignment(run, job, nil, secrets, nil, store.ResolvedProfile{}, nil, nil)
	if err != nil {
		t.Fatalf("BuildAssignment: %v", err)
	}
	if len(got.Tasks) != 1 {
		t.Fatalf("tasks = %+v", got.Tasks)
	}
	plug := got.Tasks[0].GetPlugin()
	if plug == nil {
		t.Fatalf("expected plugin task")
	}
	wantSettings := map[string]string{
		"image":    "registry.example.com/monorepo-app",
		"username": "deploybot",
		"password": "hunter2",
	}
	for k, v := range wantSettings {
		if plug.Settings[k] != v {
			t.Errorf("settings[%q] = %q, want %q", k, plug.Settings[k], v)
		}
	}
	// Secret values must show up in LogMasks so the runner redacts
	// them from the `docker login` echo and any subsequent error.
	var maskHits int
	for _, m := range got.LogMasks {
		if m == "deploybot" || m == "hunter2" {
			maskHits++
		}
	}
	if maskHits != 2 {
		t.Errorf("LogMasks missing secret values: %v", got.LogMasks)
	}
}

// TestBuildAssignment_RejectsUnresolvedRefBeforeDispatch guards the
// "fail fast at scheduler, not inside the plugin container" contract.
// Without this, an operator typo (`${{ DOCKR_USERNAME }}`) would only
// surface as a downstream auth error miles away from the cause.
func TestBuildAssignment_RejectsUnresolvedRefBeforeDispatch(t *testing.T) {
	def := domain.Pipeline{
		Stages: []string{"publish"},
		Jobs: []domain.Job{{
			Name:    "buildx",
			Stage:   "publish",
			Secrets: []string{"DOCKER_USERNAME"},
			Tasks: []domain.Task{{
				Plugin: &domain.PluginStep{
					Image: "buildx:v1",
					Settings: map[string]string{
						"username": "${{ DOCKER_USERNAME }}",
						"password": "${{ NOT_DECLARED }}", // typo
					},
				},
			}},
		}},
	}
	defJSON, _ := json.Marshal(def)
	run := store.RunForDispatch{ID: uuid.New(), PipelineID: uuid.New(), Definition: defJSON}
	job := store.DispatchableJob{ID: uuid.New(), Name: "buildx"}

	_, err := scheduler.BuildAssignment(run, job, nil,
		map[string]string{"DOCKER_USERNAME": "deploybot"}, nil, store.ResolvedProfile{}, nil, nil)
	if err == nil {
		t.Fatal("expected error for unresolved ref")
	}
	if !strings.Contains(err.Error(), "NOT_DECLARED") {
		t.Errorf("err missing the unresolved name: %v", err)
	}
	// The error must NOT leak the resolved secret value (the
	// neighbouring `${{ DOCKER_USERNAME }}` ref).
	if strings.Contains(err.Error(), "deploybot") {
		t.Errorf("err leaked sibling secret value: %v", err)
	}
}

// TestE2E_OutputsRoundTripFromBumpToPublish is the integration cover
// for issue #10: a YAML pipeline declares `outputs:` on an upstream
// job, the agent persists outputs on CompleteJob, and the downstream's
// dispatch resolves `${{ needs.X.outputs.Y }}` to the persisted value.
// Covers every layer end to end (parser → store → CompleteJob →
// ListJobOutputsForRun → groupNeedsOutputs → substituteNeedsRefs →
// JobAssignment env) so a future refactor that breaks any of them
// fails this one test.
func TestE2E_OutputsRoundTripFromBumpToPublish(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	sessions := grpcsrv.NewSessionStore()
	sched := scheduler.New(s, sessions, quietLogger(), testDSN)
	ctx := context.Background()

	// ApplyProject with a 2-job pipeline: `bump` declares
	// outputs.next, `publish` references it in env via the
	// `${{ needs.bump.outputs.next }}` syntax.
	fp := domain.GitFingerprint("https://github.com/org/e2e-outputs", "main")
	applyRes, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: "e2e-outputs", Name: "E2E Outputs",
		Pipelines: []*domain.Pipeline{{
			Name:   "release",
			Stages: []string{"tag", "publish"},
			Materials: []domain.Material{{
				Type: domain.MaterialGit, Fingerprint: fp, AutoUpdate: true,
				Git: &domain.GitMaterial{URL: "https://github.com/org/e2e-outputs", Branch: "main", Events: []string{"push"}},
			}},
			Jobs: []domain.Job{
				{
					Name: "bump", Stage: "tag", Image: "alpine:3.20",
					Tasks:   []domain.Task{{Script: "echo NEXT=v1.2.3 > $GOCDNEXT_OUTPUT_FILE"}},
					Outputs: map[string]string{"next": "NEXT"},
				},
				{
					Name:      "publish", Stage: "publish", Image: "alpine:3.20",
					Variables: map[string]string{"IMAGE_TAG": "${{ needs.bump.outputs.next }}"},
					Tasks:     []domain.Task{{Script: "echo $IMAGE_TAG"}},
					Needs:     []string{"bump"},
				},
			},
		}},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	pipelineID := applyRes.Pipelines[0].PipelineID

	var materialID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM materials WHERE fingerprint = $1`, fp).Scan(&materialID); err != nil {
		t.Fatalf("mat lookup: %v", err)
	}
	runRes, err := s.CreateRunFromModification(ctx, store.CreateRunFromModificationInput{
		PipelineID: pipelineID, MaterialID: materialID,
		Revision: "abc0123456789abc0123456789abc0123456789a", Branch: "main",
		Provider: "github", Delivery: "test-outputs", TriggeredBy: "system:webhook",
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	runID := runRes.RunID

	// Dispatch tick 1: only `bump` is dispatchable (publish gated
	// by needs). The dispatched JobAssignment must carry the
	// `outputs:` declaration so the agent knows to filter the
	// $GOCDNEXT_OUTPUT_FILE against `next` → `NEXT`.
	agentID := seedAgentRow(t, pool, "e2e-agent")
	sess := sessions.CreateSession(agentID, nil, 2, 0)
	sched.DispatchRun(ctx, runID)

	var bumpAssign struct {
		jobID   uuid.UUID
		outputs map[string]string
	}
	select {
	case msg := <-sess.Out():
		assign := msg.GetAssign()
		if assign == nil || assign.GetName() != "bump" {
			t.Fatalf("first dispatch = %+v, want bump JobAssignment", msg)
		}
		bumpAssign.jobID = uuid.MustParse(assign.GetJobId())
		bumpAssign.outputs = assign.GetOutputs()
	case <-time.After(2 * time.Second):
		t.Fatal("no bump assignment delivered")
	}
	// Layer 6 contract — declarations land on the JobAssignment so
	// the agent can filter+rekey the parsed output file.
	if bumpAssign.outputs["next"] != "NEXT" {
		t.Errorf("JobAssignment.Outputs = %v, want next: NEXT", bumpAssign.outputs)
	}

	// Simulate the agent: the plugin would write
	// `NEXT=v1.2.3` to $GOCDNEXT_OUTPUT_FILE; the agent's
	// parseOutputsFile rekeys to alias `next` → `v1.2.3` and
	// ships it in JobResult.outputs. We invoke CompleteJob with
	// that same alias-keyed map (what the gRPC handler would
	// forward after validation).
	if _, ok, err := s.CompleteJob(ctx, store.CompleteJobInput{
		JobRunID:        bumpAssign.jobID,
		Status:          "success",
		ExitCode:        0,
		ExpectedAgentID: agentID,
		ExpectedAttempt: 0,
		Outputs:         map[string]string{"next": "v1.2.3"},
	}); err != nil || !ok {
		t.Fatalf("complete bump: ok=%v err=%v", ok, err)
	}

	// Dispatch tick 2: publish is now gated-satisfied. Its
	// JobAssignment env must contain the resolved IMAGE_TAG.
	sched.DispatchRun(ctx, runID)
	select {
	case msg := <-sess.Out():
		assign := msg.GetAssign()
		if assign == nil || assign.GetName() != "publish" {
			t.Fatalf("second dispatch = %+v, want publish JobAssignment", msg)
		}
		got := assign.GetEnv()["IMAGE_TAG"]
		if got != "v1.2.3" {
			t.Errorf("publish.env[IMAGE_TAG] = %q, want v1.2.3 — outputs round-trip broken", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no publish assignment delivered after bump completed")
	}
}

func TestGroupNeedsOutputs_SingleRowFolds(t *testing.T) {
	// Non-matrix upstream → single row, matrix_key="", fold trivially.
	rows := []store.JobOutputs{
		{Name: "bump", MatrixKey: "", Status: "success", Outputs: map[string]string{"next": "v1.0.0"}},
	}
	got, err := scheduler.GroupNeedsOutputs(rows)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got["bump"]["next"] != "v1.0.0" {
		t.Errorf("got %v, want bump.next=v1.0.0", got)
	}
}

func TestGroupNeedsOutputs_NonSuccessRowsAreDropped(t *testing.T) {
	// A failed/canceled upstream's outputs are NOT promoted into
	// the substitution table. The needsSatisfied gate already
	// blocks the dispatch, but defence in depth — anything that
	// somehow leaks past gate would still fail substitution with
	// the clearer "did not produce" error.
	rows := []store.JobOutputs{
		{Name: "bump", MatrixKey: "", Status: "failed", Outputs: map[string]string{"next": "v1.0.0"}},
	}
	got, err := scheduler.GroupNeedsOutputs(rows)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := got["bump"]; ok {
		t.Errorf("non-success row should not be folded into NeedsOutputs; got %v", got)
	}
}

func TestGroupNeedsOutputs_MatrixAmbiguityRejected(t *testing.T) {
	// Kleber's #6 invariant: > 1 row sharing a name = ambiguous
	// matrix; refuse with LOUD error listing the matrix keys.
	rows := []store.JobOutputs{
		{Name: "build", MatrixKey: "linux-amd64", Status: "success", Outputs: map[string]string{"digest": "sha256:aaa"}},
		{Name: "build", MatrixKey: "linux-arm64", Status: "success", Outputs: map[string]string{"digest": "sha256:bbb"}},
	}
	_, err := scheduler.GroupNeedsOutputs(rows)
	if err == nil {
		t.Fatal("expected matrix-ambiguity rejection")
	}
	// UX: message must cite the upstream name AND every matrix key
	// so the operator immediately sees which instances exist.
	for _, want := range []string{"build", "linux-amd64", "linux-arm64", "ambiguous"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should contain %q, got: %v", want, err)
		}
	}
	// Roadmap hint should be there for the operator's next step.
	if !strings.Contains(err.Error(), "explicit per-row selector") {
		t.Errorf("error should mention the roadmap selector form, got: %v", err)
	}
}

func TestGroupNeedsOutputs_EmptyMatrixKeyMappedToPlaceholder(t *testing.T) {
	// Edge case: two non-matrix rows of the same name (data
	// corruption or a buggy migration). Mode keys would both be
	// ""; we render them as "<empty>" in the message so the
	// operator can spot the duplicate clearly.
	rows := []store.JobOutputs{
		{Name: "dup", MatrixKey: "", Status: "success"},
		{Name: "dup", MatrixKey: "", Status: "success"},
	}
	_, err := scheduler.GroupNeedsOutputs(rows)
	if err == nil {
		t.Fatal("expected duplicate-row rejection")
	}
	if !strings.Contains(err.Error(), "<empty>") {
		t.Errorf("error should render empty matrix-key as <empty>, got: %v", err)
	}
}

func TestBuildAssignment_ResolvesNeedsOutputsInEnv(t *testing.T) {
	// End-to-end: an `${{ needs.bump.outputs.next }}` ref in a
	// downstream's variables: resolves against the NeedsOutputs
	// table the scheduler hands BuildAssignment. The pre-pass
	// runs BEFORE the standard substituteRefs so the result is
	// already plain text by the time secrets/CI vars get checked.
	def := domain.Pipeline{
		Jobs: []domain.Job{
			{
				Name:      "publish",
				Variables: map[string]string{"IMAGE_TAG": "${{ needs.bump.outputs.next }}"},
				Tasks:     []domain.Task{{Script: "echo $IMAGE_TAG"}},
				Needs:     []string{"bump"},
			},
		},
	}
	defJSON, _ := json.Marshal(def)
	run := store.RunForDispatch{ID: uuid.New(), PipelineID: uuid.New(), Definition: defJSON}
	job := store.DispatchableJob{ID: uuid.New(), Name: "publish", Needs: []string{"bump"}}

	needsOutputs := scheduler.NeedsOutputs{
		"bump": {"next": "v1.3.0"},
	}
	got, err := scheduler.BuildAssignment(run, job, nil, nil, nil, store.ResolvedProfile{}, nil, needsOutputs)
	if err != nil {
		t.Fatalf("BuildAssignment: %v", err)
	}
	if got.Env["IMAGE_TAG"] != "v1.3.0" {
		t.Errorf("IMAGE_TAG = %q, want v1.3.0", got.Env["IMAGE_TAG"])
	}
}

func TestBuildAssignment_ResolvesNeedsOutputsInPluginSettings(t *testing.T) {
	// Same shape but the ref is inside a plugin `with:` setting —
	// the per-plugin pre-pass must run too, otherwise plugin jobs
	// would lose access to upstream outputs.
	def := domain.Pipeline{
		Jobs: []domain.Job{
			{
				Name:  "sign",
				Needs: []string{"promote"},
				Tasks: []domain.Task{{
					Plugin: &domain.PluginStep{
						Image: "ghcr.io/x/cosign:v1",
						Settings: map[string]string{
							"image": "ghcr.io/org/app@${{ needs.promote.outputs.digest }}",
						},
					},
				}},
			},
		},
	}
	defJSON, _ := json.Marshal(def)
	run := store.RunForDispatch{ID: uuid.New(), PipelineID: uuid.New(), Definition: defJSON}
	job := store.DispatchableJob{ID: uuid.New(), Name: "sign", Needs: []string{"promote"}}

	needsOutputs := scheduler.NeedsOutputs{
		"promote": {"digest": "sha256:beef1234"},
	}
	got, err := scheduler.BuildAssignment(run, job, nil, nil, nil, store.ResolvedProfile{}, nil, needsOutputs)
	if err != nil {
		t.Fatalf("BuildAssignment: %v", err)
	}
	plug := got.Tasks[0].GetPlugin()
	if got, want := plug.Settings["image"], "ghcr.io/org/app@sha256:beef1234"; got != want {
		t.Errorf("plugin image = %q, want %q", got, want)
	}
}

func TestBuildAssignment_NeedsOutputValuesEnterLogMasks(t *testing.T) {
	// Findings round (Kleber): output values resolved via
	// `${{ needs.X.outputs.Y }}` may unintentionally carry token-
	// like data; the scheduler adds them to LogMasks for defence
	// in depth. Length floor matches the runner's applyMasks
	// (>= 4 there; we use >= 8 to dodge false positives on short
	// version strings).
	def := domain.Pipeline{
		Jobs: []domain.Job{
			{
				Name:      "publish",
				Variables: map[string]string{"TOKEN": "${{ needs.bump.outputs.token }}"},
				Tasks:     []domain.Task{{Script: "echo $TOKEN"}},
				Needs:     []string{"bump"},
			},
		},
	}
	defJSON, _ := json.Marshal(def)
	run := store.RunForDispatch{ID: uuid.New(), PipelineID: uuid.New(), Definition: defJSON}
	job := store.DispatchableJob{ID: uuid.New(), Name: "publish", Needs: []string{"bump"}}

	const longValue = "abcdef1234567890token"
	needsOutputs := scheduler.NeedsOutputs{
		"bump": {
			"token": longValue,
			"short": "v1",  // < 8 chars → must NOT enter masks
		},
	}
	got, err := scheduler.BuildAssignment(run, job, nil, nil, nil, store.ResolvedProfile{}, nil, needsOutputs)
	if err != nil {
		t.Fatalf("BuildAssignment: %v", err)
	}
	foundLong := false
	for _, m := range got.GetLogMasks() {
		if m == longValue {
			foundLong = true
		}
		if m == "v1" {
			t.Errorf("short value 'v1' should NOT enter masks (false-positive risk)")
		}
	}
	if !foundLong {
		t.Errorf("long output value not added to LogMasks; resolved value would leak in downstream logs")
	}
}

func TestBuildAssignment_NeedsRefErrorsWrapSentinel(t *testing.T) {
	// Findings round (Kleber): missing needs refs MUST surface as
	// ErrNeedsRefUnresolved so the scheduler distinguishes
	// CONFIGURATION errors (terminalise the job) from transient
	// build errors (log + continue). The wire-up: refs.go errors
	// wrap the sentinel; scheduler matches `errors.Is`.
	def := domain.Pipeline{
		Jobs: []domain.Job{
			{
				Name:      "publish",
				Variables: map[string]string{"TAG": "${{ needs.bump.outputs.missing }}"},
				Tasks:     []domain.Task{{Script: "echo $TAG"}},
				Needs:     []string{"bump"},
			},
		},
	}
	defJSON, _ := json.Marshal(def)
	run := store.RunForDispatch{ID: uuid.New(), PipelineID: uuid.New(), Definition: defJSON}
	job := store.DispatchableJob{ID: uuid.New(), Name: "publish", Needs: []string{"bump"}}

	needsOutputs := scheduler.NeedsOutputs{"bump": {"next": "v1.3.0"}}
	_, err := scheduler.BuildAssignment(run, job, nil, nil, nil, store.ResolvedProfile{}, nil, needsOutputs)
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, scheduler.ErrNeedsRefUnresolved) {
		t.Errorf("error does not wrap ErrNeedsRefUnresolved (scheduler would loop-queue this job): %v", err)
	}
}

func TestBuildAssignment_MissingNeedsOutputErrors(t *testing.T) {
	// Operator referenced an alias the upstream didn't produce —
	// substituteNeedsRefs errors LOUD with the UX message that
	// cites the upstream + alias. Error must reach the caller
	// intact so the scheduler can fail the job with a useful
	// reason ("did not produce the named output").
	def := domain.Pipeline{
		Jobs: []domain.Job{
			{
				Name:      "publish",
				Variables: map[string]string{"TAG": "${{ needs.bump.outputs.missing }}"},
				Tasks:     []domain.Task{{Script: "echo $TAG"}},
				Needs:     []string{"bump"},
			},
		},
	}
	defJSON, _ := json.Marshal(def)
	run := store.RunForDispatch{ID: uuid.New(), PipelineID: uuid.New(), Definition: defJSON}
	job := store.DispatchableJob{ID: uuid.New(), Name: "publish", Needs: []string{"bump"}}

	needsOutputs := scheduler.NeedsOutputs{
		"bump": {"next": "v1.3.0"}, // declared `next`, NOT `missing`
	}
	_, err := scheduler.BuildAssignment(run, job, nil, nil, nil, store.ResolvedProfile{}, nil, needsOutputs)
	if err == nil {
		t.Fatal("expected error for missing alias")
	}
	// Kleber's UX invariant: message MUST cite the upstream + alias
	// so the operator doesn't chase a confusing failure downstream.
	if !strings.Contains(err.Error(), "needs.bump.outputs.missing") {
		t.Errorf("error should cite needs.bump.outputs.missing, got: %v", err)
	}
	if !strings.Contains(err.Error(), "did not produce") {
		t.Errorf("error should explain the missing-output case, got: %v", err)
	}
}

func TestBuildAssignment_MissingUpstreamJobErrors(t *testing.T) {
	// Operator referenced a job not in `needs:` — caught by the
	// pre-pass with a clear "not in this job's needs:" message.
	def := domain.Pipeline{
		Jobs: []domain.Job{
			{
				Name:      "publish",
				Variables: map[string]string{"TAG": "${{ needs.unknown.outputs.next }}"},
				Tasks:     []domain.Task{{Script: "echo $TAG"}},
				Needs:     []string{"bump"}, // doesn't list `unknown`
			},
		},
	}
	defJSON, _ := json.Marshal(def)
	run := store.RunForDispatch{ID: uuid.New(), PipelineID: uuid.New(), Definition: defJSON}
	job := store.DispatchableJob{ID: uuid.New(), Name: "publish", Needs: []string{"bump"}}

	needsOutputs := scheduler.NeedsOutputs{
		"bump": {"next": "v1.0.0"},
	}
	_, err := scheduler.BuildAssignment(run, job, nil, nil, nil, store.ResolvedProfile{}, nil, needsOutputs)
	if err == nil {
		t.Fatal("expected error for missing upstream job")
	}
	if !strings.Contains(err.Error(), "needs.unknown") {
		t.Errorf("error should cite the offending ref, got: %v", err)
	}
	if !strings.Contains(err.Error(), "needs:") {
		t.Errorf("error should explain the missing-needs case, got: %v", err)
	}
}

// TestBuildAssignment_SubstitutesCIVarsAndShellRefs is the e2e cover
// for v0.4.11: a plugin `with:` value that mixes `${{ SECRET }}`
// (gocdnext-template, strict) and `${CI_*}` (shell-style, soft)
// resolves both at dispatch time. The user's real-world failure was
// `tags: 1.${CI_RUN_COUNTER}.${CI_COMMIT_SHORT_SHA}-gocdnext` reaching
// `docker buildx build` literal — invalid tag reference format.
func TestBuildAssignment_SubstitutesCIVarsAndShellRefs(t *testing.T) {
	def := domain.Pipeline{
		Stages: []string{"publish"},
		Jobs: []domain.Job{{
			Name:    "buildx",
			Stage:   "publish",
			Secrets: []string{"DOCKER_USERNAME"},
			Tasks: []domain.Task{{
				Plugin: &domain.PluginStep{
					Image: "ghcr.io/klinux/gocdnext-plugin-buildx:v1",
					Settings: map[string]string{
						"image":    "registry.example.com/app",
						"username": "${{ DOCKER_USERNAME }}",
						"tags":     "1.${CI_RUN_COUNTER}.${CI_COMMIT_SHORT_SHA}-gocdnext",
						"branch":   "${CI_BRANCH}",
					},
				},
			}},
		}},
	}
	defJSON, _ := json.Marshal(def)
	materialID := uuid.New()
	run := store.RunForDispatch{
		ID:         uuid.New(),
		PipelineID: uuid.New(),
		ProjectID:  uuid.New(),
		Counter:    42,
		Definition: defJSON,
		Revisions: json.RawMessage(`{"` + materialID.String() +
			`":{"revision":"f5b5f8a66a753e4fc64fc80ec518ad27be57e75c","branch":"gocdnext-tests"}}`),
	}
	job := store.DispatchableJob{ID: uuid.New(), Name: "buildx"}

	got, err := scheduler.BuildAssignment(run, job, nil,
		map[string]string{"DOCKER_USERNAME": "deploybot"},
		nil, store.ResolvedProfile{}, nil, nil)
	if err != nil {
		t.Fatalf("BuildAssignment: %v", err)
	}
	plug := got.Tasks[0].GetPlugin()
	want := map[string]string{
		"image":    "registry.example.com/app",
		"username": "deploybot",
		"tags":     "1.42.f5b5f8a6-gocdnext",
		"branch":   "gocdnext-tests",
	}
	for k, v := range want {
		if plug.Settings[k] != v {
			t.Errorf("settings[%q] = %q, want %q", k, plug.Settings[k], v)
		}
	}
	// CI vars must also flow into the container env so script tasks
	// in the same job can `echo $CI_COMMIT_SHORT_SHA` natively.
	if got.Env["CI_COMMIT_SHORT_SHA"] != "f5b5f8a6" {
		t.Errorf("CI_COMMIT_SHORT_SHA missing from env: %v", got.Env["CI_COMMIT_SHORT_SHA"])
	}
	if got.Env["CI_RUN_COUNTER"] != "42" {
		t.Errorf("CI_RUN_COUNTER missing from env: %v", got.Env["CI_RUN_COUNTER"])
	}
}

func TestBuildAssignment_CloneTokenRewritesURLAndMasks(t *testing.T) {
	// The dispatch path mints an installation-scoped token for the
	// material's URL and hands it to BuildAssignment in cloneTokens.
	// The checkout URL must come out with the token embedded in the
	// https://x-access-token:TOKEN@host form so plain `git clone`
	// picks it up, AND the token must land in LogMasks so the agent
	// redacts it from the `$ git clone ...` echo and any error trail.
	def := domain.Pipeline{
		Stages: []string{"build"},
		Jobs:   []domain.Job{{Name: "compile", Stage: "build", Tasks: []domain.Task{{Script: "true"}}}},
	}
	defJSON, _ := json.Marshal(def)
	materialID := uuid.New()
	run := store.RunForDispatch{
		ID:         uuid.New(),
		PipelineID: uuid.New(),
		Definition: defJSON,
		Revisions: json.RawMessage(`{"` + materialID.String() +
			`":{"revision":"deadbeef","branch":"main"}}`),
	}
	job := store.DispatchableJob{ID: uuid.New(), Name: "compile", Image: "alpine"}
	gitCfg, _ := json.Marshal(domain.GitMaterial{
		URL:    "https://github.com/acme-org/private-repo",
		Branch: "main",
	})
	materials := []store.Material{{ID: materialID, Type: string(domain.MaterialGit), Config: gitCfg}}
	cloneTokens := map[string]string{materialID.String(): "ghs_fake_install_token"}

	got, err := scheduler.BuildAssignment(run, job, materials, nil, nil, store.ResolvedProfile{}, cloneTokens, nil)
	if err != nil {
		t.Fatalf("BuildAssignment: %v", err)
	}
	if len(got.Checkouts) != 1 {
		t.Fatalf("checkouts = %+v", got.Checkouts)
	}
	wantURL := "https://x-access-token:ghs_fake_install_token@github.com/acme-org/private-repo"
	if got.Checkouts[0].Url != wantURL {
		t.Errorf("checkout url = %q, want %q", got.Checkouts[0].Url, wantURL)
	}
	var masked bool
	for _, m := range got.LogMasks {
		if m == "ghs_fake_install_token" {
			masked = true
			break
		}
	}
	if !masked {
		t.Errorf("token not in LogMasks: %v", got.LogMasks)
	}
}

func TestBuildAssignment_NoTokenLeavesURLUntouched(t *testing.T) {
	// Public-repo path: no token in cloneTokens for this material →
	// URL passes through verbatim, no LogMasks entry added.
	def := domain.Pipeline{
		Stages: []string{"build"},
		Jobs:   []domain.Job{{Name: "compile", Stage: "build", Tasks: []domain.Task{{Script: "true"}}}},
	}
	defJSON, _ := json.Marshal(def)
	materialID := uuid.New()
	run := store.RunForDispatch{ID: uuid.New(), PipelineID: uuid.New(), Definition: defJSON,
		Revisions: json.RawMessage(`{"` + materialID.String() + `":{"revision":"abc","branch":"main"}}`)}
	job := store.DispatchableJob{ID: uuid.New(), Name: "compile", Image: "alpine"}
	gitCfg, _ := json.Marshal(domain.GitMaterial{URL: "https://github.com/octocat/hello-world", Branch: "main"})
	materials := []store.Material{{ID: materialID, Type: string(domain.MaterialGit), Config: gitCfg}}

	got, err := scheduler.BuildAssignment(run, job, materials, nil, nil, store.ResolvedProfile{}, nil, nil)
	if err != nil {
		t.Fatalf("BuildAssignment: %v", err)
	}
	if got.Checkouts[0].Url != "https://github.com/octocat/hello-world" {
		t.Errorf("url mutated despite no token: %q", got.Checkouts[0].Url)
	}
	for _, m := range got.LogMasks {
		if m == "" {
			t.Errorf("empty mask leaked into LogMasks")
		}
	}
}

func TestBuildAssignment_PropagatesPipelineServices(t *testing.T) {
	// Pipeline-level services must ride through the assignment so
	// the agent can stand them up on a job-scoped docker network
	// before tasks run. Empty services → nil on the wire (the
	// agent's fast path that skips network plumbing entirely).
	def := domain.Pipeline{
		Stages: []string{"test"},
		Jobs: []domain.Job{
			{Name: "integration", Stage: "test", Tasks: []domain.Task{{Script: "go test"}}},
		},
		Services: []domain.Service{
			{Name: "postgres", Image: "postgres:16-alpine",
				Env: map[string]string{"POSTGRES_PASSWORD": "test"}},
			{Name: "cache", Image: "redis:7",
				Command: []string{"redis-server", "--appendonly", "no"}},
		},
	}
	defJSON, _ := json.Marshal(def)
	run := store.RunForDispatch{
		ID: uuid.New(), PipelineID: uuid.New(), Definition: defJSON,
		Revisions: json.RawMessage(`{}`),
	}
	job := store.DispatchableJob{ID: uuid.New(), Name: "integration", Image: "golang:1.25"}

	got, err := scheduler.BuildAssignment(run, job, nil, nil, nil, store.ResolvedProfile{}, nil, nil)
	if err != nil {
		t.Fatalf("BuildAssignment: %v", err)
	}
	if len(got.Services) != 2 {
		t.Fatalf("services = %d, want 2", len(got.Services))
	}
	if got.Services[0].Name != "postgres" ||
		got.Services[0].Image != "postgres:16-alpine" ||
		got.Services[0].Env["POSTGRES_PASSWORD"] != "test" {
		t.Errorf("postgres spec = %+v", got.Services[0])
	}
	if got.Services[1].Name != "cache" ||
		len(got.Services[1].Command) != 3 ||
		got.Services[1].Command[0] != "redis-server" {
		t.Errorf("cache spec = %+v", got.Services[1])
	}
}

func TestBuildAssignment_NoServicesWhenPipelineHasNone(t *testing.T) {
	// Fast path: len(def.Services)==0 → nil on the wire so engines
	// that skip service plumbing don't pay an empty-slice penalty.
	def := domain.Pipeline{
		Stages: []string{"build"},
		Jobs:   []domain.Job{{Name: "compile", Stage: "build", Tasks: []domain.Task{{Script: "make"}}}},
	}
	defJSON, _ := json.Marshal(def)
	run := store.RunForDispatch{
		ID: uuid.New(), PipelineID: uuid.New(), Definition: defJSON,
		Revisions: json.RawMessage(`{}`),
	}
	job := store.DispatchableJob{ID: uuid.New(), Name: "compile"}
	got, err := scheduler.BuildAssignment(run, job, nil, nil, nil, store.ResolvedProfile{}, nil, nil)
	if err != nil {
		t.Fatalf("BuildAssignment: %v", err)
	}
	if got.Services != nil {
		t.Errorf("services should be nil when empty, got %+v", got.Services)
	}
}

// TestRun_DispatchesOnAgentRegister exercises the SessionStore hook:
// a run sits queued because no agent was online when it was created,
// then an agent registers and the scheduler dispatches it without
// waiting for the next periodic tick. The tick is set long here so
// a failure mode where the fix only works via the tick fallback
// would time out.
func TestRun_DispatchesOnAgentRegister(t *testing.T) {
	pool := dbtest.SetupPool(t)
	dsn := dbtest.DSN()
	s := store.New(pool)
	sessions := grpcsrv.NewSessionStore()
	sched := scheduler.New(s, sessions, quietLogger(), dsn).
		WithTickInterval(30 * time.Second) // too long to save the test

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Seed a run BEFORE any agent is registered.
	seed(t, pool)

	done := make(chan error, 1)
	go func() { done <- sched.Run(ctx) }()

	// Let the scheduler finish its priming drainQueued — it'll find
	// the queued run but no agent, log "leaving queued", and wait.
	time.Sleep(300 * time.Millisecond)

	// Now bring an agent online; the ready-hook should kick the
	// scheduler into an immediate drainQueued.
	agentID := seedAgentRow(t, pool, "late-agent")
	sess := sessions.CreateSession(agentID, nil, 1, 0)

	select {
	case msg := <-sess.Out():
		if msg.GetAssign() == nil {
			t.Fatalf("unexpected message: %+v", msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("no assignment after agent register within 2s")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("scheduler did not stop after ctx cancel")
	}
}

// TestRun_ReactsToNotify exercises the LISTEN loop: NOTIFY fires, scheduler
// picks up the run, dispatches. Uses the dbtest DSN so the LISTEN connection
// sees commits from the same cluster.
func TestRun_ReactsToNotify(t *testing.T) {
	pool := dbtest.SetupPool(t)
	dsn := dbtest.DSN()
	s := store.New(pool)
	sessions := grpcsrv.NewSessionStore()
	sched := scheduler.New(s, sessions, quietLogger(), dsn).WithTickInterval(500 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	agentID := seedAgentRow(t, pool, "agent-1")
	sess := sessions.CreateSession(agentID, nil, 1, 0)

	done := make(chan error, 1)
	go func() { done <- sched.Run(ctx) }()

	// Give the scheduler a moment to LISTEN before we fire the NOTIFY via
	// a fresh run (CreateRunFromModification emits it).
	time.Sleep(200 * time.Millisecond)
	seed(t, pool)

	select {
	case msg := <-sess.Out():
		if msg.GetAssign() == nil {
			t.Fatalf("unexpected message: %+v", msg)
		}
	case <-time.After(4 * time.Second):
		t.Fatalf("no assignment after NOTIFY within 4s")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("scheduler did not stop after ctx cancel")
	}
}

// TestDispatchRun_SerialPipelineWaitsForBusyRun exercises the
// concurrency: serial gate. An earlier run is parked in 'running'
// and a fresh one is created for the same pipeline — the scheduler
// must leave it queued instead of dispatching its jobs in parallel.
func TestDispatchRun_SerialPipelineWaitsForBusyRun(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	sessions := grpcsrv.NewSessionStore()
	sched := scheduler.New(s, sessions, quietLogger(), testDSN)
	ctx := context.Background()

	fp := domain.GitFingerprint("https://github.com/org/demo", "main")
	applyRes, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: "demo", Name: "Demo",
		Pipelines: []*domain.Pipeline{{
			Name:        "deploy",
			Stages:      []string{"deploy"},
			Concurrency: domain.ConcurrencySerial,
			Materials: []domain.Material{{
				Type: domain.MaterialGit, Fingerprint: fp, AutoUpdate: true,
				Git: &domain.GitMaterial{
					URL: "https://github.com/org/demo", Branch: "main",
					Events: []string{"push"},
				},
			}},
			Jobs: []domain.Job{{
				Name: "apply", Stage: "deploy", Image: "alpine:3.19",
				Tasks: []domain.Task{{Script: "echo deploy"}},
			}},
		}},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	pipelineID := applyRes.Pipelines[0].PipelineID

	var materialID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM materials WHERE fingerprint=$1`, fp).Scan(&materialID); err != nil {
		t.Fatalf("mat lookup: %v", err)
	}

	// Run #1: mark it running by hand — stand-in for "the previous
	// trigger is still executing".
	first, err := s.CreateRunFromModification(ctx, store.CreateRunFromModificationInput{
		PipelineID:  pipelineID,
		MaterialID:  materialID,
		Revision:    "aaa0123456789aaa0123456789aaa0123456789a",
		Branch:      "main",
		Provider:    "github",
		Delivery:    "t-1",
		TriggeredBy: "system:webhook",
	})
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if err := s.MarkRunRunning(ctx, first.RunID); err != nil {
		t.Fatalf("mark running: %v", err)
	}

	// Run #2: should queue behind the running one.
	second, err := s.CreateRunFromModification(ctx, store.CreateRunFromModificationInput{
		PipelineID:  pipelineID,
		MaterialID:  materialID,
		Revision:    "bbb0123456789bbb0123456789bbb0123456789b",
		Branch:      "main",
		Provider:    "github",
		Delivery:    "t-2",
		TriggeredBy: "system:webhook",
	})
	if err != nil {
		t.Fatalf("second run: %v", err)
	}

	agentID := seedAgentRow(t, pool, "serial-agent")
	sess := sessions.CreateSession(agentID, nil, 2, 0)

	sched.DispatchRun(ctx, second.RunID)

	select {
	case msg := <-sess.Out():
		t.Fatalf("expected no dispatch while busy, got: %+v", msg)
	case <-time.After(200 * time.Millisecond):
		// good — stayed queued
	}

	// runs.queue_reason must be stamped so the UI can render
	// "waiting on #N". Issue #4 path #2: without this surface, a
	// queued run looks identical to "scheduler isn't ticking" to
	// the operator.
	var reason *string
	if err := pool.QueryRow(ctx,
		`SELECT queue_reason FROM runs WHERE id=$1`, second.RunID,
	).Scan(&reason); err != nil {
		t.Fatalf("read queue_reason: %v", err)
	}
	wantReason := "serial-busy:" + first.RunID.String()
	if reason == nil || *reason != wantReason {
		got := "<nil>"
		if reason != nil {
			got = *reason
		}
		t.Fatalf("queue_reason = %q, want %q", got, wantReason)
	}

	// Unblock: mark first as finished. Dispatching again should
	// now deliver the job to the agent without further wait.
	if _, err := pool.Exec(ctx, `UPDATE runs SET status='success', finished_at=NOW() WHERE id=$1`, first.RunID); err != nil {
		t.Fatalf("finish first: %v", err)
	}
	sched.DispatchRun(ctx, second.RunID)
	select {
	case msg := <-sess.Out():
		if msg.GetAssign() == nil {
			t.Fatalf("unexpected message: %+v", msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("no dispatch after previous run finished")
	}

	// Past the gate — queue_reason must be cleared so the run
	// detail doesn't keep advertising a "waiting on …" message
	// after the run is actually proceeding.
	if err := pool.QueryRow(ctx,
		`SELECT queue_reason FROM runs WHERE id=$1`, second.RunID,
	).Scan(&reason); err != nil {
		t.Fatalf("read queue_reason after dispatch: %v", err)
	}
	if reason != nil {
		t.Fatalf("queue_reason = %q after dispatch, want NULL (cleared)", *reason)
	}
}

// seedSameStageNeeds creates a pipeline with TWO jobs in the SAME
// stage where the second declares `needs: [first]`. Mirrors the
// Same-stage needs regression: `build needs: [types-generate]` in the same
// stage. Returns the run id + job_run ids for the two jobs so the
// caller can drive CompleteJob between dispatch ticks.
func seedSameStageNeeds(t *testing.T, pool *pgxpool.Pool) (runID, prepJobID, dependentJobID uuid.UUID) {
	t.Helper()
	s := store.New(pool)
	ctx := context.Background()

	fp := domain.GitFingerprint("https://github.com/org/needs", "main")
	applyRes, err := s.ApplyProject(ctx, store.ApplyProjectInput{
		Slug: "needs", Name: "Needs Demo",
		Pipelines: []*domain.Pipeline{{
			Name:   "ci",
			Stages: []string{"build"},
			Materials: []domain.Material{{
				Type: domain.MaterialGit, Fingerprint: fp, AutoUpdate: true,
				Git: &domain.GitMaterial{URL: "https://github.com/org/needs", Branch: "main", Events: []string{"push"}},
			}},
			Jobs: []domain.Job{
				{Name: "prep", Stage: "build", Image: "golang:1.23", Tasks: []domain.Task{{Script: "make prep"}}},
				{Name: "dependent", Stage: "build", Image: "golang:1.23", Tasks: []domain.Task{{Script: "make build"}}, Needs: []string{"prep"}},
			},
		}},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	pipelineID := applyRes.Pipelines[0].PipelineID

	var matID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM materials WHERE fingerprint = $1`, fp).Scan(&matID); err != nil {
		t.Fatalf("mat lookup: %v", err)
	}

	runRes, err := s.CreateRunFromModification(ctx, store.CreateRunFromModificationInput{
		PipelineID:  pipelineID,
		MaterialID:  matID,
		Revision:    "abc0123456789abc0123456789abc0123456789a",
		Branch:      "main",
		Provider:    "github",
		Delivery:    "test-needs",
		TriggeredBy: "system:webhook",
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	runID = runRes.RunID

	if err := pool.QueryRow(ctx,
		`SELECT id FROM job_runs WHERE run_id=$1 AND name='prep'`, runID,
	).Scan(&prepJobID); err != nil {
		t.Fatalf("prep job lookup: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT id FROM job_runs WHERE run_id=$1 AND name='dependent'`, runID,
	).Scan(&dependentJobID); err != nil {
		t.Fatalf("dependent job lookup: %v", err)
	}
	return runID, prepJobID, dependentJobID
}

// TestDispatchRun_NeedsGate_WaitsForSameStageUpstream is the
// REGRESSION GUARD for same-stage ordering: jobs in the same stage
// that declare `needs:` must NOT both dispatch concurrently. Before
// the gate, `dependent` would dispatch alongside `prep`, fail
// `resolveArtifactDeps` (upstream hadn't produced), and be marked
// `failed` permanently. With the gate, only `prep` dispatches;
// `dependent` stays queued, error column empty.
func TestDispatchRun_NeedsGate_WaitsForSameStageUpstream(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	sessions := grpcsrv.NewSessionStore()
	sched := scheduler.New(s, sessions, quietLogger(), testDSN)
	ctx := context.Background()

	runID, prepJobID, dependentJobID := seedSameStageNeeds(t, pool)
	// 2 capacity so the test isn't accidentally gated by agent
	// availability — we want to prove the NEEDS gate held back the
	// dependent, not the agent gate.
	agentID := seedAgentRow(t, pool, "agent-1")
	sess := sessions.CreateSession(agentID, nil, 2, 0)

	sched.DispatchRun(ctx, runID)

	// Drain assignments — there must be exactly 1 (prep), not 2.
	count := 0
	names := []string{}
drain:
	for {
		select {
		case msg, ok := <-sess.Out():
			if !ok {
				break drain
			}
			if a := msg.GetAssign(); a != nil {
				count++
				names = append(names, a.Name)
			}
		case <-time.After(200 * time.Millisecond):
			break drain
		}
	}
	if count != 1 {
		t.Fatalf("dispatched %d assignments %v, want 1 (only prep — needs gate must hold dependent)",
			count, names)
	}
	if names[0] != "prep" {
		t.Fatalf("dispatched %q, want prep", names[0])
	}

	// dependent must stay queued, error column NULL (not skipped, not failed).
	var status string
	var errMsg *string
	if err := pool.QueryRow(ctx,
		`SELECT status, error FROM job_runs WHERE id=$1`, dependentJobID,
	).Scan(&status, &errMsg); err != nil {
		t.Fatalf("read dependent: %v", err)
	}
	if status != "queued" {
		t.Fatalf("dependent status = %q, want queued", status)
	}
	if errMsg != nil && *errMsg != "" {
		t.Errorf("dependent error = %q, want empty (gate should NOT stamp an error on healthy wait)", *errMsg)
	}

	// prep is now running (its assignment landed).
	var prepStatus string
	_ = pool.QueryRow(ctx, `SELECT status FROM job_runs WHERE id=$1`, prepJobID).Scan(&prepStatus)
	if prepStatus != "running" {
		t.Errorf("prep status = %q, want running", prepStatus)
	}
}

// TestDispatchRun_NeedsGate_DispatchesAfterUpstreamSucceeds proves
// the gate releases when the upstream lands in terminal-success.
// Models the dispatch tick that fires on `job_completed` NOTIFY.
func TestDispatchRun_NeedsGate_DispatchesAfterUpstreamSucceeds(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	sessions := grpcsrv.NewSessionStore()
	sched := scheduler.New(s, sessions, quietLogger(), testDSN)
	ctx := context.Background()

	runID, prepJobID, dependentJobID := seedSameStageNeeds(t, pool)
	agentID := seedAgentRow(t, pool, "agent-1")
	sess := sessions.CreateSession(agentID, nil, 2, 0)

	// Tick 1: prep dispatches, dependent stays queued.
	sched.DispatchRun(ctx, runID)
	// Drain the prep assignment so the channel doesn't back up.
drainPrep:
	for {
		select {
		case <-sess.Out():
		case <-time.After(100 * time.Millisecond):
			break drainPrep
		}
	}

	// Simulate prep completing successfully — same path the agent's
	// JobResult would drive via grpcsrv.handleJobResult.
	if _, _, err := s.CompleteJob(ctx, store.CompleteJobInput{
		JobRunID:        prepJobID,
		Status:          string(domain.StatusSuccess),
		ExitCode:        0,
		ExpectedAgentID: agentID,
		ExpectedAttempt: 0,
	}); err != nil {
		t.Fatalf("complete prep: %v", err)
	}

	// Tick 2: dependent should now dispatch.
	sched.DispatchRun(ctx, runID)

	select {
	case msg := <-sess.Out():
		a := msg.GetAssign()
		if a == nil {
			t.Fatalf("expected JobAssignment, got %+v", msg)
		}
		if a.Name != "dependent" {
			t.Fatalf("dispatched %q, want dependent", a.Name)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("dependent never dispatched after prep succeeded")
	}

	var depStatus string
	_ = pool.QueryRow(ctx, `SELECT status FROM job_runs WHERE id=$1`, dependentJobID).Scan(&depStatus)
	if depStatus != "running" {
		t.Errorf("dependent status = %q, want running", depStatus)
	}
}

// TestDispatchRun_NeedsGate_FailsWhenUpstreamFailed proves the
// cascade path: when an upstream lands in terminal-failed, the
// dependent must be FAILED (not skipped, not stuck queued forever)
// so the stage cascade counts it toward run failure and the run
// terminates as failed. Was previously asserting status='skipped';
// changed to 'failed' as part of the silent-green closure — see
// FailJobWithReason / failJobNeedsUnmet.
func TestDispatchRun_NeedsGate_FailsWhenUpstreamFailed(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	sessions := grpcsrv.NewSessionStore()
	sched := scheduler.New(s, sessions, quietLogger(), testDSN)
	ctx := context.Background()

	runID, prepJobID, dependentJobID := seedSameStageNeeds(t, pool)
	agentID := seedAgentRow(t, pool, "agent-1")
	sess := sessions.CreateSession(agentID, nil, 2, 0)

	// Tick 1: prep dispatches.
	sched.DispatchRun(ctx, runID)
drainPrep:
	for {
		select {
		case <-sess.Out():
		case <-time.After(100 * time.Millisecond):
			break drainPrep
		}
	}

	// prep fails.
	if _, _, err := s.CompleteJob(ctx, store.CompleteJobInput{
		JobRunID:        prepJobID,
		Status:          string(domain.StatusFailed),
		ExitCode:        1,
		ErrorMsg:        "boom",
		ExpectedAgentID: agentID,
		ExpectedAttempt: 0,
	}); err != nil {
		t.Fatalf("fail prep: %v", err)
	}

	// Tick 2: gate cascades — dependent is FAILED (not skipped,
	// not dispatched), with an error naming the upstream.
	sched.DispatchRun(ctx, runID)

	// No assignment should be sent — assert by waiting briefly then
	// checking the channel is empty.
	select {
	case msg := <-sess.Out():
		if a := msg.GetAssign(); a != nil {
			t.Fatalf("unexpected assignment dispatched: %q", a.Name)
		}
	case <-time.After(150 * time.Millisecond):
		// good — nothing dispatched
	}

	var status string
	var errMsg *string
	if err := pool.QueryRow(ctx,
		`SELECT status, error FROM job_runs WHERE id=$1`, dependentJobID,
	).Scan(&status, &errMsg); err != nil {
		t.Fatalf("read dependent: %v", err)
	}
	if status != "failed" {
		t.Fatalf("dependent status = %q, want failed (cascade)", status)
	}
	if errMsg == nil || !strings.Contains(*errMsg, "needs unmet") {
		t.Errorf("dependent error = %v, want containing 'needs unmet'", errMsg)
	}
	if errMsg != nil && !strings.Contains(*errMsg, "prep") {
		t.Errorf("dependent error = %q, want naming the failed upstream 'prep'", *errMsg)
	}

	// Run must finalize as `failed` (not `success`): two paths
	// could reach this — prep was failed (already counted toward
	// the aggregate) OR dependent was failed via the cascade.
	// Either way the run can't be silent-green.
	var runStatus string
	_ = pool.QueryRow(ctx, `SELECT status FROM runs WHERE id=$1`, runID).Scan(&runStatus)
	if runStatus != "failed" {
		t.Errorf("run status = %q, want failed", runStatus)
	}
}

// TestDispatchRun_NeedsGate_FailsRunOnGhostUpstream is the
// silent-green guard for the snapshot-drift scenario. The parser
// rejects `needs: [ghost]` at apply time (see TestValidateNeeds
// in the parser package), but a snapshot drift — older parser
// accepted it, schema changed, manual DB poke — could still
// produce a runtime needs-pointing-at-nothing. This test bypasses
// the parser by writing a bad `needs:` value DIRECTLY into the
// job_runs row, then proves the dispatch gate STILL closes the
// silent-green path: dependent is failed (not skipped), stage
// fails, run fails. Without this defense, the run would finalize
// as success — confusing fanout, `on: success` notifications,
// webhook listeners, and the operator's UI.
func TestDispatchRun_NeedsGate_FailsRunOnGhostUpstream(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	sessions := grpcsrv.NewSessionStore()
	sched := scheduler.New(s, sessions, quietLogger(), testDSN)
	ctx := context.Background()

	// Seed a normal run with prep + dependent (dependent originally
	// needs: [prep]), then OVERWRITE dependent's needs to reference
	// a job that doesn't exist in this run. Simulates the snapshot
	// drift the parser-validation alone can't catch post-apply.
	runID, prepJobID, dependentJobID := seedSameStageNeeds(t, pool)

	if _, err := pool.Exec(ctx,
		`UPDATE job_runs SET needs = $1 WHERE id = $2`,
		[]string{"ghost-job"}, dependentJobID,
	); err != nil {
		t.Fatalf("poke ghost needs: %v", err)
	}

	agentID := seedAgentRow(t, pool, "agent-1")
	sess := sessions.CreateSession(agentID, nil, 2, 0)

	// First tick: prep dispatches (its needs are still empty).
	// dependent's needs={ghost-job} → terminal-not-in-run →
	// dependent gets failed immediately by the gate. Note both
	// happen in the same tick: the gate runs BEFORE the agent
	// lookup, so even though dependent is iterated alongside prep,
	// the gate routes it to failJobNeedsUnmet without consuming an
	// agent slot.
	sched.DispatchRun(ctx, runID)
	// Drain prep's assignment.
drainPrep:
	for {
		select {
		case <-sess.Out():
		case <-time.After(100 * time.Millisecond):
			break drainPrep
		}
	}

	// dependent must be failed with the ghost reason.
	var status string
	var errMsg *string
	if err := pool.QueryRow(ctx,
		`SELECT status, error FROM job_runs WHERE id=$1`, dependentJobID,
	).Scan(&status, &errMsg); err != nil {
		t.Fatalf("read dependent: %v", err)
	}
	if status != "failed" {
		t.Fatalf("dependent status = %q, want failed (ghost upstream MUST be a failure, not silent-green)", status)
	}
	if errMsg == nil || !strings.Contains(*errMsg, "not in this run") {
		t.Errorf("dependent error = %v, want containing 'not in this run'", errMsg)
	}
	if errMsg != nil && !strings.Contains(*errMsg, "ghost-job") {
		t.Errorf("dependent error = %q, want naming 'ghost-job'", *errMsg)
	}

	// Complete prep successfully so the run can finalize.
	if _, _, err := s.CompleteJob(ctx, store.CompleteJobInput{
		JobRunID:        prepJobID,
		Status:          string(domain.StatusSuccess),
		ExitCode:        0,
		ExpectedAgentID: agentID,
		ExpectedAttempt: 0,
	}); err != nil {
		t.Fatalf("succeed prep: %v", err)
	}

	// CRITICAL: run must NOT finalize as success despite prep
	// succeeding. dependent was failed via the ghost gate, and
	// the aggregate counts it. The whole point of this test is
	// that "ghost needs" can never produce silent-green.
	var runStatus string
	_ = pool.QueryRow(ctx, `SELECT status FROM runs WHERE id=$1`, runID).Scan(&runStatus)
	if runStatus != "failed" {
		t.Errorf("run status = %q, want failed (ghost needs must count toward run failure — silent-green is the bug we're guarding)", runStatus)
	}
}
