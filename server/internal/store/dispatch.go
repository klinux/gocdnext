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

// DispatchableJob carries the queued-job shape the scheduler needs to build a
// JobAssignment without touching sqlc types.
type DispatchableJob struct {
	ID         uuid.UUID
	RunID      uuid.UUID
	StageRunID uuid.UUID
	Name       string
	MatrixKey  string
	Image      string
	Needs      []string
	// Attempt is the current retry counter on the row — needed by
	// dispatch-time fail paths so CompleteJob's snapshot CAS matches
	// the (NULL agent, current attempt) tuple instead of defaulting
	// to attempt=0 (which races with a reaper-requeue that bumped it).
	Attempt int32
	// DeployRollback is true when this dispatch is a deployment
	// rollback (#39): the scheduler opens the deployment_revision
	// with is_rollback=true. Inert for non-deploy jobs.
	DeployRollback bool
}

// AssignedJob is the result of a successful AssignJob (row matched by
// optimistic predicate). Zero value signals another caller won the race.
type AssignedJob struct {
	ID      uuid.UUID
	RunID   uuid.UUID
	AgentID uuid.UUID
	Name    string
	// Attempt is the row's attempt counter AFTER AssignJob ran.
	// The scheduler stamps this on the target session via
	// RecordAssignment so the result handler can validate it as
	// the snapshot when CompleteJob fires — guards against a
	// stale revoked-session result completing a redispatched
	// attempt on the same agent UUID.
	Attempt int32
}

// RunForDispatch bundles the run row and its pipeline's definition snapshot,
// which is all the scheduler needs to materialize JobAssignments.
type RunForDispatch struct {
	ID         uuid.UUID
	PipelineID uuid.UUID
	ProjectID  uuid.UUID
	// ProjectSlug rides along for the OIDC id_token claims — the
	// `sub` grammar pins policies to project:{slug}:..., and slugs
	// are the operator-facing identity (UUIDs are not memorable in
	// IAM conditions). Same hot-path round-trip as everything else.
	ProjectSlug string
	Counter     int64
	Status      string
	Revisions   json.RawMessage
	// Ref is the lane key stamped on run creation. For branch-scoped
	// supersede it is the branch name; for tag/manual/no-branch runs it is
	// empty and all such runs share the pipeline lane.
	Ref        string
	Definition json.RawMessage
	ConfigPath string
	// ProjectNotifications is the owning project's notifications
	// JSONB, pulled in the same round-trip as Definition so the
	// synth-notification dispatch path can fall back to it when
	// the pipeline didn't declare its own block.
	ProjectNotifications json.RawMessage
	// Cause is the trigger that created the run — webhook,
	// pull_request, manual, upstream, schedule, poll. Materialised
	// into CI_CAUSE so pipelines can branch on `${CI_CAUSE}` (e.g.
	// "only push to prod when manual" or "skip lint on upstream
	// fanout"). Empty for legacy runs predating the column.
	Cause string
	// CauseDetail is the JSONB payload the webhook handler stamps
	// alongside Cause. For PR runs it carries pr_number / pr_title /
	// pr_head_ref / pr_base_ref / pr_author / pr_url — every CI
	// platform exposes equivalents as CI_PULL_REQUEST_* env vars,
	// and scheduler/civars.go decodes this blob into them. Nil /
	// empty / malformed JSON silently produces no PR vars (manual,
	// poll, push triggers all hit this path).
	CauseDetail json.RawMessage
}

// OtherRunningRunForPipeline returns the run_id of an in-flight
// predecessor blocking this run from advancing past the serial-
// concurrency gate, or (uuid.Nil, false, nil) when no predecessor
// exists. The two-return-value (id, exists) shape lets callers do
// one query for both the gate decision AND the predecessor id they
// stamp onto runs.queue_reason for operator visibility (issue #4
// path #2).
//
// Replaces the prior boolean-returning OtherRunningRunExistsForPipeline
// — see queries/scheduler.sql for the migration rationale.
func (s *Store) OtherRunningRunForPipeline(ctx context.Context, pipelineID, runID uuid.UUID) (uuid.UUID, bool, error) {
	row, err := s.q.OtherRunningRunForPipeline(ctx,
		db.OtherRunningRunForPipelineParams{
			PipelineID: pgUUID(pipelineID),
			ID:         pgUUID(runID),
		})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, false, nil
		}
		return uuid.Nil, false, fmt.Errorf("store: concurrency check: %w", err)
	}
	return fromPgUUID(row), true, nil
}

// ListAgentsForRun returns every distinct agent that ran (or is
// running) a job of the given run, FILTERED by engine='kubernetes'
// or legacy-empty (pre-engine-column registrations). Used by the
// run-terminal CleanupRunServices dispatch — services come up on
// whichever k8s agent ran the first job of the run, so the
// candidate set is "k8s agents that touched this run", not "every
// agent". Docker/Shell agents are excluded at the SQL layer
// because their cleanup would be a wasted RPC (no service pods to
// label-select against) AND the wasted dispatch would still count
// as ok in the server's aggregate, hiding a real leak under a
// "we tried" signal.
//
// Returns an empty slice (not error) when no k8s agent ran this
// run — could mean either a Docker/Shell-only run (no services to
// clean) or a k8s run whose agents have all been deleted from the
// agents table. The dispatch path unions this with currently-
// online k8s agents to cover the second case.
func (s *Store) ListAgentsForRun(ctx context.Context, runID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := s.q.ListAgentsForRun(ctx, pgUUID(runID))
	if err != nil {
		return nil, fmt.Errorf("store: list agents for run: %w", err)
	}
	out := make([]uuid.UUID, 0, len(rows))
	for _, r := range rows {
		out = append(out, fromPgUUID(r))
	}
	return out, nil
}

// RunHasServices is the cheap pre-flight before dispatching
// CleanupRunServices to an agent: pipelines without a `services:`
// block don't need a cleanup at all, and the alternative (always
// dispatching) costs one k8s `kubectl get pods -l ...` per run
// completion. Reads runs.has_services — a snapshot bool stamped at
// run-insert time from the parsed pipeline definition (see
// migration 00036 + store.insertRunSkeleton). Snapshot rather than
// re-parsing the pipeline's `definition` JSONB at terminal time
// means the answer survives pipeline-row deletion AND avoids the
// JSONB key-casing trap (json.Marshal emits "Services", not
// "services"). Fail-closed when the run row is gone entirely.
func (s *Store) RunHasServices(ctx context.Context, runID uuid.UUID) (bool, error) {
	has, err := s.q.RunHasServices(ctx, pgUUID(runID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("store: has services: %w", err)
	}
	return has, nil
}

// SetRunQueueReason records why a queued run isn't advancing —
// today only the scheduler's serial-busy path uses this, but the
// column is plain TEXT so future producers (no-eligible-agent,
// approval-pending, ...) can extend the vocabulary. The store-side
// helper exists separately from ClearRunQueueReason so callers
// don't need to know about the `status='queued'` SQL guard.
func (s *Store) SetRunQueueReason(ctx context.Context, runID uuid.UUID, reason string) error {
	if err := s.q.SetRunQueueReason(ctx, db.SetRunQueueReasonParams{
		ID:          pgUUID(runID),
		QueueReason: &reason,
	}); err != nil {
		return fmt.Errorf("store: set queue_reason: %w", err)
	}
	return nil
}

// ClearRunQueueReason removes a previously-stamped reason. Idempotent
// — clearing an already-clear row is a cheap no-op via the
// IS NOT NULL guard in SQL. Called by the scheduler when the gate
// finally clears AND by the run-cancel path so a canceled-while-
// queued run doesn't carry a "waiting on #N" message in the list.
func (s *Store) ClearRunQueueReason(ctx context.Context, runID uuid.UUID) error {
	if err := s.q.ClearRunQueueReason(ctx, pgUUID(runID)); err != nil {
		return fmt.Errorf("store: clear queue_reason: %w", err)
	}
	return nil
}

// ListDispatchableJobs returns queued, unassigned jobs in the run's current
// active stage (lowest ordinal still holding queued/running work).
func (s *Store) ListDispatchableJobs(ctx context.Context, runID uuid.UUID) ([]DispatchableJob, error) {
	rows, err := s.q.ListDispatchableJobs(ctx, pgUUID(runID))
	if err != nil {
		return nil, fmt.Errorf("store: list dispatchable: %w", err)
	}
	out := make([]DispatchableJob, 0, len(rows))
	for _, r := range rows {
		out = append(out, DispatchableJob{
			ID:             fromPgUUID(r.ID),
			RunID:          fromPgUUID(r.RunID),
			StageRunID:     fromPgUUID(r.StageRunID),
			Name:           r.Name,
			MatrixKey:      stringValue(r.MatrixKey),
			Image:          stringValue(r.Image),
			Needs:          append([]string(nil), r.Needs...),
			Attempt:        r.Attempt,
			DeployRollback: r.DeployRollback,
		})
	}
	return out, nil
}

// AssignJob flips a queued job to running atomically. Returns ok=false (and no
// error) when another scheduler tick or replica already claimed it.
func (s *Store) AssignJob(ctx context.Context, jobID, agentID uuid.UUID) (AssignedJob, bool, error) {
	row, err := s.q.AssignJob(ctx, db.AssignJobParams{
		ID:      pgUUID(jobID),
		AgentID: pgUUID(agentID),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return AssignedJob{}, false, nil
		}
		return AssignedJob{}, false, fmt.Errorf("store: assign job: %w", err)
	}
	return AssignedJob{
		ID:      fromPgUUID(row.ID),
		RunID:   fromPgUUID(row.RunID),
		AgentID: fromPgUUID(row.AgentID),
		Name:    row.Name,
		Attempt: row.Attempt,
	}, true, nil
}

// UnassignJob rolls back a successful AssignJob whose downstream
// Dispatch failed (busy queue, session vanished). Snapshot-CAS so
// the rollback only fires if the row is still in the exact state
// AssignJob just produced.
//
// Returns ok=true + runID when the rollback landed (caller fires
// NOTIFY so the scheduler retries the run on the next tick).
// Returns ok=false when the predicate doesn't match — meaning a
// reaper / register-fence / rerun already moved the row to a
// different state and our rollback would be incorrect.
//
// attempt is NOT bumped: a Dispatch that never reached an agent
// doesn't count as a failed attempt. The retry-cap logic in
// ReclaimJobForRetry is for AGENT-side failures.
func (s *Store) UnassignJob(ctx context.Context, jobID, agentID uuid.UUID, expectedAttempt int32) (uuid.UUID, bool, error) {
	row, err := s.q.UnassignJob(ctx, db.UnassignJobParams{
		ID:              pgUUID(jobID),
		AgentID:         pgUUID(agentID),
		ExpectedAttempt: expectedAttempt,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, false, nil
		}
		return uuid.Nil, false, fmt.Errorf("store: unassign job: %w", err)
	}
	return fromPgUUID(row.RunID), true, nil
}

// MarkRunRunning promotes a queued run (idempotent: no-op if already running).
func (s *Store) MarkRunRunning(ctx context.Context, runID uuid.UUID) error {
	if err := s.q.MarkRunRunningIfQueued(ctx, pgUUID(runID)); err != nil {
		return fmt.Errorf("store: mark run running: %w", err)
	}
	return nil
}

// MarkStageRunning promotes a queued stage (idempotent).
func (s *Store) MarkStageRunning(ctx context.Context, stageRunID uuid.UUID) error {
	if err := s.q.MarkStageRunningIfQueued(ctx, pgUUID(stageRunID)); err != nil {
		return fmt.Errorf("store: mark stage running: %w", err)
	}
	return nil
}

// JobOutputs is one upstream job's outputs as the scheduler sees
// them at downstream-dispatch time. Status comes along so the
// substitution layer can distinguish "ran, no output for that key"
// (Status terminal, key missing → hard error: operator referenced
// what wasn't produced) from "haven't run yet" (Status non-terminal
// — the dispatch should be blocked by the `needs:` gate, so this
// shouldn't surface in practice; defence in depth).
type JobOutputs struct {
	Name      string
	MatrixKey string
	Status    string
	Outputs   map[string]string
}

// ListJobOutputsForRun returns the outputs of every job_run in `runID`
// whose name appears in `names`. Used by the scheduler when building
// a downstream job's JobAssignment to resolve
// `${{ needs.<job>.outputs.<key> }}` references (issue #10) and
// `${{ needs.<job>.matrix[KEY].outputs.<key> }}` matrix selectors
// (issue #21).
//
// Returns one entry per matrix instance for matrix jobs (matrix_key
// distinguishes the row). The scheduler's groupNeedsOutputs sorts
// rows into NeedsOutputs (matrix_key=”) vs MatrixNeedsOutputs
// (matrix_key!=”); see refs.go::NeedsOutputs for the routing
// contract. Empty slice when no name matches — surfaced again
// here as "no upstream outputs" so the substitution error
// message points at the right job.
func (s *Store) ListJobOutputsForRun(ctx context.Context, runID uuid.UUID, names []string) ([]JobOutputs, error) {
	if len(names) == 0 {
		return nil, nil
	}
	rows, err := s.q.ListJobOutputsForRun(ctx, db.ListJobOutputsForRunParams{
		RunID: pgUUID(runID),
		Names: names,
	})
	if err != nil {
		return nil, fmt.Errorf("store: list job outputs: %w", err)
	}
	out := make([]JobOutputs, 0, len(rows))
	for _, r := range rows {
		decoded := map[string]string{}
		if len(r.Outputs) > 0 {
			// Outputs is JSONB the server itself marshalled on
			// CompleteJob — a parse error here is data corruption
			// or a decoder bug, never operator input. Fail loud:
			// silently dropping a row would surface as
			// "downstream sees a key missing" at a much later
			// step, masking the actual storage/decoder problem.
			if err := json.Unmarshal(r.Outputs, &decoded); err != nil {
				return nil, fmt.Errorf("store: list job outputs: decode JSONB for job %q: %w", r.Name, err)
			}
		}
		out = append(out, JobOutputs{
			Name:      r.Name,
			MatrixKey: stringValue(r.MatrixKey),
			Status:    r.Status,
			Outputs:   decoded,
		})
	}
	return out, nil
}

// GetRunForDispatch loads the run + pipeline definition snapshot.
func (s *Store) GetRunForDispatch(ctx context.Context, runID uuid.UUID) (RunForDispatch, error) {
	row, err := s.q.GetRunForDispatch(ctx, pgUUID(runID))
	if err != nil {
		return RunForDispatch{}, fmt.Errorf("store: get run: %w", err)
	}
	return RunForDispatch{
		ID:                   fromPgUUID(row.ID),
		PipelineID:           fromPgUUID(row.PipelineID),
		ProjectID:            fromPgUUID(row.ProjectID),
		ProjectSlug:          row.ProjectSlug,
		Counter:              row.Counter,
		Status:               row.Status,
		Revisions:            row.Revisions,
		Ref:                  row.Ref,
		Definition:           row.Definition,
		ConfigPath:           row.ConfigPath,
		ProjectNotifications: row.ProjectNotifications,
		Cause:                row.Cause,
		CauseDetail:          row.CauseDetail,
	}, nil
}

// ListQueuedRunIDs returns every run still in `queued` status, oldest first.
// Used by the scheduler's periodic tick as a LISTEN backstop.
func (s *Store) ListQueuedRunIDs(ctx context.Context) ([]uuid.UUID, error) {
	rows, err := s.q.ListQueuedRunIDs(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: list queued runs: %w", err)
	}
	out := make([]uuid.UUID, 0, len(rows))
	for _, r := range rows {
		out = append(out, fromPgUUID(r))
	}
	return out, nil
}

// ListPipelineMaterials returns every material row tied to a pipeline. Used
// when assembling MaterialCheckout entries on a JobAssignment.
func (s *Store) ListPipelineMaterials(ctx context.Context, pipelineID uuid.UUID) ([]Material, error) {
	rows, err := s.q.ListMaterialsByPipeline(ctx, pgUUID(pipelineID))
	if err != nil {
		return nil, fmt.Errorf("store: list materials: %w", err)
	}
	out := make([]Material, 0, len(rows))
	for _, r := range rows {
		out = append(out, Material{
			ID:          fromPgUUID(r.ID),
			PipelineID:  fromPgUUID(r.PipelineID),
			Type:        r.Type,
			Config:      r.Config,
			Fingerprint: r.Fingerprint,
			AutoUpdate:  r.AutoUpdate,
			CreatedAt:   r.CreatedAt.Time,
		})
	}
	return out, nil
}
