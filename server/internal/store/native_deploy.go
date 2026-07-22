package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/gocdnext/gocdnext/server/internal/db"
)

// StartNativeDeployInput is the dispatch-time payload for a native deploy takeover
// (ADR-0001, Model A): everything needed to record the revision + watch and flip the
// job to server-managed running, atomically.
type StartNativeDeployInput struct {
	JobRunID      uuid.UUID
	EnvironmentID uuid.UUID
	RunID         uuid.UUID
	Version       string
	DeployedBy    string

	ProjectID        uuid.UUID
	SyncMode         string
	Cluster          string
	Application      string
	Namespace        string
	ExpectedRevision string
	DeadlineAt       time.Time

	// Rollout routing denormalized onto the watch so the watcher's targetOf rebuilds
	// a complete DeploymentTarget without re-reading the deploy_target.
	RolloutAware     bool
	RolloutCluster   string
	RolloutNamespace string
	RolloutName      string
	// GoverningGate is the target's gate config denormalized onto the watch at creation
	// — an immutable per-deploy snapshot (a mid-flight target edit must not change an
	// in-flight deploy's gate). nil => this deploy is not gated.
	GoverningGate *GoverningGate
}

// StartNativeDeployResult reports the takeover outcome. Started is false when the job
// wasn't dispatchable (another tick won the race) — the scheduler simply moves on,
// exactly like a lost AssignJob CAS.
type StartNativeDeployResult struct {
	Started    bool
	RevisionID uuid.UUID
	Attempt    int32
}

// StartNativeDeploy performs the invariant-preserving takeover in ONE tx: flip the
// job queued→running (no agent), then create the deployment_revision (in_progress)
// and the deploy_watch (pre-Sync). Committing all three together means the reaper
// never observes a running-no-agent job without its owning watch, and a crash after
// this commit but before Sync leaves a recoverable pre-Sync watch (the watcher
// deadline-fails it). Sync itself is issued by the caller OUTSIDE this tx (external
// I/O must not hold a DB tx/lock).
func (s *Store) StartNativeDeploy(ctx context.Context, in StartNativeDeployInput) (StartNativeDeployResult, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return StartNativeDeployResult{}, fmt.Errorf("store: start native deploy begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	res, err := startNativeDeployTx(ctx, q, in)
	if err != nil {
		return StartNativeDeployResult{}, err
	}
	if !res.Started {
		return res, nil
	}
	if err := tx.Commit(ctx); err != nil {
		return StartNativeDeployResult{}, fmt.Errorf("store: start native deploy commit: %w", err)
	}
	return res, nil
}

// startNativeDeployTx is the shared body of the takeover, run inside a caller-owned tx
// so the declared variant can assert its expectation on a LOCKED target row first. It
// never commits — the caller owns the transaction boundary.
func startNativeDeployTx(ctx context.Context, q *db.Queries, in StartNativeDeployInput) (StartNativeDeployResult, error) {
	// Flip the job to server-managed running. Same queued+unassigned CAS as AssignJob;
	// a lost race (ErrNoRows) → Started=false, no error.
	job, err := q.StartServerManagedJob(ctx, pgUUID(in.JobRunID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return StartNativeDeployResult{Started: false}, nil
		}
		return StartNativeDeployResult{}, fmt.Errorf("store: start server-managed job: %w", err)
	}

	// Promote the stage + run to running in the SAME tx (the agent path does this
	// after dispatch, but native never touches that path). Idempotent (guarded on
	// status='queued'), so a stage/run already running from a sibling job is a no-op.
	// Atomic with the job flip → the invariant "server-managed job running implies
	// stage/run running" holds, so serial gating (which keys on runs.status='running')
	// can't start another run while this deploy is in flight.
	if err := q.MarkStageRunningIfQueued(ctx, job.StageRunID); err != nil {
		return StartNativeDeployResult{}, fmt.Errorf("store: native deploy mark stage running: %w", err)
	}
	if err := q.MarkRunRunningIfQueued(ctx, job.RunID); err != nil {
		return StartNativeDeployResult{}, fmt.Errorf("store: native deploy mark run running: %w", err)
	}

	revID, err := q.CreateDeploymentRevision(ctx, db.CreateDeploymentRevisionParams{
		EnvironmentID: pgUUID(in.EnvironmentID),
		RunID:         nullableUUID(in.RunID),
		JobRunID:      nullableUUID(in.JobRunID),
		Attempt:       job.Attempt,
		Version:       in.Version,
		IsRollback:    false,
		DeployedBy:    in.DeployedBy,
	})
	if err != nil {
		return StartNativeDeployResult{}, fmt.Errorf("store: start native deploy revision: %w", err)
	}

	// The watch's WHERE EXISTS(in_progress revision) guard is satisfied by the row
	// just inserted above (same tx, same snapshot).
	watchParams := db.CreateDeployWatchParams{
		DeploymentRevisionID: revID,
		ProjectID:            pgUUID(in.ProjectID),
		SyncMode:             in.SyncMode,
		Cluster:              in.Cluster,
		Application:          in.Application,
		Namespace:            in.Namespace,
		ExpectedRevision:     in.ExpectedRevision,
		DeadlineAt:           pgTimestamptzFromPtr(&in.DeadlineAt),
		RolloutAware:         in.RolloutAware,
		RolloutCluster:       nullableString(in.RolloutCluster),
		RolloutNamespace:     nullableString(in.RolloutNamespace),
		RolloutName:          nullableString(in.RolloutName),
	}
	// Denormalize the gate config (immutable per-deploy snapshot). gate_required NULL
	// => not gated; only a rollout-aware deploy can be gated, but we persist whatever
	// the target carries and let the watcher key on rollout_aware.
	if g := in.GoverningGate; g != nil {
		req := int32(g.Required)
		watchParams.GateApprovers = g.Approvers
		watchParams.GateApproverGroups = g.ApproverGroups
		watchParams.GateRequired = &req
		watchParams.GateDescription = nullableString(g.Description)
	}
	if _, err := q.CreateDeployWatch(ctx, watchParams); err != nil {
		return StartNativeDeployResult{}, fmt.Errorf("store: start native deploy watch: %w", err)
	}
	return StartNativeDeployResult{Started: true, RevisionID: fromPgUUID(revID), Attempt: job.Attempt}, nil
}

// DeclaredTargetExpectation is the base target a pipeline declared, carried into the
// takeover so the tx can assert the row still matches before creating anything. Only the
// fields YAML owns — the rollout routing and the gate are ADOPTED from the locked row,
// because the file does not express them.
type DeclaredTargetExpectation struct {
	Environment string
	Cluster     string
	Application string
	Namespace   string
	SyncMode    string
}

// DeclaredTakeoverOutcome distinguishes the two ways a declared takeover can be refused.
// They must not collapse: a gate is a terminal config fault (fail the job loud), while a
// base mismatch or a vanished row is a benign race with a concurrent edit (retry, and
// let the next tick's reconcile decide who wins).
type DeclaredTakeoverOutcome string

const (
	DeclaredTakeoverStarted  DeclaredTakeoverOutcome = "started"
	DeclaredTakeoverLostRace DeclaredTakeoverOutcome = "lost_race" // job no longer dispatchable
	DeclaredTakeoverGated    DeclaredTakeoverOutcome = "gated"     // terminal
	DeclaredTakeoverDrifted  DeclaredTakeoverOutcome = "drifted"   // retry
)

// StartNativeDeployDeclared is StartNativeDeploy for a pipeline-DECLARED target: it
// locks the target row and asserts the declaration still holds INSIDE the takeover tx.
//
// Checking just before StartNativeDeploy would not be enough — that function never re-reads
// deploy_targets, so a governing_gate inserted between the check and the insert would
// still produce an ungated deploy. The window is small; it is also exactly the one an
// admin hits when gating a target that is mid-deploy.
//
// The target row is locked BEFORE the job flip so this tx does not hold job/stage/run
// locks while waiting on it.
func (s *Store) StartNativeDeployDeclared(ctx context.Context, in StartNativeDeployInput, want DeclaredTargetExpectation) (StartNativeDeployResult, DeclaredTakeoverOutcome, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return StartNativeDeployResult{}, "", fmt.Errorf("store: start declared native deploy begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	tgt, err := q.LockDeployTargetForDeploy(ctx, db.LockDeployTargetForDeployParams{
		ProjectID: pgUUID(in.ProjectID),
		Name:      want.Environment,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Deleted between the reconcile and here — retry; the next tick re-creates it.
			return StartNativeDeployResult{}, DeclaredTakeoverDrifted, nil
		}
		return StartNativeDeployResult{}, "", fmt.Errorf("store: lock deploy target: %w", err)
	}
	// A gate appeared: never start an UNGATED deploy of a target an admin just gated.
	if len(tgt.GoverningGate) > 0 {
		return StartNativeDeployResult{}, DeclaredTakeoverGated, nil
	}
	// Compare only what the file owns.
	if tgt.Cluster != want.Cluster || tgt.Application != want.Application ||
		tgt.Namespace != want.Namespace || tgt.SyncMode != want.SyncMode {
		return StartNativeDeployResult{}, DeclaredTakeoverDrifted, nil
	}

	// Everything below is built from the LOCKED row, not the caller's pre-lock snapshot:
	// an environment deleted-and-recreated would otherwise write a stale environment_id
	// (or trip the FK), and rollout routing changed since the reconcile must be adopted,
	// not overwritten with what we read earlier.
	in.EnvironmentID = fromPgUUID(tgt.EnvironmentID)
	in.SyncMode = tgt.SyncMode
	in.Cluster = tgt.Cluster
	in.Application = tgt.Application
	in.Namespace = tgt.Namespace
	in.RolloutAware = tgt.RolloutAware
	in.RolloutCluster = stringValue(tgt.RolloutCluster)
	in.RolloutNamespace = stringValue(tgt.RolloutNamespace)
	in.RolloutName = stringValue(tgt.RolloutName)
	in.GoverningGate = nil // asserted absent under the lock

	res, err := startNativeDeployTx(ctx, q, in)
	if err != nil {
		return StartNativeDeployResult{}, "", err
	}
	if !res.Started {
		return res, DeclaredTakeoverLostRace, nil
	}
	if err := tx.Commit(ctx); err != nil {
		return StartNativeDeployResult{}, "", fmt.Errorf("store: start declared native deploy commit: %w", err)
	}
	return res, DeclaredTakeoverStarted, nil
}
