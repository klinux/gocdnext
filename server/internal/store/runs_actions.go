package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

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
	ErrJobRunTerminal            = errors.New("store: job_run already terminal")
	// ErrCannotRerunGate rejects rerunning an approval gate directly (#97): a gate
	// is a state transition, not an executable job, so re-queueing it would let it
	// dispatch as a task-less job and "pass" without the allow-list/quorum/marker.
	// Re-arm a gate through the normal approve/reject path, not rerun.
	ErrCannotRerunGate = errors.New("store: cannot rerun an approval gate")
	// ErrSnapshotStale is returned by snapshot-CAS write paths
	// (currently WriteTestResults) when the row's current
	// (agent_id, attempt) no longer matches what the caller
	// observed. Callers treat this as "another path took ownership
	// of this row; drop my write" rather than a hard error.
	ErrSnapshotStale = errors.New("store: snapshot stale — row changed under us")
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
	// ServiceGeneration is the run's service_generation captured in the cancel UPDATE
	// (#97). The cancel service cleanup carries it as max_generation so a rerun that
	// revives the run into a higher generation keeps its fresh pods. Captured
	// atomically with the flip to canceled — never re-read after, which could see the
	// bumped (post-revive) value and delete the revived pods.
	ServiceGeneration int64
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

	// Cancel the run row FIRST so any racing scheduler pass sees the new
	// status before it tries to claim the next job. CancelActiveRun is a no-op
	// if the status moved away under us between the SELECT above and this UPDATE
	// — the downstream stage/job cancellations still gate on status='queued'.
	canceled, err := s.q.CancelActiveRun(ctx, pgUUID(runID))
	if err != nil {
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

	// Snapshot + stamp running jobs AFTER the queued cancel (review MED, mirrors
	// supersede's terminalizer). AssignJob is a bare `status='queued'` CAS that
	// never checks runs.status, so a job queued when we started can flip to
	// running concurrently. Snapshotting BEFORE the cancel would miss it AND
	// CancelQueuedJobsInRun would skip it (now running) — leaving a job executing
	// inside a canceled run with no CancelJob frame. Canceling first forces the
	// contention onto the shared job_runs row; by now every job is either canceled
	// (above) or committed-running, and StampCancelRequestedAtForRun stamps the
	// cancel intent AND RETURNS (id, agent_id) as the exact CancelJob work-list.
	// The intent stamp is what lets a Dispatch landing in the Revoke→Register race
	// window survive: the agent's next Register replays it, the reaper finalises
	// stragglers. A job whose JobResult already cleared agent_id is terminal and
	// correctly absent (it finished; no frame needed).
	stamped, err := s.q.StampCancelRequestedAtForRun(ctx, pgUUID(runID))
	if err != nil {
		return CancelRunResult{}, fmt.Errorf("store: cancel run: stamp pending: %w", err)
	}
	running := make([]RunningJobRef, 0, len(stamped))
	for _, r := range stamped {
		running = append(running, RunningJobRef{JobID: fromPgUUID(r.ID), AgentID: fromPgUUID(r.AgentID)})
	}
	return CancelRunResult{RunningJobs: running, ServiceGeneration: canceled.ServiceGeneration}, nil
}

// CancelJobRunResult surfaces what CancelJobRun did. The handler
// keys its HTTP response on NeedsDispatch:
//
//   - NeedsDispatch=false → the row was queued and is now `canceled`
//     in the DB; the cancel has already taken effect. Handler
//     returns 202 with signaled=false; no gRPC frame required.
//
//   - NeedsDispatch=true  → the row was running. The cancel will
//     only take effect after the handler pushes a CancelJob frame
//     down the agent's session OR the agent's next Register drains
//     the stamped cancel_requested_at via ListPendingCancelsForAgent.
//     Dispatched carries the (job_run, agent) pair the handler
//     dispatches to.
//
//   - Dispatched populated + Dispatch SUCCESS → 202 canceling,
//     signaled=true. Agent will report JobResult; row flips to
//     canceled cleanly.
//
//   - Dispatched populated + Dispatch FAILURE → 202 canceling,
//     signaled=false, deferred=true. cancel_requested_at IS
//     stamped; the replay path lands the cancel on the next
//     Register, or the reaper finalises if the agent stays gone.
//
//   - Dispatched is nil (running row but no agent_id yet —
//     transient AssignJob→ack window) → 503 dispatch_failed.
//     The stamp predicate requires agent_id NOT NULL so no
//     intent was persisted; operator retries when agent_id is
//     populated.
//
// Splitting "did the cancel land?" out of the result lets the
// handler avoid the bug where a dispatch failure returned
// HTTP 202 status="canceled" while the job kept running, while
// the deferred-stamp path keeps the cancel intent durable across
// session recycles.
type CancelJobRunResult struct {
	RunID         uuid.UUID
	JobRunID      uuid.UUID
	JobName       string
	NeedsDispatch bool
	Dispatched    *RunningJobRef
}

// CancelJobRun cancels exactly one job_run, leaving siblings (and
// the run itself) untouched. Two regimes by current status:
//
//   - queued → flip status='canceled' in this tx + cascade
//     (the cascade may complete the stage + run if this was the
//     last unfinished job; same path CompleteJob takes). Downstream
//     jobs whose `needs:` reference this one will surface
//     "canceled" via needsSatisfied at the next scheduler tick and
//     be failed via failJobNeedsUnmet — no special handling here.
//
//   - running → leave the row in 'running' and return the agent +
//     job_run id pair so the handler can push a CancelJob frame.
//     The agent's runner ctx cancels, the container terminates,
//     and the resulting JobResult flips status to canceled (or
//     failed) through the normal CompleteJob cascade. Audit-trail-
//     honest: actual finished_at is when the container actually
//     stopped, not when the operator clicked Cancel.
//
//   - any terminal status → ErrJobRunTerminal (HTTP 409).
//
//   - missing id → ErrJobRunNotFound (HTTP 404).
//
// Idempotent: re-cancelling an already-canceled job is a 409 by
// design (the operator clicked again on a stale UI; they didn't
// "do" anything new).
func (s *Store) CancelJobRun(ctx context.Context, jobRunID uuid.UUID) (CancelJobRunResult, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return CancelJobRunResult{}, fmt.Errorf("store: cancel job: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	row, err := q.GetJobRunForCancel(ctx, pgUUID(jobRunID))
	if errors.Is(err, pgx.ErrNoRows) {
		return CancelJobRunResult{}, ErrJobRunNotFound
	}
	if err != nil {
		return CancelJobRunResult{}, fmt.Errorf("store: cancel job: lookup: %w", err)
	}

	switch row.Status {
	case "running":
		// Persist the cancel INTENT (cancel_requested_at) on the
		// row in this same tx, BEFORE the handler tries to push
		// the CancelJob frame down the agent's session. The
		// intent survives even if the session is in flux when
		// Dispatch is attempted (Revoke→Register race during a
		// pod restart) — the agent honors it via
		// ListPendingCancelsForAgent right after the new session
		// comes up, or the reaper finalises it via
		// ReclaimPendingCancelsForOfflineAgent if the agent
		// stays gone. Idempotent on the timestamp (COALESCE in
		// the SQL keeps the first cancel's at-time).
		//
		// Two skip conditions:
		//
		//   1. Already stamped (re-click): row.CancelRequestedAt
		//      is Valid → no-op, the first click's at-time is
		//      authoritative.
		//   2. agent_id IS NULL: the AssignJob window — status is
		//      'running' but the row hasn't yet been attributed
		//      to an agent (race-improbable but defensible). The
		//      stamp's predicate refuses the row; we'd interpret
		//      that as invariant violation. Skip the stamp; the
		//      handler sees Dispatched=nil and returns 503 with
		//      a retry hint, the same path as before this column
		//      existed. By the time the operator retries, AssignJob
		//      has populated agent_id and the stamp lands cleanly.
		alreadyRequested := row.CancelRequestedAt.Valid
		if !alreadyRequested && row.AgentID.Valid {
			if _, err := q.StampCancelRequestedAt(ctx, pgUUID(jobRunID)); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					// The row is running AND agent_id is set (we
					// just checked row.AgentID.Valid) — the only
					// way the predicate misses here is a logic
					// bug. Surface so we notice.
					return CancelJobRunResult{}, fmt.Errorf(
						"store: cancel job: stamp missed under FOR UPDATE — invariant violation")
				}
				return CancelJobRunResult{}, fmt.Errorf("store: cancel job: stamp: %w", err)
			}
		}

		// Agent owns the row's lifecycle until JobResult lands.
		// We commit the tx so the SELECT FOR UPDATE lock and the
		// cancel_requested_at stamp both publish. NeedsDispatch=true
		// tells the handler the cancel has NOT yet taken effect —
		// it depends on the gRPC frame landing (best-effort) or
		// the agent's reconnect-time honor (always-effective).
		if err := tx.Commit(ctx); err != nil {
			return CancelJobRunResult{}, fmt.Errorf("store: cancel job: commit (running): %w", err)
		}
		out := CancelJobRunResult{
			RunID:         fromPgUUID(row.RunID),
			JobRunID:      jobRunID,
			JobName:       row.Name,
			NeedsDispatch: true,
		}
		// agent_id may legitimately be NULL even with status='running'
		// during the brief window between AssignJob committing and the
		// agent's session ack landing. Without an agent there's no one
		// to push CancelJob to AND the stamp predicate (StampCancel-
		// RequestedAt requires agent_id NOT NULL) refused the row, so
		// cancel_requested_at is NOT set. Dispatched stays nil; the
		// handler returns 503 with a retry hint. Operator retries
		// once AssignJob has populated agent_id; on retry both the
		// stamp and Dispatch land normally.
		if row.AgentID.Valid {
			out.Dispatched = &RunningJobRef{
				JobID:   jobRunID,
				AgentID: fromPgUUID(row.AgentID),
			}
		}
		return out, nil

	case "queued":
		// Flip directly. The cascade may bubble up to stage/run
		// completion if this was the only unfinished job — same
		// path CompleteJob takes. With FOR UPDATE on the SELECT
		// above, the scheduler's AssignJob is serialised behind us,
		// so this UPDATE can no longer miss its predicate due to a
		// concurrent dispatch — if no rows are returned here, it's
		// a genuine logic bug rather than a race, and we surface it
		// as 500 rather than the misleading 409 the prior cut shipped.
		if _, err := q.CancelQueuedJobRun(ctx, pgUUID(jobRunID)); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return CancelJobRunResult{}, fmt.Errorf(
					"store: cancel job: queued flip missed under FOR UPDATE — invariant violation")
			}
			return CancelJobRunResult{}, fmt.Errorf("store: cancel job: queued flip: %w", err)
		}

		// Cascade: stage progress reads the canonical job_runs table,
		// sees one more terminal row, and decides whether the stage
		// (and run) are done. comp is only used to satisfy the helper
		// signature — handler doesn't surface it.
		comp := JobCompletion{
			JobRunID:   jobRunID,
			RunID:      fromPgUUID(row.RunID),
			StageRunID: fromPgUUID(row.StageRunID),
			JobName:    row.Name,
		}
		if err := cascadeAfterJobCompletion(ctx, q, row.StageRunID, row.RunID, &comp); err != nil {
			return CancelJobRunResult{}, fmt.Errorf("store: cancel job: cascade: %w", err)
		}

		if err := tx.Commit(ctx); err != nil {
			return CancelJobRunResult{}, fmt.Errorf("store: cancel job: commit (queued): %w", err)
		}

		// Wake the scheduler so downstreams that declared `needs:` on
		// this job re-evaluate immediately — needsSatisfied sees
		// status='canceled' as UpstreamTerminal and fails the
		// dependent jobs via failJobNeedsUnmet on the next tick.
		// Non-fatal on error: the periodic tick still catches it.
		if err := s.NotifyRunQueued(context.Background(), fromPgUUID(row.RunID)); err != nil {
			// emit at the caller-log level — store doesn't have a logger.
			_ = err
		}

		return CancelJobRunResult{
			RunID:    fromPgUUID(row.RunID),
			JobRunID: jobRunID,
			JobName:  row.Name,
		}, nil

	default:
		// success, failed, canceled, skipped → terminal
		return CancelJobRunResult{}, ErrJobRunTerminal
	}
}

// PendingCancel surfaces a cancel request that an agent didn't
// observe through the gRPC stream (Dispatch failed because the
// session was in flux between Revoke and Register, OR the agent
// pod was restarted between cancel-request and the next Connect).
// The agent calls ListPendingCancelsForAgent right after Register
// and synthesises a CancelJob handler invocation for each entry.
type PendingCancel struct {
	JobRunID uuid.UUID
	RunID    uuid.UUID
}

// ListPendingCancelsForAgent returns every still-running job_run
// belonging to the agent that has cancel_requested_at stamped.
// Called by the agent's Connect path right after the session is
// established so a cancel that landed during a session recycle
// is honored as if the gRPC frame had arrived. Empty result is
// the hot path — most Register events have no pending cancels —
// so we return a nil slice rather than allocating zero-length.
func (s *Store) ListPendingCancelsForAgent(ctx context.Context, agentID uuid.UUID) ([]PendingCancel, error) {
	rows, err := s.q.ListPendingCancelsForAgent(ctx, pgUUID(agentID))
	if err != nil {
		return nil, fmt.Errorf("store: list pending cancels: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	out := make([]PendingCancel, 0, len(rows))
	for _, r := range rows {
		out = append(out, PendingCancel{
			JobRunID: fromPgUUID(r.ID),
			RunID:    fromPgUUID(r.RunID),
		})
	}
	return out, nil
}

// FinalizedPendingCancel is what the reaper's
// ReclaimPendingCancelsForOfflineAgent sweep flipped to canceled
// because the owning agent went offline past the grace window
// without acknowledging. The reaper logs each entry and fires a
// NOTIFY so the scheduler re-evaluates dependent jobs (same
// cascade as a normal cancel landing).
type FinalizedPendingCancel struct {
	JobRunID          uuid.UUID
	RunID             uuid.UUID
	StageRunID        uuid.UUID
	AgentID           uuid.UUID
	CancelRequestedAt time.Time
}

// ReclaimPendingCancelsForOfflineAgent runs in the reaper tick.
// Sweeps every running job_run with cancel_requested_at stamped
// whose owning agent is unreachable (status='offline' OR
// heartbeat older than `grace`). Each row flips to
// status='canceled' with finished_at=NOW() AND cascades into
// stage_runs/runs so a canceled last-job-of-stage completes the
// stage instead of leaving it stuck on 'running' forever.
//
// `grace` should be wide enough to accommodate normal agent pod
// churn (rolling restart, K8s evictions on node patch) so the
// reaper doesn't finalise rows whose agent is about to come back
// in 30s and honor the cancel via ListPendingCancelsForAgent.
// Default 5min upstream; operators on flakier infra can extend.
//
// Wraps the UPDATE + cascade in a single tx so a partial cascade
// failure can't leave half the run with terminal job_runs and a
// stale stage_run pointing at the run.
func (s *Store) ReclaimPendingCancelsForOfflineAgent(ctx context.Context, grace time.Duration) ([]FinalizedPendingCancel, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("store: reclaim pending cancels: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	rows, err := q.ReclaimPendingCancelsForOfflineAgent(ctx,
		pgtype.Interval{Microseconds: grace.Microseconds(), Valid: true})
	if err != nil {
		return nil, fmt.Errorf("store: reclaim pending cancels: %w", err)
	}
	if len(rows) == 0 {
		// Commit to release any locks held by the UPDATE — even
		// the no-op SELECT path inside a tx is best closed cleanly.
		if err := tx.Commit(ctx); err != nil {
			return nil, fmt.Errorf("store: reclaim pending cancels: commit (empty): %w", err)
		}
		return nil, nil
	}

	// Cascade each finalised row through stage/run completion. If
	// this was the last unfinished job under its stage, the cascade
	// marks the stage terminal; if also the last unfinished stage,
	// the run terminal. Same path CompleteJob takes when an agent
	// reports JobResult naturally.
	out := make([]FinalizedPendingCancel, 0, len(rows))
	for _, r := range rows {
		comp := JobCompletion{
			JobRunID:   fromPgUUID(r.ID),
			RunID:      fromPgUUID(r.RunID),
			StageRunID: fromPgUUID(r.StageRunID),
			JobName:    r.Name,
		}
		if err := cascadeAfterJobCompletion(ctx, q, r.StageRunID, r.RunID, &comp); err != nil {
			return nil, fmt.Errorf("store: reclaim pending cancels: cascade %s: %w",
				comp.JobRunID, err)
		}
		out = append(out, FinalizedPendingCancel{
			JobRunID:          comp.JobRunID,
			RunID:             comp.RunID,
			StageRunID:        comp.StageRunID,
			AgentID:           fromPgUUID(r.AgentID),
			CancelRequestedAt: r.CancelRequestedAt.Time,
		})
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("store: reclaim pending cancels: commit: %w", err)
	}
	return out, nil
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

	// Preserve the original run's cause + cause_detail so CI vars derived
	// from them resolve identically on the rerun. Without this a rerun of
	// a tag run was demoted to cause="manual" with no tag_name, so
	// CI_TAG_NAME vanished and a `deploy.version: ${CI_TAG_NAME}` (or any
	// ${CI_*} shell ref) failed to resolve at dispatch ("CI var not
	// present this run"). Strip the bookkeeping keys — the rerun gets its
	// own provider/delivery/material_id/modification_id from
	// CreateRunFromModification's base — and stamp rerun_of. The semantic
	// keys (tag_name/tag_message/tagger, pr_number/pr_labels, …) carry
	// through so addTagVars / addPullRequestVars rebuild the same CI_* set.
	detail := map[string]any{}
	if len(row.CauseDetail) > 0 {
		_ = json.Unmarshal(row.CauseDetail, &detail)
	}
	for _, k := range []string{"provider", "delivery", "material_id", "modification_id"} {
		delete(detail, k)
	}
	detail["rerun_of"] = in.RunID.String()
	causeDetail, _ := json.Marshal(detail)

	cause := row.Cause
	if cause == "" {
		cause = "manual"
	}
	return s.CreateRunFromModification(ctx, CreateRunFromModificationInput{
		PipelineID:     fromPgUUID(row.PipelineID),
		MaterialID:     materialID,
		ModificationID: modKey.ID,
		Revision:       revision,
		Branch:         branchStr,
		Provider:       "api",
		Delivery:       "rerun-" + in.RunID.String(),
		TriggeredBy:    triggeredBy,
		Cause:          cause,
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
	// IsRollback marks this rerun as a deployment rollback (#39
	// phase 3): the deploy job of a past run is re-run so its
	// immutable outputs re-resolve the old version. Stamped on the
	// row as deploy_rollback so the scheduler opens the new
	// deployment_revision with is_rollback=true. False for an
	// ordinary rerun (which clears any stale flag from a prior one).
	IsRollback bool
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

	// FOR UPDATE locks the row for the life of the tx so the
	// check-then-reset below is atomic against a concurrent
	// rerun/rollback. Without it two callers could both read the job
	// terminal and both reset it: at best a skipped attempt, at worst
	// one resets a job the other already redispatched (running →
	// queued), orphaning the in_progress deploy revision of the live
	// attempt. The loser blocks here, then reads the now-queued status
	// below and bails with ErrJobRunActive.
	var runID, stageRunID uuid.UUID
	var status string
	var isGate bool
	err = tx.QueryRow(ctx, `
		SELECT run_id, stage_run_id, status, approval_gate FROM job_runs WHERE id = $1 FOR UPDATE
	`, in.JobRunID).Scan(&runID, &stageRunID, &status, &isGate)
	if errors.Is(err, pgx.ErrNoRows) {
		return RerunJobResult{}, ErrJobRunNotFound
	}
	if err != nil {
		return RerunJobResult{}, fmt.Errorf("store: rerun job: lookup: %w", err)
	}
	// An approval gate can be non-terminal (awaiting_approval) or terminal
	// (success/failed/canceled), so the active-status guard below wouldn't catch it.
	// Rerunning it would re-queue a gate the dispatch path would run as a task-less
	// job — bypassing approval entirely. Refuse; gates re-arm via approve/reject.
	if isGate {
		return RerunJobResult{}, ErrCannotRerunGate
	}
	if status == "queued" || status == "running" {
		return RerunJobResult{}, ErrJobRunActive
	}

	var attempt int32
	err = tx.QueryRow(ctx, `
		UPDATE job_runs SET
			status              = 'queued',
			agent_id            = NULL,
			started_at          = NULL,
			finished_at         = NULL,
			exit_code           = NULL,
			error               = NULL,
			cancel_requested_at = NULL,
			logs_archive_uri    = NULL,
			logs_archived_at    = NULL,
			deploy_rollback     = $2,
			attempt             = attempt + 1
		WHERE id = $1
		RETURNING attempt
	`, in.JobRunID, in.IsRollback).Scan(&attempt)
	if err != nil {
		return RerunJobResult{}, fmt.Errorf("store: rerun job: reset: %w", err)
	}
	// cancel_requested_at = NULL: the operator's rerun-click is a
	// fresh intent that doesn't inherit the prior attempt's
	// (possibly deferred) cancel. Without this reset, a row that
	// was finalised via the cancel replay/reaper path and then
	// rerun would carry the stamp into the new attempt — and any
	// Register the agent issues mid-rerun would re-honor the OLD
	// cancel via ListPendingCancelsForAgent, killing the new
	// attempt before it had a chance.
	//
	// logs_archive_uri / logs_archived_at = NULL: the prior
	// attempt's archive points at a GCS object that holds the
	// OLD run's logs. The reads.go cold-archive fallback consults
	// logs_archive_uri before hitting log_lines, so a rerun whose
	// log_lines we DELETE below would otherwise show the previous
	// attempt's logs in the UI ("logs of finished job show
	// previous job's logs"). Clearing the URI here pushes reads
	// back to the live log_lines path until the archiver runs
	// again for the new attempt.

	// Clear the previous attempt's logs — mirrors what
	// ReclaimJobForRetry does for reaper-driven retries and keeps
	// the log tab honest about what the new attempt produced.
	if _, err := tx.Exec(ctx, `DELETE FROM log_lines WHERE job_run_id = $1`, in.JobRunID); err != nil {
		return RerunJobResult{}, fmt.Errorf("store: rerun job: clear logs: %w", err)
	}
	// Same treatment for test_results: WriteTestResults is
	// delete+reinsert per job_run_id, so a rerun whose new attempt
	// either crashes before emitting or produces a different test
	// set would leave the old results visible in the Tests tab
	// under the rerun. Clear them up-front.
	if _, err := tx.Exec(ctx, `DELETE FROM test_results WHERE job_run_id = $1`, in.JobRunID); err != nil {
		return RerunJobResult{}, fmt.Errorf("store: rerun job: clear test results: %w", err)
	}
	// Same for artifacts (issue #3): a rerun re-uploads the same
	// paths, and without retiring the prior attempt's rows the new
	// inserts would either fail the partial-unique-index in
	// migration 00035 OR (pre-migration) accumulate duplicate
	// `ready` rows. Soft-delete here, sweeper GC's the storage
	// objects in the background — mirrors RetireArtifactsByJobRun's
	// behaviour in sweeper.requeueStaleJob.
	// pinned_at = NULL: same reasoning as RetireArtifactsByJobRun —
	// the prior attempt is being thrown away; preserving its pin
	// would leave the storage object orphan because the sweeper
	// skips pinned rows.
	if _, err := tx.Exec(ctx,
		`UPDATE artifacts
		    SET status = 'deleting', deleted_at = NOW(),
		        expires_at = NOW(), pinned_at = NULL
		  WHERE job_run_id = $1 AND deleted_at IS NULL`,
		in.JobRunID); err != nil {
		return RerunJobResult{}, fmt.Errorf("store: rerun job: retire artifacts: %w", err)
	}
	// Same for coverage (job_run_id UNIQUE): without clearing, the new attempt
	// inherits the previous attempt's report if it stops emitting one. Mirrors
	// DeleteCoverageReportsByJobRun in sweeper.requeueStaleJob.
	if _, err := tx.Exec(ctx, `DELETE FROM coverage_reports WHERE job_run_id = $1`, in.JobRunID); err != nil {
		return RerunJobResult{}, fmt.Errorf("store: rerun job: clear coverage: %w", err)
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
	// Reviving a run also clears any supersede state (#97): a run that was superseded
	// (canceled + superseded_by) and then rerun is a live run again, not "superseded
	// by #N". Resetting supersede_effects_{claimed,}_at too is load-bearing — without
	// it a LATER re-supersede of this run could never claim its effects (the claim
	// requires effects_at IS NULL), so its CancelJob frames / cleanup / audit would
	// never fire.
	//
	// Bumping service_generation here is what makes the generation-aware service
	// cleanup work (#97): a still-pending supersede/terminal CleanupRunServices carries
	// the OLD generation, and the revived run now dispatches its `services:` pods under
	// generation+1 (fresh name + label), so the stale cleanup (delete <= old gen) can't
	// touch them. The `status IN (terminal)` guard scopes the bump to a genuine REVIVE:
	// rerunning one job of an already-'running' run matches 0 rows here, so live
	// siblings keep reusing their current-generation pod (no split-pod set).
	if _, err := tx.Exec(ctx, `
		UPDATE runs
		SET status = 'running', finished_at = NULL,
		    superseded_by = NULL, cancel_reason = NULL,
		    supersede_effects_claimed_at = NULL, supersede_effects_at = NULL,
		    service_generation = service_generation + 1
		WHERE id = $1 AND status IN ('success', 'failed', 'canceled')
	`, runID); err != nil {
		return RerunJobResult{}, fmt.Errorf("store: rerun job: reopen run: %w", err)
	}

	// Revive downstream work an earlier fail-fast canceled. When this
	// job failed before, cascadeAfterJobCompletion canceled every queued
	// stage/job AFTER it (CancelQueuedStagesInRun + CancelQueuedJobsInRun)
	// — including awaiting approval gates. Reopening only THIS job's own
	// stage (above) isn't enough: a successful rerun would re-finalize
	// the run with those rows stuck 'canceled', so the promote gate never
	// reappears and production is silently skipped. (Observed live: a
	// release whose deploy failed on a missing secret, was fixed + rerun,
	// and completed 'success' with the prod gate dead in 'canceled'.)
	//
	// Scope: strictly downstream stages (ordinal greater than the rerun
	// job's stage) whose rows the SYSTEM canceled — cancel_requested_at
	// IS NULL leaves a user's explicit cancel-kill intact. Non-gate jobs
	// go back to 'queued'. Gates go straight back to 'awaiting_approval'
	// (re-stamping awaiting_since) because the dispatch query only sees
	// 'queued' rows: a gate revived as 'queued' would either be picked up
	// as a task-less job OR never re-arm. The scheduler's needs-gate
	// re-culls any revived job whose upstream is still failed, so reviving
	// the whole tail is self-correcting.
	if _, err := tx.Exec(ctx, `
		UPDATE job_runs
		SET status = 'queued', agent_id = NULL, started_at = NULL,
		    finished_at = NULL, exit_code = NULL, error = NULL
		WHERE run_id = $1
		  AND status = 'canceled'
		  AND cancel_requested_at IS NULL
		  AND approval_gate = false
		  AND stage_run_id IN (
		      SELECT id FROM stage_runs
		      WHERE run_id = $1
		        AND ordinal > (SELECT ordinal FROM stage_runs WHERE id = $2)
		  )
	`, runID, stageRunID); err != nil {
		return RerunJobResult{}, fmt.Errorf("store: rerun job: revive downstream jobs: %w", err)
	}
	rearmRows, err := tx.Query(ctx, `
		UPDATE job_runs
		SET status = 'awaiting_approval', awaiting_since = NOW(),
		    agent_id = NULL, started_at = NULL, finished_at = NULL,
		    exit_code = NULL, error = NULL,
		    decided_by = NULL, decided_at = NULL, decision = NULL
		WHERE run_id = $1
		  AND status = 'canceled'
		  AND cancel_requested_at IS NULL
		  AND approval_gate = true
		  AND stage_run_id IN (
		      SELECT id FROM stage_runs
		      WHERE run_id = $1
		        AND ordinal > (SELECT ordinal FROM stage_runs WHERE id = $2)
		  )
		RETURNING id
	`, runID, stageRunID)
	if err != nil {
		return RerunJobResult{}, fmt.Errorf("store: rerun job: re-arm downstream gates: %w", err)
	}
	rearmedGates, err := pgx.CollectRows(rearmRows, func(r pgx.CollectableRow) (uuid.UUID, error) {
		var id uuid.UUID
		return id, r.Scan(&id)
	})
	if err != nil {
		return RerunJobResult{}, fmt.Errorf("store: rerun job: collect re-armed gates: %w", err)
	}
	// A re-armed gate is approved from scratch: drop the votes it accrued before the
	// cancel/supersede. Votes are keyed only by (job_run_id, user_id), so without
	// this a stale pre-cancel vote counts toward the fresh quorum — a quorum=2 gate
	// with 1 old vote would pass on a single new one, bypassing the intended quorum.
	if len(rearmedGates) > 0 {
		if _, err := tx.Exec(ctx,
			`DELETE FROM job_run_approvals WHERE job_run_id = ANY($1::uuid[])`, rearmedGates); err != nil {
			return RerunJobResult{}, fmt.Errorf("store: rerun job: clear re-armed gate votes: %w", err)
		}
	}
	// Reopen the stages that just got a job back so GetRunProgress counts
	// them as unfinished again (it keys on queued/running). A downstream
	// stage whose jobs were ALL user-canceled gets no revived job and
	// correctly stays 'canceled'.
	if _, err := tx.Exec(ctx, `
		UPDATE stage_runs
		SET status = 'queued', started_at = NULL, finished_at = NULL
		WHERE run_id = $1
		  AND status = 'canceled'
		  AND ordinal > (SELECT ordinal FROM stage_runs WHERE id = $2)
		  AND EXISTS (
		      SELECT 1 FROM job_runs jr
		      WHERE jr.stage_run_id = stage_runs.id
		        AND jr.status IN ('queued', 'awaiting_approval')
		  )
	`, runID, stageRunID); err != nil {
		return RerunJobResult{}, fmt.Errorf("store: rerun job: reopen downstream stages: %w", err)
	}

	// Drop gate-pass markers invalidated by the re-armed gates (#97): those gates are
	// awaiting_approval again, so the run's "cleared env" claim for them is stale and
	// must be re-earned through approval. Runs after the re-arm so it sees them.
	if err := s.clearRevivedGatePassMarkers(ctx, tx, runID); err != nil {
		return RerunJobResult{}, err
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
