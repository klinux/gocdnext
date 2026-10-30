package store

// The approval allow-list check shared by the job gate (decideGate) and the rollout
// gate (DecideRolloutGate). One implementation so the two engines can't drift on WHO
// may decide — design in adr/0002-approval-identity-and-group-resolution.md (#172).

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/gocdnext/gocdnext/server/internal/db"
	"github.com/gocdnext/gocdnext/server/pkg/domain"
)

// ProviderServiceAccount is the Provider the auth middleware stamps on the synthetic
// User it builds from a service-account API token.
const ProviderServiceAccount = "service_account"

// ErrApproverNotHuman signals a machine identity (service account) tried to decide a
// gate. A gate exists to answer "which human authorised this"; a service token voting
// would make quorum count the machine and the audit name no one. Callers map it to 403.
var ErrApproverNotHuman = errors.New("store: approvals require a human identity")

// ErrApproverGroupsUnavailable signals the external group source couldn't answer at
// vote time. Fail-closed: the vote is refused rather than decided against a stale or
// partial membership. Callers map it to 503 (retryable).
var ErrApproverGroupsUnavailable = errors.New("store: approver group membership unavailable")

// ApproverIdentity is the acting identity a gate's allow-list is evaluated against.
type ApproverIdentity struct {
	UserID uuid.UUID
	// Label (name-preferred) and Email match plain `approvers:` entries.
	Label string
	Email string
	// Provider + Subject (users.provider / users.external_id — the IdP's stable `sub`)
	// match `sub:<provider>:<subject>` entries, and are what an external group source
	// resolves membership by.
	Provider string
	Subject  string
}

// ApproverGroupSource resolves a voter's group names from a source outside gocdnext
// (the IdP, an embedding platform) AT DECISION TIME, so a membership revoked after
// login isn't honoured from a login-time snapshot. It is additive to the local
// group_members table, consulted only when the local table didn't already grant.
//
// The call runs BEFORE the decision transaction opens (resolveExternalApproverGroups),
// never inside it: a slow source must not pin a pooled connection or hold the gate's
// row lock. Implementations own their timeout, and must return an error rather than
// an empty list when they couldn't resolve: an empty list means "member of nothing",
// an error refuses the vote.
type ApproverGroupSource interface {
	ApproverGroupNames(ctx context.Context, who ApproverIdentity) ([]string, error)
}

// WithApproverGroupSource enables external group resolution for approval gates. Nil
// (the default) keeps the local group table as the only source. Set at wiring time,
// before the store serves requests.
func (s *Store) WithApproverGroupSource(src ApproverGroupSource) *Store {
	s.approverGroups = src
	return s
}

// authorizeApprover decides whether `who` may vote on a gate with the given
// allow-list. Order, cheapest first:
//
//  1. a service account is never an approver (even on an open gate);
//  2. empty approvers AND groups = any authenticated human;
//  3. listed in `approvers` (subject ref, or name/email for plain entries);
//  4. member of a listed group in the local table (by user id);
//  5. member of a listed group per `external` — the names resolveExternalApproverGroups
//     fetched before the transaction (nil when no source, or it wasn't needed).
//
// q must be the caller's tx-bound queries: decideGate runs everything on one tx.
func (s *Store) authorizeApprover(ctx context.Context, q *db.Queries, who ApproverIdentity, approvers, groups, external []string) error {
	if who.Provider == ProviderServiceAccount {
		return ErrApproverNotHuman
	}
	if len(approvers) == 0 && len(groups) == 0 {
		return nil
	}
	if approverListed(who, approvers) {
		return nil
	}
	// Group membership keys on a real user; anonymous (dev-mode) callers only
	// get the string match above.
	if len(groups) == 0 || who.UserID == uuid.Nil {
		return ErrApproverNotAllowed
	}
	local, err := q.ListUserGroupNames(ctx, pgUUID(who.UserID))
	if err != nil {
		return fmt.Errorf("store: load user groups: %w", err)
	}
	if intersects(local, groups) || intersects(external, groups) {
		return nil
	}
	return ErrApproverNotAllowed
}

// resolveExternalApproverGroups asks the external source for `who`'s groups, OUTSIDE
// any transaction, and only when the answer can matter: a source is configured, the
// gate lists groups, `who` is a human not already named in `approvers`, and the local
// table doesn't already grant. Otherwise it returns nil without calling the source, so
// votes the source isn't needed for keep working while it's down.
//
// The allow-list passed here is a pre-transaction read; authorizeApprover re-evaluates
// against the locked read. Both gate configs are immutable snapshots (job_runs rows,
// deploy_watches gate columns), and `external` is only membership data — a stale list
// here can't grant anything the locked check wouldn't.
//
// Source error = fail closed: ErrApproverGroupsUnavailable, before any vote is written.
func (s *Store) resolveExternalApproverGroups(ctx context.Context, who ApproverIdentity, approvers, groups []string) ([]string, error) {
	if s.approverGroups == nil || len(groups) == 0 || who.UserID == uuid.Nil ||
		who.Provider == ProviderServiceAccount || approverListed(who, approvers) {
		return nil, nil
	}
	// This duplicates authorizeApprover's in-tx lookup on purpose: here it only spares
	// the external call when the local table already grants; the in-tx read stays the
	// authoritative one. It runs only on the external path (source wired, groups
	// listed, voter not named), so ordinary votes pay for one lookup, not two.
	local, err := s.q.ListUserGroupNames(ctx, pgUUID(who.UserID))
	if err != nil {
		return nil, fmt.Errorf("store: load user groups: %w", err)
	}
	if intersects(local, groups) {
		return nil, nil
	}
	external, err := s.approverGroups.ApproverGroupNames(ctx, who)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrApproverGroupsUnavailable, err)
	}
	return external, nil
}

// approverListed reports whether `who` is named by an `approvers:` entry. A subject
// ref matches only the identity's own provider+subject; plain entries match the
// display label or the email (#51). Never cross-matched — see
// domain.ApproverSubjectPrefix for the spoofing this prevents.
func approverListed(who ApproverIdentity, approvers []string) bool {
	ref := domain.ApproverSubjectRef(who.Provider, who.Subject)
	for _, a := range approvers {
		if strings.HasPrefix(a, domain.ApproverSubjectPrefix) {
			if ref != "" && a == ref {
				return true
			}
			continue
		}
		if (who.Label != "" && a == who.Label) || (who.Email != "" && a == who.Email) {
			return true
		}
	}
	return false
}

// externalGroupsForJobGate reads a pending job gate's allow-list WITHOUT a transaction
// and resolves the external groups it may need. Free when no source is configured. A
// missing / decided gate resolves nothing — the transaction reports it properly.
func (s *Store) externalGroupsForJobGate(ctx context.Context, jobRunID uuid.UUID, who ApproverIdentity) ([]string, error) {
	if s.approverGroups == nil {
		return nil, nil
	}
	var approvers, groups []string
	err := s.pool.QueryRow(ctx, `
		SELECT approvers, approver_groups FROM job_runs
		WHERE id = $1 AND approval_gate AND status = 'awaiting_approval'
	`, jobRunID).Scan(&approvers, &groups)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: pre-read approval allow-list: %w", err)
	}
	return s.resolveExternalApproverGroups(ctx, who, approvers, groups)
}

// externalGroupsForRolloutGate is externalGroupsForJobGate for an armed, undecided
// rollout gate (allow-list denormalized on the deploy_watch).
func (s *Store) externalGroupsForRolloutGate(ctx context.Context, revisionID, projectID uuid.UUID, who ApproverIdentity) ([]string, error) {
	if s.approverGroups == nil {
		return nil, nil
	}
	var approvers, groups []string
	err := s.pool.QueryRow(ctx, `
		SELECT gate_approvers, gate_approver_groups FROM deploy_watches
		WHERE deployment_revision_id = $1 AND project_id = $2
		  AND gate_id IS NOT NULL AND gate_decision IS NULL
	`, revisionID, projectID).Scan(&approvers, &groups)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: pre-read rollout gate allow-list: %w", err)
	}
	return s.resolveExternalApproverGroups(ctx, who, approvers, groups)
}

func intersects(have, want []string) bool {
	for _, h := range have {
		if slices.Contains(want, h) {
			return true
		}
	}
	return false
}
