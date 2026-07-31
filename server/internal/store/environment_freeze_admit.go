package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/gocdnext/gocdnext/server/internal/db"
)

// ---- reads ---------------------------------------------------------------

// EnvironmentFrozenState returns the freeze on (project, name).
// found=false means not frozen — a routine answer, not an error.
func (s *Store) EnvironmentFrozenState(ctx context.Context, projectID uuid.UUID, name string) (EnvironmentFreeze, bool, error) {
	row, err := s.q.GetEnvironmentFreeze(ctx, db.GetEnvironmentFreezeParams{
		ProjectID: pgUUID(projectID),
		Name:      name,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return EnvironmentFreeze{}, false, nil
		}
		return EnvironmentFreeze{}, false, fmt.Errorf("store: environment frozen state %q: %w", name, err)
	}
	return EnvironmentFreeze{
		Name:     name,
		FrozenAt: row.FrozenAt.Time,
		FrozenBy: row.FrozenBy,
		Reason:   row.Reason,
	}, true, nil
}

// FrozenEnvironments returns which of `names` are frozen, sorted by name.
//
// This is the core BATCH read: the scheduler calls it once per tick for the
// distinct deploy environments of the active stage's ready jobs, rather than
// probing per job. Sorted so a caller with several frozen envs can pick a
// deterministic winner — the run-level queue_reason holds ONE env, and it must
// not alternate between `frozen-deploy:prod` and `frozen-deploy:staging` from
// tick to tick.
//
// An empty `names` short-circuits without a round-trip: a deploy-less pipeline
// must not pay for this feature at all.
func (s *Store) FrozenEnvironments(ctx context.Context, projectID uuid.UUID, names []string) ([]string, error) {
	if len(names) == 0 {
		return nil, nil
	}
	rows, err := s.q.FrozenEnvironments(ctx, db.FrozenEnvironmentsParams{
		ProjectID: pgUUID(projectID),
		Names:     names,
	})
	if err != nil {
		return nil, fmt.Errorf("store: frozen environments: %w", err)
	}
	return rows, nil
}

// FrozenGovernedEnvs is FrozenEnvironments named for its approval-path caller:
// the set it is asked about is a gate's GOVERNED envs (domain.GovernedEnvs), and
// a call site reading `FrozenEnvironments(projectID, envs)` loses that context.
// Same query, same ordering — the approval error lists every frozen env, so the
// operator learns about all of them in one 409 instead of one per retry.
func (s *Store) FrozenGovernedEnvs(ctx context.Context, projectID uuid.UUID, envs []string) ([]string, error) {
	return s.FrozenEnvironments(ctx, projectID, envs)
}

// ListRunsHeldByEnvironment returns the runs currently stamped
// `frozen-deploy:<name>` in this project, so unfreeze can wake each one with a
// run_queued NOTIFY instead of leaving it for the next periodic tick.
//
// BEST-EFFORT by construction: it keys on the stamp, so a run that was never
// stamped (frozen between the pre-scan and the admission recheck) or whose
// reason was overwritten is not returned. The periodic drain tick is the
// backstop that recovers it — at most one tick of extra latency.
func (s *Store) ListRunsHeldByEnvironment(ctx context.Context, projectID uuid.UUID, name string) ([]uuid.UUID, error) {
	rows, err := s.q.ListRunsHeldByEnvironment(ctx, db.ListRunsHeldByEnvironmentParams{
		ProjectID: pgUUID(projectID),
		Name:      name,
	})
	if err != nil {
		return nil, fmt.Errorf("store: list runs held by environment %q: %w", name, err)
	}
	out := make([]uuid.UUID, 0, len(rows))
	for _, r := range rows {
		out = append(out, fromPgUUID(r))
	}
	return out, nil
}

// NotifyRunsQueued wakes several runs in ONE round-trip.
//
// Lifting a freeze on a busy environment can release dozens of held runs at
// once; a per-run pg_notify would make the unfreeze endpoint's latency scale
// with how effective the freeze was — the more it held, the slower it is to
// lift. pg_notify over unnest keeps it a single statement. Empty input is a
// no-op with no round-trip.
func (s *Store) NotifyRunsQueued(ctx context.Context, runIDs []uuid.UUID) error {
	if len(runIDs) == 0 {
		return nil
	}
	ids := make([]string, 0, len(runIDs))
	for _, id := range runIDs {
		ids = append(ids, id.String())
	}
	if _, err := s.pool.Exec(ctx,
		`SELECT pg_notify($1, id) FROM unnest($2::text[]) AS id`,
		RunQueuedChannel, ids); err != nil {
		return fmt.Errorf("store: notify run_queued batch: %w", err)
	}
	return nil
}

// ---- authoritative admission ---------------------------------------------

// DeployAdmission is the outcome of AssignDeployJobIfEnvNotFrozen. A bare bool
// would collapse "the environment is frozen" into "another scheduler won the
// CAS" — two outcomes that need different logs, different metrics and different
// test assertions.
type DeployAdmission string

const (
	// DeployAdmitted: the job is now running and owned by this caller.
	DeployAdmitted DeployAdmission = "admitted"
	// DeployAdmissionFrozen: a freeze on the target env committed before this
	// tx took the lock. Expected — stamp queue_reason and retry next tick.
	DeployAdmissionFrozen DeployAdmission = "frozen"
	// DeployAdmissionLost: the optimistic CAS lost to another tick/replica.
	DeployAdmissionLost DeployAdmission = "lost"
)

// AssignDeployJobIfEnvNotFrozen is the ADMISSION BOUNDARY for a plugin/agent
// deploy: it takes the per-(project, env) freeze lock, re-reads
// environment_freezes under it, and performs the assignment — all in ONE
// transaction. Freeze and unfreeze take the same lock, so once the freeze
// endpoint returns, no deploy to that environment can be admitted.
//
// What this does NOT cover, precisely: a deploy admitted BEFORE the freeze
// committed still EXECUTES afterwards (the agent runs it; a native deploy's
// ArgoCD Sync goes out). Holding a lock across that external I/O is not an
// option — no network calls inside a transaction — so "in-flight" is defined as
// exactly the admitted-before set, and cancelling it is out of scope.
//
// Deliberately a SEPARATE method from the generic AssignJob, which stays
// untouched: the overwhelming majority of dispatches are non-deploy jobs, and
// they must not pay for a lock + a freeze probe they can never need.
func (s *Store) AssignDeployJobIfEnvNotFrozen(ctx context.Context, jobID, agentID, projectID uuid.UUID, env string) (AssignedJob, DeployAdmission, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return AssignedJob{}, "", fmt.Errorf("store: assign deploy job begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Global lock order: the caller already holds the lane-env SESSION lock
	// (the supersede guard) when supersede is on; the freeze lock comes next,
	// and only then the row-level CAS inside AssignJob.
	if err := lockProjectEnvFreeze(ctx, tx, projectID, env); err != nil {
		return AssignedJob{}, "", err
	}

	q := s.q.WithTx(tx)
	frozen, err := q.EnvironmentIsFrozen(ctx, db.EnvironmentIsFrozenParams{
		ProjectID: pgUUID(projectID),
		Name:      env,
	})
	if err != nil {
		return AssignedJob{}, "", fmt.Errorf("store: assign deploy job freeze check: %w", err)
	}
	if frozen {
		return AssignedJob{}, DeployAdmissionFrozen, nil
	}

	row, err := q.AssignJob(ctx, db.AssignJobParams{
		ID:      pgUUID(jobID),
		AgentID: pgUUID(agentID),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return AssignedJob{}, DeployAdmissionLost, nil
		}
		return AssignedJob{}, "", fmt.Errorf("store: assign deploy job: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return AssignedJob{}, "", fmt.Errorf("store: assign deploy job commit: %w", err)
	}
	return AssignedJob{
		ID:      fromPgUUID(row.ID),
		RunID:   fromPgUUID(row.RunID),
		AgentID: fromPgUUID(row.AgentID),
		Name:    row.Name,
		Attempt: row.Attempt,
	}, DeployAdmitted, nil
}

// guardDeployAdmissionNotFrozen is the native-path equivalent of the check
// above, run inside a caller-owned takeover tx. It returns ErrEnvironmentFrozen
// (rather than an outcome enum) because the native takeover already propagates
// errors, and the scheduler maps this ONE sentinel to a clean skip instead of a
// failed deploy.
func guardDeployAdmissionNotFrozen(ctx context.Context, tx pgx.Tx, q *db.Queries, projectID uuid.UUID, env string) error {
	if env == "" {
		// Defence in depth: a takeover with no environment name cannot be
		// checked against a name-keyed freeze table, and silently admitting it
		// would be a hole straight through the guarantee.
		return fmt.Errorf("store: native deploy admission: environment name is required")
	}
	if err := lockProjectEnvFreeze(ctx, tx, projectID, env); err != nil {
		return err
	}
	frozen, err := q.EnvironmentIsFrozen(ctx, db.EnvironmentIsFrozenParams{
		ProjectID: pgUUID(projectID),
		Name:      env,
	})
	if err != nil {
		return fmt.Errorf("store: native deploy freeze check: %w", err)
	}
	if frozen {
		return fmt.Errorf("%w: %s", ErrEnvironmentFrozen, env)
	}
	return nil
}
