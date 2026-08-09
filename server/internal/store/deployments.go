package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gocdnext/gocdnext/server/internal/db"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

// Deployment status values (deployment_revisions.status). Mirrors the
// CHECK constraint in migrations 00046 + 00079 (canceled).
const (
	DeployStatusInProgress = "in_progress"
	DeployStatusSuccess    = "success"
	DeployStatusFailed     = "failed"
	// DeployStatusCanceled: the deploy job was canceled (#207). Never becomes the
	// current deployment, stays in history, excluded from DORA (the analytics
	// queries filter status IN ('success','failed')).
	DeployStatusCanceled = "canceled"
)

// Environment is a project-scoped deploy target (#39).
type Environment struct {
	ID          uuid.UUID
	ProjectID   uuid.UUID
	Name        string
	Description string
	// TotalDeploys is the number of deployment_revisions rows for this
	// environment. It is maintained transactionally by the create/delete
	// revision paths so environment list pages do not need COUNT(*) probes.
	TotalDeploys int64
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// DeploymentRevision is one recorded deploy attempt. RunID/JobRunID
// are pointers because they go NULL when the run is garbage-collected
// (the revision survives as an audit fact). FinishedAt is nil while
// in_progress.
type DeploymentRevision struct {
	ID            uuid.UUID
	EnvironmentID uuid.UUID
	RunID         *uuid.UUID
	JobRunID      *uuid.UUID
	Attempt       int32
	Version       string
	Status        string
	IsRollback    bool
	DeployedBy    string
	CreatedAt     time.Time
	FinishedAt    *time.Time
}

// CreateDeploymentRevisionInput is the dispatch-time payload. Attempt
// is the job_run's dispatch attempt — a job_run keeps its id across
// retries/reaper-requeues, so (JobRunID, Attempt) is what identifies
// one deploy attempt.
type CreateDeploymentRevisionInput struct {
	EnvironmentID uuid.UUID
	RunID         uuid.UUID
	JobRunID      uuid.UUID
	Attempt       int32
	Version       string
	IsRollback    bool
	DeployedBy    string
}

// DeploymentRevisionGuardStatus is the dispatch-time supersede backstop verdict.
type DeploymentRevisionGuardStatus string

const (
	DeploymentRevisionGuardAllowed  DeploymentRevisionGuardStatus = "allowed"
	DeploymentRevisionGuardBlocked  DeploymentRevisionGuardStatus = "blocked"
	DeploymentRevisionGuardLockBusy DeploymentRevisionGuardStatus = "lock_busy"
)

// BeginDeploymentRevisionGuardInput is the lane/env identity for a deploy job
// before AssignJob runs. The scheduler holds the returned guard until after
// DispatchAssignment, so approve-time gate-pass marker writes serialize against
// the whole AssignJob -> dispatch window.
type BeginDeploymentRevisionGuardInput struct {
	PipelineID  uuid.UUID
	Counter     int64
	Ref         string
	LaneMode    string
	Environment string
}

// DeploymentRevisionGuard owns a session-level advisory lock on one lane/env.
// Release must be called before the pooled connection is returned.
type DeploymentRevisionGuard struct {
	conn     *pgxpool.Conn
	lockKey  int64
	released bool
}

// BeginDeploymentRevisionGuard takes the Phase-2 supersede dispatch backstop lock
// and checks whether a newer non-canceled run in the same lane has already cleared
// the gate for this concrete environment. A blocked/errored guard means the caller
// must not dispatch the deploy job.
func (s *Store) BeginDeploymentRevisionGuard(ctx context.Context, in BeginDeploymentRevisionGuardInput) (*DeploymentRevisionGuard, DeploymentRevisionGuardStatus, error) {
	if in.LaneMode != domain.SupersedeBranch && in.LaneMode != domain.SupersedePipeline {
		return nil, DeploymentRevisionGuardAllowed, nil
	}
	key := LaneEnvLockKey(in.PipelineID, in.LaneMode, in.Ref, in.Environment)
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("store: deployment guard acquire: %w", err)
	}
	guard := &DeploymentRevisionGuard{conn: conn, lockKey: key}
	ok, err := guard.tryLock(ctx)
	if err != nil {
		raw := conn.Hijack()
		_ = raw.Close(context.Background())
		return nil, "", err
	}
	if !ok {
		conn.Release()
		return nil, DeploymentRevisionGuardLockBusy, nil
	}

	blocked, err := guard.newerGatePassExists(ctx, in)
	if err != nil {
		_ = guard.Release(ctx)
		return nil, "", err
	}
	if blocked {
		_ = guard.Release(ctx)
		return nil, DeploymentRevisionGuardBlocked, nil
	}
	return guard, DeploymentRevisionGuardAllowed, nil
}

func (g *DeploymentRevisionGuard) tryLock(ctx context.Context) (bool, error) {
	var ok bool
	if err := g.conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, g.lockKey).Scan(&ok); err != nil {
		return false, fmt.Errorf("store: deployment guard lock: %w", err)
	}
	return ok, nil
}

func (g *DeploymentRevisionGuard) newerGatePassExists(ctx context.Context, in BeginDeploymentRevisionGuardInput) (bool, error) {
	var blocked bool
	if in.LaneMode == domain.SupersedeBranch {
		err := g.conn.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1
				FROM run_gate_pass gp
				JOIN runs r ON r.id = gp.run_id
				WHERE gp.pipeline_id = $1
				  AND gp.ref = $2
				  AND gp.environment = $3
				  AND gp.counter > $4
				  AND r.status <> 'canceled'
			)
		`, pgUUID(in.PipelineID), in.Ref, in.Environment, in.Counter).Scan(&blocked)
		if err != nil {
			return false, fmt.Errorf("store: deployment guard newer gate-pass: %w", err)
		}
		return blocked, nil
	}
	err := g.conn.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM run_gate_pass gp
			JOIN runs r ON r.id = gp.run_id
			WHERE gp.pipeline_id = $1
			  AND gp.environment = $2
			  AND gp.counter > $3
			  AND r.status <> 'canceled'
		)
	`, pgUUID(in.PipelineID), in.Environment, in.Counter).Scan(&blocked)
	if err != nil {
		return false, fmt.Errorf("store: deployment guard newer gate-pass: %w", err)
	}
	return blocked, nil
}

// Release drops the session-level advisory lock and returns the pooled connection.
func (g *DeploymentRevisionGuard) Release(_ context.Context) error {
	if g == nil || g.released {
		return nil
	}
	g.released = true
	unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var ok bool
	if err := g.conn.QueryRow(unlockCtx, `SELECT pg_advisory_unlock($1)`, g.lockKey).Scan(&ok); err != nil {
		conn := g.conn.Hijack()
		_ = conn.Close(context.Background())
		return fmt.Errorf("store: deployment guard unlock: %w", err)
	}
	g.conn.Release()
	if !ok {
		return fmt.Errorf("store: deployment guard unlock: lock was not held")
	}
	return nil
}

// EnsureEnvironment lazy-creates (or touches) the named environment
// for a project and returns its id. Called at dispatch of a job
// carrying a deploy: block — the first deploy to "production" is what
// brings the environment into existence.
func (s *Store) EnsureEnvironment(ctx context.Context, projectID uuid.UUID, name string) (uuid.UUID, error) {
	id, err := s.q.UpsertEnvironment(ctx, db.UpsertEnvironmentParams{
		ProjectID: pgUUID(projectID),
		Name:      name,
	})
	if err != nil {
		return uuid.Nil, fmt.Errorf("store: ensure environment %q: %w", name, err)
	}
	return fromPgUUID(id), nil
}

// EnvDeleteOutcome is the result of DeleteEnvironment.
type EnvDeleteOutcome int

const (
	// EnvDeleteDeleted: the environment was idle and is gone (with its finalized
	// history + any target, via cascade).
	EnvDeleteDeleted EnvDeleteOutcome = iota
	// EnvDeleteActive: an in-flight deploy (in_progress revision or a live
	// deploy_watch) blocked the delete → 409.
	EnvDeleteActive
	// EnvDeleteAbsent: the environment isn't in this project → 404.
	EnvDeleteAbsent
)

// DeleteEnvironment hard-deletes an environment scoped to its project, but ONLY
// when it has no in-flight deploy — an in_progress deployment_revision or a live
// deploy_watch. Deleting one out from under a running deploy would orphan the
// job: its revision + watch cascade away while the job_run stays running with no
// agent (the reaper would later re-see it as orphaned). Once idle, ON DELETE
// CASCADE fans the delete out to the FINALIZED history + any registered target
// (incl. a gated one), which is why the API gates this to admin. The idle check
// and the delete are one atomic statement, so a deploy that starts concurrently
// can't slip through a check→delete gap. Environments are lazy, so a later deploy
// to the same name re-creates it empty.
func (s *Store) DeleteEnvironment(ctx context.Context, projectID, envID uuid.UUID) (EnvDeleteOutcome, error) {
	row, err := s.q.DeleteEnvironmentIfIdle(ctx, db.DeleteEnvironmentIfIdleParams{
		ProjectID: pgUUID(projectID),
		ID:        pgUUID(envID),
	})
	if err != nil {
		return EnvDeleteAbsent, fmt.Errorf("store: delete environment %s: %w", envID, err)
	}
	switch {
	case row.Deleted:
		return EnvDeleteDeleted, nil
	case !row.Existed:
		return EnvDeleteAbsent, nil
	default:
		// Existed but survived the delete → an in-flight deploy held it.
		return EnvDeleteActive, nil
	}
}

// CreateDeploymentRevision records an in_progress deploy at dispatch.
func (s *Store) CreateDeploymentRevision(ctx context.Context, in CreateDeploymentRevisionInput) (uuid.UUID, error) {
	id, err := s.q.CreateDeploymentRevision(ctx, db.CreateDeploymentRevisionParams{
		EnvironmentID: pgUUID(in.EnvironmentID),
		RunID:         nullableUUID(in.RunID),
		JobRunID:      nullableUUID(in.JobRunID),
		Attempt:       in.Attempt,
		Version:       in.Version,
		IsRollback:    in.IsRollback,
		DeployedBy:    in.DeployedBy,
	})
	if err != nil {
		return uuid.Nil, fmt.Errorf("store: create deployment revision: %w", err)
	}
	return fromPgUUID(id), nil
}

// FinalizeDeploymentRevision flips the in_progress revision of a
// SPECIFIC (job_run, attempt) to success/failed. Called on the job's
// terminal result (with the attempt that ran) and by the reaper (with
// the attempt it just declared dead). Keying on attempt is what keeps
// a success on attempt 1 from also flipping a stale attempt-0 row.
// Returns rows updated — 0 means no deploy: block on this attempt, or
// already finalised (the status='in_progress' guard is idempotent).
func (s *Store) FinalizeDeploymentRevision(ctx context.Context, jobRunID uuid.UUID, attempt int32, status string) (int64, error) {
	if status != DeployStatusSuccess && status != DeployStatusFailed && status != DeployStatusCanceled {
		return 0, fmt.Errorf("store: finalize deployment revision: invalid status %q", status)
	}
	n, err := s.q.FinalizeDeploymentRevision(ctx, db.FinalizeDeploymentRevisionParams{
		JobRunID: pgUUID(jobRunID),
		Attempt:  attempt,
		Status:   status,
	})
	if err != nil {
		return 0, fmt.Errorf("store: finalize deployment revision: %w", err)
	}
	return n, nil
}

// DeleteDeploymentRevision removes an in_progress revision created at
// dispatch when the dispatch then failed to reach an agent. Scoped to
// in_progress in SQL so it can never erase a finalized audit row.
func (s *Store) DeleteDeploymentRevision(ctx context.Context, id uuid.UUID) error {
	if err := s.q.DeleteDeploymentRevision(ctx, pgUUID(id)); err != nil {
		return fmt.Errorf("store: delete deployment revision: %w", err)
	}
	return nil
}

// EnvironmentWithCurrent pairs an environment with its current
// deployment (newest successful revision), or Current=nil when nothing
// has deployed there yet. Backs the Environments tab.
//
// It does NOT embed Environment. The listing must also surface FREEZE-ONLY
// rows (00078) — a pre-emptive freeze on an env nothing has deployed to yet,
// or an orphan freeze left by deleting a frozen env — and those have no
// environments row at all. Embedding the strong type would force a zero UUID
// and zero timestamps onto them, which the UI would faithfully render as
// "id 00000000-…, created 0001-01-01". So the LISTING shape gets optional
// identity, and the strong `Environment` type (with a real ID) stays exactly
// as it is for every flow that needs an actual environment row.
type EnvironmentWithCurrent struct {
	// ID is nil for a freeze-only row. HasEnvironmentRow is the explicit
	// discriminator: callers gate id-dependent affordances (history, rollback,
	// delete) on it rather than on a nil check they might forget.
	ID                *uuid.UUID
	HasEnvironmentRow bool
	ProjectID         uuid.UUID
	Name              string
	Description       string
	TotalDeploys      int64
	// CreatedAt/UpdatedAt are nil for a freeze-only row.
	CreatedAt *time.Time
	UpdatedAt *time.Time

	// Frozen is the change-freeze state. FrozenBy/FreezeReason are
	// operator-sensitive (they can carry incident detail) and are redacted
	// below maintainer at the API boundary; Frozen/FrozenAt are not.
	Frozen       bool
	FrozenAt     *time.Time
	FrozenBy     string
	FreezeReason string

	Current *DeploymentRevision
}

// ListEnvironmentsWithCurrent returns every environment of a project with its
// current deployment attached, PLUS any environment that exists only as a
// change-freeze — one query, no N+1.
func (s *Store) ListEnvironmentsWithCurrent(ctx context.Context, projectID uuid.UUID) ([]EnvironmentWithCurrent, error) {
	rows, err := s.q.ListEnvironmentsWithCurrentByProject(ctx, pgUUID(projectID))
	if err != nil {
		return nil, fmt.Errorf("store: list environments with current: %w", err)
	}
	out := make([]EnvironmentWithCurrent, 0, len(rows))
	for _, r := range rows {
		ewc := EnvironmentWithCurrent{
			HasEnvironmentRow: r.HasEnvironmentRow,
			ProjectID:         projectID,
			Name:              r.Name,
			Description:       r.Description,
			TotalDeploys:      r.TotalDeploys,
			CreatedAt:         pgTimePtr(r.CreatedAt),
			UpdatedAt:         pgTimePtr(r.UpdatedAt),
			Frozen:            r.Frozen,
			FrozenAt:          pgTimePtr(r.FrozenAt),
			FrozenBy:          r.FrozenBy,
			FreezeReason:      r.FreezeReason,
		}
		if r.ID.Valid {
			id := fromPgUUID(r.ID)
			ewc.ID = &id
		}
		// current_id NULL = nothing deployed yet (see query comment). A
		// freeze-only row can never have one — the LATERAL probes on e.id.
		if r.CurrentID.Valid && ewc.ID != nil {
			ewc.Current = &DeploymentRevision{
				ID:            fromPgUUID(r.CurrentID),
				EnvironmentID: *ewc.ID,
				RunID:         pgUUIDPtr(r.CurrentRunID),
				Attempt:       r.CurrentAttempt,
				Version:       r.CurrentVersion,
				Status:        DeployStatusSuccess,
				IsRollback:    r.CurrentIsRollback,
				DeployedBy:    r.CurrentDeployedBy,
				CreatedAt:     r.CurrentCreatedAt.Time,
				FinishedAt:    pgTimePtr(r.CurrentFinishedAt),
			}
		}
		out = append(out, ewc)
	}
	return out, nil
}

// ListEnvironments returns a project's environments, ordered by name.
func (s *Store) ListEnvironments(ctx context.Context, projectID uuid.UUID) ([]Environment, error) {
	rows, err := s.q.ListEnvironmentsByProject(ctx, pgUUID(projectID))
	if err != nil {
		return nil, fmt.Errorf("store: list environments: %w", err)
	}
	out := make([]Environment, 0, len(rows))
	for _, r := range rows {
		out = append(out, Environment{
			ID:           fromPgUUID(r.ID),
			ProjectID:    fromPgUUID(r.ProjectID),
			Name:         r.Name,
			Description:  r.Description,
			TotalDeploys: r.TotalDeploys,
			CreatedAt:    r.CreatedAt.Time,
			UpdatedAt:    r.UpdatedAt.Time,
		})
	}
	return out, nil
}

// UpdateEnvironmentDescription edits the human-facing description.
func (s *Store) UpdateEnvironmentDescription(ctx context.Context, projectID, envID uuid.UUID, desc string) error {
	if err := s.q.UpdateEnvironmentDescription(ctx, db.UpdateEnvironmentDescriptionParams{
		ProjectID:   pgUUID(projectID),
		ID:          pgUUID(envID),
		Description: desc,
	}); err != nil {
		return fmt.Errorf("store: update environment description: %w", err)
	}
	return nil
}

// CurrentDeployment returns the environment's current version (newest
// successful revision). found=false when nothing has deployed yet.
func (s *Store) CurrentDeployment(ctx context.Context, envID uuid.UUID) (DeploymentRevision, bool, error) {
	row, err := s.q.CurrentDeploymentByEnvironment(ctx, pgUUID(envID))
	if errors.Is(err, pgx.ErrNoRows) {
		return DeploymentRevision{}, false, nil
	}
	if err != nil {
		return DeploymentRevision{}, false, fmt.Errorf("store: current deployment: %w", err)
	}
	return revisionFromRow(row), true, nil
}

// Rollback sentinels — the endpoint maps each to an HTTP status.
var (
	ErrEnvironmentNotFound      = errors.New("environment not found")
	ErrRevisionNotFound         = errors.New("deployment revision not found")
	ErrRevisionWrongEnvironment = errors.New("revision belongs to a different environment")
	ErrRollbackNotSuccessful    = errors.New("can only roll back to a successful deploy")
	ErrRollbackRunGone          = errors.New("the deploy's run was garbage-collected; cannot roll back to it")
	ErrRedeployNoCurrent        = errors.New("environment has no current deploy to redeploy")
)

// RollbackInput points the rollback at one past revision of an
// environment, on behalf of an actor (#39 phase 3).
type RollbackInput struct {
	ProjectID     uuid.UUID
	EnvironmentID uuid.UUID
	RevisionID    uuid.UUID
	TriggeredBy   string
}

// RedeployCurrentInput re-runs the environment's current successful deploy as a
// normal deploy (not a rollback), on behalf of an actor.
type RedeployCurrentInput struct {
	ProjectID     uuid.UUID
	EnvironmentID uuid.UUID
	TriggeredBy   string
}

// RollbackToRevision re-runs the deploy job of the run that produced
// the target revision, flagged as a rollback. The version "freezes"
// for free: re-running that job re-resolves needs.outputs from its
// run's immutable outputs, so the SAME version ships again, and the
// dispatch records a fresh revision with is_rollback=true. Validates
// the full chain (env owns by project, revision in env, revision is a
// successful deploy whose run still exists) and returns a sentinel for
// each failure so the endpoint maps it to the right HTTP status. The
// underlying RerunJob may return ErrJobRunActive (the deploy job is
// mid-run) — surfaced as-is.
func (s *Store) RollbackToRevision(ctx context.Context, in RollbackInput) (RerunJobResult, error) {
	owns, err := s.EnvironmentBelongsToProject(ctx, in.ProjectID, in.EnvironmentID)
	if err != nil {
		return RerunJobResult{}, err
	}
	if !owns {
		return RerunJobResult{}, ErrEnvironmentNotFound
	}

	row, err := s.q.GetDeploymentRevision(ctx, pgUUID(in.RevisionID))
	if errors.Is(err, pgx.ErrNoRows) {
		return RerunJobResult{}, ErrRevisionNotFound
	}
	if err != nil {
		return RerunJobResult{}, fmt.Errorf("store: rollback: get revision: %w", err)
	}
	rev := revisionFromRow(row)
	if rev.EnvironmentID != in.EnvironmentID {
		return RerunJobResult{}, ErrRevisionWrongEnvironment
	}
	if rev.Status != DeployStatusSuccess {
		return RerunJobResult{}, ErrRollbackNotSuccessful
	}
	if rev.JobRunID == nil {
		return RerunJobResult{}, ErrRollbackRunGone
	}

	// A rollback IS a deploy to this environment, so a change-freeze must refuse
	// it — and the check has to happen inside the rerun's own transaction, not
	// before it: RerunJob opens its own tx, so a pre-check here would leave a
	// window for a freeze to commit between "not frozen" and the rerun write.
	// The guard takes ProjectEnvFreezeLockKey before any row lock (global lock
	// order) and re-reads the table under it.
	return s.rerunJobTx(ctx, RerunJobInput{
		JobRunID:    *rev.JobRunID,
		TriggeredBy: in.TriggeredBy,
		IsRollback:  true,
	}, func(ctx context.Context, tx pgx.Tx) error {
		return guardDeployRerunNotFrozen(ctx, tx, in.ProjectID, in.EnvironmentID)
	})
}

// RedeployCurrent re-runs the job that produced the environment's current
// successful deploy, without stamping deploy_rollback. The server resolves
// "current" at request time so a stale UI cannot accidentally ask for an older
// revision while calling it a redeploy.
func (s *Store) RedeployCurrent(ctx context.Context, in RedeployCurrentInput) (RerunJobResult, error) {
	owns, err := s.EnvironmentBelongsToProject(ctx, in.ProjectID, in.EnvironmentID)
	if err != nil {
		return RerunJobResult{}, err
	}
	if !owns {
		return RerunJobResult{}, ErrEnvironmentNotFound
	}

	rev, found, err := s.CurrentDeployment(ctx, in.EnvironmentID)
	if err != nil {
		return RerunJobResult{}, err
	}
	if !found {
		return RerunJobResult{}, ErrRedeployNoCurrent
	}
	if rev.JobRunID == nil {
		return RerunJobResult{}, ErrRollbackRunGone
	}

	// Redeploying is still a deploy to this environment, so the same freeze
	// guard as rollback runs inside RerunJob's transaction.
	return s.rerunJobTx(ctx, RerunJobInput{
		JobRunID:    *rev.JobRunID,
		TriggeredBy: in.TriggeredBy,
		IsRollback:  false,
	}, func(ctx context.Context, tx pgx.Tx) error {
		return guardDeployRerunNotFrozen(ctx, tx, in.ProjectID, in.EnvironmentID)
	})
}

// guardDeployRerunNotFrozen resolves the environment's NAME from its id inside
// the rerun tx, then locks + freeze-checks on that name.
//
// The name lookup is here rather than in the caller because it must see the same
// snapshot as the check: freezes are name-keyed (environments rows are lazy and
// can be deleted/recreated), so resolving the name outside the tx would risk
// locking one name and re-running a deploy against another.
func guardDeployRerunNotFrozen(ctx context.Context, tx pgx.Tx, projectID, envID uuid.UUID) error {
	var name string
	err := tx.QueryRow(ctx,
		`SELECT name FROM environments WHERE id = $1 AND project_id = $2`,
		pgUUID(envID), pgUUID(projectID)).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		// Deleted between the ownership check above and here. Not a freeze
		// concern — report it as the missing environment it is.
		return ErrEnvironmentNotFound
	}
	if err != nil {
		return fmt.Errorf("store: rollback freeze check: resolve environment: %w", err)
	}
	if err := lockProjectEnvFreeze(ctx, tx, projectID, name); err != nil {
		return err
	}
	var frozen bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM environment_freezes WHERE project_id = $1 AND name = $2)`,
		pgUUID(projectID), name).Scan(&frozen); err != nil {
		return fmt.Errorf("store: rollback freeze check: %w", err)
	}
	if frozen {
		return fmt.Errorf("%w: %s", ErrEnvironmentFrozen, name)
	}
	return nil
}

// EnvironmentBelongsToProject reports whether envID is owned by
// projectID — the scope guard the read API uses before serving an
// environment's deployments through a project-scoped URL.
func (s *Store) EnvironmentBelongsToProject(ctx context.Context, projectID, envID uuid.UUID) (bool, error) {
	ok, err := s.q.EnvironmentBelongsToProject(ctx, db.EnvironmentBelongsToProjectParams{
		ProjectID: pgUUID(projectID),
		ID:        pgUUID(envID),
	})
	if err != nil {
		return false, fmt.Errorf("store: environment scope check: %w", err)
	}
	return ok, nil
}

// EnvironmentDeploymentTotal returns the persisted history size for envID, while
// also scope-checking that the environment belongs to projectID.
func (s *Store) EnvironmentDeploymentTotal(ctx context.Context, projectID, envID uuid.UUID) (int64, bool, error) {
	total, err := s.q.GetEnvironmentDeploymentTotalByProject(ctx, db.GetEnvironmentDeploymentTotalByProjectParams{
		ProjectID: pgUUID(projectID),
		ID:        pgUUID(envID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("store: environment deployment total: %w", err)
	}
	return total, true, nil
}

// ListDeploymentHistory returns the environment's timeline (all
// statuses), newest first, capped at limit.
func (s *Store) ListDeploymentHistory(ctx context.Context, envID uuid.UUID, limit int32) ([]DeploymentRevision, error) {
	rows, err := s.q.ListDeploymentHistory(ctx, db.ListDeploymentHistoryParams{
		EnvironmentID: pgUUID(envID),
		Limit:         limit,
	})
	if err != nil {
		return nil, fmt.Errorf("store: list deployment history: %w", err)
	}
	out := make([]DeploymentRevision, 0, len(rows))
	for _, r := range rows {
		out = append(out, revisionFromRow(r))
	}
	return out, nil
}

// ErrDeploymentRevisionNotFound is returned when no revision matches the id.
var ErrDeploymentRevisionNotFound = errors.New("store: deployment revision not found")

// GetDeploymentRevision fetches a single revision by id (used by the deploy watcher
// to read back a terminalized deploy's status).
func (s *Store) GetDeploymentRevision(ctx context.Context, id uuid.UUID) (DeploymentRevision, error) {
	r, err := s.q.GetDeploymentRevision(ctx, pgUUID(id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return DeploymentRevision{}, ErrDeploymentRevisionNotFound
		}
		return DeploymentRevision{}, fmt.Errorf("store: get deployment revision: %w", err)
	}
	return revisionFromRow(r), nil
}

func revisionFromRow(r db.DeploymentRevision) DeploymentRevision {
	return DeploymentRevision{
		ID:            fromPgUUID(r.ID),
		EnvironmentID: fromPgUUID(r.EnvironmentID),
		RunID:         pgUUIDPtr(r.RunID),
		JobRunID:      pgUUIDPtr(r.JobRunID),
		Attempt:       r.Attempt,
		Version:       r.Version,
		Status:        r.Status,
		IsRollback:    r.IsRollback,
		DeployedBy:    r.DeployedBy,
		CreatedAt:     r.CreatedAt.Time,
		FinishedAt:    pgTimePtr(r.FinishedAt),
	}
}
