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
			Delivery:       "manual-" + in.PipelineID.String(),
			TriggeredBy:    triggeredBy,
			Cause:          "manual",
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

	causeDetail, _ := json.Marshal(map[string]any{
		"delivery": "manual-" + in.PipelineID.String(),
	})
	return s.insertRunSkeleton(ctx, insertRunSkeletonInput{
		PipelineID:  in.PipelineID,
		Cause:       "manual",
		CauseDetail: causeDetail,
		Revisions:   json.RawMessage(`{}`),
		TriggeredBy: triggeredBy,
	})
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
