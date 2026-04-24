package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/gocdnext/gocdnext/server/internal/db"
)

// Sentinel errors for the run-action handlers. ErrRunNotFound is
// defined in reads.go (shared with GetRunDetail). The handler layer
// maps these to HTTP status codes (404 / 409 / 422).
var (
	ErrRunAlreadyTerminal        = errors.New("store: run already terminal")
	ErrNoModificationForPipeline = errors.New("store: no modification for pipeline")
	ErrRunRevisionsMissing       = errors.New("store: run has no revisions to replay")
	ErrJobRunNotFound            = errors.New("store: job_run not found")
	ErrJobRunActive              = errors.New("store: job_run still active (queued/running)")
)

// RunningJobRef points the HTTP handler at a job_run that was still
// executing on an agent when CancelRun fired. The handler uses the
// pair to dispatch a `CancelJob` gRPC message down the owning
// agent's session — without that push, the run-level DB cancel
// would leave the container burning until it finished naturally.
type RunningJobRef struct {
	JobID   uuid.UUID
	AgentID uuid.UUID
}

// CancelRunResult surfaces what CancelRun touched. Today only
// RunningJobs is actionable, but keeping it in a struct leaves
// room for future signals (e.g. "queued jobs we skipped").
type CancelRunResult struct {
	RunningJobs []RunningJobRef
}

// CancelRun marks a run and its queued/running descendants as
// canceled and returns the agent-assigned jobs that were still
// running so the caller can push CancelJob messages through the
// gRPC stream. Idempotent: second call on a terminal run returns
// ErrRunAlreadyTerminal.
//
// Queued jobs are flipped to canceled directly here (they haven't
// reached an agent yet). Running jobs stay marked `running` until
// the agent reports a final JobResult — that keeps the audit
// trail honest about when each one actually stopped.
func (s *Store) CancelRun(ctx context.Context, runID uuid.UUID) (CancelRunResult, error) {
	// Check that the run exists before we start. Distinguishing "not
	// found" from "already terminal" matters for 404 vs 409.
	row, err := s.q.GetRunForAction(ctx, pgUUID(runID))
	if errors.Is(err, pgx.ErrNoRows) {
		return CancelRunResult{}, ErrRunNotFound
	}
	if err != nil {
		return CancelRunResult{}, fmt.Errorf("store: cancel run: lookup: %w", err)
	}
	if row.Status != "queued" && row.Status != "running" {
		return CancelRunResult{}, ErrRunAlreadyTerminal
	}

	// Snapshot the set of jobs we need to push Cancel messages to
	// BEFORE touching run/stage state. Doing it after would race
	// the agent's own JobResult (which clears agent_id).
	running, err := s.listRunningJobsForRun(ctx, runID)
	if err != nil {
		return CancelRunResult{}, fmt.Errorf("store: cancel run: list running: %w", err)
	}

	// Cancel the run row first so any racing scheduler pass sees the
	// new status before it tries to claim the next job. CancelActiveRun
	// is a no-op if the status moved away under us between the SELECT
	// above and this UPDATE — the downstream stage/job cancellations
	// are still safe because they gate on status='queued'.
	if _, err := s.q.CancelActiveRun(ctx, pgUUID(runID)); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return CancelRunResult{}, ErrRunAlreadyTerminal
		}
		return CancelRunResult{}, fmt.Errorf("store: cancel run: update: %w", err)
	}

	if err := s.q.CancelQueuedStagesInRun(ctx, pgUUID(runID)); err != nil {
		return CancelRunResult{}, fmt.Errorf("store: cancel run: stages: %w", err)
	}
	if err := s.q.CancelQueuedJobsInRun(ctx, pgUUID(runID)); err != nil {
		return CancelRunResult{}, fmt.Errorf("store: cancel run: jobs: %w", err)
	}
	return CancelRunResult{RunningJobs: running}, nil
}

// listRunningJobsForRun returns (job_run_id, agent_id) pairs for
// every running job under a run that's actually been dispatched.
// Raw SQL — not worth a sqlc entry for a single query that's only
// used from CancelRun. `agent_id IS NOT NULL` guard skips queued
// jobs that hadn't reached an agent yet; those are handled by
// CancelQueuedJobsInRun.
func (s *Store) listRunningJobsForRun(ctx context.Context, runID uuid.UUID) ([]RunningJobRef, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, agent_id
		FROM job_runs
		WHERE run_id = $1 AND status = 'running' AND agent_id IS NOT NULL
	`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RunningJobRef
	for rows.Next() {
		var jobID, agentID uuid.UUID
		if err := rows.Scan(&jobID, &agentID); err != nil {
			return nil, err
		}
		out = append(out, RunningJobRef{JobID: jobID, AgentID: agentID})
	}
	return out, rows.Err()
}

// RerunRunInput configures a rerun. TriggeredBy lands on the new run
// row (e.g., "user:klinux@…", "api", "rerun:<orig>"). Unspecified
// keeps the original run's triggered_by for traceability.
type RerunRunInput struct {
	RunID       uuid.UUID
	TriggeredBy string
}

// RerunRun creates a fresh run on the same pipeline, replaying the
// same revision that the original run consumed. Uses the revisions
// snapshot stored on the original row, so it works for webhook,
// pull_request and manual origins alike.
func (s *Store) RerunRun(ctx context.Context, in RerunRunInput) (RunCreated, error) {
	row, err := s.q.GetRunForAction(ctx, pgUUID(in.RunID))
	if errors.Is(err, pgx.ErrNoRows) {
		return RunCreated{}, ErrRunNotFound
	}
	if err != nil {
		return RunCreated{}, fmt.Errorf("store: rerun: lookup: %w", err)
	}

	materialID, revision, branch, err := pickPrimaryRevision(row.Revisions)
	if err != nil {
		return RunCreated{}, err
	}

	branchStr := ""
	if branch != nil {
		branchStr = *branch
	}
	modKey, err := s.q.GetModificationByKey(ctx, db.GetModificationByKeyParams{
		MaterialID: pgUUID(materialID),
		Revision:   revision,
		Branch:     branch,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// The modification has been pruned or the run was constructed
		// outside the webhook path. Bail with a helpful error — the
		// handler translates to 422.
		return RunCreated{}, ErrNoModificationForPipeline
	}
	if err != nil {
		return RunCreated{}, fmt.Errorf("store: rerun: modification lookup: %w", err)
	}

	triggeredBy := in.TriggeredBy
	if triggeredBy == "" {
		triggeredBy = "rerun:" + in.RunID.String()
	}

	causeDetail, _ := json.Marshal(map[string]any{"rerun_of": in.RunID.String()})
	return s.CreateRunFromModification(ctx, CreateRunFromModificationInput{
		PipelineID:     fromPgUUID(row.PipelineID),
		MaterialID:     materialID,
		ModificationID: modKey.ID,
		Revision:       revision,
		Branch:         branchStr,
		Provider:       "api",
		Delivery:       "rerun-" + in.RunID.String(),
		TriggeredBy:    triggeredBy,
		Cause:          "manual",
		CauseDetail:    causeDetail,
	})
}

// TriggerManualRunInput configures a manual pipeline trigger.
// Revision + branch are optional: leave them empty to pick the
// pipeline's newest modification.
type TriggerManualRunInput struct {
	PipelineID  uuid.UUID
	TriggeredBy string
	// Cause overrides the default "manual" tagging. Scheduled fires
	// (project_crons ticker, cron materials) pass "schedule" here
	// so the runs list distinguishes operator-initiated from
	// auto-fired runs. CauseDetail is merged as-is onto the run's
	// base metadata (material_id, delivery, etc.).
	Cause       string
	CauseDetail json.RawMessage
}

// TriggerManualRun starts a new run on a pipeline.
//
// For git-backed pipelines we reuse the most recent modification row
// so the run is tied to a real commit (build caching, revision
// display, log correlation all keep working). When the pipeline has
// never seen a push we return ErrNoModificationForPipeline so the
// handler can 422 with "push to seed…".
//
// For pipelines whose only materials are upstream / manual / cron
// there's nothing to seed from — the webhook path doesn't apply.
// We insert a bare run skeleton (empty revisions) so operators can
// kick those pipelines by hand. The scheduler's assignment builder
// already skips checkout for non-git materials, so no revision on
// the run is fine.
func (s *Store) TriggerManualRun(ctx context.Context, in TriggerManualRunInput) (RunCreated, error) {
	triggeredBy := in.TriggeredBy
	if triggeredBy == "" {
		triggeredBy = "manual"
	}
	cause := in.Cause
	if cause == "" {
		cause = "manual"
	}
	delivery := cause + "-" + in.PipelineID.String()

	mod, err := s.q.GetLatestModificationForPipeline(ctx, pgUUID(in.PipelineID))
	switch {
	case err == nil:
		branch := ""
		if mod.Branch != nil {
			branch = *mod.Branch
		}
		return s.CreateRunFromModification(ctx, CreateRunFromModificationInput{
			PipelineID:     in.PipelineID,
			MaterialID:     fromPgUUID(mod.MaterialID),
			ModificationID: mod.ID,
			Revision:       mod.Revision,
			Branch:         branch,
			Provider:       "api",
			Delivery:       delivery,
			TriggeredBy:    triggeredBy,
			Cause:          cause,
			CauseDetail:    in.CauseDetail,
		})
	case errors.Is(err, pgx.ErrNoRows):
		// Fall through to the no-material trigger path below.
	default:
		return RunCreated{}, fmt.Errorf("store: manual trigger: modification: %w", err)
	}

	// No modification — decide whether that's because the pipeline is
	// git-backed and never saw a push (→ 422) or because it has no
	// git material at all (→ bare run).
	hasGit, err := s.pipelineHasGitMaterial(ctx, in.PipelineID)
	if err != nil {
		return RunCreated{}, fmt.Errorf("store: manual trigger: material check: %w", err)
	}
	if hasGit {
		return RunCreated{}, ErrNoModificationForPipeline
	}

	// Merge caller CauseDetail onto the base metadata. Same
	// precedence as CreateRunFromModification — caller's keys win
	// on collision so cron can stamp schedule_id / schedule_name.
	base := map[string]any{"delivery": delivery}
	if len(in.CauseDetail) > 0 {
		var extra map[string]any
		if err := json.Unmarshal(in.CauseDetail, &extra); err == nil {
			for k, v := range extra {
				base[k] = v
			}
		}
	}
	causeDetail, _ := json.Marshal(base)
	return s.insertRunSkeleton(ctx, insertRunSkeletonInput{
		PipelineID:  in.PipelineID,
		Cause:       cause,
		CauseDetail: causeDetail,
		Revisions:   json.RawMessage(`{}`),
		TriggeredBy: triggeredBy,
	})
}

// RerunJobInput points at one job_run to re-execute inside its
// existing run. Cheaper than a full-pipeline rerun: reuses the
// same workspace revisions, the same run_id, and — crucially —
// already-uploaded artefacts from sibling jobs, so a failing
// typecheck can be retried without paying the pnpm install of
// the deps stage again.
type RerunJobInput struct {
	JobRunID    uuid.UUID
	TriggeredBy string
}

type RerunJobResult struct {
	RunID    uuid.UUID
	JobRunID uuid.UUID
	Attempt  int32
}

// RerunJob flips one terminal job_run back to queued (bumping its
// attempt counter), wipes its log lines, and un-finishes the
// parent stage + run so the scheduler picks the job up on the
// next NOTIFY. Refuses when the target is still queued or running
// — operator has to Cancel first. Parent runs that were terminal
// (success / failed / canceled) get bumped to `running` so the UI
// stops showing a fake final state.
//
// Per-attempt log separation is not kept (same trade-off as the
// reaper's retry path — see migration 00003). The old attempt's
// log lines are deleted before the new dispatch so the consumer
// sees a clean slate instead of the previous run's output
// intermixed with this one.
func (s *Store) RerunJob(ctx context.Context, in RerunJobInput) (RerunJobResult, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return RerunJobResult{}, fmt.Errorf("store: rerun job: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var runID, stageRunID uuid.UUID
	var status string
	err = tx.QueryRow(ctx, `
		SELECT run_id, stage_run_id, status FROM job_runs WHERE id = $1
	`, in.JobRunID).Scan(&runID, &stageRunID, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return RerunJobResult{}, ErrJobRunNotFound
	}
	if err != nil {
		return RerunJobResult{}, fmt.Errorf("store: rerun job: lookup: %w", err)
	}
	if status == "queued" || status == "running" {
		return RerunJobResult{}, ErrJobRunActive
	}

	var attempt int32
	err = tx.QueryRow(ctx, `
		UPDATE job_runs SET
			status      = 'queued',
			agent_id    = NULL,
			started_at  = NULL,
			finished_at = NULL,
			exit_code   = NULL,
			error       = NULL,
			attempt     = attempt + 1
		WHERE id = $1
		RETURNING attempt
	`, in.JobRunID).Scan(&attempt)
	if err != nil {
		return RerunJobResult{}, fmt.Errorf("store: rerun job: reset: %w", err)
	}

	// Clear the previous attempt's logs — mirrors what
	// ReclaimJobForRetry does for reaper-driven retries and keeps
	// the log tab honest about what the new attempt produced.
	if _, err := tx.Exec(ctx, `DELETE FROM log_lines WHERE job_run_id = $1`, in.JobRunID); err != nil {
		return RerunJobResult{}, fmt.Errorf("store: rerun job: clear logs: %w", err)
	}

	// Un-finish the parent stage + run so dispatch + UI stop
	// treating them as done. Leaves sibling jobs / stages alone —
	// those already terminal with their real outcome.
	if _, err := tx.Exec(ctx, `
		UPDATE stage_runs
		SET status = 'running', finished_at = NULL
		WHERE id = $1 AND status IN ('success', 'failed', 'canceled')
	`, stageRunID); err != nil {
		return RerunJobResult{}, fmt.Errorf("store: rerun job: reopen stage: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE runs
		SET status = 'running', finished_at = NULL
		WHERE id = $1 AND status IN ('success', 'failed', 'canceled')
	`, runID); err != nil {
		return RerunJobResult{}, fmt.Errorf("store: rerun job: reopen run: %w", err)
	}

	// Notify the scheduler the same way a fresh run does — it'll
	// pick up the newly-queued job on its next LISTEN tick.
	if _, err := tx.Exec(ctx, `SELECT pg_notify($1, $2)`, RunQueuedChannel, runID.String()); err != nil {
		return RerunJobResult{}, fmt.Errorf("store: rerun job: notify: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return RerunJobResult{}, fmt.Errorf("store: rerun job: commit: %w", err)
	}
	return RerunJobResult{
		RunID:    runID,
		JobRunID: in.JobRunID,
		Attempt:  attempt,
	}, nil
}

// pipelineHasGitMaterial reports whether any of the pipeline's
// materials is of type git. Upstream/manual/cron-only pipelines
// return false — those can't be seeded from a push, so the manual
// trigger path has to synthesise a run instead of bailing.
func (s *Store) pipelineHasGitMaterial(ctx context.Context, pipelineID uuid.UUID) (bool, error) {
	rows, err := s.q.ListMaterialsByPipeline(ctx, pgUUID(pipelineID))
	if err != nil {
		return false, err
	}
	for _, r := range rows {
		if r.Type == "git" {
			return true, nil
		}
	}
	return false, nil
}

// pickPrimaryRevision unmarshals the revisions JSONB (shape:
// {"<material_id>": {"revision": "...", "branch": "..."}}) and
// returns the first entry. Runs today only have one material slot,
// so "first" is stable enough for replay semantics.
func pickPrimaryRevision(raw []byte) (uuid.UUID, string, *string, error) {
	if len(raw) == 0 {
		return uuid.Nil, "", nil, ErrRunRevisionsMissing
	}
	var parsed map[string]struct {
		Revision string `json:"revision"`
		Branch   string `json:"branch"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return uuid.Nil, "", nil, fmt.Errorf("store: decode revisions: %w", err)
	}
	for k, v := range parsed {
		matID, err := uuid.Parse(k)
		if err != nil {
			return uuid.Nil, "", nil, fmt.Errorf("store: revisions key not a UUID: %w", err)
		}
		branch := v.Branch
		var branchPtr *string
		if branch != "" {
			branchPtr = &branch
		}
		return matID, v.Revision, branchPtr, nil
	}
	return uuid.Nil, "", nil, ErrRunRevisionsMissing
}
