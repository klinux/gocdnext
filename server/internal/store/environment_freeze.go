package store

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/gocdnext/gocdnext/server/internal/db"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

// FreezeQueueReasonPrefix is the runs.queue_reason key the scheduler stamps on a
// run held by an environment freeze. The full value is
// `frozen-deploy:<environment>` — the vocabulary migration 00034 reserved.
// Exported because the scheduler composes it and the UI parses it.
const FreezeQueueReasonPrefix = "frozen-deploy:"

// AuthDisabledActorEmail is the documented sentinel recorded as frozen_by (and as
// the audit actor_email) when there is no authenticated user — RequireMinRole
// passes with no user when auth is disabled, and a freeze row must never carry an
// empty or client-supplied actor.
const AuthDisabledActorEmail = "auth-disabled@local"

// Bounds mirrored from the environment_freezes CHECK constraints (00078). They
// are re-asserted in Go so a too-long value surfaces as a typed validation error
// the API maps to 400, instead of a constraint violation the API maps to 500.
// Counted in RUNES, exactly like the SQL char_length() they mirror.
const (
	freezeActorMaxRunes  = 320
	freezeReasonMaxRunes = 500
)

// Freeze sentinels. Callers map them to HTTP statuses.
var (
	// ErrEnvironmentFrozen is returned by every ADMISSION path — dispatch,
	// native takeover, approval, rollback — when the target environment is
	// frozen. It is an EXPECTED outcome, never an infrastructure failure: the
	// scheduler treats it as "skip this tick and retry", the API as 409.
	//
	// Deliberately unprefixed (unlike most store errors): the approval handler
	// surfaces the wrapped message verbatim, and it names only environment
	// names — operator-facing, non-sensitive, and already visible to anyone who
	// can read the environments list.
	ErrEnvironmentFrozen = errors.New("environment is frozen")

	// ErrFreezeNameInvalid: the name isn't a legal deploy environment name.
	ErrFreezeNameInvalid = errors.New("store: invalid environment name")

	// ErrFreezeReasonInvalid: empty (or whitespace-only) or over the bound.
	ErrFreezeReasonInvalid = errors.New("store: invalid freeze reason")

	// ErrFreezeActorUnusable: none of email/name/id yielded a value that fits
	// frozen_by. Raised BEFORE the insert so the DB CHECK is never the thing
	// that reports it — a 500 on "your IdP email is 900 chars" is unhelpful.
	ErrFreezeActorUnusable = errors.New("store: cannot derive a freeze actor")
)

// ProjectEnvFreezeLockKey is the advisory-lock key serialising a freeze/unfreeze
// against every ADMISSION of a deploy to that (project, environment).
//
// It is deliberately NOT LaneEnvLockKey: that key is (pipeline, lane, ref, env)
// — the wrong grain, since a freeze crosses every pipeline in the project. It
// also lives in its OWN namespace (the "envfreeze:" prefix below), distinct from
// the gate-pass, rollup and compliance keys, so an accidental collision can't
// make two unrelated subsystems serialise against each other.
//
// MANDATORY GLOBAL LOCK ORDER (deadlock-safety — read before adding a caller):
//
//	(1) LaneEnvLockKey(s), in `name` order
//	(2) ProjectEnvFreezeLockKey(s), in `name` order
//	(3) row-level FOR UPDATE / mutations
//
// Dispatch already fits this shape (lane guard, then freeze at assignment);
// approval and rollback were reordered to match. Taking them the other way round
// deadlocks approval (freeze -> lane, via writeGatePassMarkers) against dispatch
// (lane -> freeze), and Postgres may NOT detect it: the lane lock is a
// SESSION-level lock held on a different connection, so the wait graph is
// invisible to the deadlock detector — the symptom is hung requests and a
// drained connection pool, not a clean error.
//
// Postgres advisory *xact* locks are reentrant within a transaction, so a path
// that re-takes a lock it already holds (writeGatePassMarkers re-taking the lane
// lock after approval took it) is a harmless no-op.
func ProjectEnvFreezeLockKey(projectID uuid.UUID, env string) int64 {
	h := fnv.New64a()
	// Namespace prefix first: a distinct byte prefix keeps this key space
	// disjoint from LaneEnvLockKey's (which starts with the lane mode).
	_, _ = h.Write([]byte("envfreeze:"))
	_, _ = h.Write(projectID[:])
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(env))
	return int64(h.Sum64())
}

// FreezeActor is the CANONICAL identity of whoever froze or unfroze, built by the
// API from the authenticated store.User. It is never assembled from a client
// string: frozen_by is quoted back to every operator looking at a blocked
// production deploy, and the audit row derives its actor from the same struct.
//
// store.User has ID / Email / Name (there is no Login), so those are the only
// three candidates.
type FreezeActor struct {
	ID    uuid.UUID
	Email string
	Name  string
}

// FreezeActorFromUser builds the actor from an authenticated user.
func FreezeActorFromUser(u User) FreezeActor {
	return FreezeActor{ID: u.ID, Email: u.Email, Name: u.Name}
}

// SystemFreezeActor is the auth-disabled fallback. RequireMinRole passes with no
// user when auth is off, so both freeze and unfreeze must still record SOMETHING
// canonical rather than an empty frozen_by.
func SystemFreezeActor() FreezeActor {
	return FreezeActor{Email: AuthDisabledActorEmail}
}

// label picks the value stored in frozen_by: the first of email -> name -> id
// that is non-empty AFTER trimming and fits the column bound.
//
// Trim-then-test, not test-then-trim: OIDC claims are not normalised by the
// provider and are persisted raw, so a whitespace-only email must FALL THROUGH
// to name/id rather than being selected and then rejected by the CHECK. An
// oversized email falls through the same way. A uuid is always 36 chars, so the
// id candidate can only fail when the actor carries no id at all — the
// auth-disabled sentinel supplies an email instead, so in practice this only
// returns an error for a malformed internal caller.
func (a FreezeActor) label() (string, error) {
	for _, candidate := range []string{a.Email, a.Name} {
		c := strings.TrimSpace(candidate)
		if c != "" && utf8.RuneCountInString(c) <= freezeActorMaxRunes {
			return c, nil
		}
	}
	if a.ID != uuid.Nil {
		return a.ID.String(), nil
	}
	return "", ErrFreezeActorUnusable
}

// EnvironmentFreeze is the freeze state of one environment.
type EnvironmentFreeze struct {
	Name     string
	FrozenAt time.Time
	// FrozenBy is the canonical actor label (see FreezeActor.label). Sensitive
	// enough to redact for viewers at the API boundary, like FreezeReason.
	FrozenBy string
	Reason   string
}

// NormalizeEnvironmentName trims and validates an environment name against the
// grammar the pipeline parser and the deploy-target registry use.
//
// Exported because the API must normalise ONCE, up front, and then use the
// result everywhere: the store trims internally, so a handler that keeps the raw
// path segment would freeze `production` but look for runs held by
// `frozen-deploy: production ` — the freeze works, the immediate wake silently
// misses every run, and only the periodic tick recovers.
func NormalizeEnvironmentName(name string) (string, error) {
	return normalizeFreezeName(name)
}

// normalizeFreezeName trims and validates an environment name against the same
// grammar the pipeline parser and the deploy-target registry use. Re-validated
// in the store and not only at the API because internal and test callers bypass
// the handler — defence in depth, and it keeps a '/' out of a name the DELETE
// route carries in its path.
func normalizeFreezeName(name string) (string, error) {
	n := strings.TrimSpace(name)
	if !domain.ValidEnvironmentName(n) {
		return "", fmt.Errorf("%w: %q", ErrFreezeNameInvalid, n)
	}
	return n, nil
}

func normalizeFreezeReason(reason string) (string, error) {
	r := strings.TrimSpace(reason)
	if r == "" {
		return "", fmt.Errorf("%w: reason is required", ErrFreezeReasonInvalid)
	}
	if utf8.RuneCountInString(r) > freezeReasonMaxRunes {
		return "", fmt.Errorf("%w: reason exceeds %d characters", ErrFreezeReasonInvalid, freezeReasonMaxRunes)
	}
	return r, nil
}

// freezeAuditTargetID is the audit row's target_id for a freeze event:
// "<project_id>:<name>". Keyed by NAME rather than environments.id because a
// freeze legitimately exists with no environments row (pre-emptive or orphan
// freeze), and an audit trail that can't identify half its subjects is not a
// trail. Stable and filterable — combined with target_type="environment" it
// scopes an investigation to one project's freezes.
func freezeAuditTargetID(projectID uuid.UUID, name string) string {
	return projectID.String() + ":" + name
}

// FreezeEnvironment freezes (project, name) and records the audit event in the
// SAME transaction.
//
// The atomicity is the point: audit.Emit is best-effort by design (a failed
// audit write must not roll back a successful deploy), but freeze history cannot
// be best-effort — "production was frozen for six hours and nobody knows who did
// it" is exactly the failure this feature exists to prevent. So the mutation and
// its audit row commit together or not at all.
//
// Returns froze=false when the environment was ALREADY frozen: idempotent, and
// deliberately non-destructive — the original frozen_at/frozen_by/reason are
// preserved (ON CONFLICT DO NOTHING) and no second audit event is emitted.
//
// The advisory lock taken here is what makes the guarantee true: once this
// commits, no deploy to that environment can be admitted, because every
// admission path takes the same lock and re-reads the table under it.
func (s *Store) FreezeEnvironment(ctx context.Context, projectID uuid.UUID, name string, actor FreezeActor, reason string) (bool, error) {
	env, err := normalizeFreezeName(name)
	if err != nil {
		return false, err
	}
	r, err := normalizeFreezeReason(reason)
	if err != nil {
		return false, err
	}
	who, err := actor.label()
	if err != nil {
		return false, err
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return false, fmt.Errorf("store: freeze environment begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := lockProjectEnvFreeze(ctx, tx, projectID, env); err != nil {
		return false, err
	}

	q := s.q.WithTx(tx)
	if _, err := q.InsertEnvironmentFreeze(ctx, db.InsertEnvironmentFreezeParams{
		ProjectID: pgUUID(projectID),
		Name:      env,
		FrozenBy:  who,
		Reason:    r,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Already frozen. Commit the (empty) tx so the lock is released
			// promptly rather than waiting on the deferred rollback.
			if cerr := tx.Commit(ctx); cerr != nil {
				return false, fmt.Errorf("store: freeze environment commit: %w", cerr)
			}
			return false, nil
		}
		return false, fmt.Errorf("store: freeze environment %q: %w", env, err)
	}

	if err := insertFreezeAudit(ctx, q, AuditActionEnvironmentFreeze, projectID, env, actor, who,
		map[string]any{"project_id": projectID.String(), "name": env, "reason": r}); err != nil {
		return false, err
	}

	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("store: freeze environment commit: %w", err)
	}
	return true, nil
}

// UnfreezeEnvironment lifts the freeze on (project, name), audited in the same
// transaction — unfreezing is exactly as audit-sensitive as freezing, so it takes
// the same structured actor rather than a label.
//
// Returns thawed=false when it wasn't frozen (idempotent; no audit event). There
// is no EnvironmentBelongsToProject precheck: the project_id predicate IS the
// scope check, and a precheck would be actively wrong — an orphan freeze has no
// environments row to check against, and refusing to lift it would leave a
// permanently un-unfreezable environment.
func (s *Store) UnfreezeEnvironment(ctx context.Context, projectID uuid.UUID, name string, actor FreezeActor) (bool, error) {
	env, err := normalizeFreezeName(name)
	if err != nil {
		return false, err
	}
	who, err := actor.label()
	if err != nil {
		return false, err
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return false, fmt.Errorf("store: unfreeze environment begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := lockProjectEnvFreeze(ctx, tx, projectID, env); err != nil {
		return false, err
	}

	q := s.q.WithTx(tx)
	row, err := q.DeleteEnvironmentFreeze(ctx, db.DeleteEnvironmentFreezeParams{
		ProjectID: pgUUID(projectID),
		Name:      env,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			if cerr := tx.Commit(ctx); cerr != nil {
				return false, fmt.Errorf("store: unfreeze environment commit: %w", cerr)
			}
			return false, nil
		}
		return false, fmt.Errorf("store: unfreeze environment %q: %w", env, err)
	}

	// The lifted freeze's own metadata rides along: "who froze it, and for how
	// long" is the question asked right after an unfreeze, and the row is gone
	// by the time anyone reads the audit log.
	if err := insertFreezeAudit(ctx, q, AuditActionEnvironmentUnfreeze, projectID, env, actor, who,
		map[string]any{
			"project_id":    projectID.String(),
			"name":          env,
			"was_frozen_by": row.FrozenBy,
			"was_frozen_at": row.FrozenAt.Time.UTC().Format(time.RFC3339),
			"reason":        row.Reason,
		}); err != nil {
		return false, err
	}

	// Stamp the freeze-epoch floor (#208): a REAL unfreeze grants a fresh
	// approval-expiry window so lifting a change-freeze doesn't instantly cancel
	// the gates it was holding. Under the freeze advisory lock already held above,
	// so an expiry racing this reads a floor consistent with the freeze state it
	// checks under the same key. Only on the delete-succeeded path — an idempotent
	// unfreeze (the ErrNoRows branch above) returns before here and never renews.
	if err := q.UpsertFreezeEpoch(ctx, db.UpsertFreezeEpochParams{
		ProjectID:   pgUUID(projectID),
		Environment: env,
	}); err != nil {
		return false, fmt.Errorf("store: unfreeze environment epoch: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("store: unfreeze environment commit: %w", err)
	}
	return true, nil
}

// lockProjectEnvFreeze takes the per-(project, env) freeze lock for the rest of
// the transaction. See ProjectEnvFreezeLockKey for the mandatory global order.
func lockProjectEnvFreeze(ctx context.Context, tx pgx.Tx, projectID uuid.UUID, env string) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`,
		ProjectEnvFreezeLockKey(projectID, env)); err != nil {
		return fmt.Errorf("store: environment freeze lock: %w", err)
	}
	return nil
}

// insertFreezeAudit writes the freeze/unfreeze audit row on the SAME tx-bound
// queries handle as the mutation, so a failure here aborts the whole thing. The
// actor is the canonical one: actor_id from the authenticated user (NULL for the
// auth-disabled sentinel, via nullableUUID) and actor_email the exact string
// stored in frozen_by, so the row and the log agree.
func insertFreezeAudit(ctx context.Context, q *db.Queries, action string, projectID uuid.UUID, env string, actor FreezeActor, who string, metadata map[string]any) error {
	meta, err := marshalAuditMetadata(metadata)
	if err != nil {
		return fmt.Errorf("store: %s audit metadata: %w", action, err)
	}
	if _, err := q.InsertAuditEvent(ctx, db.InsertAuditEventParams{
		ActorID:    nullableUUID(actor.ID),
		ActorEmail: who,
		Action:     action,
		TargetType: "environment",
		TargetID:   freezeAuditTargetID(projectID, env),
		Metadata:   meta,
	}); err != nil {
		return fmt.Errorf("store: %s audit: %w", action, err)
	}
	return nil
}
