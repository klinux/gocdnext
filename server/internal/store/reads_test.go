package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gocdnext/gocdnext/server/internal/dbtest"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

func TestListProjects_EmptyWhenNoProjects(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	got, err := s.ListProjects(ctx)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("want empty, got %d", len(got))
	}
}

func TestListProjects_CountsAndLatestRun(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	pipelineID, materialID, _ := seedPipeline(t, pool, false)
	if _, err := s.CreateRunFromModification(ctx, baseTriggerInput(pipelineID, materialID, 1)); err != nil {
		t.Fatalf("create run: %v", err)
	}

	got, err := s.ListProjects(ctx)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 project, got %d", len(got))
	}
	p := got[0]
	if p.Slug != "demo" || p.Name != "Demo" {
		t.Fatalf("project = %+v", p)
	}
	if p.PipelineCount != 1 {
		t.Fatalf("PipelineCount = %d, want 1", p.PipelineCount)
	}
	if p.LatestRunAt == nil {
		t.Fatalf("LatestRunAt should be set after first run")
	}
	if time.Since(*p.LatestRunAt) > time.Minute {
		t.Fatalf("LatestRunAt looks stale: %v", p.LatestRunAt)
	}

	// After a run is created the preview should surface it so the
	// projects page card can render the status node instead of the
	// grey "never run" pill. Regression guard for the list-card vs
	// detail-card parity the user called out.
	if len(p.TopPipelines) != 1 {
		t.Fatalf("TopPipelines = %d, want 1", len(p.TopPipelines))
	}
	tp := p.TopPipelines[0]
	if tp.LatestRunStatus == "" {
		t.Fatalf("TopPipelines[0].LatestRunStatus is empty — run wasn't attached")
	}
	if len(tp.LatestRunStages) == 0 {
		t.Fatalf("TopPipelines[0].LatestRunStages is empty — stage_runs weren't attached")
	}
}

func TestGetProjectDetail_NotFound(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)

	_, err := s.GetProjectDetail(context.Background(), "nope", 10)
	if !errors.Is(err, store.ErrProjectNotFound) {
		t.Fatalf("err = %v, want ErrProjectNotFound", err)
	}
}

func TestGetProjectDetail_ReturnsPipelinesAndRuns(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	pipelineID, materialID, _ := seedPipeline(t, pool, false)
	run1, err := s.CreateRunFromModification(ctx, baseTriggerInput(pipelineID, materialID, 1))
	if err != nil {
		t.Fatalf("run1: %v", err)
	}
	in2 := baseTriggerInput(pipelineID, materialID, 2)
	in2.Revision = "b111111111111111111111111111111111111111"
	if _, err := s.CreateRunFromModification(ctx, in2); err != nil {
		t.Fatalf("run2: %v", err)
	}

	got, err := s.GetProjectDetail(ctx, "demo", 10)
	if err != nil {
		t.Fatalf("GetProjectDetail: %v", err)
	}
	if got.Project.Slug != "demo" {
		t.Fatalf("project slug = %q", got.Project.Slug)
	}
	if len(got.Pipelines) != 1 || got.Pipelines[0].Name != "build" {
		t.Fatalf("pipelines = %+v", got.Pipelines)
	}
	if len(got.Runs) != 2 {
		t.Fatalf("runs = %d, want 2", len(got.Runs))
	}
	// Most recent first — run2 was second insert, so it should lead.
	if got.Runs[0].Counter != 2 || got.Runs[1].Counter != 1 {
		t.Fatalf("run order = %+v", got.Runs)
	}
	if got.Runs[1].ID != run1.RunID {
		t.Fatalf("run ids mismatch")
	}
}

func TestGetProjectDetail_LatestRunMetaCarriesPRRef(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	pipelineID, materialID, _ := seedPipeline(t, pool, false)

	// A pull_request run stamps cause + cause_detail.pr_number. The
	// pipeline card reads these through LatestRunMeta to render a PR
	// reference ("PR #1135") instead of a branch icon.
	in := baseTriggerInput(pipelineID, materialID, 1)
	in.Cause = "pull_request"
	in.CauseDetail = json.RawMessage(`{"pr_number": 1135}`)
	if _, err := s.CreateRunFromModification(ctx, in); err != nil {
		t.Fatalf("create PR run: %v", err)
	}

	got, err := s.GetProjectDetail(ctx, "demo", 10)
	if err != nil {
		t.Fatalf("GetProjectDetail: %v", err)
	}
	if len(got.Pipelines) != 1 {
		t.Fatalf("pipelines = %d, want 1", len(got.Pipelines))
	}
	meta := got.Pipelines[0].LatestRunMeta
	if meta == nil {
		t.Fatal("LatestRunMeta is nil — PR run produced no meta row")
	}
	if meta.Cause != "pull_request" {
		t.Fatalf("Cause = %q, want pull_request", meta.Cause)
	}
	if meta.PRNumber != 1135 {
		t.Fatalf("PRNumber = %d, want 1135", meta.PRNumber)
	}
	// Branch still flows so the tooltip can show the head ref.
	if meta.Branch != "main" {
		t.Fatalf("Branch = %q, want main", meta.Branch)
	}
}

func TestGetProjectDetail_LatestRunMetaNonPRHasZeroPRNumber(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	pipelineID, materialID, _ := seedPipeline(t, pool, false)

	// A plain webhook run carries no pr_number. COALESCE(...,0) must
	// keep the scan from blowing up on the NULL JSON path and leave
	// PRNumber at 0 so the card falls back to the branch icon.
	if _, err := s.CreateRunFromModification(ctx, baseTriggerInput(pipelineID, materialID, 1)); err != nil {
		t.Fatalf("create webhook run: %v", err)
	}

	got, err := s.GetProjectDetail(ctx, "demo", 10)
	if err != nil {
		t.Fatalf("GetProjectDetail: %v", err)
	}
	meta := got.Pipelines[0].LatestRunMeta
	if meta == nil {
		t.Fatal("LatestRunMeta is nil")
	}
	if meta.PRNumber != 0 {
		t.Fatalf("PRNumber = %d, want 0 for non-PR run", meta.PRNumber)
	}
	if meta.Cause == "pull_request" {
		t.Fatalf("Cause = %q, should not be pull_request", meta.Cause)
	}
}

func TestGetProjectDetail_LatestRunMetaPRNumberIsSanitised(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	pipelineID, materialID, _ := seedPipeline(t, pool, false)

	// A bad cause_detail.pr_number must never reach the int4 cast raw:
	// "abc" would be an invalid-syntax error and a 10-digit value an
	// out-of-range error, either of which would 500 the cards. The
	// regex+CASE guard maps all of these to 0. Each case appends a
	// newer run so it becomes the pipeline's latest, exercising the
	// same query against fresh JSON each time.
	cases := []struct {
		name   string
		detail string
		rev    string
		modID  int64
		wantPR int
	}{
		{"non_numeric", `{"pr_number":"abc"}`, "a111111111111111111111111111111111111111", 1, 0},
		{"overflow_int32", `{"pr_number":"9999999999"}`, "b222222222222222222222222222222222222222", 2, 0},
		{"negative", `{"pr_number":"-5"}`, "c333333333333333333333333333333333333333", 3, 0},
		{"valid", `{"pr_number":42}`, "d444444444444444444444444444444444444444", 4, 42},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := baseTriggerInput(pipelineID, materialID, tc.modID)
			in.Revision = tc.rev
			in.Cause = "pull_request"
			in.CauseDetail = json.RawMessage(tc.detail)
			if _, err := s.CreateRunFromModification(ctx, in); err != nil {
				t.Fatalf("create run: %v", err)
			}

			got, err := s.GetProjectDetail(ctx, "demo", 10)
			if err != nil {
				t.Fatalf("GetProjectDetail must not error on pr_number=%s: %v", tc.detail, err)
			}
			meta := got.Pipelines[0].LatestRunMeta
			if meta == nil {
				t.Fatal("LatestRunMeta is nil")
			}
			if meta.PRNumber != tc.wantPR {
				t.Fatalf("PRNumber = %d, want %d (detail=%s)", meta.PRNumber, tc.wantPR, tc.detail)
			}
		})
	}
}

func TestListProjects_LatestRunMetaCarriesPRRef(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	pipelineID, materialID, _ := seedPipeline(t, pool, false)

	// Sibling query to GetProjectDetail's: LatestRunMetaPerProject
	// feeds the /projects list cards. Same PR-ref + guard contract.
	in := baseTriggerInput(pipelineID, materialID, 1)
	in.Cause = "pull_request"
	in.CauseDetail = json.RawMessage(`{"pr_number": 1135}`)
	if _, err := s.CreateRunFromModification(ctx, in); err != nil {
		t.Fatalf("create PR run: %v", err)
	}

	got, err := s.ListProjects(ctx)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("projects = %d, want 1", len(got))
	}
	meta := got[0].LatestRunMeta
	if meta == nil {
		t.Fatal("LatestRunMeta is nil for project list card")
	}
	if meta.Cause != "pull_request" {
		t.Fatalf("Cause = %q, want pull_request", meta.Cause)
	}
	if meta.PRNumber != 1135 {
		t.Fatalf("PRNumber = %d, want 1135", meta.PRNumber)
	}
}

func TestGetProjectDetail_MetricsNilWhenNoTerminalRuns(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	pipelineID, materialID, _ := seedPipeline(t, pool, false)
	// Freshly-created run is queued — not a terminal status, so the
	// metrics aggregate should stay unset. Regression guard: a
	// COALESCE(..., 0) elsewhere could easily let zeroed stats leak
	// through as if they were real values.
	if _, err := s.CreateRunFromModification(ctx, baseTriggerInput(pipelineID, materialID, 1)); err != nil {
		t.Fatalf("create run: %v", err)
	}

	got, err := s.GetProjectDetail(ctx, "demo", 10)
	if err != nil {
		t.Fatalf("GetProjectDetail: %v", err)
	}
	if got.Pipelines[0].Metrics != nil {
		t.Fatalf("Metrics populated without terminal runs: %+v", got.Pipelines[0].Metrics)
	}
}

func TestGetProjectDetail_MetricsAggregatesTerminalRuns(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	pipelineID, materialID, _ := seedPipeline(t, pool, false)
	r1, err := s.CreateRunFromModification(ctx, baseTriggerInput(pipelineID, materialID, 1))
	if err != nil {
		t.Fatalf("run1: %v", err)
	}
	in2 := baseTriggerInput(pipelineID, materialID, 2)
	in2.Revision = "b111111111111111111111111111111111111111"
	r2, err := s.CreateRunFromModification(ctx, in2)
	if err != nil {
		t.Fatalf("run2: %v", err)
	}

	// Mark both runs as finished with a known duration so the p50
	// math is predictable. Direct SQL because the production code
	// path to terminal status is orchestration-driven (stage
	// completion → run update) — way more setup than a metrics
	// assertion needs.
	now := time.Now().UTC()
	mark := func(runID [16]byte, status string, leadSec int) {
		t.Helper()
		start := now.Add(-time.Duration(leadSec+60) * time.Second)
		end := start.Add(time.Duration(leadSec) * time.Second)
		if _, err := pool.Exec(ctx,
			`UPDATE runs SET status=$1, started_at=$2, finished_at=$3 WHERE id=$4`,
			status, start, end, runID,
		); err != nil {
			t.Fatalf("update run: %v", err)
		}
		// Stage timings: build half + test half, so the sum equals
		// the overall run duration. Process time p50 should match
		// lead time p50 in this fixture.
		mid := start.Add(time.Duration(leadSec/2) * time.Second)
		if _, err := pool.Exec(ctx,
			`UPDATE stage_runs SET status='success', started_at=$1, finished_at=$2 WHERE run_id=$3 AND ordinal=0`,
			start, mid, runID,
		); err != nil {
			t.Fatalf("update stage build: %v", err)
		}
		if _, err := pool.Exec(ctx,
			`UPDATE stage_runs SET status=$1, started_at=$2, finished_at=$3 WHERE run_id=$4 AND ordinal=1`,
			status, mid, end, runID,
		); err != nil {
			t.Fatalf("update stage test: %v", err)
		}
	}
	mark(r1.RunID, "success", 60)
	mark(r2.RunID, "failed", 120)

	got, err := s.GetProjectDetail(ctx, "demo", 10)
	if err != nil {
		t.Fatalf("GetProjectDetail: %v", err)
	}
	m := got.Pipelines[0].Metrics
	if m == nil {
		t.Fatalf("Metrics nil after two terminal runs")
	}
	if m.RunsConsidered != 2 {
		t.Fatalf("RunsConsidered = %d, want 2", m.RunsConsidered)
	}
	if m.SuccessRate != 0.5 {
		t.Fatalf("SuccessRate = %v, want 0.5", m.SuccessRate)
	}
	// p50 of {60, 120} = 90. Allow small float slack.
	if m.LeadTimeP50Sec < 89 || m.LeadTimeP50Sec > 91 {
		t.Fatalf("LeadTimeP50Sec = %v, want ~90", m.LeadTimeP50Sec)
	}
	if len(m.StageStats) != 2 {
		t.Fatalf("StageStats = %d, want 2", len(m.StageStats))
	}
}

func TestGetRunDetail_NotFound(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)

	_, err := s.GetRunDetail(context.Background(), nonexistentUUID(), 0, nil)
	if !errors.Is(err, store.ErrRunNotFound) {
		t.Fatalf("err = %v, want ErrRunNotFound", err)
	}
}

func TestGetRunDetail_StagesJobsAndOptionalLogs(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	pipelineID, materialID, _ := seedPipeline(t, pool, false)
	run, err := s.CreateRunFromModification(ctx, baseTriggerInput(pipelineID, materialID, 1))
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	// Attach a log line to compile so the logs tail path is exercised.
	compileID := run.JobRuns[0].ID
	if err := s.InsertLogLine(ctx, store.LogLine{
		JobRunID: compileID, Seq: 1, Stream: "stdout",
		At: time.Now().UTC(), Text: "hello world",
	}); err != nil {
		t.Fatalf("log: %v", err)
	}

	got, err := s.GetRunDetail(ctx, run.RunID, 50, nil)
	if err != nil {
		t.Fatalf("GetRunDetail: %v", err)
	}
	if got.RunSummary.ID != run.RunID {
		t.Fatalf("id mismatch")
	}
	if got.ProjectSlug != "demo" {
		t.Fatalf("project_slug = %q", got.ProjectSlug)
	}
	if len(got.Stages) != 2 {
		t.Fatalf("stages = %d", len(got.Stages))
	}
	if got.Stages[0].Name != "build" || got.Stages[1].Name != "test" {
		t.Fatalf("stage order: %+v", got.Stages)
	}
	if len(got.Stages[0].Jobs) != 1 || got.Stages[0].Jobs[0].Name != "compile" {
		t.Fatalf("build jobs: %+v", got.Stages[0].Jobs)
	}

	var found bool
	for _, j := range got.Stages[0].Jobs {
		if j.ID == compileID {
			found = true
			if len(j.Logs) != 1 || j.Logs[0].Text != "hello world" {
				t.Fatalf("logs = %+v", j.Logs)
			}
		}
	}
	if !found {
		t.Fatalf("compile job missing")
	}
}

func TestGetRunDetail_LogCursorReturnsOnlyDelta(t *testing.T) {
	// When the caller passes a per-job cursor, the store returns
	// only lines with seq > cursor — the polling client's delta
	// fetch. Jobs without a cursor in the map fall back to tail
	// behaviour so the same endpoint still serves the initial load.
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	pipelineID, materialID, _ := seedPipeline(t, pool, false)
	run, err := s.CreateRunFromModification(ctx, baseTriggerInput(pipelineID, materialID, 1))
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	jobID := run.JobRuns[0].ID
	now := time.Now().UTC()
	for seq := int64(1); seq <= 5; seq++ {
		if err := s.InsertLogLine(ctx, store.LogLine{
			JobRunID: jobID, Seq: seq, Stream: "stdout",
			At:   now.Add(time.Duration(seq) * time.Millisecond),
			Text: fmt.Sprintf("line %d", seq),
		}); err != nil {
			t.Fatalf("log %d: %v", seq, err)
		}
	}

	got, err := s.GetRunDetail(ctx, run.RunID, 100, map[uuid.UUID]int64{jobID: 3})
	if err != nil {
		t.Fatalf("GetRunDetail: %v", err)
	}

	// Only lines 4 and 5 should come back for the cursor'd job.
	var seqs []int64
	for _, st := range got.Stages {
		for _, j := range st.Jobs {
			if j.ID != jobID {
				continue
			}
			for _, l := range j.Logs {
				seqs = append(seqs, l.Seq)
			}
		}
	}
	if len(seqs) != 2 || seqs[0] != 4 || seqs[1] != 5 {
		t.Fatalf("cursor delta returned seqs=%v, want [4 5]", seqs)
	}
}

// TestGetRunDetailWithLogs_HeadAndTailRendersStartAndEnd covers the
// #18 regression: pre-v0.14.7 the tail-only fetch hid the START of
// long jobs (Gradle daemon setup, dependency resolution from
// Nexus, JDK toolchain selection — all in the first few hundred
// lines of a 23k-line build). After: passing LogWindow.Head=N
// returns the first N lines AND the last M, plus LogsOmitted
// counting the verbose middle the UI displays as a divider.
func TestGetRunDetailWithLogs_HeadAndTailRendersStartAndEnd(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	pipelineID, materialID, _ := seedPipeline(t, pool, false)
	run, err := s.CreateRunFromModification(ctx, baseTriggerInput(pipelineID, materialID, 1))
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	jobID := run.JobRuns[0].ID

	// Seed 100 lines so head=5 + tail=5 = 10 visible, 90 omitted.
	now := time.Now().UTC()
	for seq := int64(1); seq <= 100; seq++ {
		if err := s.InsertLogLine(ctx, store.LogLine{
			JobRunID: jobID, Seq: seq, Stream: "stdout",
			At:   now.Add(time.Duration(seq) * time.Millisecond),
			Text: fmt.Sprintf("line %d", seq),
		}); err != nil {
			t.Fatalf("log %d: %v", seq, err)
		}
	}

	got, err := s.GetRunDetailWithLogs(ctx, run.RunID, store.LogWindow{Head: 5, Tail: 5})
	if err != nil {
		t.Fatalf("GetRunDetailWithLogs: %v", err)
	}

	var jd store.JobDetail
	for _, st := range got.Stages {
		for _, j := range st.Jobs {
			if j.ID == jobID {
				jd = j
			}
		}
	}
	if jd.ID == uuid.Nil {
		t.Fatal("compile job not found in detail")
	}

	if len(jd.LogsHead) != 5 {
		t.Errorf("LogsHead = %d, want 5", len(jd.LogsHead))
	}
	if len(jd.Logs) != 5 {
		t.Errorf("Logs (tail) = %d, want 5", len(jd.Logs))
	}
	if jd.LogsOmitted != 90 {
		t.Errorf("LogsOmitted = %d, want 90 (100 - 5 head - 5 tail)", jd.LogsOmitted)
	}
	// Head = first 5 by seq, ascending.
	if jd.LogsHead[0].Seq != 1 || jd.LogsHead[4].Seq != 5 {
		t.Errorf("head seqs = [%d..%d], want [1..5]",
			jd.LogsHead[0].Seq, jd.LogsHead[len(jd.LogsHead)-1].Seq)
	}
	// Tail = last 5 by seq, also ascending within the window.
	if jd.Logs[0].Seq != 96 || jd.Logs[4].Seq != 100 {
		t.Errorf("tail seqs = [%d..%d], want [96..100]",
			jd.Logs[0].Seq, jd.Logs[len(jd.Logs)-1].Seq)
	}
}

// TestGetRunDetailWithLogs_HeadTailOverlapDedupes covers the short-
// job case where head + tail >= total. Pre-fix this would render
// the same lines twice in the UI (head shows 1..5, tail shows
// 1..5 too, operator sees a doubled log). After dedupe, head
// drops the overlap so tail's window stays canonical and the
// operator sees each line exactly once with omitted=0.
func TestGetRunDetailWithLogs_HeadTailOverlapDedupes(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	pipelineID, materialID, _ := seedPipeline(t, pool, false)
	run, _ := s.CreateRunFromModification(ctx, baseTriggerInput(pipelineID, materialID, 1))
	jobID := run.JobRuns[0].ID

	now := time.Now().UTC()
	for seq := int64(1); seq <= 5; seq++ {
		if err := s.InsertLogLine(ctx, store.LogLine{
			JobRunID: jobID, Seq: seq, Stream: "stdout",
			At: now.Add(time.Duration(seq) * time.Millisecond), Text: fmt.Sprintf("L%d", seq),
		}); err != nil {
			t.Fatalf("log: %v", err)
		}
	}

	got, _ := s.GetRunDetailWithLogs(ctx, run.RunID, store.LogWindow{Head: 10, Tail: 10})

	var jd store.JobDetail
	for _, st := range got.Stages {
		for _, j := range st.Jobs {
			if j.ID == jobID {
				jd = j
			}
		}
	}

	if jd.LogsOmitted != 0 {
		t.Errorf("LogsOmitted = %d, want 0 (head+tail covers everything)", jd.LogsOmitted)
	}
	if len(jd.Logs) != 5 {
		t.Errorf("tail = %d, want 5 (all lines)", len(jd.Logs))
	}
	// Head should be empty (or near-empty) after dedupe — every
	// seq is in tail too. Operator sees the 5 lines once via tail.
	if len(jd.LogsHead) != 0 {
		t.Errorf("head after dedupe = %d, want 0 (all overlap with tail)", len(jd.LogsHead))
	}
}

// TestGetRunDetailWithLogs_CursorSkipsHead — cursor-driven polling
// only ships the tail delta; the head was already delivered on
// the initial load and re-shipping it on every tick would
// dominate the response. Initial load asks Head=N, deltas don't.
func TestGetRunDetailWithLogs_CursorSkipsHead(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	pipelineID, materialID, _ := seedPipeline(t, pool, false)
	run, _ := s.CreateRunFromModification(ctx, baseTriggerInput(pipelineID, materialID, 1))
	jobID := run.JobRuns[0].ID

	now := time.Now().UTC()
	for seq := int64(1); seq <= 10; seq++ {
		_ = s.InsertLogLine(ctx, store.LogLine{
			JobRunID: jobID, Seq: seq, Stream: "stdout",
			At: now, Text: fmt.Sprintf("L%d", seq),
		})
	}

	got, _ := s.GetRunDetailWithLogs(ctx, run.RunID, store.LogWindow{
		Head: 5, Tail: 100, Since: map[uuid.UUID]int64{jobID: 7},
	})

	var jd store.JobDetail
	for _, st := range got.Stages {
		for _, j := range st.Jobs {
			if j.ID == jobID {
				jd = j
			}
		}
	}

	if len(jd.LogsHead) != 0 {
		t.Errorf("LogsHead = %d, want 0 on cursor-driven fetch", len(jd.LogsHead))
	}
	if jd.LogsOmitted != 0 {
		t.Errorf("LogsOmitted = %d, want 0 on cursor delta", jd.LogsOmitted)
	}
	// Tail delta: lines 8, 9, 10 (after cursor=7).
	if len(jd.Logs) != 3 {
		t.Errorf("tail delta = %d, want 3 (8,9,10)", len(jd.Logs))
	}
}

func TestGetRunDetail_LogsSkippedWhenLimitZero(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	pipelineID, materialID, _ := seedPipeline(t, pool, false)
	run, err := s.CreateRunFromModification(ctx, baseTriggerInput(pipelineID, materialID, 1))
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if err := s.InsertLogLine(ctx, store.LogLine{
		JobRunID: run.JobRuns[0].ID, Seq: 1, Stream: "stdout",
		At: time.Now().UTC(), Text: "x",
	}); err != nil {
		t.Fatalf("log: %v", err)
	}

	got, err := s.GetRunDetail(ctx, run.RunID, 0, nil)
	if err != nil {
		t.Fatalf("GetRunDetail: %v", err)
	}
	for _, st := range got.Stages {
		for _, j := range st.Jobs {
			if len(j.Logs) != 0 {
				t.Fatalf("logs populated despite limit=0: %+v", j.Logs)
			}
		}
	}
}

// TestGetRunDetail_CancelRequestedAtSurfaced — when a running
// job has a cancel intent stamped (operator hit Cancel, agent
// hasn't acknowledged), GetRunDetail must surface the
// timestamp on JobDetail so the UI can render "Canceling…".
// This is the read-side of the v0.15.1 deferred cancel
// contract: the field is the SINGLE source of truth for the
// "we asked, waiting for the agent" state — status stays
// "running" until the agent's JobResult lands.
func TestGetRunDetail_CancelRequestedAtSurfaced(t *testing.T) {
	pool := dbtest.SetupPool(t)
	s := store.New(pool)
	ctx := context.Background()

	pipelineID, materialID, _ := seedPipeline(t, pool, false)
	run, err := s.CreateRunFromModification(ctx, baseTriggerInput(pipelineID, materialID, 1))
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	target := run.JobRuns[0].ID

	// Simulate the operator-side cancel landing: status stays
	// running, cancel_requested_at gets stamped. Bypass the
	// API path here — we're asserting the read serialisation,
	// not the action handler.
	if _, err := pool.Exec(ctx,
		`UPDATE job_runs SET status='running', cancel_requested_at = NOW() WHERE id = $1`,
		target,
	); err != nil {
		t.Fatalf("stamp cancel: %v", err)
	}

	got, err := s.GetRunDetail(ctx, run.RunID, 0, nil)
	if err != nil {
		t.Fatalf("GetRunDetail: %v", err)
	}
	var seen bool
	for _, st := range got.Stages {
		for _, j := range st.Jobs {
			if j.ID != target {
				continue
			}
			seen = true
			if j.Status != "running" {
				t.Errorf("status = %q, want running (cancel is deferred, not terminal)", j.Status)
			}
			if j.CancelRequestedAt == nil {
				t.Errorf("CancelRequestedAt = nil; want a non-nil pointer so the UI can render Canceling…")
			}
		}
	}
	if !seen {
		t.Fatalf("target job missing from RunDetail")
	}

	// Sibling jobs (status running, no cancel intent stamped)
	// MUST NOT carry the field — `omitempty` on the json tag
	// only kicks in on nil, so an over-eager pgTimePtr would
	// leak Canceling… to every sibling job.
	for _, st := range got.Stages {
		for _, j := range st.Jobs {
			if j.ID == target {
				continue
			}
			if j.CancelRequestedAt != nil {
				t.Errorf("sibling job %s carries CancelRequestedAt = %v, want nil",
					j.Name, j.CancelRequestedAt)
			}
		}
	}
}

func nonexistentUUID() (u [16]byte) {
	// Deterministic, doesn't need to be random.
	u[0] = 0xff
	return u
}
